// Package webhooks delivers the installation's outbound notifications: on an
// event close, the plan's payload goes to every endpoint that asked for it,
// signed so the receiver can prove where it came from.
//
// Three rules shape everything here.
//
// **The signature is over the bytes, not over the struct.** The body is marshalled
// once per event and the same slice is signed and sent, on every attempt. A
// receiver that recomputes `HMAC-SHA256(secret, body)` — `scripts/verify.sh` step 9
// does exactly that, externally — has to be handed the bytes that were signed, and
// a retry that re-marshalled the payload could sign a different body than the one
// it sent if any field's encoding were ever to move.
//
// **Every attempt is a row.** The operator's question is "did it get through", and
// the answer is the sequence — 500, 500, 200 — not the last entry. So each send
// writes its own `webhook_deliveries` row with its status, its attempt number, the
// status code it saw and the error it saw.
//
// **Delivery never blocks the signal that caused it.** A close is applied inside
// the worker's request, and an endpoint that is down would otherwise hold that
// request open for the whole 72-second ladder. `NotifyClose` resolves the
// endpoints, hands each one a goroutine, and returns.
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nvr/server/models"
	"nvr/server/storage"
	"nvr/server/store"
)

// TriggerClose is the v1 trigger: an event that finished. The webhook row stores
// the list of triggers it wants, so a later `event.open` or `segment.aged` is a new
// constant here and not a schema change.
const TriggerClose = "event.close"

// SignatureHeader, UserAgent and the `sha256=` prefix are part of the receiver's
// contract. A receiver is written once against these strings; changing one is a
// breaking change for every endpoint in the field.
const (
	SignatureHeader = "X-NVR-Signature"
	UserAgent       = "nvr/1"
	SignaturePrefix = "sha256="
)

// AttemptWaits is the plan's ladder: three attempts, at 2 s, 10 s and 60 s after
// the trigger.
//
// The offsets are measured from the trigger, not from the previous attempt, so a
// slow endpoint (one that answers at 9 s) still gets its second attempt at the 10 s
// mark rather than at 19 s. The ladder is short on purpose: a webhook endpoint is
// usually a LAN automation host that is either up or restarting for a few seconds,
// and the whole ladder spans 72 s. The upload queue's 20 attempts over four and a
// half days is where stopping early mattered; here, running the ladder every time
// is cheap and the rows are the diagnosis.
var AttemptWaits = []time.Duration{
	2 * time.Second,
	10 * time.Second,
	60 * time.Second,
}

// RequestTimeout bounds one attempt. An endpoint that accepts the connection and
// then says nothing would otherwise hold a goroutine and a database write open
// indefinitely, and the ladder's schedule would measure the hang instead of the
// endpoint.
const RequestTimeout = 10 * time.Second

// bodySnippetBytes is how much of a failing response's body is kept. The receiver's
// own error message is the useful part of a 500 — "unknown camera" beats "500" —
// and a receiver that answers with an HTML error page should not put a page in
// every delivery row.
const bodySnippetBytes = 200

// timeLayout is the API's own (`server/api/users.go`), so a receiver that reads a
// webhook and then asks the API about the same event sees the same shape of
// timestamp in both.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// Payload is the plan's event payload, in the plan's field order.
//
// The order is not cosmetic. The signature covers the marshalled bytes, and
// `encoding/json` emits struct fields in declaration order, so the order lives in
// this type rather than in a map whose iteration order it would not fix. A
// receiver that recomputes the signature from a re-marshalled copy of the payload
// needs the same order to produce the same bytes.
type Payload struct {
	ID         string   `json:"id"`
	CameraID   string   `json:"camera_id"`
	CameraSlug string   `json:"camera_slug"`
	StartedAt  string   `json:"started_at"`
	EndedAt    string   `json:"ended_at"`
	Seconds    float64  `json:"seconds"`
	Classes    []string `json:"classes"`
	Zones      []string `json:"zones"`
	Rules      []string `json:"rules"`
	Severity   int      `json:"severity"`

	// The two URLs are the server's own routes, never a path on its disk: the
	// receiver cannot read this installation's CLIPS_DIR, and the tier's address
	// carries a credential. Both are absolute so a receiver behind a different
	// name than the server calls itself can still fetch them.
	SnapshotURL string `json:"snapshot_url"`
	ClipURL     string `json:"clip_url"`
}

// Sign returns the header value for one body: `sha256=<hex(HMAC-SHA256(secret,
// body))>`.
//
// The comparison a receiver performs is `hmac.Equal`, and the one this server would
// perform on the way back is too — a signature is a MAC, not a secret to compare in
// constant time by accident, but `hmac.Equal` costs nothing and removes the
// question.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether a header value matches a body under a secret. The API does
// not receive signed requests in v1; the test suite and `verify.sh` use it, and it
// exists here so the check is written once, next to the thing it checks.
func Verify(secret string, body []byte, header string) bool {
	value, ok := strings.CutPrefix(header, SignaturePrefix)
	if !ok {
		return false
	}
	got, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	want := hmac.New(sha256.New, []byte(secret))
	want.Write(body)
	return hmac.Equal(got, want.Sum(nil))
}

