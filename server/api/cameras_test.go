package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"nvr/server/config"
	"nvr/server/go2rtc"
	"nvr/server/models"
	"nvr/server/store"
	"nvr/server/ws"
)

// The camera suite runs against a fake go2rtc: the probe, the snapshot and the
// config write are all sidecar calls, and the assertions that matter are what the
// handler sent and how it answered. The fake records them.

type mediaFake struct {
	server *httptest.Server

	mu           sync.Mutex
	streamsJSON  string
	frameBody    []byte
	frameQuery   url.Values
	configWrites int
	lastConfig   string
	deleted      []string
	answered     []string
}

func newMediaFake(t *testing.T) *mediaFake {
	t.Helper()
	f := &mediaFake{streamsJSON: `{}`, frameBody: []byte("\xff\xd8\xff\x0a\xff\xd9")}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.answered = append(f.answered, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/streams":
			body := f.streamsJSON
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/api/config":
			body := make([]byte, 0)
			buf := make([]byte, 64)
			for {
				n, err := r.Body.Read(buf)
				body = append(body, buf[:n]...)
				if err != nil {
					break
				}
			}
			f.configWrites++
			f.lastConfig = string(body)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodDelete && r.URL.Path == "/api/streams":
			f.deleted = append(f.deleted, r.URL.Query().Get("name"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/api/frame.jpeg":
			q := r.URL.Query()
			frame := f.frameBody
			f.frameQuery = q
			f.mu.Unlock()
			w.Header().Set("Content-Type", "image/jpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(frame)
			return
		default:
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *mediaFake) client() *go2rtc.Client {
	return go2rtc.NewClient(&config.Config{Go2rtcURL: f.server.URL})
}

func (f *mediaFake) setStreams(json string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamsJSON = json
}

func (f *mediaFake) configCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configWrites
}

func (f *mediaFake) lastConfigBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastConfig
}

func (f *mediaFake) deletedStreams() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// answeredCalls is the sidecar's call log: the order the endpoints were hit,
// which is how the boot sequence's "probe, then apply" is asserted.
func (f *mediaFake) answeredCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.answered...)
}

func (f *mediaFake) snapshotQuery() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.frameQuery
}

// cameraFixture is a store, handlers and router with a sidecar and a media tree:
// the camera routes are the first ones that touch both.
type cameraFixture struct {
	Store        *store.Store
	Handlers     *Handlers
	Router       *gin.Engine
	Server       *httptest.Server
	Hub          *ws.Hub
	Media        *mediaFake
	SegmentsDir  string
	ClipsDir     string
	SnapshotsDir string

	AdminTok    string
	OperatorTok string
	ViewerTok   string
}

func newCameraFixture(t *testing.T) *cameraFixture {
	t.Helper()
	st := newStore(t)
	f := newMediaFake(t)

	h := newHandlers(t, st)
	segments := t.TempDir()
	clips := t.TempDir()
	snapshots := t.TempDir()
	h.Cfg.SegmentsDir = segments
	h.Cfg.ClipsDir = clips
	h.Cfg.SnapshotsDir = snapshots
	h.Cfg.Go2rtcURL = f.server.URL
	h.Cfg.WebURL = "http://localhost:3000"
	h.Go2rtc = f.client()
	// The confirm window is a 5 s wait in production; the suite measures it with
	// the clock, not with a sleep.
	h.Go2rtc.SetSleep(func(time.Duration) {})

	jwt := h.JWT
	admin := newUser(t, st, "cam-admin", models.RoleAdmin)
	operator := newUser(t, st, "cam-operator", models.RoleOperator)
	viewer := newUser(t, st, "cam-viewer", models.RoleViewer)

	// The hub is mounted before the router is built: the `/ws` route exists only
	// when a hub is there at registration time.
	hub := ws.NewHub(jwt, []string{"http://localhost:3000"})
	h.Hub = hub

	r := routerFor(t, h)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	return &cameraFixture{
		Store: st, Handlers: h, Router: r, Server: server, Hub: hub, Media: f,
		SegmentsDir: segments, ClipsDir: clips, SnapshotsDir: snapshots,
		AdminTok:    accessToken(t, st, jwt, admin),
		OperatorTok: accessToken(t, st, jwt, operator),
		ViewerTok:   accessToken(t, st, jwt, viewer),
	}
}

