// The internal worker API's suite (S5-10).
//
// Every test here is named `TestInternal…` because the card's verification runs
// `go test ./api -run Internal`, and a suite that only runs when someone remembers
// to drop the filter is a suite that does not run.
//
// The signals route is asserted as the worker sees it: one POST, a batch of
// signals, and a per-signal answer. The refusals are the interesting part — an
// unknown camera and a camera whose detection was switched off are both facts a
// retry cannot change, so they must be 200s with an error on the signal, and a
// worker that buffered them forever would be a worker with a growing backlog and
// no events.

package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"nvr/server/media"
	"nvr/server/models"
	"nvr/server/store"
)

// detectorFixture is a store, a recorder and a router with the internal routes
// mounted behind a worker token of the test's choosing.
type detectorFixture struct {
	Store        *store.Store
	Handlers     *Handlers
	Router       *gin.Engine
	Recorder     *media.Recorder
	SnapshotsDir string
	WorkerTok    string
}

const testWorkerToken = "worker-token-for-the-internal-api"

func newDetectorFixture(t *testing.T, workerToken string) *detectorFixture {
	t.Helper()
	st := newStore(t)
	h := newHandlers(t, st)

	snapshots := t.TempDir()
	h.Cfg.WorkerToken = workerToken
	h.Cfg.SnapshotsDir = snapshots
	// The event window's length is the post-roll. Left at the zero value a fixture
	// has a window that is over before it began, and "the signal opened the window"
	// becomes unobservable.
	h.Cfg.PostRollSeconds = 15
	// Two providers, comma-separated, with a space after the comma: the split is
	// the deliverable, so the fixture feeds it the shape an operator types.
	h.Cfg.DetProviders = "CUDAExecutionProvider, CPUExecutionProvider"
	h.Cfg.EventMaxSeconds = 300
	h.Cfg.Go2rtcMjpegWidth = 640
	h.Cfg.Go2rtcMjpegHeight = 360
	h.Cfg.Go2rtcMjpegFPS = 5

	// The recorder is the process-wide one main.go builds. The window bookkeeping
	// is what the signal path touches, and it needs no ffmpeg to be right.
	rec := media.NewRecorder(h.Cfg, st, nil)
	h.Recorder = rec

	r := routerFor(t, h)
	return &detectorFixture{
		Store: st, Handlers: h, Router: r, Recorder: rec,
		SnapshotsDir: snapshots, WorkerTok: workerToken,
	}
}

// addCamera puts a camera row in the store directly. The camera routes need a
// sidecar and an operator token; the worker API needs a row, and this test is
// about the row.
func (f *detectorFixture) addCamera(t *testing.T, slug string, detectionEnabled, active bool, mode string, zones string) *models.Camera {
	t.Helper()
	if zones == "" {
		zones = `[{"id":"z1","name":"driveway","polygon":[[0,0],[1,0],[1,1],[0,1]],` +
			`"classes":["person"],"rule":"loiter","dwell_seconds":30,"severity":3,` +
			`"min_confidence":0.4,"min_area_ratio":0.004,"enabled":true}]`
	}
	cam := &models.Camera{
		Slug:             slug,
		Name:             strings.ToTitle(slug),
		SourceURI:        "rtsp://cam/" + slug,
		RecordMode:       mode,
		DetectionEnabled: detectionEnabled,
		DetectionFPS:     5,
		Sensitivity:      0.45,
		MinAreaRatio:     0.004,
		ZonesJSON:        zones,
		Active:           active,
	}
	if err := f.Store.Cameras.Create(cam); err != nil {
		t.Fatalf("create camera %s: %v", slug, err)
	}
	return cam
}

// signal is one signal body. The defaults are an open on a driveway zone; tests
// override the one field they are about.
func signal(cameraID, eventKey, action string) map[string]any {
	return map[string]any{
		"camera_id":   cameraID,
		"event_key":   eventKey,
		"action":      action,
		"rule":        models.ZoneRuleIntrusion,
		"zone_id":     "z1",
		"zone_name":   "driveway",
		"severity":    3,
		"classes":     []any{"person"},
		"ts":          time.Now().UTC().Format(time.RFC3339),
		"objects":     []any{map[string]any{"class": "person", "conf": 0.9, "box": []any{100.0, 50.0, 140.0, 150.0}}},
		"record_mode": models.RecordModeContinuous,
	}
}

