// The health pair's suite (S9-07).
//
// Every test is named `TestHealth…` or `TestHeartbeat…` because the card's
// verification runs `go test ./api -run Health` — and `Heartbeat` matches too,
// since `-run` is an unanchored regex.
//
// The three things worth asserting are the three exit criteria: the 90 s
// boundary is asserted on both sides of the line, a detector that never
// reported answers stale (a 200, not an error), and `/health` still answers
// with the database closed — because the point of that endpoint is that it can
// never be the thing that restarts a recording server.

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// healthFixture is the full route table with a worker token and a settable
// clock: the heartbeat stamps with `h.now()`, so the 90 s boundary is a clock
// move, not a sleep.
type healthFixture struct {
	Router   *gin.Engine
	Handlers *Handlers
}

const healthWorkerToken = "worker-token-for-health"

func newHealthFixture(t *testing.T) *healthFixture {
	t.Helper()
	st := newStore(t)
	h := newHandlers(t, st)
	h.Cfg.WorkerToken = healthWorkerToken
	return &healthFixture{Router: routerFor(t, h), Handlers: h}
}

func (f *healthFixture) do(method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.Router.ServeHTTP(rec, req)
	return rec
}

// beat posts a heartbeat at the handler's current clock.
func (f *healthFixture) beat(t *testing.T) {
	t.Helper()
	rec := f.do(http.MethodPost, "/api/internal/v1/heartbeat", `{"uptime_seconds": 12.5}`, healthWorkerToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

type healthzBody struct {
	Status   string `json:"status"`
	Detector struct {
		Status     string   `json:"status"`
		LastSeen   *string  `json:"last_seen"`
		AgeSeconds *float64 `json:"age_seconds"`
		WindowSecs int      `json:"window_seconds"`
	} `json:"detector"`
}

func (f *healthFixture) healthz(t *testing.T) (int, healthzBody) {
	t.Helper()
	rec := f.do(http.MethodGet, "/api/healthz", "", "")
	if rec.Body.Len() == 0 {
		t.Fatalf("healthz answered %d with no body", rec.Code)
	}
	var body healthzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body does not parse: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, body
}

// TestHealthzFreshStaleBoundaryAtNinetySeconds asserts the line on both sides:
// 89.9 s after a beat is fresh, 90.0 s is stale. The window is three missed
// 30 s beats, and "exactly 90" belongs to stale — a beat that has aged the
// whole window has missed its next one.
func TestHealthzFreshStaleBoundaryAtNinetySeconds(t *testing.T) {
	f := newHealthFixture(t)
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	f.Handlers.SetClock(func() time.Time { return base })

	f.beat(t)

	code, body := f.healthz(t)
	if code != http.StatusOK || body.Status != "ok" || body.Detector.Status != "fresh" {
		t.Fatalf("just after a beat: got code=%d status=%s detector=%s, want 200 ok fresh",
			code, body.Status, body.Detector.Status)
	}
	if body.Detector.LastSeen == nil || body.Detector.AgeSeconds == nil {
		t.Fatalf("a fresh detector must carry last_seen and age_seconds: %+v", body.Detector)
	}

	// 89.9 s: still inside the window.
	f.Handlers.SetClock(func() time.Time { return base.Add(89900 * time.Millisecond) })
	_, body = f.healthz(t)
	if body.Detector.Status != "fresh" {
		t.Fatalf("at 89.9 s: detector=%s, want fresh", body.Detector.Status)
	}

	// 90.0 s: the window is `< 90 s`, so this side is stale — and still a 200.
	f.Handlers.SetClock(func() time.Time { return base.Add(90 * time.Second) })
	code, body = f.healthz(t)
	if code != http.StatusOK {
		t.Fatalf("stale is a 200, not an error: got %d", code)
	}
	if body.Detector.Status != "stale" || body.Status != "degraded" {
		t.Fatalf("at 90.0 s: status=%s detector=%s, want degraded stale", body.Status, body.Detector.Status)
	}
}

// TestHealthzNeverReportedIsStaleNotError is the fresh-installation answer: no
// heartbeat row has ever existed, and the wall must say "stale" with a 200 —
// not ok, not an error.
func TestHealthzNeverReportedIsStaleNotError(t *testing.T) {
	f := newHealthFixture(t)

	code, body := f.healthz(t)
	if code != http.StatusOK {
		t.Fatalf("never-reported must answer 200: got %d", code)
	}
	if body.Status != "degraded" || body.Detector.Status != "stale" {
		t.Fatalf("never-reported: status=%s detector=%s, want degraded stale", body.Status, body.Detector.Status)
	}
	if body.Detector.LastSeen != nil || body.Detector.AgeSeconds != nil {
		t.Fatalf("never-reported carries no last_seen and no age: %+v", body.Detector)
	}
}

// TestHealthzUnparseableHeartbeatIsStale: a damaged settings row is a stale
// detector, not a 500 — the health endpoint's job is to answer.
func TestHealthzUnparseableHeartbeatIsStale(t *testing.T) {
	f := newHealthFixture(t)
	if err := f.Handlers.St.Settings.Set("detector_heartbeat_at", "not-a-timestamp"); err != nil {
		t.Fatalf("seeding the damaged row: %v", err)
	}

	code, body := f.healthz(t)
	if code != http.StatusOK || body.Detector.Status != "stale" {
		t.Fatalf("damaged row: code=%d detector=%s, want 200 stale", code, body.Detector.Status)
	}
}

// TestHealthNeverTouchesTheDB closes the store's underlying database and asks
// /health anyway. A handler that read the store in any way would fail here;
// the signature taking no store is the structural half of the guarantee.
func TestHealthNeverTouchesTheDB(t *testing.T) {
	f := newHealthFixture(t)

	sqlDB, err := f.Handlers.St.DB.DB()
	if err != nil {
		t.Fatalf("reaching the sql handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("closing the database: %v", err)
	}

	rec := f.do(http.MethodGet, "/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/health with a closed database: got %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/health body does not parse: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("/health status=%v, want ok", body["status"])
	}
}

// TestHeartbeatRequiresTheWorkerToken: the beat is an internal write. No token
// is a 401; an installation with WORKER_TOKEN empty gets the internal API
// switched off, which is the documented 403.
func TestHeartbeatRequiresTheWorkerToken(t *testing.T) {
	f := newHealthFixture(t)

	rec := f.do(http.MethodPost, "/api/internal/v1/heartbeat", `{"uptime_seconds": 1}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
	rec = f.do(http.MethodPost, "/api/internal/v1/heartbeat", `{"uptime_seconds": 1}`, "wrong-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rec.Code)
	}

	f.Handlers.Cfg.WorkerToken = ""
	// The middleware closes the group at mount time from the config value, so
	// switching it off for the test means a fresh router.
	off := routerFor(t, f.Handlers)
	req := httptest.NewRequest(http.MethodPost, "/api/internal/v1/heartbeat", strings.NewReader(`{"uptime_seconds": 1}`))
	recOff := httptest.NewRecorder()
	off.ServeHTTP(recOff, req)
	if recOff.Code != http.StatusForbidden {
		t.Fatalf("empty WORKER_TOKEN: got %d, want 403", recOff.Code)
	}
}

// TestHeartbeatValidatesUptime: the body must carry a non-negative number. The
// value is validated and then not stored — the stored fact is the receipt
// stamp on the server's clock.
func TestHeartbeatValidatesUptime(t *testing.T) {
	f := newHealthFixture(t)

	for _, body := range []string{`{}`, `{"uptime_seconds": -1}`, `{"uptime_seconds": "long"}`} {
		rec := f.do(http.MethodPost, "/api/internal/v1/heartbeat", body, healthWorkerToken)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: got %d, want 400", body, rec.Code)
		}
	}

	// And a good beat moves the healthz answer.
	f.beat(t)
	_, hb := f.healthz(t)
	if hb.Detector.Status != "fresh" {
		t.Fatalf("after a good beat: detector=%s, want fresh", hb.Detector.Status)
	}
}
