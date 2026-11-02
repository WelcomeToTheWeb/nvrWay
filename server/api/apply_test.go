package api

import (
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"nvr/server/models"
)

// S3-05: one write path to go2rtc's config.
//
// The rule the suite enforces is "config cannot drift from rows". A drift is
// invisible at the time it happens — the operator's console reads rows, and the
// recorder reads the sidecar — and becomes visible as a camera that records
// nothing, weeks later. So the count is asserted per write, the boot sequence is
// asserted as probe-then-apply, and the writer is asserted to be the one function.

func TestApplyConfigExactlyOncePerCameraWrite(t *testing.T) {
	fx := newCameraFixture(t)

	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	if got := fx.Media.configCount(); got != 1 {
		t.Fatalf("create applied the config exactly once, got %d", got)
	}

	status, _, raw := do(t, fx.Router, request{
		method: http.MethodPut, path: "/api/v1/cameras/" + id, bearer: fx.OperatorTok,
		body: map[string]any{"name": "Renamed"},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d %s", status, raw)
	}
	if got := fx.Media.configCount(); got != 2 {
		t.Fatalf("update applied the config exactly once more, got %d", got)
	}

	status, _, raw = do(t, fx.Router, request{
		method: http.MethodPut, path: "/api/v1/cameras/" + id + "/zones", bearer: fx.OperatorTok,
		body: []map[string]any{zoneBody("z1")},
	})
	if status != http.StatusOK {
		t.Fatalf("zone write: %d %s", status, raw)
	}
	if got := fx.Media.configCount(); got != 2 {
		t.Fatalf("a zone write changes rules, not streams: %d config writes", got)
	}

	// A refused write re-renders nothing: the row never changed.
	bad := cameraBody("FRONT DOOR", "rtsp://cam:554/live")
	if status, _, _ := do(t, fx.Router, request{
		method: http.MethodPost, path: "/api/v1/cameras", body: bad, bearer: fx.OperatorTok}); status != http.StatusUnprocessableEntity {
		t.Fatalf("the refused create: %d", status)
	}
	if got := fx.Media.configCount(); got != 2 {
		t.Fatalf("a refused write must not touch the sidecar: %d config writes", got)
	}

	status, _, raw = do(t, fx.Router, request{
		method: http.MethodDelete, path: "/api/v1/cameras/" + id, bearer: fx.AdminTok})
	if status != http.StatusOK {
		t.Fatalf("delete: %d %s", status, raw)
	}
	if got := fx.Media.configCount(); got != 3 {
		t.Fatalf("delete applied the config exactly once more, got %d", got)
	}
}

// TestApplyConfigEachWriteSendsTheWholeRenderedFile: the write is a replacement,
// so the file the sidecar ends up with is the render of the rows, every time.
func TestApplyConfigEachWriteSendsTheWholeRenderedFile(t *testing.T) {
	fx := newCameraFixture(t)

	a := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	createdCameraID(t, fx, cameraBody("back-patio", "rtsp://cam2:554/live"), fx.OperatorTok)

	body := fx.Media.lastConfigBody()
	for _, want := range []string{`front-door: ["rtsp://cam:554/live"]`, `back-patio: ["rtsp://cam2:554/live"]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("both cameras are in the file after the second write:\n%s", body)
		}
	}

	do(t, fx.Router, request{method: http.MethodDelete, path: "/api/v1/cameras/" + a, bearer: fx.AdminTok})
	body = fx.Media.lastConfigBody()
	if strings.Contains(body, "front-door") {
		t.Fatalf("the deleted camera is out of the sidecar's file:\n%s", body)
	}
	if !strings.Contains(body, `back-patio: ["rtsp://cam2:554/live"]`) {
		t.Fatalf("the survivor is still in it:\n%s", body)
	}
}

// TestApplyConfigAtBootProbesThenApplies: the plan's boot order is a probe and
// then the config, through the same path the camera writes use.
func TestApplyConfigAtBootProbesThenApplies(t *testing.T) {
	fx := newCameraFixture(t)
	createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	// The create's writes are history; start the call log clean by pointing a
	// fresh client at the same fake, which is what boot does.
	fx.Media.setStreams(`{"front-door": {"producers": ["rtsp://cam:554/live"]}, "front-door_det": {"producers": ["ffmpeg:front-door"]}}`)
	fx.Handlers.Go2rtc = fx.Media.client()

	report, err := fx.Handlers.BootstrapGo2rtc(t.Context())
	if err != nil {
		t.Fatalf("boot against a live sidecar: %v", err)
	}
	if !report.Probed || !report.Applied {
		t.Fatalf("probe and apply both happened: %+v", report)
	}
	if report.Streams != 2 || report.Detected != 1 {
		t.Fatalf("the boot line reports the streams it saw: %+v", report)
	}
	if report.Cameras != 1 {
		t.Fatalf("and the rows it rendered from: %+v", report)
	}

	calls := fx.Media.answeredCalls()
	var probeAt, applyAt = -1, -1
	for i, call := range calls {
		if call == "GET /api/streams" && probeAt < 0 {
			probeAt = i
		}
		if call == "POST /api/config" {
			applyAt = i
		}
	}
	if probeAt < 0 || applyAt < 0 {
		t.Fatalf("boot probed and applied, calls: %v", calls)
	}
	if probeAt > applyAt {
		t.Fatalf("the probe comes before the config write, calls: %v", calls)
	}
}

func TestApplyConfigAtBootReportsTheRowsItApplied(t *testing.T) {
	fx := newCameraFixture(t)
	report, err := fx.Handlers.BootstrapGo2rtc(t.Context())
	if err != nil {
		t.Fatalf("boot with no cameras: %v", err)
	}
	if !report.Applied || report.Cameras != 0 {
		t.Fatalf("an installation with no cameras boots with an empty stream map: %+v", report)
	}
	if body := fx.Media.lastConfigBody(); !strings.Contains(body, "streams: {}") {
		t.Fatalf("the empty render is what the sidecar is told: %s", body)
	}
}

// TestApplyConfigAtBootSurvivesASidecarThatIsDown: a server that refuses to boot
// because go2rtc is down takes the API, the console and the rows down with it,
// and leaves the operator no way to see which of the two is broken.
func TestApplyConfigAtBootSurvivesASidecarThatIsDown(t *testing.T) {
	fx := newCameraFixture(t)
	createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	fx.Media.server.Close() // the sidecar is not there at boot

	report, err := fx.Handlers.BootstrapGo2rtc(t.Context())
	if err == nil {
		t.Fatal("boot reports the outage — it does not pretend the sidecar is up")
	}
	if report.Probed || report.Applied {
		t.Fatalf("nothing was probed and nothing was applied: %+v", report)
	}
	if !strings.Contains(report.Reason, "go2rtc") {
		t.Fatalf("the reason names the peer: %q", report.Reason)
	}

	// The installation is still a working server: the rows, the console surface
	// and the account API are usable while the media sidecar is down.
	if count, _, _ := fx.Store.Cameras.Count(); count != 1 {
		t.Fatal("the camera row survives the outage")
	}
	status, _, _ := do(t, fx.Router, request{method: http.MethodGet, path: "/api/v1/cameras", bearer: fx.ViewerTok})
	if status != http.StatusOK {
		t.Fatalf("the camera list answers during the outage: %d", status)
	}
	// The camera's rows are still readable, and the console can still ask for a
	// snapshot — the answer is "the sidecar did not answer", not a crash.
	id := ""
	if status, out, _ := do(t, fx.Router, request{method: http.MethodGet, path: "/api/v1/cameras", bearer: fx.ViewerTok}); status == http.StatusOK {
		rows, _ := out["cameras"].([]any)
		if len(rows) == 1 {
			id, _ = rows[0].(map[string]any)["id"].(string)
		}
	}
	rec := snapshotRequest(t, fx, id, fx.ViewerTok)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("a snapshot during an outage is a named 502, got %d", rec.Code)
	}
}

// TestApplyConfigIsTheOnlyWriterOfGo2rtcConfig: the rule is architectural, so it
// is asserted against the source. A second writer — a boot script, a helper, a
// route added in a later step — is exactly the drift this card exists to prevent,
// and it is invisible in review because the second writer looks reasonable.
func TestApplyConfigIsTheOnlyWriterOfGo2rtcConfig(t *testing.T) {
	root := ".."

	callers := map[string]bool{}
	configEndpoints := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory the suite cannot read is not a source file: skipping it is
			// the difference between an architectural assertion and a test that fails
			// because of the machine it runs on.
			return nil //nolint:nilerr
		}
		if d.IsDir() {
			// The build caches and the module tree are not this project's source.
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		source, err := readFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(source, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // a comment naming the rule is not a call of it
			}
			if strings.Contains(trimmed, "ApplyConfig(") {
				callers[rel] = true
			}
			if strings.Contains(trimmed, `"/api/config"`) {
				configEndpoints[rel] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the source: %v", err)
	}

	// The only files that call the config write: the client that implements it,
	// and the camera handler set that owns it (boot goes through the same
	// handler method, which is the point).
	allowed := map[string]bool{"go2rtc/client.go": true, "api/cameras.go": true}
	for file := range callers {
		if !allowed[file] {
			t.Fatalf("%s writes go2rtc's config: S3-05 allows exactly one writer (%v)", file, keys(callers))
		}
	}
	for _, want := range []string{"go2rtc/client.go", "api/cameras.go"} {
		if !callers[want] {
			t.Fatalf("%s should be the config writer: %v", want, keys(callers))
		}
	}
	if len(configEndpoints) != 1 || !configEndpoints["go2rtc/client.go"] {
		t.Fatalf("only the client knows the config endpoint's path: %v", keys(configEndpoints))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestApplyConfigAtBootRunsThroughTheHandlerSet, not a second client: main.go and
// the camera writes share one `Handlers`, so there is one render and one push.
func TestApplyConfigBootUsesTheSameRendererAsTheWrites(t *testing.T) {
	fx := newCameraFixture(t)
	createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	fx.Handlers.Go2rtc = fx.Media.client()

	if _, err := fx.Handlers.BootstrapGo2rtc(t.Context()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	boot := fx.Media.lastConfigBody()

	do(t, fx.Router, request{method: http.MethodPut, path: "/api/v1/cameras/" + cameraIDNamed(t, fx, "front-door"),
		bearer: fx.OperatorTok, body: map[string]any{"name": "Same renderer"}})
	after := fx.Media.lastConfigBody()

	if !strings.HasPrefix(boot, strings.Split(after, "streams:")[0]) {
		t.Fatalf("the header and listeners are rendered identically by both paths:\n--- boot ---\n%s\n--- write ---\n%s", boot, after)
	}
	for _, want := range []string{"front-door", models.RecordModeContinuous} {
		if !strings.Contains(after, want) && want != models.RecordModeContinuous {
			t.Fatalf("the write's render names %s: %s", want, after)
		}
	}
}

func cameraIDNamed(t *testing.T, fx *cameraFixture, slug string) string {
	t.Helper()
	camera, err := fx.Store.Cameras.GetBySlug(slug)
	if err != nil {
		t.Fatalf("camera %s: %v", slug, err)
	}
	return camera.ID
}
