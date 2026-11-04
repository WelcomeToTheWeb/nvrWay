package webhooks

// S6-06 — signed delivery, three attempts, one row per send.
//
// The three exit criteria are the signature recomputed byte-for-byte by the
// receiver, a disabled endpoint sending nothing, and status/attempt/error
// recorded per send. Everything else here is the shape of the payload the plan
// promises and the two ways a delivery can be cancelled.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"nvr/server/models"
	"nvr/server/storage"
	"nvr/server/store"
)

// receiver is the endpoint under test: it records every request and answers with
// what the test told it to. The answer is a function of the attempt number, which
// is how "500, 500, 200" is expressed without a mock framework.
type receiver struct {
	server *httptest.Server

	mu       sync.Mutex
	attempts []capturedRequest
	answer   func(n int) (status int, body string)

	// gate, when set, is taken before the handler answers. A test that needs the
	// caller to still be inside `NotifyClose` holds it.
	gate chan struct{}
}

type capturedRequest struct {
	body   []byte
	header http.Header
}

func newReceiver(t *testing.T, answer func(n int) (int, string)) *receiver {
	t.Helper()
	r := &receiver{answer: answer}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.gate != nil {
			<-r.gate
		}
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.attempts = append(r.attempts, capturedRequest{body: body, header: req.Header.Clone()})
		n := len(r.attempts)
		r.mu.Unlock()

		status, text := 200, ""
		if r.answer != nil {
			status, text = r.answer(n)
		}
		w.WriteHeader(status)
		if text != "" {
			_, _ = io.WriteString(w, text)
		}
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.attempts)
}

func (r *receiver) attempt(n int) capturedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > len(r.attempts) {
		return capturedRequest{}
	}
	return r.attempts[n-1]
}

// fixture is a database, a deliverer with a millisecond ladder, and one event with
// one camera.
type fixture struct {
	st     *store.Store
	d      *Deliverer
	camera *models.Camera
	event  *models.Event

	// secret is what an operator typed. The column holds it sealed, the way every
	// credential this server stores is kept (S7-02), and a receiver verifies with
	// the plaintext — so a test needs both.
	secret string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := st.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	camera := &models.Camera{
		Slug:       "front-door",
		Name:       "Front door",
		SourceURI:  "rtsp://cam/front-door",
		RecordMode: models.RecordModeContinuous,
		ZonesJSON:  `[{"id":"zone-1","name":"porch","polygon":[[0,0],[1,0],[1,1]],"classes":["person"],"rule":"loiter","dwell_seconds":30,"severity":2,"min_confidence":0.4,"min_area_ratio":0.004,"enabled":true}]`,
		Active:     true,
	}
	if err := st.Cameras.Create(camera); err != nil {
		t.Fatalf("create camera: %v", err)
	}

	started := time.Date(2026, 11, 4, 9, 15, 3, 400000000, time.UTC)
	event := &models.Event{
		// A direct DB.Create does not generate the primary key the way the store's
		// upsert does, and an empty id would make the row unreachable afterwards.
		ID:                uuid.NewString(),
		EventKey:          "cam:zone:loiter:1762247703",
		CameraID:          camera.ID,
		Action:            models.SignalActionClose,
		StartedAt:         started,
		EndedAt:           started.Add(42 * time.Second),
		Open:              false,
		ClassesJSON:       `["person","dog"]`,
		ZonesJSON:         `["zone-1"]`,
		RulesJSON:         `["loiter"]`,
		Severity:          3,
		LocalSnapshotPath: "/var/lib/nvr/snapshots/e.jpg",
	}
	if err := st.DB.Create(event).Error; err != nil {
		t.Fatalf("create event: %v", err)
	}

	d := New(st, "http://nvr.lan:8080", sealKey())
	d.Waits = []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}
	d.Log = func(format string, args ...any) {
		fmt.Fprintf(io.Discard, format, args...)
	}
	t.Cleanup(d.Close)

	return &fixture{st: st, d: d, camera: camera, event: event, secret: "a-secret-nobody-sees-again"}
}

// sealKey is this suite's seal key, derived the way `config.SealKey` derives one
// when NVR_SECRET_KEY is unset.
func sealKey() [32]byte {
	return [32]byte(sha256.Sum256([]byte("unit-test-seal-key")))
}