func postSignals(t *testing.T, f *detectorFixture, signals []map[string]any, bearer string) (int, map[string]any, string) {
	t.Helper()
	return do(t, f.Router, request{
		method: http.MethodPost,
		path:   "/api/internal/v1/signals",
		body:   map[string]any{"signals": signals},
		bearer: bearer,
	})
}

// eventSignals indexes the per-signal answers by position.
func repliedEvents(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["events"].([]any)
	if !ok {
		t.Fatalf("the answer names its per-signal results under \"events\": %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("each result is an object: %v", item)
		}
		out = append(out, m)
	}
	return out
}

// -- the config poll -------------------------------------------------------

func TestInternalDetectorConfigAnswersOnlyTheCamerasTheWorkerShouldWatch(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	watched := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")
	f.addCamera(t, "quiet", false, true, models.RecordModeContinuous, "")
	f.addCamera(t, "retired", true, false, models.RecordModeContinuous, "")

	status, body, raw := do(t, f.Router, request{
		method: http.MethodGet, path: "/api/internal/v1/detector/config", bearer: testWorkerToken,
	})
	if status != http.StatusOK {
		t.Fatalf("config poll: got %d %s, want 200", status, raw)
	}

	if got := body["providers"]; fmt.Sprint(got) != "[CUDAExecutionProvider CPUExecutionProvider]" {
		t.Errorf("providers came from DET_PROVIDERS split: got %v", got)
	}
	settings, _ := body["settings"].(map[string]any)
	if settings == nil {
		t.Fatalf("the payload carries its settings: %v", body)
	}
	if settings["event_max_seconds"] != float64(300) {
		t.Errorf("settings.event_max_seconds = %v, want 300 (EVENT_MAX_SECONDS)", settings["event_max_seconds"])
	}
	if settings["det_width"] != float64(640) || settings["det_height"] != float64(360) || settings["det_fps"] != float64(5) {
		t.Errorf("the detection tap is in the payload: got %v", settings)
	}

	cameras, _ := body["cameras"].([]any)
	if len(cameras) != 1 {
		t.Fatalf("only detection_enabled && active cameras are offered: got %d of 3 rows: %v", len(cameras), body)
	}
	cam, _ := cameras[0].(map[string]any)
	if cam["id"] != watched.ID {
		t.Errorf("the offered camera is the watched one: got %v, want %s", cam["id"], watched.ID)
	}
	if cam["slug"] != "porch" || cam["det_stream"] != "porch_det" {
		t.Errorf("the tap is named by the slug this server wrote into go2rtc: got %v / %v", cam["slug"], cam["det_stream"])
	}
	if cam["record_mode"] != models.RecordModeContinuous {
		t.Errorf("record_mode comes from the row: got %v", cam["record_mode"])
	}
	if cam["fps"] != float64(5) || cam["sensitivity"] != 0.45 || cam["min_area_ratio"] != 0.004 {
		t.Errorf("the camera's own floors travel with it: got %v", cam)
	}
	if providers, _ := cam["providers"].([]any); len(providers) != 2 {
		t.Errorf("each camera carries the provider list: got %v", cam["providers"])
	}

	zones, _ := cam["zones"].([]any)
	if len(zones) != 1 {
		t.Fatalf("the zone rules the operator drew are in the payload: got %v", cam["zones"])
	}
	zone, _ := zones[0].(map[string]any)
	for field, want := range map[string]any{
		"id": "z1", "name": "driveway", "rule": "loiter", "severity": float64(3),
		"dwell_seconds": float64(30), "min_confidence": 0.4, "enabled": true,
	} {
		if zone[field] != want {
			t.Errorf("zone %s = %v, want %v", field, zone[field], want)
		}
	}
	if classes, _ := zone["classes"].([]any); len(classes) != 1 || classes[0] != "person" {
		t.Errorf("zone classes: got %v", zone["classes"])
	}
	if poly, _ := zone["polygon"].([]any); len(poly) != 4 {
		t.Errorf("the polygon is the four vertices the operator drew: got %v", zone["polygon"])
	}
}

func TestInternalDetectorConfigAnswersAnEmptyCameraListNotNull(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)

	status, body, raw := do(t, f.Router, request{
		method: http.MethodGet, path: "/api/internal/v1/detector/config", bearer: testWorkerToken,
	})
	if status != http.StatusOK {
		t.Fatalf("config poll with no cameras: got %d %s, want 200", status, raw)
	}
	cameras, ok := body["cameras"].([]any)
	if !ok || len(cameras) != 0 {
		t.Fatalf("an installation with no detection switched on answers [], not null: %v", body["cameras"])
	}
}

