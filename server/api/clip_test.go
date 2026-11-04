package api

// S6-05 — clip delivery: the local copy with Range/206, and the tier copy
// streamed from byte 0.
//
// The three exit criteria are the local seek, the remote stream, and the seek
// gap being documented rather than hidden. The remote half is where the shape of
// the response matters most: there is no seek to recover with, so the headers
// have to be right before the first byte goes.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvr/server/models"
)

// clipFetch runs one request against the event routes and answers the recorder,
// so the assertions can read the headers. `do` answers status, body and raw
// bytes, and the media contract is mostly made of headers.
func clipFetch(t *testing.T, f *eventsFixture, token, method, path string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.Engine.ServeHTTP(rec, req)

	body := map[string]any{}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("the JSON answer is not JSON (%d %s): %q", rec.Code, path, rec.Body.String())
		}
	}
	return rec, body
}

// addFSBackend puts a storage tier on disk: the default backend row points at a
// temp directory, and its objects are files under it.
func (f *eventsFixture) addFSBackend(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	row := &models.StorageBackend{
		Name: name, Kind: models.StorageKindFS, RootPath: root,
		Enabled: true, IsDefault: true,
	}
	if err := f.Store.StorageBackends.Create(row); err != nil {
		t.Fatalf("creating the backend: %v", err)
	}
	return root
}