func cameraBody(slug, source string) map[string]any {
	return map[string]any{
		"slug":              slug,
		"name":              "Cam " + slug,
		"source_uri":        source,
		"record_mode":       models.RecordModeContinuous,
		"detection_enabled": true,
		"detection_fps":     5,
		"detection_width":   640,
		"detection_height":  360,
	}
}

func createdCameraID(t *testing.T, fx *cameraFixture, body map[string]any, bearer string) string {
	t.Helper()
	status, out, raw := do(t, fx.Router, request{method: http.MethodPost, path: "/api/v1/cameras", body: body, bearer: bearer})
	if status != http.StatusCreated {
		t.Fatalf("create camera: got %d %s, want 201", status, raw)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("the created camera is answered with its row: %s", raw)
	}
	return id
}

// TestCamerasCRUDRoundTrip is the shape of the surface: create, read, update,
// delete, and the config write each mutation owes.
func TestCamerasCRUDRoundTrip(t *testing.T) {
	fx := newCameraFixture(t)

	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	status, list, _ := do(t, fx.Router, request{method: http.MethodGet, path: "/api/v1/cameras", bearer: fx.ViewerTok})
	if status != http.StatusOK {
		t.Fatalf("list: %d", status)
	}
	rows, ok := list["cameras"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("the list is the cameras: %v", list)
	}
	row := rows[0].(map[string]any)
	if row["slug"] != "front-door" || row["id"] != id {
		t.Fatalf("row: %v", row)
	}
	if _, has := row["zones"]; !has {
		t.Fatalf("the row answers its zones as a parsed array: %v", row)
	}
	if row["name"] != "Cam front-door" {
		t.Fatalf("name: %v", row["name"])
	}

	status, one, _ := do(t, fx.Router, request{method: http.MethodGet, path: "/api/v1/cameras/" + id, bearer: fx.ViewerTok})
	if status != http.StatusOK || one["slug"] != "front-door" {
		t.Fatalf("get: %d %v", status, one)
	}

	status, updated, _ := do(t, fx.Router, request{
		method: http.MethodPut, path: "/api/v1/cameras/" + id, bearer: fx.OperatorTok,
		body: map[string]any{"name": "Front Door (west)"},
	})
	if status != http.StatusOK || updated["name"] != "Front Door (west)" {
		t.Fatalf("update: %d %v", status, updated)
	}
	if updated["source_uri"] != "rtsp://cam:554/live" {
		t.Fatalf("a PUT that omits fields must keep them: %v", updated)
	}

	status, _, raw := do(t, fx.Router, request{method: http.MethodDelete, path: "/api/v1/cameras/" + id, bearer: fx.AdminTok})
	if status != http.StatusOK {
		t.Fatalf("delete: %d %s", status, raw)
	}
	if status, _, _ := do(t, fx.Router, request{method: http.MethodGet, path: "/api/v1/cameras/" + id, bearer: fx.ViewerTok}); status != http.StatusNotFound {
		t.Fatal("the camera is gone")
	}
	if status, list, _ := do(t, fx.Router, request{method: http.MethodGet, path: "/api/v1/cameras", bearer: fx.ViewerTok}); status != http.StatusOK {
		t.Fatalf("list after delete: %d %v", status, list)
	}
}

func TestCamerasUnknownIDIs404(t *testing.T) {
	fx := newCameraFixture(t)
	for _, path := range []string{"/api/v1/cameras/nope", "/api/v1/cameras/nope/snapshot", "/api/v1/cameras/nope/probe"} {
		method := http.MethodGet
		if strings.Contains(path, "probe") {
			method = http.MethodPost
		}
		status, _, _ := do(t, fx.Router, request{method: method, path: path, bearer: fx.OperatorTok})
		if status != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404", path, status)
		}
	}
}