// addHook puts an endpoint in the table pointing at the receiver. The signing
// secret is stored sealed, because that is what the column holds in a running
// installation: the seal is the integration with S7-02, and a deliverer that signed
// the sealed text instead would fail every receiver's check.
func (f *fixture) addHook(t *testing.T, url string, mutate func(*models.Webhook)) *models.Webhook {
	t.Helper()
	sealed, err := storage.Seal(f.secret, sealKey())
	if err != nil {
		t.Fatalf("sealing the signing secret: %v", err)
	}
	hook := &models.Webhook{
		Name:    fmt.Sprintf("endpoint-%d", time.Now().UnixNano()),
		URL:     url,
		Secret:  sealed,
		Enabled: true,
		Events:  `["event.close"]`,
	}
	if mutate != nil {
		mutate(hook)
	}
	if err := f.st.Webhooks.Create(hook); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	return hook
}

// deliveries reads the history newest-first, which is how the API will hand it to
// the Settings page.
func (f *fixture) deliveries(t *testing.T, webhookID string) []models.WebhookDelivery {
	t.Helper()
	rows, err := f.st.WebhookDeliveries.ListForWebhook(webhookID, "", 50, 0)
	if err != nil {
		t.Fatalf("read deliveries: %v", err)
	}
	return rows
}

func TestSignatureRecomputesByteForByte(t *testing.T) {
	// The exit criterion, performed by the receiver: it reads the bytes, recomputes
	// the HMAC with its own copy of the secret, and compares. `scripts/verify.sh`
	// step 9 does the same thing from outside the process.
	f := newFixture(t)
	rec := newReceiver(t, nil)
	f.addHook(t, rec.server.URL, nil)

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if rec.count() != 1 {
		t.Fatalf("the endpoint got %d requests, want 1", rec.count())
	}
	sent := rec.attempt(1)

	header := sent.header.Get(SignatureHeader)
	if !strings.HasPrefix(header, SignaturePrefix) {
		t.Fatalf("%s is %q, want the %q prefix", SignatureHeader, header, SignaturePrefix)
	}

	got := sent.header.Get("User-Agent")
	if got != UserAgent {
		t.Errorf("User-Agent is %q, want %q", got, UserAgent)
	}

	// Recompute from the raw bytes, not from a decoded copy: a decoded-then-
	// remarshalled body can differ in field order or number formatting, and the
	// signature would then be over different bytes than the ones that arrived. The
	// key is the plaintext the operator typed, which is what the receiver holds —
	// the column's sealed text never leaves this process.
	mac := hmac.New(sha256.New, []byte(f.secret))
	mac.Write(sent.body)
	want := SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
	if header != want {
		t.Errorf("the receiver recomputed %s and the request carried %s", want, header)
	}
	if !Verify(f.secret, sent.body, header) {
		t.Error("Verify rejected a signature the receiver just recomputed")
	}
	if Verify("some-other-secret", sent.body, header) {
		t.Error("Verify accepted a signature made with the wrong secret")
	}
	if Verify(f.secret, append(append([]byte{}, sent.body...), ' '), header) {
		t.Error("Verify accepted a body with one extra byte")
	}
}

func TestTheSameBytesAreSignedOnEveryAttempt(t *testing.T) {
	// The body is marshalled once and the same slice is signed and sent on each
	// attempt, so a receiver that verified attempt 1 and then receives attempt 3
	// sees a signature that still matches. A re-marshal per attempt would put that
	// at the mercy of any field whose encoding ever moved.
	f := newFixture(t)
	rec := newReceiver(t, func(int) (int, string) { return http.StatusBadGateway, "" })
	f.addHook(t, rec.server.URL, nil)

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if rec.count() != 3 {
		t.Fatalf("the endpoint got %d requests, want 3", rec.count())
	}
	first := rec.attempt(1)
	for n := 2; n <= 3; n++ {
		next := rec.attempt(n)
		if string(next.body) != string(first.body) {
			t.Errorf("attempt %d sent a different body than attempt 1", n)
		}
		if next.header.Get(SignatureHeader) != first.header.Get(SignatureHeader) {
			t.Errorf("attempt %d carried a different signature than attempt 1", n)
		}
	}
}

