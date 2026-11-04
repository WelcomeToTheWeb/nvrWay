// The signal path's half of card S6-06.
//
// `webhooks.Deliverer`'s own suite proves what a delivery does: the signature, the
// ladder, one row per send. What lives here is the *call* — that the route the
// worker posts to hands a closed event to the notifier, and hands an open or an
// extend to nothing, because an event that is still moving is not a fact anyone
// should be notified about yet.
//
// The notifier is the real one, not a spy, because the thing worth proving across
// the boundary is that the bytes which leave this process still verify: the signal
// path builds the event row, the deliverer builds the payload from it, and a
// receiver that checks the signature is the only test that spans both.

package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nvr/server/models"
	"nvr/server/storage"
	"nvr/server/store"
	"nvr/server/webhooks"
)

// notifierFixture is the worker's signal route with a real deliverer pointed at a
// receiver that records what arrived.
type notifierFixture struct {
	*detectorFixture

	// received holds one entry per POST, each the raw body and the signature the
	// request carried. Guarded because the handler runs on the receiver's
	// goroutine while the test reads it.
	mu       sync.Mutex
	received []capturedDelivery
	server   *httptest.Server
	hook     *models.Webhook
}

type capturedDelivery struct {
	body      []byte
	signature string
}

// notifierSecret is what an operator typed into the secret field.
const notifierSecret = "the-signing-secret"

func newNotifierFixture(t *testing.T) *notifierFixture {
	t.Helper()

	f := &notifierFixture{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.received = append(f.received, capturedDelivery{body: body, signature: r.Header.Get(webhooks.SignatureHeader)})
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.server.Close)

	f.detectorFixture = newDetectorFixture(t, testWorkerToken)

	sealKey, err := f.Handlers.Cfg.SealKey()
	if err != nil {
		t.Fatalf("SealKey: %v", err)
	}
	notifier := webhooks.New(f.Store, "http://nvr.lan:8080", sealKey)
	// The plan's ladder is 2 s / 10 s / 60 s and its own suite asserts those
	// numbers; a wiring test that slept through them would be a slow wiring test.
	notifier.Waits = []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}
	notifier.Log = func(string, ...any) {}
	f.Handlers.Webhooks = notifier
	t.Cleanup(notifier.Close)

	// The signing secret is stored sealed, the way S7-02 stores every credential
	// this server keeps. The receiver verifies with the plaintext, which is what the
	// operator typed and what no route ever sends back.
	sealed, err := storage.Seal(notifierSecret, sealKey)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	f.hook = &models.Webhook{
		Name:    "home-automation",
		URL:     f.server.URL,
		Secret:  sealed,
		Enabled: true,
		Events:  `["event.close"]`,
	}
	if err := f.Store.Webhooks.Create(f.hook); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	return f
}

func (f *notifierFixture) deliveries() []capturedDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedDelivery{}, f.received...)
}

func (f *notifierFixture) addEndpointCamera(t *testing.T) *models.Camera {
	t.Helper()
	return f.addCamera(t, "porch", true, true, models.RecordModeContinuous,
		`[{"id":"z1","name":"front step","polygon":[[0,0],[1,0],[1,1],[0,1]],`+
			`"classes":["person"],"rule":"loiter","dwell_seconds":30,"severity":3,`+
			`"min_confidence":0.4,"min_area_ratio":0.004,"enabled":true}]`)
}