// TestCamerasInvalidSlugNamesThePattern: the slug is the stream name, the RTSP
// path and the file prefix, so it is refused with the pattern in the message.
func TestCamerasInvalidSlugNamesThePattern(t *testing.T) {
	fx := newCameraFixture(t)

	for _, slug := range []string{"Front-Door", "front door", "-door", "d", "door/../x", strings.Repeat("d", 33)} {
		body := cameraBody(slug, "rtsp://cam:554/live")
		status, out, _ := do(t, fx.Router, request{method: http.MethodPost, path: "/api/v1/cameras", body: body, bearer: fx.OperatorTok})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("slug %q: got %d, want 422", slug, status)
		}
		msg, _ := out["error"].(string)
		if !strings.Contains(msg, "^[a-z0-9][a-z0-9-]{1,31}$") {
			t.Fatalf("slug %q: the message must name the pattern the operator can satisfy: %q", slug, msg)
		}
	}
	if count, _, _ := fx.Store.Cameras.Count(); count != 0 {
		t.Fatal("nothing was written")
	}
}

func TestCamerasDuplicateSlugIs409(t *testing.T) {
	fx := newCameraFixture(t)

	createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	status, out, raw := do(t, fx.Router, request{
		method: http.MethodPost, path: "/api/v1/cameras",
		body: cameraBody("front-door", "rtsp://other:554/main"), bearer: fx.OperatorTok,
	})
	if status != http.StatusConflict {
		t.Fatalf("a second row on one stream name: got %d %s, want 409", status, raw)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "slug taken") || !strings.Contains(msg, "front-door") {
		t.Fatalf("the 409 names the slug: %q", msg)
	}

	// The same collision through the update path: the slug is the stream name, so
	// a PUT cannot move a camera onto another camera's name either.
	other := createdCameraID(t, fx, cameraBody("back-patio", "rtsp://cam2:554/live"), fx.OperatorTok)
	status, _, raw = do(t, fx.Router, request{
		method: http.MethodPut, path: "/api/v1/cameras/" + other, bearer: fx.OperatorTok,
		body: map[string]any{"slug": "front-door"},
	})
	if status != http.StatusConflict {
		t.Fatalf("PUT onto a taken slug: got %d %s, want 409", status, raw)
	}
}

func TestCamerasUpdateKeepsTheFieldsTheBodyOmits(t *testing.T) {
	fx := newCameraFixture(t)

	body := cameraBody("front-door", "rtsp://cam:554/live")
	body["remote_retention_days"] = 90
	body["local_retention_days"] = 7
	body["sensitivity"] = 0.61
	id := createdCameraID(t, fx, body, fx.OperatorTok)

	// A console that sends only the field it edited must not zero the retention
	// the operator set from another screen: Store.Update writes every mutable
	// column, so the handler merges onto the row.
	status, updated, _ := do(t, fx.Router, request{
		method: http.MethodPut, path: "/api/v1/cameras/" + id, bearer: fx.OperatorTok,
		body: map[string]any{"name": "Renamed only"},
	})
	if status != http.StatusOK {
		t.Fatalf("update: %d", status)
	}
	if updated["remote_retention_days"] != float64(90) {
		t.Fatalf("remote_retention_days survived: %v", updated["remote_retention_days"])
	}
	if updated["local_retention_days"] != float64(7) {
		t.Fatalf("local_retention_days survived: %v", updated["local_retention_days"])
	}
	if updated["sensitivity"] != 0.61 {
		t.Fatalf("sensitivity survived: %v", updated["sensitivity"])
	}
	if updated["record_mode"] != models.RecordModeContinuous {
		t.Fatalf("record_mode survived: %v", updated["record_mode"])
	}
}

func TestCamerasRejectsAnUnusableRecordMode(t *testing.T) {
	fx := newCameraFixture(t)

	body := cameraBody("front-door", "rtsp://cam:554/live")
	body["record_mode"] = "always"
	status, out, _ := do(t, fx.Router, request{method: http.MethodPost, path: "/api/v1/cameras", body: body, bearer: fx.OperatorTok})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("record_mode: %d", status)
	}
	if !strings.Contains(out["error"].(string), "continuous") {
		t.Fatalf("name the modes that exist: %v", out["error"])
	}
}