func TestInternalDetectorConfigSkipsADisabledZone(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	f.addCamera(t, "porch", true, true, models.RecordModeContinuous,
		`[{"id":"z1","name":"on","polygon":[[0,0],[1,0],[1,1],[0,1]],"classes":[],"rule":"intrusion","dwell_seconds":0,"severity":1,"min_confidence":0,"min_area_ratio":0,"enabled":true},`+
			`{"id":"z2","name":"off","polygon":[[0,0],[1,0],[1,1],[0,1]],"classes":[],"rule":"intrusion","dwell_seconds":0,"severity":1,"min_confidence":0,"min_area_ratio":0,"enabled":false}]`)

	_, body, _ := do(t, f.Router, request{
		method: http.MethodGet, path: "/api/internal/v1/detector/config", bearer: testWorkerToken,
	})
	cameras, _ := body["cameras"].([]any)
	cam, _ := cameras[0].(map[string]any)
	zones, _ := cam["zones"].([]any)
	if len(zones) != 1 {
		t.Fatalf("a zone the operator switched off is not sent: got %v", cam["zones"])
	}
	if zone, _ := zones[0].(map[string]any); zone["id"] != "z1" {
		t.Errorf("the enabled zone is the one kept: got %v", zones[0])
	}
}

// -- the routes are behind the worker token --------------------------------

func TestInternalRoutesAreBehindTheWorkerTokenGate(t *testing.T) {
	routes := []struct {
		method string
		path   string
		body   map[string]any
	}{
		{http.MethodGet, "/api/internal/v1/detector/config", nil},
		{http.MethodPost, "/api/internal/v1/signals", map[string]any{"signals": []any{}}},
	}

	// An empty WORKER_TOKEN is the internal API switched off, and that has to be
	// true of the routes themselves, not only of a request that reaches them.
	off := newDetectorFixture(t, "")
	for _, route := range routes {
		status, body, raw := do(t, off.Router, request{method: route.method, path: route.path, body: route.body, bearer: testWorkerToken})
		if status != http.StatusForbidden {
			t.Errorf("%s %s with WORKER_TOKEN empty: got %d %s, want 403", route.method, route.path, status, raw)
		}
		if body["error"] != "internal api disabled" {
			t.Errorf("%s %s answered %v, want \"internal api disabled\"", route.method, route.path, body["error"])
		}
	}

	on := newDetectorFixture(t, testWorkerToken)
	for _, bearer := range []string{"", "wrong-token", "worker-token-for-the-internal-ap"} {
		for _, route := range routes {
			status, body, raw := do(t, on.Router, request{method: route.method, path: route.path, body: route.body, bearer: bearer})
			if status != http.StatusUnauthorized {
				t.Errorf("%s %s with bearer %q: got %d %s, want 401", route.method, route.path, bearer, status, raw)
			}
			if body["error"] != "internal token is not valid" {
				t.Errorf("%s %s with bearer %q answered %v", route.method, route.path, bearer, body["error"])
			}
		}
	}

	// A user's JWT is not a worker token: the two surfaces do not accept each
	// other's credentials.
	fx := newCameraFixture(t)
	for _, route := range routes {
		status, _, _ := do(t, fx.Router, request{method: route.method, path: route.path, body: route.body, bearer: fx.AdminTok})
		if status != http.StatusForbidden {
			t.Errorf("an admin JWT on %s %s: got %d, want 403 (the internal API is the worker's)", route.method, route.path, status)
		}
	}

	for _, route := range routes {
		status, _, raw := do(t, on.Router, request{method: route.method, path: route.path, body: route.body, bearer: testWorkerToken})
		if status != http.StatusOK {
			t.Errorf("%s %s with the worker's own token: got %d %s, want 200", route.method, route.path, status, raw)
		}
	}
}

// -- the signals batch -----------------------------------------------------