// BuildPayload marshals the payload for one event and returns the exact bytes that
// will be signed and sent.
//
// `cam` may be nil — a camera deleted while its event was still open — and the
// payload degrades rather than failing: no slug, and zone ids where names were.
// The event is the fact, and an endpoint that asked for it should get it even when
// the camera that produced it is gone.
func BuildPayload(ev *models.Event, cam *models.Camera, baseURL string) ([]byte, error) {
	slug := ""
	zoneNames := map[string]string{}
	clipWillExist := true
	if cam != nil {
		slug = cam.Slug
		if zones, err := models.ParseZones(cam.ZonesJSON); err == nil {
			for _, z := range zones {
				zoneNames[z.ID] = z.Name
			}
		}
		// A camera in `record_mode=off` records nothing, so no clip will ever be
		// assembled for its events and the route will answer its named 404
		// forever. Saying so in the payload beats handing a receiver a URL that
		// never becomes a video.
		clipWillExist = cam.RecordMode != models.RecordModeOff
	}

	named := func(ids []string) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			if name := zoneNames[id]; name != "" {
				out = append(out, name)
				continue
			}
			// A zone deleted after the event keeps its id: ugly, and truer than
			// dropping the zone the event actually happened in.
			out = append(out, id)
		}
		return out
	}

	payload := Payload{
		ID:         ev.ID,
		CameraID:   ev.CameraID,
		CameraSlug: slug,
		StartedAt:  ev.StartedAt.UTC().Format(timeLayout),
		EndedAt:    ev.EndedAt.UTC().Format(timeLayout),
		Seconds:    ev.EndedAt.Sub(ev.StartedAt).Seconds(),
		Classes:    models.JSONStringList(ev.ClassesJSON),
		Zones:      named(models.JSONStringList(ev.ZonesJSON)),
		Rules:      models.JSONStringList(ev.RulesJSON),
		Severity:   ev.Severity,
	}

	// The picture exists by the time a close arrives — it is written on the open —
	// so an empty string here means there is no picture, and a receiver can branch
	// on the field instead of fetching a 404.
	if ev.LocalSnapshotPath != "" || ev.SnapshotUploadedAt != nil {
		payload.SnapshotURL = mediaURL(baseURL, ev.ID, "snapshot")
	}
	// The clip is the other case, and the asymmetry is deliberate: assembly runs
	// *after* the close (at `ended_at + POST_ROLL_SECONDS`), so the URL is an
	// address that becomes a video a few seconds later. A receiver that fetches it
	// immediately and gets the named 404 should ask again, and this is the place
	// that rule is written down.
	if clipWillExist {
		payload.ClipURL = mediaURL(baseURL, ev.ID, "clip")
	}

	// `json.Marshal` of a nil slice is `null`, and a receiver iterating over
	// `classes` should not have to handle both `null` and `[]`.
	if payload.Classes == nil {
		payload.Classes = []string{}
	}
	if payload.Zones == nil {
		payload.Zones = []string{}
	}
	if payload.Rules == nil {
		payload.Rules = []string{}
	}

	return json.Marshal(payload)
}