func TestTheSecretNeverTravelsInTheBody(t *testing.T) {
	// The console writes the signing secret and never reads it back, exactly like a
	// storage credential. A payload that carried it would hand the secret to
	// whoever logs webhook bodies.
	f := newFixture(t)
	rec := newReceiver(t, nil)
	f.addHook(t, rec.server.URL, nil)

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if strings.Contains(string(rec.attempt(1).body), f.secret) {
		t.Error("the payload contains the signing secret")
	}
}

func TestDisabledWebhookSendsNothing(t *testing.T) {
	f := newFixture(t)
	rec := newReceiver(t, nil)
	f.addHook(t, rec.server.URL, func(h *models.Webhook) { h.Enabled = false })

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if rec.count() != 0 {
		t.Errorf("a disabled endpoint got %d requests, want none", rec.count())
	}
}

func TestAnEndpointScopedToAnotherCameraSendsNothing(t *testing.T) {
	f := newFixture(t)
	rec := newReceiver(t, nil)
	f.addHook(t, rec.server.URL, func(h *models.Webhook) { h.CameraID = "00000000-0000-4000-8000-000000000001" })

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if rec.count() != 0 {
		t.Errorf("an endpoint for another camera got %d requests, want none", rec.count())
	}
}

func TestAnEndpointThatDidNotAskForThisTriggerSendsNothing(t *testing.T) {
	// Enabled and addressed to this camera, but subscribed to a trigger this build
	// does not send. The row said what it wanted; the delivery loop honours it
	// rather than POSTing and letting the receiver sort it out.
	f := newFixture(t)
	rec := newReceiver(t, nil)
	f.addHook(t, rec.server.URL, func(h *models.Webhook) { h.Events = `["event.open"]` })

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if rec.count() != 0 {
		t.Errorf("an endpoint that did not ask got %d requests, want none", rec.count())
	}
}

func TestThreeAttemptsAreMadeAndEachSendIsARow(t *testing.T) {
	// The exit criterion: status, attempt and error per send. The endpoint answers
	// 502 every time, so the ladder runs out and the history shows all three.
	f := newFixture(t)
	rec := newReceiver(t, func(int) (int, string) { return http.StatusBadGateway, "nginx is bored" })
	hook := f.addHook(t, rec.server.URL, nil)

	res := f.d.deliver(context.Background(), hook, TriggerClose, mustPayload(t, f))

	if res.Attempts != 3 {
		t.Errorf("the ladder made %d attempts, want 3", res.Attempts)
	}
	if res.Delivered {
		t.Error("a ladder of 502s reported itself delivered")
	}

	rows := f.deliveries(t, hook.ID)
	if len(rows) != 3 {
		t.Fatalf("the history holds %d rows, want one per send", len(rows))
	}
	// Newest first, which is the order the Settings page reads.
	for i, row := range rows {
		wantAttempt := 3 - i
		if row.Attempt != wantAttempt {
			t.Errorf("row %d carries attempt %d, want %d", i, row.Attempt, wantAttempt)
		}
		if row.Status != models.DeliveryStatusFailed {
			t.Errorf("attempt %d has status %q, want %q", row.Attempt, row.Status, models.DeliveryStatusFailed)
		}
		if row.StatusCode != http.StatusBadGateway {
			t.Errorf("attempt %d recorded status code %d, want 502", row.Attempt, row.StatusCode)
		}
		if !strings.Contains(row.Error, "502") {
			t.Errorf("attempt %d's error is %q, want it to name the status", row.Attempt, row.Error)
		}
		if !strings.Contains(row.Error, "nginx is bored") {
			t.Errorf("attempt %d's error is %q, want the receiver's own words kept", row.Attempt, row.Error)
		}
		if row.DeliveredAt != nil {
			t.Errorf("attempt %d has a delivered_at, which belongs to a send that worked", row.Attempt)
		}
		if row.Event != TriggerClose {
			t.Errorf("attempt %d is filed under %q, want %q", row.Attempt, row.Event, TriggerClose)
		}
		if row.Payload == "" {
			t.Errorf("attempt %d kept no payload, and the operator's next question is what was sent", row.Attempt)
		}
	}
}