func TestInternalSignalsSkipAnUnknownCameraAndACameraWithDetectionDisabled(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	quiet := f.addCamera(t, "quiet", false, true, models.RecordModeContinuous, "")

	unknown := "00000000-0000-0000-0000-000000000000"
	_, body, raw := postSignals(t, f, []map[string]any{
		signal(unknown, unknown+":z1:intrusion:1000", models.SignalActionOpen),
		signal(quiet.ID, quiet.ID+":z1:intrusion:1000", models.SignalActionOpen),
	}, testWorkerToken)

	events := repliedEvents(t, body)
	if len(events) != 2 {
		t.Fatalf("every signal is answered: %v", body)
	}
	if events[0]["error"] != "unknown camera" {
		t.Errorf("an unknown camera_id is skipped with \"unknown camera\": got %v (%s)", events[0], raw)
	}
	if events[1]["error"] != "detection disabled" {
		t.Errorf("a camera with detection off is skipped with \"detection disabled\": got %v", events[1])
	}
	if body["accepted"] != float64(0) {
		t.Errorf("neither signal was accepted: got %v", body["accepted"])
	}

	// The refusals are 200s. A 4xx would send a worker into its retry loop over a
	// signal that can never be accepted, and the buffer would never drain.
	status, _, _ := postSignals(t, f, []map[string]any{signal(unknown, "k", models.SignalActionOpen)}, testWorkerToken)
	if status != http.StatusOK {
		t.Errorf("a refused signal answers 200: got %d", status)
	}

	if total, err := f.Store.Events.List(store.EventFilter{}, 0); err != nil || len(total) != 0 {
		t.Fatalf("a skipped signal files no event row: got %d rows (%v)", len(total), err)
	}
}

func TestInternalSignalsMirrorTheEventStateMachine(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")
	key := cam.ID + ":z1:intrusion:1000"

	_, opened, _ := postSignals(t, f, []map[string]any{signal(cam.ID, key, models.SignalActionOpen)}, testWorkerToken)
	events := repliedEvents(t, opened)
	if events[0]["error"] != nil {
		t.Fatalf("the open was accepted: %v", events[0])
	}
	if events[0]["action"] != models.SignalActionOpen || events[0]["event_key"] != key {
		t.Errorf("the answer names the event it applied: got %v", events[0])
	}
	id, _ := events[0]["id"].(string)
	if id == "" {
		t.Fatalf("the answer carries the row's id so the console can be told about it: %v", events[0])
	}
	if opened["accepted"] != float64(1) {
		t.Errorf("accepted = %v, want 1", opened["accepted"])
	}

	_, extended, _ := postSignals(t, f, []map[string]any{signal(cam.ID, key, models.SignalActionExtend)}, testWorkerToken)
	if got := repliedEvents(t, extended)[0]; got["id"] != id || got["action"] != models.SignalActionExtend {
		t.Errorf("an extend lands on the same row: got %v, want id %s", got, id)
	}

	_, closed, _ := postSignals(t, f, []map[string]any{signal(cam.ID, key, models.SignalActionClose)}, testWorkerToken)
	if got := repliedEvents(t, closed)[0]; got["id"] != id || got["action"] != models.SignalActionClose {
		t.Errorf("the close ends the same row: got %v", got)
	}

	// The late signal: a worker re-sending a buffered extend after the close.
	_, late, _ := postSignals(t, f, []map[string]any{signal(cam.ID, key, models.SignalActionExtend)}, testWorkerToken)
	got := repliedEvents(t, late)[0]
	if got["error"] != "event closed" {
		t.Errorf("a signal for a closed event answers \"event closed\": got %v", got)
	}
	if got["id"] != id {
		t.Errorf("and it names the event it refused, so the worker can drop it: got %v", got)
	}

	row, err := f.Store.Events.GetByKey(key)
	if err != nil {
		t.Fatalf("fetch the event: %v", err)
	}
	if row.SignalCount != 3 {
		t.Errorf("the refused signal mutated nothing: signal_count = %d, want 3", row.SignalCount)
	}
	if row.Open {
		t.Errorf("the refused signal did not reopen a closed event")
	}
}