// mediaURL puts one media route behind the installation's public base, preserving a
// base path so a server mounted behind a reverse proxy at `/nvr` hands out URLs its
// receivers can actually fetch.
func mediaURL(base, eventID, kind string) string {
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		// A base the server cannot parse is a configuration mistake, and the
		// payload still has to go out: an event with a relative URL is worth more
		// to a receiver than no event at all.
		return "/api/v1/events/" + eventID + "/" + kind
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/v1/events/" + eventID + "/" + kind
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// Result is one endpoint's outcome once the ladder finished: an attempt succeeded,
// or the last attempt failed and its reason is here.
type Result struct {
	WebhookID  string
	Trigger    string
	Attempts   int
	Delivered  bool
	StatusCode int
	Error      string
}

// Deliverer posts triggers to the endpoints that asked for them.
//
// The zero value is not usable — `St` is required — but `New` fills everything else,
// and the exported fields exist because the ladder's waits and the HTTP client are
// the two things a test must be able to shorten.
type Deliverer struct {
	St *store.Store

	// BaseURL is the installation's public origin — scheme, host and any path
	// prefix — used to build the payload's two URLs. Empty means "this host, the
	// port the server listens on", which is right for the common single-box
	// installation and wrong for a receiver on another network.
	BaseURL string

	// SealKey opens the endpoint's stored signing secret. The column holds sealed
	// text like every other credential this server writes (S7-02), so the HMAC key
	// has to be opened before it can be used — see `signingSecret`.
	SealKey [32]byte

	// HTTP is the client used for every attempt. Its timeout bounds one attempt,
	// not the ladder.
	HTTP *http.Client

	// Waits overrides AttemptWaits. Tests set it to milliseconds; production
	// leaves it nil so the plan's ladder is the one that runs, and a test that
	// wants to prove the ladder asserts the package value rather than this field.
	Waits []time.Duration

	// Sleep waits between attempts and reports false when the context was
	// cancelled mid-wait.
	Sleep func(ctx context.Context, d time.Duration) bool

	// Now is the clock the ladder's schedule is measured against.
	Now func() time.Time

	// Log receives the lines an operator reads when a receiver is misbehaving.
	// Nil means the standard logger.
	Log func(format string, args ...any)

	httpc  *http.Client
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

// New builds a deliverer. `baseURL` may be empty, in which case the payload's URLs
// are built from the loopback origin — see `BaseURL`. `sealKey` is
// `config.SealKey()`'s value, the one that opens the stored signing secrets.
func New(st *store.Store, baseURL string, sealKey [32]byte) *Deliverer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Deliverer{
		St:      st,
		BaseURL: baseURL,
		SealKey: sealKey,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Close stops the ladder of every in-flight delivery. Shutdown calls it so a
// process that is leaving does not spend another 60 seconds POSTing to an endpoint
// that is also going down.
func (d *Deliverer) Close() { d.cancel() }

// Wait blocks until every delivery `NotifyClose` started has finished. It is the
// shutdown seam (finish what you started) and the test seam (assert on the rows
// after the sends, not during).
func (d *Deliverer) Wait() { d.wg.Wait() }

func (d *Deliverer) waits() []time.Duration {
	if len(d.Waits) > 0 {
		return d.Waits
	}
	return AttemptWaits
}

func (d *Deliverer) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	if d.httpc == nil {
		d.httpc = &http.Client{Timeout: RequestTimeout}
	}
	return d.httpc
}

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Deliverer) sleep(ctx context.Context, wait time.Duration) bool {
	if d.Sleep != nil {
		return d.Sleep(ctx, wait)
	}
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (d *Deliverer) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log(format, args...)
		return
	}
	log.Printf(format, args...)
}

// NotifyClose hands an event close to every endpoint that subscribed to it, and
// returns without waiting for any of them.
//
// The endpoint list is resolved here, on the caller's goroutine, for two reasons:
// one indexed query is cheaper than one goroutine per endpoint that then discovers
// it has nothing to do, and a disabled endpoint must send *nothing* — not "nothing
// after a goroutine starts". The payload is built once and shared, because the
// signature is over its bytes and every endpoint receives the same body.
func (d *Deliverer) NotifyClose(ev *models.Event, cam *models.Camera) {
	if d == nil || d.St == nil || ev == nil {
		return
	}

	hooks, err := d.St.Webhooks.ListEnabledForCamera(ev.CameraID)
	if err != nil {
		d.logf("webhooks: the endpoint list could not be read for event %s: %v", ev.ID, err)
		return
	}

	var targets []*models.Webhook
	for i := range hooks {
		hook := hooks[i]
		if !hook.Delivers(TriggerClose) {
			// An endpoint that asked only for a trigger this build does not send
			// gets no request at all, which is what its row said it wanted.
			continue
		}
		targets = append(targets, &hook)
	}
	if len(targets) == 0 {
		return
	}

	body, err := BuildPayload(ev, cam, d.baseURL())
	if err != nil {
		d.logf("webhooks: the payload for event %s could not be built: %v", ev.ID, err)
		return
	}

	for _, hook := range targets {
		d.wg.Add(1)
		go func(hook *models.Webhook) {
			defer d.wg.Done()
			d.deliver(d.ctx, hook, TriggerClose, body)
		}(hook)
	}
}

func (d *Deliverer) baseURL() string {
	if d.BaseURL != "" {
		return d.BaseURL
	}
	return "http://localhost:8080"
}