func TestASuccessStopsTheLadderAndBothSendsAreRows(t *testing.T) {
	// 500 then 200: the history has to show both, because "did it get through" is
	// answered by the sequence and not by the last row.
	f := newFixture(t)
	rec := newReceiver(t, func(n int) (int, string) {
		if n == 1 {
			return http.StatusInternalServerError, ""
		}
		return http.StatusOK, ""
	})
	hook := f.addHook(t, rec.server.URL, nil)

	res := f.d.deliver(context.Background(), hook, TriggerClose, mustPayload(t, f))
	if !res.Delivered {
		t.Errorf("the second attempt answered 200 and the result says %q", res.Error)
	}
	if res.Attempts != 2 {
		t.Errorf("the ladder made %d attempts, want 2: a success stops it", res.Attempts)
	}
	if rec.count() != 2 {
		t.Errorf("the endpoint got %d requests, want 2", rec.count())
	}

	rows := f.deliveries(t, hook.ID)
	if len(rows) != 2 {
		t.Fatalf("the history holds %d rows, want 2", len(rows))
	}
	newest, older := rows[0], rows[1]
	if newest.Status != models.DeliveryStatusDelivered || newest.StatusCode != http.StatusOK {
		t.Errorf("the newest row is %s/%d, want delivered/200", newest.Status, newest.StatusCode)
	}
	if newest.DeliveredAt == nil {
		t.Error("a delivered row has no delivered_at")
	}
	if newest.Error != "" {
		t.Errorf("a delivered row carries error %q", newest.Error)
	}
	if older.Status != models.DeliveryStatusFailed || older.StatusCode != http.StatusInternalServerError {
		t.Errorf("the older row is %s/%d, want failed/500", older.Status, older.StatusCode)
	}
}

func TestAnEndpointThatNeverAnswersIsStillAnAttempt(t *testing.T) {
	// A receiver that is down is the common case, and the row has to say so with
	// something: there is no status code, so the error text is the whole answer.
	f := newFixture(t)
	rec := newReceiver(t, nil)
	hook := f.addHook(t, rec.server.URL, nil)
	rec.server.Close() // the port is now nobody's

	res := f.d.deliver(context.Background(), hook, TriggerClose, mustPayload(t, f))
	if res.Delivered {
		t.Error("a closed port reported itself delivered")
	}

	rows := f.deliveries(t, hook.ID)
	if len(rows) != 3 {
		t.Fatalf("the history holds %d rows, want 3", len(rows))
	}
	for _, row := range rows {
		if row.StatusCode != 0 {
			t.Errorf("a send with no response recorded status code %d, want 0", row.StatusCode)
		}
		if !strings.Contains(row.Error, "did not answer") {
			t.Errorf("the error is %q, want it to say the endpoint did not answer", row.Error)
		}
	}
}