// writeTierObject files one object where the layout says it lives.
func writeTierObject(t *testing.T, root, key string, body []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// clipEvent files an event and gives it a clip: the local file, the tier copy,
// or neither. The row's upload stamp is what the route reads to decide whether
// the tier is worth asking.
func (f *eventsFixture) clipEvent(t *testing.T, cam *models.Camera, local, remoteKey string) *models.Event {
	t.Helper()

	ev := f.fileEvent(t, cam, eventSpec{
		Key:     fmt.Sprintf("%s:zone:person:%d", cam.ID, time.Now().UnixNano()),
		Started: time.Now().UTC().Add(-time.Hour),
		Seconds: 12, Severity: 2, Classes: []string{"person"},
	})
	if local != "" {
		if err := f.Store.Events.SetLocalClip(ev.ID, local, 12); err != nil {
			t.Fatalf("filing the local clip: %v", err)
		}
		ev.LocalClipPath = local
	}
	if remoteKey != "" {
		if err := f.Store.Events.MarkClipUploaded(ev.ID, remoteKey); err != nil {
			t.Fatalf("marking the clip uploaded: %v", err)
		}
		ev.ClipKey = remoteKey
		now := time.Now().UTC()
		ev.ClipUploadedAt = &now
	}
	return ev
}

func clipCamera(t *testing.T, f *eventsFixture) *models.Camera {
	t.Helper()
	cam := &models.Camera{
		Slug: "front-door", Name: "Front door", SourceURI: "rtsp://cam/front-door",
		RecordMode: models.RecordModeContinuous, Active: true,
	}
	if err := f.Store.Cameras.Create(cam); err != nil {
		t.Fatalf("creating the camera: %v", err)
	}
	return cam
}

func TestClipRangeRequestAnswers206AgainstATempMP4(t *testing.T) {
	// The scrubber's contract. A clip that cannot be ranged is a clip the drawer
	// downloads whole every time someone moves the play head.
	f := newEventsFixture(t)
	cam := clipCamera(t, f)

	body := make([]byte, 1200)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	path := filepath.Join(f.ClipsDir, "clip-1.mp4")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	ev := f.clipEvent(t, cam, path, "")

	rec, _ := clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/clip",
		map[string]string{"Range": "bytes=0-999"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range: bytes=0-999 answered %d, want 206", rec.Code)
	}
	if rec.Body.Len() != 1000 {
		t.Errorf("the 206 carried %d bytes, want the 1000 the window names", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-999/1200" {
		t.Errorf("Content-Range is %q, want the window and the whole size", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges is %q, want bytes", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type is %q, want video/mp4", got)
	}

	// A suffix range, which is what a player sends when it wants the tail.
	rec, _ = clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/clip",
		map[string]string{"Range": "bytes=1100-"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("a suffix range answered %d, want 206", rec.Code)
	}
	if rec.Body.Len() != 100 {
		t.Errorf("the tail range carried %d bytes, want 100", rec.Body.Len())
	}
}

func TestClipStreamsFromTheTierWithNoLocalCopy(t *testing.T) {
	f := newEventsFixture(t)
	cam := clipCamera(t, f)
	root := f.addFSBackend(t, "tier")

	const key = "nvr/events/evt-1/clip.mp4"
	writeTierObject(t, root, key, []byte("a clip that only exists on the tier"))
	ev := f.clipEvent(t, cam, "", key)

	rec, _ := clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/clip", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("a tier-only clip answered %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "a clip that only exists on the tier" {
		t.Errorf("the body is %q, want the object's bytes from byte 0", got)
	}

	// The contract's headers. There is no seek on a remote clip, so the size has
	// to be right and the cache has to be stated: a drawer that re-asks for the
	// whole object on every redraw is paying a tier read per frame.
	for header, want := range map[string]string{
		"Content-Type":   "video/mp4",
		"Accept-Ranges":  "bytes",
		"Cache-Control":  "private, max-age=3600",
		"Content-Length": "35",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s is %q, want %q", header, got, want)
		}
	}
}

func TestClipFromTheTierHasNoSeekAndSaysSoByAnswering200(t *testing.T) {
	// The plan's remote-playback gap, in the response rather than in a comment.
	// The tier's `Backend` reads from byte 0 and has no ranged read, so a Range
	// request gets the whole object with a 200 — the fallback HTTP sanctions, and
	// what a browser does with it. Seek-on-remote is the v2 item.
	f := newEventsFixture(t)
	cam := clipCamera(t, f)
	root := f.addFSBackend(t, "tier")

	const key = "nvr/events/evt-2/clip.mp4"
	writeTierObject(t, root, key, []byte("the whole object, from byte 0, every time"))
	ev := f.clipEvent(t, cam, "", key)

	rec, _ := clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/clip",
		map[string]string{"Range": "bytes=10-19"})
	if rec.Code != http.StatusOK {
		t.Fatalf("a range against a tier clip answered %d, want 200: this build cannot seek on the tier", rec.Code)
	}
	if got := rec.Body.String(); got != "the whole object, from byte 0, every time" {
		t.Errorf("the body is %q, want the whole object", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Errorf("a response that is not partial carries Content-Range %q", got)
	}
}

func TestClipPrefersTheLocalCopyOverTheTier(t *testing.T) {
	// Both copies exist, and the local one wins: it is already on this host, it
	// answers Range, and asking the tier for bytes this disk holds is a round trip
	// and a bill.
	f := newEventsFixture(t)
	cam := clipCamera(t, f)
	root := f.addFSBackend(t, "tier")

	const key = "nvr/events/evt-3/clip.mp4"
	writeTierObject(t, root, key, []byte("the tier copy"))
	local := filepath.Join(f.ClipsDir, "evt-3.mp4")
	if err := os.WriteFile(local, []byte("the local copy, which is nearer"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := f.clipEvent(t, cam, local, key)

	rec, _ := clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/clip",
		map[string]string{"Range": "bytes=0-3"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("with a local copy the route answered %d, want 206: the local file is the one that can seek", rec.Code)
	}
	if got := rec.Body.String(); got != "the " {
		t.Errorf("the body came from %q, want the local copy", got)
	}
}

func TestClipWithNoCopyAnywhereIsANamedFourOhFour(t *testing.T) {
	f := newEventsFixture(t)
	cam := clipCamera(t, f)
	f.addFSBackend(t, "tier")

	ev := f.clipEvent(t, cam, "", "")

	rec, body := clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/clip", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an event with no clip answered %d, want 404", rec.Code)
	}
	if body["error"] != "clip not available" {
		t.Errorf("the 404 says %v, want the named \"clip not available\"", body["error"])
	}
}

func TestClipOnATierThatLostTheObjectIsANamedFourOhFour(t *testing.T) {
	// The row says the clip was uploaded and the tier does not have it: aged out
	// by the retention sweep, or removed by hand. The event stands and its media
	// route says so — a 500 here would read as a server fault and send the
	// operator to the logs for a fact about retention.
	f := newEventsFixture(t)
	cam := clipCamera(t, f)
	f.addFSBackend(t, "tier")

	ev := f.clipEvent(t, cam, "", "nvr/events/evt-4/clip.mp4")

	rec, body := clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/clip", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a vanished tier object answered %d %v, want 404", rec.Code, body)
	}
	if body["error"] != "clip not available" {
		t.Errorf("the 404 says %v, want the named \"clip not available\"", body["error"])
	}
}