func TestInternalSignalsRefuseASignalWhoseTimestampWillNotParse(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")

	bad := signal(cam.ID, cam.ID+":z1:intrusion:1000", models.SignalActionOpen)
	bad["ts"] = "yesterday, around four"
	_, body, _ := postSignals(t, f, []map[string]any{bad}, testWorkerToken)
	got := repliedEvents(t, body)[0]
	if got["error"] == nil || !strings.Contains(got["error"].(string), "RFC3339") {
		t.Errorf("a ts this server cannot parse is refused by name: got %v", got)
	}

	// The worker's clock is not the event's clock: a ts an hour in the future is
	// accepted for its shape and then discarded.
	ahead := signal(cam.ID, cam.ID+":z1:intrusion:2000", models.SignalActionOpen)
	ahead["ts"] = time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	_, body, _ = postSignals(t, f, []map[string]any{ahead}, testWorkerToken)
	if got := repliedEvents(t, body)[0]; got["error"] != nil {
		t.Fatalf("a future ts is accepted: %v", got)
	}
	row, err := f.Store.Events.GetByKey(cam.ID + ":z1:intrusion:2000")
	if err != nil {
		t.Fatalf("fetch the event: %v", err)
	}
	if row.StartedAt.After(time.Now().UTC()) {
		t.Errorf("started_at is the server's clock, not the worker's: got %v", row.StartedAt)
	}
}

func TestInternalSignalsRequireABody(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	status, body, _ := do(t, f.Router, request{
		method: http.MethodPost, path: "/api/internal/v1/signals", body: map[string]any{"signals": "not a list"}, bearer: testWorkerToken,
	})
	if status != http.StatusBadRequest {
		t.Errorf("a body that is not the contract answers 400: got %d %v", status, body)
	}
	if body["error"] == nil {
		t.Errorf("and says why: %v", body)
	}
}

// -- the snapshot ----------------------------------------------------------

func TestInternalSignalsWriteTheSnapshotTheOpenCarried(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")

	s := signal(cam.ID, cam.ID+":z1:intrusion:1000", models.SignalActionOpen)
	s["snapshot_base64"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("j", 3000)))
	_, body, _ := postSignals(t, f, []map[string]any{s}, testWorkerToken)
	got := repliedEvents(t, body)[0]
	if got["error"] != nil {
		t.Fatalf("the open was accepted: %v", got)
	}
	id, _ := got["id"].(string)

	path := filepath.Join(f.SnapshotsDir, id+".jpg")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the snapshot is filed at <SNAPSHOTS_DIR>/<event_id>.jpg: %v", err)
	}
	if len(raw) != 3000 {
		t.Errorf("the snapshot is the bytes the worker sent: got %d, want 3000", len(raw))
	}
	row, err := f.Store.Events.GetByID(id)
	if err != nil {
		t.Fatalf("fetch the event: %v", err)
	}
	if row.LocalSnapshotPath != path {
		t.Errorf("the row points at the file: got %q, want %q", row.LocalSnapshotPath, path)
	}
}

func TestInternalSignalsStoreAnOversizedSnapshotAsAbsentAndKeepTheEvent(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")

	// 260 KB of base64 decodes to ~195 KB, under the cap; 400 KB of raw bytes is
	// over it, and that is the case the card names.
	oversized := signal(cam.ID, cam.ID+":z1:intrusion:1000", models.SignalActionOpen)
	oversized["snapshot_base64"] = base64.StdEncoding.EncodeToString(make([]byte, MaxSignalSnapshotBytes+1))
	_, body, _ := postSignals(t, f, []map[string]any{oversized}, testWorkerToken)
	got := repliedEvents(t, body)[0]
	if got["error"] != nil {
		t.Fatalf("an oversized snapshot does not refuse the signal: %v", got)
	}
	id, _ := got["id"].(string)

	row, err := f.Store.Events.GetByID(id)
	if err != nil {
		t.Fatalf("the event survives: %v", err)
	}
	if row.LocalSnapshotPath != "" {
		t.Errorf("the snapshot is stored as absent: got %q", row.LocalSnapshotPath)
	}
	if _, err := os.Stat(filepath.Join(f.SnapshotsDir, id+".jpg")); !os.IsNotExist(err) {
		t.Errorf("no file was written for a snapshot over the cap: %v", err)
	}

	// A malformed snapshot is the same story: the picture is lost, the event is not.
	broken := signal(cam.ID, cam.ID+":z1:intrusion:2000", models.SignalActionOpen)
	broken["snapshot_base64"] = "this is not base64 at all: @@@@"
	_, body, _ = postSignals(t, f, []map[string]any{broken}, testWorkerToken)
	if got := repliedEvents(t, body)[0]; got["error"] != nil {
		t.Errorf("a snapshot that will not decode does not refuse the signal: %v", got)
	}
}