func TestTheEndpointRowCarriesTheLastOutcome(t *testing.T) {
	// The Settings page answers "is this webhook working" from the endpoint row,
	// without reading the history.
	f := newFixture(t)

	rec := newReceiver(t, func(int) (int, string) { return http.StatusBadRequest, "unknown camera" })
	failing := f.addHook(t, rec.server.URL, nil)
	f.d.deliver(context.Background(), failing, TriggerClose, mustPayload(t, f))

	stored, err := f.st.Webhooks.GetByID(failing.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.LastError == "" {
		t.Error("an endpoint whose last ladder failed has no last_error")
	}
	if stored.LastDeliveredAt == nil {
		t.Error("an endpoint that was just attempted has no last_delivered_at")
	}

	ok := newReceiver(t, nil)
	working := f.addHook(t, ok.server.URL, nil)
	f.d.deliver(context.Background(), working, TriggerClose, mustPayload(t, f))

	stored, err = f.st.Webhooks.GetByID(working.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.LastError != "" {
		t.Errorf("an endpoint that answered 200 carries last_error %q", stored.LastError)
	}
	if stored.LastDeliveredAt == nil {
		t.Error("an endpoint that answered 200 has no last_delivered_at")
	}
}

func TestASealedSigningSecretIsOpenedBeforeSigning(t *testing.T) {
	// The integration with S7-02: every credential this server stores is sealed, a
	// webhook's signing secret included, so the deliverer must open the column
	// before it can sign with it. Signing the sealed text instead would produce a
	// signature no receiver can reproduce, and the failure would look like the
	// receiver's problem.
	f := newFixture(t)
	rec := newReceiver(t, nil)
	hook := f.addHook(t, rec.server.URL, nil)

	if hook.Secret == f.secret {
		t.Fatal("the fixture stored the secret unsealed, which makes this test vacuous")
	}

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if rec.count() != 1 {
		t.Fatalf("the endpoint got %d requests, want 1", rec.count())
	}
	sent := rec.attempt(1)
	mac := hmac.New(sha256.New, []byte(f.secret))
	mac.Write(sent.body)
	if got, want := sent.header.Get(SignatureHeader), SignaturePrefix+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Errorf("a sealed secret signed as %s, want the HMAC under the plaintext %s", got, want)
	}
}

func TestAPlaintextSigningSecretIsStillUsedAsWritten(t *testing.T) {
	// A row written before sealing existed, or one inserted by hand: the value is
	// not a sealed value, and the answer is to use what the operator typed rather
	// than refuse to sign.
	f := newFixture(t)
	rec := newReceiver(t, nil)
	f.addHook(t, rec.server.URL, func(h *models.Webhook) { h.Secret = f.secret })

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if rec.count() != 1 {
		t.Fatalf("the endpoint got %d requests, want 1", rec.count())
	}
	sent := rec.attempt(1)
	mac := hmac.New(sha256.New, []byte(f.secret))
	mac.Write(sent.body)
	if got, want := sent.header.Get(SignatureHeader), SignaturePrefix+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Errorf("an unsealed secret signed as %s, want %s", got, want)
	}
}

func TestASealedSecretUnderTheWrongKeySendsNoSignature(t *testing.T) {
	// `NVR_SECRET_KEY` changed. The event still goes out — a notification with no
	// signature is news the receiver can still act on — but the header is absent
	// rather than made from the sealed text, because a signature that looks valid
	// sends the operator to the endpoint instead of to the key.
	f := newFixture(t)
	rec := newReceiver(t, nil)

	otherKey := [32]byte(sha256.Sum256([]byte("a-key-this-process-does-not-have")))
	sealed, err := storage.Seal(f.secret, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	f.addHook(t, rec.server.URL, func(h *models.Webhook) { h.Secret = sealed })

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if rec.count() != 1 {
		t.Fatalf("the endpoint got %d requests, want 1: a notification still arrives", rec.count())
	}
	if got := rec.attempt(1).header.Get(SignatureHeader); got != "" {
		t.Errorf("an unopenable secret sent signature %q, want no header at all", got)
	}
}

func TestAnEndpointWithNoSigningSecretSendsNoSignatureHeader(t *testing.T) {
	// The secret column defaults to empty, and an endpoint with no secret is a
	// receiver that does not check. Sending `HMAC("", body)` would be a signature
	// that means nothing and looks like one that does.
	f := newFixture(t)
	rec := newReceiver(t, nil)
	f.addHook(t, rec.server.URL, func(h *models.Webhook) { h.Secret = "" })

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if rec.count() != 1 {
		t.Fatalf("the endpoint got %d requests, want 1", rec.count())
	}
	if got := rec.attempt(1).header.Get(SignatureHeader); got != "" {
		t.Errorf("an endpoint with no secret sent signature %q, want no header", got)
	}
	if got := rec.attempt(1).header.Get("User-Agent"); got != UserAgent {
		t.Errorf("User-Agent is %q, want %q even without a signature", got, UserAgent)
	}
}

func TestTheLadderIsThePlansThreeAttemptsAtTwoTenAndSixtySeconds(t *testing.T) {
	if len(AttemptWaits) != 3 {
		t.Fatalf("the ladder has %d attempts, want 3", len(AttemptWaits))
	}
	want := []time.Duration{2 * time.Second, 10 * time.Second, 60 * time.Second}
	for i, d := range want {
		if AttemptWaits[i] != d {
			t.Errorf("attempt %d is scheduled at %v, want %v", i+1, AttemptWaits[i], d)
		}
	}

	// The offsets are measured from the trigger, not from the previous attempt: a
	// receiver that answers slowly does not push the whole ladder back. The fake
	// clock advances only by the waits, so the sleeps prove the schedule.
	f := newFixture(t)
	rec := newReceiver(t, func(int) (int, string) { return http.StatusBadGateway, "" })
	hook := f.addHook(t, rec.server.URL, nil)

	clk := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var slept []time.Duration
	f.d.Waits = nil
	f.d.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clk }
	f.d.Sleep = func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		clk = clk.Add(d)
		slept = append(slept, d)
		mu.Unlock()
		return ctx.Err() == nil
	}

	f.d.deliver(context.Background(), hook, TriggerClose, mustPayload(t, f))

	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 3 {
		t.Fatalf("the ladder slept %d times, want 3", len(slept))
	}
	// 2s, then 10-2, then 60-10: the attempt times are 2s, 10s and 60s after the
	// trigger, whatever the waits in between were called.
	for i, want := range []time.Duration{2 * time.Second, 8 * time.Second, 50 * time.Second} {
		if slept[i] != want {
			t.Errorf("sleep %d is %v, want %v (the attempts land at 2s, 10s and 60s)", i, slept[i], want)
		}
	}
}