// deliver runs the ladder for one endpoint and returns its outcome. It is
// synchronous on purpose: `NotifyClose` is the goroutine, and this is the body a
// test can call directly with a millisecond ladder and assert on afterwards.
func (d *Deliverer) deliver(ctx context.Context, hook *models.Webhook, trigger string, body []byte) Result {
	res := Result{WebhookID: hook.ID, Trigger: trigger}
	start := d.now()

	// The signing key is resolved once per delivery rather than once per attempt:
	// a secret this process cannot open is not going to open on the second try, and
	// three identical log lines would read as three different problems.
	secret, signed := d.signingSecret(hook)

	for i, at := range d.waits() {
		if ctx.Err() != nil {
			// A shutdown between attempts. The attempts that ran are already
			// recorded; an attempt that never happened gets no row, because a row
			// claiming a send this process never made would be a history that
			// never happened.
			return res
		}
		if wait := at - d.now().Sub(start); wait > 0 {
			if !d.sleep(ctx, wait) {
				return res
			}
		}

		res.Attempts = i + 1
		code, reason := d.post(ctx, hook.URL, body, secret, signed)
		res.StatusCode, res.Error = code, reason

		delivered := reason == ""
		row := &models.WebhookDelivery{
			WebhookID:  hook.ID,
			Event:      trigger,
			Payload:    string(body),
			Attempt:    res.Attempts,
			StatusCode: code,
			Error:      reason,
		}
		if delivered {
			row.Status = models.DeliveryStatusDelivered
			at := d.now()
			row.DeliveredAt = &at
		} else {
			row.Status = models.DeliveryStatusFailed
		}
		if err := d.St.WebhookDeliveries.Create(row); err != nil {
			// The delivery history is a diagnosis, not the delivery: a row that
			// cannot be written costs the operator a line in the history and must
			// not abort a send that the endpoint already answered.
			d.logf("webhooks: attempt %d for %s could not be recorded: %v", res.Attempts, hook.Name, err)
		}

		if delivered {
			res.Delivered = true
			break
		}
		d.logf("webhooks: attempt %d for %s (%s) failed: %s", res.Attempts, hook.Name, hook.URL, reason)
	}

	d.markEndpoint(hook.ID, res)
	return res
}

// signingSecret resolves the key one endpoint's signature is made with, and says
// whether to send a signature at all.
//
// The column holds sealed text, because S7-02 sealed every credential this server
// stores and a webhook's signing secret is one. The three ways that can fail are
// answered differently, because the operator's fix is different in each:
//
//   - an empty secret: there is nothing to sign with, and the request goes out with
//     no header. A receiver that configured a secret and sees none knows something
//     is wrong; one that configured none does not look.
//   - a value that is not a sealed value at all: a row written before sealing
//     existed, or one inserted by hand. It is used as written, because that is what
//     the operator typed.
//   - a well-formed sealed value this process cannot open: `NVR_SECRET_KEY` changed.
//     Signing the sealed text instead would produce a signature that verifies
//     nowhere, and a header that looks valid is worse than no header — the receiver
//     reports a bad signature and the operator looks at the endpoint instead of at
//     the key.
func (d *Deliverer) signingSecret(hook *models.Webhook) (string, bool) {
	if hook.Secret == "" {
		return "", false
	}
	plain, err := storage.Unseal(hook.Secret, d.SealKey)
	if err == nil {
		return plain, true
	}
	if errors.Is(err, storage.ErrNotSealed) {
		d.logf("webhooks: %s holds an unsealed signing secret; signing with it as written", hook.Name)
		return hook.Secret, true
	}
	d.logf("webhooks: %s's signing secret cannot be opened with this installation's key (%v); sending without a signature", hook.Name, err)
	return "", false
}

// post performs one attempt and answers the status code and the reason it failed
// ("" when it did not).
//
// Everything here returns a reason rather than an error because the reason is what
// goes into the delivery row: "the endpoint did not answer: dial tcp …" is the
// sentence that tells an operator whether to fix the URL, the receiver, or the
// secret.
func (d *Deliverer) post(ctx context.Context, endpoint string, body []byte, secret string, signed bool) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Sprintf("the endpoint URL cannot be dialed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if signed {
		req.Header.Set(SignatureHeader, Sign(secret, body))
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := d.client().Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 0, "the delivery was cancelled"
		}
		return 0, fmt.Sprintf("the endpoint did not answer: %v", err)
	}
	defer resp.Body.Close()

	// A little of the body is kept: the receiver's own words are the useful part of
	// a 500, and leaving the body unread stops the transport reusing the connection.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, bodySnippetBytes))

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return resp.StatusCode, ""
	}

	reason := fmt.Sprintf("the endpoint answered %d", resp.StatusCode)
	if text := strings.TrimSpace(string(snippet)); text != "" {
		if len(text) > bodySnippetBytes {
			text = text[:bodySnippetBytes]
		}
		reason += ": " + text
	}
	return resp.StatusCode, reason
}

// markEndpoint writes the last outcome on the endpoint row, which is what lets the
// Settings page answer "is this webhook working" without reading the delivery
// history.
func (d *Deliverer) markEndpoint(id string, res Result) {
	if res.Attempts == 0 {
		// Nothing was sent, so nothing happened to the endpoint. Writing a row
		// here would date an endpoint that never ran.
		return
	}
	at := d.now()
	if err := d.St.Webhooks.MarkDelivered(id, res.Error, at); err != nil {
		d.logf("webhooks: the outcome for %s could not be recorded: %v", id, err)
	}
}