// -- the recorder window ---------------------------------------------------

func TestInternalSignalsOpenTheRecorderWindowOnlyForAnEventsCamera(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	eventsCam := f.addCamera(t, "gate", true, true, models.RecordModeEvents, "")
	continuousCam := f.addCamera(t, "drive", true, true, models.RecordModeContinuous, "")

	_, body, _ := postSignals(t, f, []map[string]any{signal(eventsCam.ID, eventsCam.ID+":z1:intrusion:1000", models.SignalActionOpen)}, testWorkerToken)
	if got := repliedEvents(t, body)[0]; got["error"] != nil {
		t.Fatalf("the open was accepted: %v", got)
	}
	end, ok := f.Recorder.WindowEnd(eventsCam.ID)
	if !ok {
		t.Fatalf("a record_mode=events signal opens the recorder window (S4-01)")
	}
	if !end.After(time.Now().UTC()) {
		t.Errorf("the window ends in the future: got %v", end)
	}

	_, body, _ = postSignals(t, f, []map[string]any{signal(continuousCam.ID, continuousCam.ID+":z1:intrusion:1000", models.SignalActionOpen)}, testWorkerToken)
	if got := repliedEvents(t, body)[0]; got["error"] != nil {
		t.Fatalf("the continuous open was accepted: %v", got)
	}
	if _, ok := f.Recorder.WindowEnd(continuousCam.ID); ok {
		t.Errorf("a continuous camera is already recording; a signal must not open a window on it")
	}

	// The row is the authority, not the message: the signal's copy of the mode is
	// as fresh as the last config poll, and a camera the operator switched back to
	// `continuous` is already recording.
	fromSignal := signal(continuousCam.ID, continuousCam.ID+":z1:intrusion:3000", models.SignalActionOpen)
	fromSignal["record_mode"] = models.RecordModeEvents
	_, body, _ = postSignals(t, f, []map[string]any{fromSignal}, testWorkerToken)
	got := repliedEvents(t, body)[0]
	if got["error"] != nil {
		t.Fatalf("the open was accepted: %v", got)
	}
	if _, ok := f.Recorder.WindowEnd(continuousCam.ID); ok {
		t.Errorf("a continuous row does not open a window because a signal said events")
	}
}

func TestInternalSignalsTolerateANilRecorder(t *testing.T) {
	// A server built without the recorder (the suite's, and any process that has
	// not started it yet) still files events; the window is a side effect.
	f := newDetectorFixture(t, testWorkerToken)
	f.Handlers.Recorder = nil
	cam := f.addCamera(t, "gate", true, true, models.RecordModeEvents, "")

	_, body, _ := postSignals(t, f, []map[string]any{signal(cam.ID, cam.ID+":z1:intrusion:1000", models.SignalActionOpen)}, testWorkerToken)
	if got := repliedEvents(t, body)[0]; got["error"] != nil {
		t.Errorf("signals are ingested with no recorder: got %v", got)
	}
}

// -- the batch as the worker sends it --------------------------------------

func TestInternalSignalsAnswerOneBatchOfMixedSignalsInOrder(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	watched := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")
	quiet := f.addCamera(t, "quiet", false, true, models.RecordModeContinuous, "")

	batch := []map[string]any{
		signal(watched.ID, watched.ID+":z1:intrusion:1", models.SignalActionOpen),
		signal(quiet.ID, quiet.ID+":z1:intrusion:2", models.SignalActionOpen),
		signal(watched.ID, watched.ID+":z1:intrusion:3", models.SignalActionOpen),
		signal("00000000-0000-0000-0000-000000000000", "nobody:z1:intrusion:4", models.SignalActionExtend),
	}
	_, body, _ := postSignals(t, f, batch, testWorkerToken)

	if body["accepted"] != float64(2) {
		t.Errorf("accepted counts the signals that landed: got %v, want 2", body["accepted"])
	}
	events := repliedEvents(t, body)
	if len(events) != 4 {
		t.Fatalf("one result per signal: %v", body)
	}
	wantErrors := []any{nil, "detection disabled", nil, "unknown camera"}
	for i, want := range wantErrors {
		if events[i]["error"] != want {
			t.Errorf("signal %d answered %v, want %v", i, events[i]["error"], want)
		}
	}
	if events[0]["id"] == events[2]["id"] {
		t.Errorf("two different keys are two different events")
	}
}