func TestACancelledLadderRecordsNoAttemptThatNeverHappened(t *testing.T) {
	// Shutdown between attempts. The send that ran is in the history; the two that
	// never left the process are not, because a row claiming a request this server
	// never made is a history that never happened.
	f := newFixture(t)
	rec := newReceiver(t, func(int) (int, string) { return http.StatusBadGateway, "" })
	hook := f.addHook(t, rec.server.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	// The clock is frozen so the ladder's offsets always dominate the elapsed time.
	// With the real clock and a millisecond ladder, a request that takes longer than
	// the next offset leaves `wait` negative, and the loop correctly skips the sleep
	// — which would run all three attempts and make this test about how fast the
	// machine is.
	f.d.Now = func() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }
	calls := 0
	f.d.Sleep = func(context.Context, time.Duration) bool {
		calls++
		if calls >= 2 {
			cancel()
			return false
		}
		return true
	}

	res := f.d.deliver(ctx, hook, TriggerClose, mustPayload(t, f))
	if res.Attempts != 1 {
		t.Errorf("the ladder recorded %d attempts, want 1", res.Attempts)
	}
	if rows := f.deliveries(t, hook.ID); len(rows) != 1 {
		t.Errorf("the history holds %d rows, want the one send that happened", len(rows))
	}
	if rec.count() != 1 {
		t.Errorf("the endpoint got %d requests, want 1", rec.count())
	}
}