// TestCamerasRejectsATapGo2rtcCannotOpen: a detection tap with no frame rate is a
// camera that is green in the tile and detecting nothing forever.
func TestCamerasRejectsATapGo2rtcCannotOpen(t *testing.T) {
	fx := newCameraFixture(t)
	fx.Handlers.Cfg.Go2rtcMjpegFPS = 0 // the installation default is 0, and config_test says an explicit 0 is honoured

	body := cameraBody("front-door", "rtsp://cam:554/live")
	body["detection_fps"] = 0
	status, out, _ := do(t, fx.Router, request{method: http.MethodPost, path: "/api/v1/cameras", body: body, bearer: fx.OperatorTok})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a zero-rate tap: got %d, want 422", status)
	}
	if !strings.Contains(out["error"].(string), "GO2RTC_MJPEG_FPS") {
		t.Fatalf("the message names the knob: %v", out["error"])
	}
	if count, _, _ := fx.Store.Cameras.Count(); count != 0 {
		t.Fatal("nothing was written")
	}
}

// TestCamerasProbeAnswersFromTheStreamMap: the row always says the camera exists,
// so the probe's answer is the sidecar's, not the row's.
func TestCamerasProbeAnswersFromTheStreamMap(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	fx.Media.setStreams(`{"front-door": {"producers": [{"format":"rtsp","address":"rtsp://cam:554/live"}], "consumers": []}}`)
	status, out, _ := do(t, fx.Router, request{method: http.MethodPost, path: "/api/v1/cameras/" + id + "/probe", bearer: fx.OperatorTok})
	if status != http.StatusOK {
		t.Fatalf("probe: %d", status)
	}
	if out["online"] != true {
		t.Fatalf("a stream with a producer is online: %v", out)
	}
	producers, ok := out["producers"].([]any)
	if !ok || len(producers) != 1 || producers[0] != "rtsp://cam:554/live" {
		t.Fatalf("producers are go2rtc's own words for what feeds the stream: %v", out["producers"])
	}
	if out["slug"] != "front-door" || out["camera_id"] != id {
		t.Fatalf("the probe answer names the camera it probed: %v", out)
	}

	// Configured, in the config, and not connecting: present with no producers.
	fx.Media.setStreams(`{"front-door": {}}`)
	status, out, _ = do(t, fx.Router, request{method: http.MethodPost, path: "/api/v1/cameras/" + id + "/probe", bearer: fx.OperatorTok})
	if status != http.StatusOK || out["online"] != false {
		t.Fatalf("an unfed stream is offline: %d %v", status, out)
	}
	if list, ok := out["producers"].([]any); !ok || len(list) != 0 {
		t.Fatalf("an unfed stream has no producers: %v", out["producers"])
	}

	// Not in the map at all: the sidecar is not running this camera's config.
	fx.Media.setStreams(`{}`)
	status, out, _ = do(t, fx.Router, request{method: http.MethodPost, path: "/api/v1/cameras/" + id + "/probe", bearer: fx.OperatorTok})
	if status != http.StatusOK || out["online"] != false {
		t.Fatalf("absent from the sidecar is offline: %d %v", status, out)
	}
}

func TestCamerasProbeReportsASidecarThatWillNotAnswer(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	fx.Media.server.Close() // the sidecar is gone

	status, out, _ := do(t, fx.Router, request{method: http.MethodPost, path: "/api/v1/cameras/" + id + "/probe", bearer: fx.OperatorTok})
	if status != http.StatusBadGateway {
		t.Fatalf("a probe with no sidecar is not an online camera: got %d", status)
	}
	if !strings.Contains(out["error"].(string), "go2rtc") {
		t.Fatalf("say which peer failed: %v", out["error"])
	}
	rows := auditRows(t, fx.Store, models.AuditCameraProbe)
	if len(rows) == 0 || rows[0].Outcome != models.AuditOutcomeError {
		t.Fatalf("the failed probe is a ledger row: %+v", rows)
	}
}

// TestCamerasSnapshotProxiesTheFrameWithTheCachePromise.
func TestCamerasSnapshotProxiesTheFrameWithTheCachePromise(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	rec := snapshotRequest(t, fx, id, fx.ViewerTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=5" {
		t.Fatalf("Cache-Control = %q, want the plan's \"private, max-age=5\"", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type = %q", got)
	}
	if rec.Body.Len() < 4 {
		t.Fatal("the frame bytes are the body")
	}

	query := fx.Media.snapshotQuery()
	if query.Get("src") != "front-door" {
		t.Fatalf("the snapshot reads the camera's own stream: %v", query)
	}
	if query.Get("width") != "640" || query.Get("cache") != "5s" {
		t.Fatalf("the snapshot is the plan's 640px frame with a 5s cache: %v", query)
	}
}

func TestCamerasSnapshotReportsACameraThatIsNotProducing(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	// go2rtc answers 200 with an empty body when nothing feeds the source: the
	// client calls that a rejection, and the console should be able to draw "no
	// snapshot yet" from it rather than read an outage.
	fx.Media.mu.Lock()
	fx.Media.frameBody = []byte{}
	fx.Media.mu.Unlock()

	rec := snapshotRequest(t, fx, id, fx.ViewerTok)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a camera with no producer is a 404, not a 200 with no image: got %d", rec.Code)
	}
}