func TestClipWithNoStorageTierConfiguredIsANamedFourOhFour(t *testing.T) {
	// A local-only installation: the row carries an upload stamp from an
	// installation that had a tier, and this one does not. That is not a bad
	// gateway — there is no gateway to be bad.
	f := newEventsFixture(t)
	cam := clipCamera(t, f)

	ev := f.clipEvent(t, cam, "", "nvr/events/evt-5/clip.mp4")

	rec, body := clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/clip", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("with no backend configured the route answered %d %v, want 404", rec.Code, body)
	}
	if body["error"] != "clip not available" {
		t.Errorf("the 404 says %v, want the named \"clip not available\"", body["error"])
	}
}

func TestClipFromAnUnreachableTierIsABadGatewayWithTheNamedState(t *testing.T) {
	// The tier is configured and not usable. That is not the client's 404 and not
	// a mystery 500: the answer carries the named state, because
	// `bucket missing` and `bad credentials` are different fixes.
	f := newEventsFixture(t)
	cam := clipCamera(t, f)

	root := f.addFSBackend(t, "unmounted")
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	ev := f.clipEvent(t, cam, "", "nvr/events/evt-6/clip.mp4")

	rec, body := clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/clip", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("an unmounted tier answered %d %v, want 502", rec.Code, body)
	}
	if body["tier"] != "bucket missing" {
		t.Errorf("the answer carries tier=%v, want the named state the console draws", body["tier"])
	}
	if body["error"] != "the storage tier is not reachable" {
		t.Errorf("the answer says %v", body["error"])
	}
}

func TestSnapshotStreamsFromTheTierWhenTheClipDoes(t *testing.T) {
	// The two media routes resolve "where are the bytes" the same way, or they
	// disagree inside one drawer: a clip that plays next to a broken `<img>` is a
	// bug the console cannot fix from the event view.
	f := newEventsFixture(t)
	cam := clipCamera(t, f)
	root := f.addFSBackend(t, "tier")

	const snapshotKey = "nvr/events/evt-7/snapshot.jpg"
	const clipKey = "nvr/events/evt-7/clip.mp4"
	writeTierObject(t, root, snapshotKey, []byte("a jpeg that only exists on the tier"))
	writeTierObject(t, root, clipKey, []byte("a clip that only exists on the tier"))

	ev := f.fileEvent(t, cam, eventSpec{
		Key:     fmt.Sprintf("%s:zone:person:%d", cam.ID, time.Now().UnixNano()),
		Started: time.Now().UTC().Add(-time.Hour),
		Seconds: 12, Severity: 2, Classes: []string{"person"},
	})
	if err := f.Store.Events.MarkSnapshotUploaded(ev.ID, snapshotKey); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.Events.MarkClipUploaded(ev.ID, clipKey); err != nil {
		t.Fatal(err)
	}

	rec, _ := clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("a tier-only snapshot answered %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type is %q, want image/jpeg", got)
	}
	if got := rec.Body.String(); got != "a jpeg that only exists on the tier" {
		t.Errorf("the body is %q, want the object's bytes", got)
	}

	clip, _ := clipFetch(t, f, f.Viewer, http.MethodGet, "/api/v1/events/"+ev.ID+"/clip", nil)
	if clip.Code != http.StatusOK {
		t.Fatalf("the clip beside it answered %d, want 200", clip.Code)
	}
}

func TestClipRouteStillAnswersForAnEventThatDoesNotExist(t *testing.T) {
	f := newEventsFixture(t)
	clipCamera(t, f)

	_, body := clipFetch(t, f, f.Viewer, http.MethodGet,
		"/api/v1/events/00000000-0000-4000-8000-000000000000/clip", nil)
	if body["error"] != "event not found" {
		t.Errorf("an unknown event answered %v, want the named \"event not found\"", body["error"])
	}
}