func TestADeliveryCancelledBeforeItsFirstAttemptSendsNothing(t *testing.T) {
	// The first attempt is 2 s after the trigger, so a process that shuts down
	// immediately after a close sends nothing at all — and records nothing. A row
	// for a POST that never happened would tell the operator the endpoint was tried
	// and found wanting.
	f := newFixture(t)
	rec := newReceiver(t, nil)
	hook := f.addHook(t, rec.server.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := f.d.deliver(ctx, hook, TriggerClose, mustPayload(t, f))
	if res.Attempts != 0 {
		t.Errorf("a cancelled delivery recorded %d attempts, want 0", res.Attempts)
	}
	if rec.count() != 0 {
		t.Errorf("a cancelled delivery sent %d requests, want none", rec.count())
	}
	if rows := f.deliveries(t, hook.ID); len(rows) != 0 {
		t.Errorf("a cancelled delivery left %d rows, want none", len(rows))
	}

	stored, err := f.st.Webhooks.GetByID(hook.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.LastDeliveredAt != nil {
		t.Error("a delivery that never ran dated the endpoint")
	}
}

func TestNotifyCloseDoesNotBlockTheCaller(t *testing.T) {
	// A close is applied inside the worker's request. An endpoint that is down must
	// not hold that request open for the ladder, so the caller gets its answer
	// while the first attempt is still in flight.
	f := newFixture(t)
	rec := newReceiver(t, nil)
	rec.gate = make(chan struct{})
	f.addHook(t, rec.server.URL, nil)

	returned := make(chan struct{})
	go func() {
		f.d.NotifyClose(f.event, f.camera)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("NotifyClose did not return while the endpoint was still holding the request")
	}

	close(rec.gate)
	f.d.Wait()
	if rec.count() != 1 {
		t.Errorf("the endpoint got %d requests, want 1", rec.count())
	}
}

func TestPayloadIsThePlansShapeInItsPlansOrder(t *testing.T) {
	f := newFixture(t)
	body := mustPayload(t, f)

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the payload is not JSON: %v", err)
	}

	// The plan's field list, in the plan's order. The order is asserted on the raw
	// bytes because the signature covers them, and a receiver that recomputes the
	// HMAC from its own re-marshal needs the same order to produce the same bytes.
	want := []string{
		"id", "camera_id", "camera_slug", "started_at", "ended_at", "seconds",
		"classes", "zones", "rules", "severity", "snapshot_url", "clip_url",
	}
	var keys []string
	if err := json.Unmarshal(body, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.Token()
	for decoder.More() {
		tok, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		if key, ok := tok.(string); ok {
			keys = append(keys, key)
			var skip json.RawMessage
			if err := decoder.Decode(&skip); err != nil {
				t.Fatal(err)
			}
		}
	}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("the payload's fields are %s, want the plan's order %s", strings.Join(keys, ","), strings.Join(want, ","))
	}

	if decoded["id"] != f.event.ID {
		t.Errorf("id is %v, want the event's", decoded["id"])
	}
	if decoded["camera_id"] != f.camera.ID {
		t.Errorf("camera_id is %v, want the camera's", decoded["camera_id"])
	}
	if decoded["camera_slug"] != "front-door" {
		t.Errorf("camera_slug is %v, want the slug: a receiver knows names, not uuids", decoded["camera_slug"])
	}
	if decoded["seconds"] != float64(42) {
		t.Errorf("seconds is %v, want 42", decoded["seconds"])
	}
	if decoded["severity"] != float64(3) {
		t.Errorf("severity is %v, want 3", decoded["severity"])
	}
	for _, ts := range []string{"started_at", "ended_at"} {
		value, ok := decoded[ts].(string)
		if !ok {
			t.Fatalf("%s is %v, want an RFC3339 string", ts, decoded[ts])
		}
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			t.Errorf("%s is %q, which is not RFC3339: %v", ts, value, err)
		}
		if !strings.HasSuffix(value, "Z") {
			t.Errorf("%s is %q, want UTC: two hosts' clocks must not both be in the payload", ts, value)
		}
	}

	// Zones carry the *names* the operator gave them. A receiver has never seen
	// this installation's zone ids, and the API's own event view resolves them the
	// same way.
	if got := fmt.Sprint(decoded["zones"]); got != "[porch]" {
		t.Errorf("zones is %s, want the zone names the operator typed", got)
	}
	if got := fmt.Sprint(decoded["classes"]); got != "[person dog]" {
		t.Errorf("classes is %s, want the classes that contributed", got)
	}
	if got := fmt.Sprint(decoded["rules"]); got != "[loiter]" {
		t.Errorf("rules is %s, want the rules that contributed", got)
	}
}

func TestPayloadURLsAreTheServersRoutesAndNotItsDisk(t *testing.T) {
	f := newFixture(t)
	body := mustPayload(t, f)

	var decoded map[string]string
	if err := json.Unmarshal(body, &decoded); err != nil {
		// The nested fields are arrays, so decode the two URLs out of the whole.
		var all map[string]any
		if err := json.Unmarshal(body, &all); err != nil {
			t.Fatal(err)
		}
		decoded = map[string]string{
			"snapshot_url": all["snapshot_url"].(string),
			"clip_url":     all["clip_url"].(string),
		}
	}

	if want := "http://nvr.lan:8080/api/v1/events/" + f.event.ID + "/snapshot"; decoded["snapshot_url"] != want {
		t.Errorf("snapshot_url is %q, want %q", decoded["snapshot_url"], want)
	}
	if want := "http://nvr.lan:8080/api/v1/events/" + f.event.ID + "/clip"; decoded["clip_url"] != want {
		t.Errorf("clip_url is %q, want %q", decoded["clip_url"], want)
	}
	if strings.Contains(string(body), "/var/lib/nvr") {
		t.Error("the payload carries a path on this server's disk")
	}
}