func TestCamerasSnapshotWithoutASidecarSaysSo(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	fx.Handlers.Go2rtc = nil

	rec := snapshotRequest(t, fx, id, fx.ViewerTok)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no sidecar configured is not a server fault: got %d", rec.Code)
	}
}

// TestCamerasDeleteRemovesRowsLocalMediaAndQueuesTheRemotePurge.
func TestCamerasDeleteRemovesRowsLocalMediaAndQueuesTheRemotePurge(t *testing.T) {
	fx := newCameraFixture(t)

	backend := &models.StorageBackend{
		Name: "local-tier", Kind: models.StorageKindFS, Enabled: true, IsDefault: true,
		RootPath: t.TempDir(),
	}
	if err := fx.Store.StorageBackends.Create(backend); err != nil {
		t.Fatalf("creating the storage tier: %v", err)
	}

	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	// One segment already on the tier, one only local, one left on disk by a
	// crashed ffmpeg with no row at all.
	recorded := &models.Segment{
		ID: "seg-remote", CameraID: id, Generation: 1,
		Path:    filepath.Join(fx.SegmentsDir, "front-door-1-0.mp4"),
		StartTS: time.Now().UTC().Add(-time.Minute), EndTS: time.Now().UTC().Add(-time.Minute + 5*time.Second),
		Seconds: 5, Bytes: 1024, Kind: models.SegmentKindContinuous,
	}
	if err := fx.Store.Segments.Create(recorded); err != nil {
		t.Fatalf("creating the uploaded segment: %v", err)
	}
	if err := fx.Store.Segments.MarkUploaded(recorded.ID, "nvr/front-door/2026/11/02/front-door-1-0.mp4"); err != nil {
		t.Fatalf("marking it uploaded: %v", err)
	}
	local := &models.Segment{
		ID: "seg-local", CameraID: id, Generation: 1,
		Path:    filepath.Join(fx.SegmentsDir, "front-door-1-1.mp4"),
		StartTS: time.Now().UTC(), EndTS: time.Now().UTC().Add(5 * time.Second),
		Seconds: 5, Bytes: 512, Kind: models.SegmentKindContinuous,
	}
	if err := fx.Store.Segments.Create(local); err != nil {
		t.Fatalf("creating the local segment: %v", err)
	}

	for _, path := range []string{recorded.Path, local.Path,
		filepath.Join(fx.SegmentsDir, "front-door-9-9.mp4"), // written, never reported
		filepath.Join(fx.SegmentsDir, "front-door.csv"),     // the recorder's segment list
		filepath.Join(fx.SnapshotsDir, "front-door-20261102.jpg"),
		filepath.Join(fx.ClipsDir, "front-door-clip.mp4"),
		filepath.Join(fx.SegmentsDir, "back-patio-1-0.mp4"), // another camera: not ours to touch
	} {
		if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	status, out, raw := do(t, fx.Router, request{method: http.MethodDelete, path: "/api/v1/cameras/" + id, bearer: fx.AdminTok})
	if status != http.StatusOK {
		t.Fatalf("delete: %d %s", status, raw)
	}
	if out["deleted"] != id {
		t.Fatalf("the answer names what was deleted: %s", raw)
	}

	if _, err := fx.Store.Cameras.GetByID(id); err == nil {
		t.Fatal("the camera row is gone")
	}
	if total, _, err := fx.Store.Segments.CountByCamera(id); err != nil || total != 0 {
		t.Fatalf("the camera's segment rows are gone: %d %v", total, err)
	}

	// The camera's files are gone, the other camera's are not.
	for _, name := range []string{"front-door-1-0.mp4", "front-door-1-1.mp4", "front-door-9-9.mp4", "front-door.csv"} {
		if _, err := os.Stat(filepath.Join(fx.SegmentsDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s is still on disk", name)
		}
	}
	for _, pair := range [][2]string{{fx.SnapshotsDir, "front-door-20261102.jpg"}, {fx.ClipsDir, "front-door-clip.mp4"}} {
		if _, err := os.Stat(filepath.Join(pair[0], pair[1])); !os.IsNotExist(err) {
			t.Fatalf("%s is still on disk", pair[1])
		}
	}
	if _, err := os.Stat(filepath.Join(fx.SegmentsDir, "back-patio-1-0.mp4")); err != nil {
		t.Fatal("a delete must not touch another camera's footage")
	}

	// The uploaded segment is queued for remote deletion; the local-only one is
	// not (there is nothing remote to purge).
	jobs, err := fx.Store.StorageJobs.ListByState(models.JobStatePending, 50, 0)
	if err != nil {
		t.Fatalf("reading the queue: %v", err)
	}
	var purged []models.StorageJob
	for _, job := range jobs {
		if job.Action == models.JobActionDeleteRemote {
			purged = append(purged, job)
		}
	}
	if len(purged) != 1 {
		t.Fatalf("exactly the uploaded segment is queued for remote purge: %+v", jobs)
	}
	if purged[0].ID != recorded.ID || purged[0].BackendID != backend.ID {
		t.Fatalf("the job names the object and its tier: %+v", purged[0])
	}
	// The key layout is nvr/<slug>/<YYYY>/<MM>/<DD>/<basename> (S7-07): the job
	// carries it, so the purge survives the deletion of the row that named it.
	if purged[0].Target != "nvr/front-door/2026/11/02/front-door-1-0.mp4" {
		t.Fatalf("the job carries the remote key, so it outlives the segment row: %q", purged[0].Target)
	}

	rows := auditRows(t, fx.Store, models.AuditCameraDelete)
	if len(rows) == 0 {
		t.Fatal("the delete is a ledger row")
	}
	details := map[string]any{}
	if err := json.Unmarshal([]byte(rows[0].Details), &details); err != nil {
		t.Fatalf("the delete's ledger row carries JSON details: %v", err)
	}
	if details["segments"] != float64(2) {
		t.Fatalf("the row says how much footage went with it: %v", details)
	}
	if details["remote_jobs"] != float64(1) {
		t.Fatalf("the row says what was queued: %v", details)
	}
}

func TestCamerasDeleteWithoutAStorageTierIsNotAnError(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	status, out, raw := do(t, fx.Router, request{method: http.MethodDelete, path: "/api/v1/cameras/" + id, bearer: fx.AdminTok})
	if status != http.StatusOK {
		t.Fatalf("a local-only installation is deletable: got %d %s", status, raw)
	}
	if out["remote_jobs"] != float64(0) {
		t.Fatalf("nothing remote to purge: %v", out)
	}
}

func TestCamerasDeleteIsAdminOnly(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	// Operator: the CRUD floor stops at the delete, because deleting a camera
	// destroys its footage ledger.
	status, out, _ := do(t, fx.Router, request{method: http.MethodDelete, path: "/api/v1/cameras/" + id, bearer: fx.OperatorTok})
	if status != http.StatusForbidden {
		t.Fatalf("operator delete: got %d, want 403", status)
	}
	if out["error"] != "insufficient role" {
		t.Fatalf("the refusal is the named one: %v", out)
	}
	// Step 2's denial registry already names this route's verb, so the refusal is
	// a `camera.delete` row with outcome refused — the ledger answers "who tried to
	// delete a camera" without knowing which card registered the route.
	rows := auditRows(t, fx.Store, models.AuditCameraDelete)
	refused := 0
	for _, row := range rows {
		if row.Outcome == models.AuditOutcomeRefused && row.TargetID == id {
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("the denial is a ledger row naming the camera: %+v", rows)
	}
	if _, err := fx.Store.Cameras.GetByID(id); err != nil {
		t.Fatal("the refusal left the camera alone")
	}
}

// TestCamerasWriteSurvivesTheRowAndReportsTheConfig: rows are the truth, so a
// camera the database has and go2rtc does not is a 502, never a 200.
func TestCamerasWriteSurvivesTheRowAndReportsTheConfig(t *testing.T) {
	fx := newCameraFixture(t)
	fx.Media.server.Close() // the sidecar will not take the config

	body := cameraBody("front-door", "rtsp://cam:554/live")
	rec := doRaw(t, fx.Router, request{method: http.MethodPost, path: "/api/v1/cameras", body: body, bearer: fx.OperatorTok})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("a write whose config the sidecar refused is a 502, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "camera saved, go2rtc config not applied") {
		t.Fatalf("the answer says what is true: %s", rec.Body.String())
	}
	// The row is not rolled back: boot re-applies, the recorder polls rows, and a
	// 200 here would be an operator who believes the camera is live.
	count, _, err := fx.Store.Cameras.Count()
	if err != nil || count != 1 {
		t.Fatalf("the row landed: %d %v", count, err)
	}
}

func TestCamerasEachWriteReRendersTheConfigFromTheRows(t *testing.T) {
	fx := newCameraFixture(t)

	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	if fx.Media.configCount() != 1 {
		t.Fatalf("the create applied the config once, got %d", fx.Media.configCount())
	}
	body := fx.Media.lastConfigBody()
	if !strings.Contains(body, `front-door: ["rtsp://cam:554/live"]`) {
		t.Fatalf("the config sent is the rendered rows: %s", body)
	}

	do(t, fx.Router, request{method: http.MethodPut, path: "/api/v1/cameras/" + id, bearer: fx.OperatorTok,
		body: map[string]any{"source_uri": "rtsp://cam:554/sub"}})
	if fx.Media.configCount() != 2 {
		t.Fatalf("the update applied the config again, got %d", fx.Media.configCount())
	}
	if !strings.Contains(fx.Media.lastConfigBody(), `front-door: ["rtsp://cam:554/sub"]`) {
		t.Fatalf("the re-render carries the new source: %s", fx.Media.lastConfigBody())
	}

	do(t, fx.Router, request{method: http.MethodDelete, path: "/api/v1/cameras/" + id, bearer: fx.AdminTok})
	if fx.Media.configCount() != 3 {
		t.Fatalf("the delete applied the config too, got %d", fx.Media.configCount())
	}
	if !strings.Contains(fx.Media.lastConfigBody(), "streams: {}") {
		t.Fatalf("with the camera gone the sidecar is told so: %s", fx.Media.lastConfigBody())
	}
}

// TestCamerasZoneWritesDoNotReRenderTheConfig: the sidecar knows streams, not
// rules, so the one-write-path rule is about stream-bearing writes.
func TestCamerasZoneWritesDoNotReRenderTheConfig(t *testing.T) {
	fx := newCameraFixture(t)
	id := createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)
	before := fx.Media.configCount()

	do(t, fx.Router, request{method: http.MethodPut, path: "/api/v1/cameras/" + id + "/zones", bearer: fx.OperatorTok,
		body: []map[string]any{zoneBody("z1")}})

	if fx.Media.configCount() != before {
		t.Fatalf("a zone write does not touch the sidecar: %d writes", fx.Media.configCount())
	}
}

// TestCamerasCameraWriteBroadcastsCameraStatus: a camera write that does not move
// the tile is a tile that lies until the next poll.
func TestCamerasCameraWriteBroadcastsCameraStatus(t *testing.T) {
	fx := newCameraFixture(t)

	fx.Media.setStreams(`{"front-door": {"producers": [{"format":"rtsp","address":"rtsp://cam:554/live"}]}}`)
	frames := watchConsole(t, fx)
	createdCameraID(t, fx, cameraBody("front-door", "rtsp://cam:554/live"), fx.OperatorTok)

	found := false
	for !found {
		select {
		case frame := <-frames:
			if frame["type"] != "camera.status" {
				continue
			}
			found = true
			payload, _ := frame["payload"].(map[string]any)
			if payload["slug"] != "front-door" || payload["online"] != true {
				t.Fatalf("the tile is told the truth: %v", payload)
			}
			if list, ok := payload["producers"].([]any); !ok || len(list) != 1 {
				t.Fatalf("the broadcast carries the producers: %v", payload)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the write broadcast: no camera.status frame reached the console")
		}
	}
}