// TestEventCloseIsTheOnlySignalThatNotifies walks one event through open, extend
// and close over the route the worker posts to.
func TestEventCloseIsTheOnlySignalThatNotifies(t *testing.T) {
	f := newNotifierFixture(t)
	cam := f.addEndpointCamera(t)

	key := "webhook-wiring-1"
	for _, action := range []string{models.SignalActionOpen, models.SignalActionExtend, models.SignalActionClose} {
		s := signal(cam.ID, key, action)
		status, _, raw := postSignals(t, f.detectorFixture, []map[string]any{s}, testWorkerToken)
		if status != http.StatusOK {
			t.Fatalf("%s answered %d: %s", action, status, raw)
		}
	}
	f.Handlers.Webhooks.Wait()

	// The row the signals filed, read back the way the API reads it: the payload
	// names the event, so the test needs the id the store generated.
	event, err := f.Store.Events.GetByKey(key)
	if err != nil {
		t.Fatalf("read the event back: %v", err)
	}

	got := f.deliveries()
	if len(got) != 1 {
		t.Fatalf("the endpoint received %d POSTs, want exactly 1: the close", len(got))
	}

	// The signature still verifies at the far end of the process. This is the check
	// `scripts/verify.sh` step 9 performs from outside, performed here from the
	// inside: the receiver recomputes the HMAC over the bytes it read.
	mac := hmac.New(sha256.New, []byte(notifierSecret))
	mac.Write(got[0].body)
	want := webhooks.SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
	if got[0].signature != want {
		t.Errorf("the receiver recomputed %s and the request carried %s", want, got[0].signature)
	}

	var payload map[string]any
	if err := json.Unmarshal(got[0].body, &payload); err != nil {
		t.Fatalf("the payload is not JSON: %v\n%s", err, got[0].body)
	}
	if payload["id"] != event.ID {
		t.Errorf("the payload names event %v, want %s", payload["id"], event.ID)
	}
	if payload["camera_slug"] != "porch" {
		t.Errorf("camera_slug is %v, want the slug of the camera that produced the event", payload["camera_slug"])
	}
	// The zone name is resolved from the camera row, which is why the deliverer is
	// handed the camera and not just the event: the row stores the id.
	if zones := fmt.Sprint(payload["zones"]); zones != "[front step]" {
		t.Errorf("zones is %s, want the name the operator typed", zones)
	}
	// The clip URL is present because this camera records, so a clip will exist for
	// its events; the snapshot URL is absent because this signal carried no frame,
	// and an empty field is how the payload says "no picture" without making the
	// receiver fetch a 404.
	if clip, _ := payload["clip_url"].(string); !strings.HasPrefix(clip, "http://nvr.lan:8080/api/v1/events/") {
		t.Errorf("clip_url is %v, want the server's own route under its public base", clip)
	}
	if snapshot, _ := payload["snapshot_url"].(string); snapshot != "" {
		t.Errorf("snapshot_url is %q for an event with no picture, want the empty string", snapshot)
	}

	// And the history shows the send, which is what the Settings page reads.
	rows, err := f.Store.WebhookDeliveries.ListForWebhook(f.hook.ID, "", 10, 0)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the history holds %d rows, want the one send", len(rows))
	}
	if rows[0].Status != models.DeliveryStatusDelivered {
		t.Errorf("the send is recorded %q, want %q", rows[0].Status, models.DeliveryStatusDelivered)
	}
}

func TestASignalPathWithNoNotifierStillFilesEvents(t *testing.T) {
	// `Webhooks` nil is a stated state, not a crash: a process built without the
	// notifier still accepts signals and files events.
	f := newNotifierFixture(t)
	f.Handlers.Webhooks = nil
	cam := f.addEndpointCamera(t)

	key := "webhook-wiring-2"
	for _, action := range []string{models.SignalActionOpen, models.SignalActionClose} {
		s := signal(cam.ID, key, action)
		status, _, raw := postSignals(t, f.detectorFixture, []map[string]any{s}, testWorkerToken)
		if status != http.StatusOK {
			t.Fatalf("%s answered %d: %s", action, status, raw)
		}
	}

	events, err := f.Store.Events.List(store.EventFilter{CameraID: cam.ID}, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("the event list holds %d rows, want the one event the signals filed", len(events))
	}
}

func TestADisabledEndpointHearsNothingFromTheSignalPath(t *testing.T) {
	// The exit criterion seen from the route: the close is applied, the endpoint is
	// disabled, and no request leaves the process.
	f := newNotifierFixture(t)
	cam := f.addEndpointCamera(t)

	// The fixture's endpoint is turned off rather than replaced: the row is the one
	// the deliverer reads, and `enabled` is the gate.
	if err := f.Store.DB.Model(&models.Webhook{}).Where("id = ?", f.hook.ID).
		Update("enabled", false).Error; err != nil {
		t.Fatalf("disable: %v", err)
	}

	key := "webhook-wiring-3"
	s := signal(cam.ID, key, models.SignalActionClose)
	if status, _, raw := postSignals(t, f.detectorFixture, []map[string]any{s}, testWorkerToken); status != http.StatusOK {
		t.Fatalf("close answered %d: %s", status, raw)
	}
	f.Handlers.Webhooks.Wait()

	if got := f.deliveries(); len(got) != 0 {
		t.Errorf("a disabled endpoint received %d POSTs, want none", len(got))
	}
	if rows, err := f.Store.WebhookDeliveries.ListForWebhook(f.hook.ID, "", 10, 0); err != nil {
		t.Fatalf("read deliveries: %v", err)
	} else if len(rows) != 0 {
		t.Errorf("a disabled endpoint left %d delivery rows, want none", len(rows))
	}
}