func TestPayloadURLsSurviveAReverseProxyPath(t *testing.T) {
	// An installation behind a proxy at /nvr must hand out URLs its receivers can
	// fetch, so the base's path is part of the URL rather than dropped.
	f := newFixture(t)
	f.d.BaseURL = "https://cam.example.com/nvr"
	body := mustPayload(t, f)

	if !strings.Contains(string(body), `https://cam.example.com/nvr/api/v1/events/`) {
		t.Errorf("the URLs dropped the proxy's path prefix: %s", body)
	}
}

func TestAPayloadForACameraThatRecordsNothingHasNoClipURL(t *testing.T) {
	// `record_mode=off` means no clip will ever exist for this event, and the route
	// will answer its named 404 forever. The payload says so instead of handing the
	// receiver a URL that never becomes a video.
	f := newFixture(t)
	f.camera.RecordMode = models.RecordModeOff
	body := mustPayload(t, f)

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["clip_url"] != "" {
		t.Errorf("clip_url is %v for a camera that records nothing, want the empty string", decoded["clip_url"])
	}
	if decoded["snapshot_url"] == "" {
		t.Error("snapshot_url is empty though the event has a picture")
	}
}

func TestAPayloadWithNoPictureHasNoSnapshotURL(t *testing.T) {
	// The picture is written on the open, so by the close an empty field means
	// there is none — and a receiver branches on the field instead of fetching a
	// 404.
	f := newFixture(t)
	f.event.LocalSnapshotPath = ""
	body := mustPayload(t, f)

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["snapshot_url"] != "" {
		t.Errorf("snapshot_url is %v for an event with no picture, want the empty string", decoded["snapshot_url"])
	}
}

func TestAPayloadForADeletedCameraStillGoesOut(t *testing.T) {
	// The event is the fact. A camera deleted while its event was open cannot make
	// the notification disappear; the payload degrades to zone ids and no slug.
	f := newFixture(t)
	body, err := BuildPayload(f.event, nil, "http://nvr.lan:8080")
	if err != nil {
		t.Fatalf("BuildPayload with no camera: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["camera_slug"] != "" {
		t.Errorf("camera_slug is %v with no camera row, want the empty string", decoded["camera_slug"])
	}
	if got := fmt.Sprint(decoded["zones"]); got != "[zone-1]" {
		t.Errorf("zones is %s, want the ids the row holds when no camera can name them", got)
	}
}

func TestAnEmptyEndpointListSendsNothing(t *testing.T) {
	f := newFixture(t)
	rec := newReceiver(t, nil)
	_ = rec

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()
}

func TestWaitReturnsAfterTheInFlightDeliveryFinishes(t *testing.T) {
	// The shutdown seam: `Close` then `Wait` is how main.go finishes what it started
	// without a sleep. If Wait returned early, the rows below would not be there.
	f := newFixture(t)
	rec := newReceiver(t, nil)
	hook := f.addHook(t, rec.server.URL, nil)

	f.d.NotifyClose(f.event, f.camera)
	f.d.Wait()

	if rows := f.deliveries(t, hook.ID); len(rows) != 1 {
		t.Fatalf("after Wait the history holds %d rows, want 1", len(rows))
	}
}

func TestTheReceiverSBodyIsKeptButNotAWholePage(t *testing.T) {
	// The receiver's own words are the useful part of a 500. A receiver that
	// answers with an HTML error page should not put a page in every row.
	f := newFixture(t)
	long := strings.Repeat("a page of html ", 100)
	rec := newReceiver(t, func(int) (int, string) { return http.StatusInternalServerError, long })
	hook := f.addHook(t, rec.server.URL, nil)

	f.d.deliver(context.Background(), hook, TriggerClose, mustPayload(t, f))

	rows := f.deliveries(t, hook.ID)
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	if len(rows[0].Error) > 400 {
		t.Errorf("the recorded error is %d bytes, want the receiver's words kept short", len(rows[0].Error))
	}
}

func TestMain(m *testing.M) {
	// The delivery loop logs; a test run that shows every failed attempt reads like
	// a broken installation.
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func mustPayload(t *testing.T, f *fixture) []byte {
	t.Helper()
	body, err := BuildPayload(f.event, f.camera, f.d.BaseURL)
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	return body
}
