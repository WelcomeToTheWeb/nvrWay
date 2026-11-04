// The snapshot suite (card S6-02).
//
// S5-10's suite already proved the two halves of the write it owns: the bytes
// the `open` signal carried land at `<SNAPSHOTS_DIR>/<event_id>.jpg`, and a
// picture that is too big or will not decode is stored as absent while the event
// survives. What this suite owns is the rest of the card:
//
//   - **only an `open` writes a picture** — an extend or a close that carries
//     one writes nothing, because the frame that shows the trigger is the open's;
//   - **a retry fills a picture the first attempt lost, and overwrites nothing**
//     — the gate is "this row has no picture", not "this signal created the row";
//   - **the file is served** by `GET /api/v1/events/:id/snapshot`, and the
//     absence is named rather than improvised: an event with no picture is a
//     normal event, and its route answers a 404 with a name on it.
//
// The card's seeded verification named `./media`, which is where the plan put
// the snapshot write. The write is in the signal path (`api/detector_api.go`,
// S5-10) and the route is in `api/events.go`, so the suite is `./api` and the
// command on the card says so.

package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nvr/server/models"
)

// jpegOf is a picture-shaped payload: the server never decodes these bytes, it
// files them and hands them back, so the suite's job is to prove the bytes that
// arrive are the bytes that were sent.
func jpegOf(n int) []byte {
	frame := make([]byte, n)
	copy(frame, []byte("\xff\xd8\xff\xe0"))
	for i := 4; i < n; i++ {
		frame[i] = byte('a' + i%26)
	}
	return frame
}

// signalWith attaches a snapshot to one signal body.
func signalWith(cameraID, eventKey, action string, frame []byte) map[string]any {
	s := signal(cameraID, eventKey, action)
	if frame != nil {
		s["snapshot_base64"] = base64.StdEncoding.EncodeToString(frame)
	}
	return s
}

// signalID pulls the event id out of a per-signal answer.
func signalID(t *testing.T, body map[string]any) string {
	t.Helper()
	got := repliedEvents(t, body)[0]
	if got["error"] != nil {
		t.Fatalf("the signal was accepted: %v", got)
	}
	id, _ := got["id"].(string)
	if id == "" {
		t.Fatalf("the answer names the event it filed: %v", got)
	}
	return id
}

// viewerTokenFor is the read credential: a viewer is the role whose whole job is
// to look at events, so if a viewer cannot read a snapshot nobody can. The name
// is numbered because a test may need two sessions on one store.
var viewerSeq int

func viewerTokenFor(t *testing.T, f *detectorFixture) string {
	t.Helper()
	viewerSeq++
	viewer := newUser(t, f.Store, fmt.Sprintf("watcher-%d", viewerSeq), models.RoleViewer)
	return accessToken(t, f.Store, f.Handlers.JWT, viewer)
}

// getRaw is `do` for a body that is not JSON. It answers the headers too, which
// is where the content type and the cache promise live.
func getRaw(t *testing.T, f *detectorFixture, path, bearer string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.Router.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

// eventRow reads the event back, so an assertion is about the row and not about
// a struct a handler still holds.
func eventRow(t *testing.T, f *detectorFixture, id string) *models.Event {
	t.Helper()
	row, err := f.Store.Events.GetByID(id)
	if err != nil {
		t.Fatalf("reading event %s back: %v", id, err)
	}
	return row
}

// postOne sends a single signal and answers the event id it filed.
func postOne(t *testing.T, f *detectorFixture, s map[string]any) string {
	t.Helper()
	status, body, raw := postSignals(t, f, []map[string]any{s}, testWorkerToken)
	if status != http.StatusOK {
		t.Fatalf("signals POST: got %d %s, want 200", status, raw)
	}
	return signalID(t, body)
}

// TestSnapshotOfAnOpenSignalLandsOnDiskAndOnTheRow is the deliverable in one
// move: the bytes arrive on the signal, they are on disk under the event's own
// name, and the row says where.
func TestSnapshotOfAnOpenSignalLandsOnDiskAndOnTheRow(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")
	frame := jpegOf(2048)

	id := postOne(t, f, signalWith(cam.ID, "snap-land", models.SignalActionOpen, frame))

	path := filepath.Join(f.SnapshotsDir, id+".jpg")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the snapshot is filed at <SNAPSHOTS_DIR>/<event_id>.jpg: %v", err)
	}
	if string(raw) != string(frame) {
		t.Fatalf("the file holds %d bytes, want the %d the worker sent", len(raw), len(frame))
	}
	row := eventRow(t, f, id)
	if row.LocalSnapshotPath != path {
		t.Fatalf("the row points at the file: got %q, want %q", row.LocalSnapshotPath, path)
	}
	// 0o640: a picture of someone's property, read by this process and nobody
	// else on the box.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat'ing the snapshot: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("the snapshot is mode %O, want 0640", info.Mode().Perm())
	}
}

// TestSnapshotIsWrittenOnlyOnOpen: the frame that shows the trigger is the
// frame the open came on. An extend or a close that happens to carry a picture
// writes nothing — otherwise a five-minute loiter would replace its own evidence
// with whatever the last frame happened to be.
func TestSnapshotIsWrittenOnlyOnOpen(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")

	// The open arrives with no picture (the worker's encoder failed).
	id := postOne(t, f, signalWith(cam.ID, "snap-open-only", models.SignalActionOpen, nil))
	// The extend and the close carry one anyway.
	for _, action := range []string{models.SignalActionExtend, models.SignalActionClose} {
		s := signalWith(cam.ID, "snap-open-only", action, jpegOf(512))
		if got := postOne(t, f, s); got != id {
			t.Fatalf("the %s signal named the same event: got %s, want %s", action, got, id)
		}
	}

	entries, err := os.ReadDir(f.SnapshotsDir)
	if err != nil {
		t.Fatalf("reading the snapshots dir: %v", err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%d file(s) in SNAPSHOTS_DIR %v, want 0: only an open writes a picture", len(entries), names)
	}
	row := eventRow(t, f, id)
	if row.LocalSnapshotPath != "" {
		t.Fatalf("the row claims a snapshot it never got: %q", row.LocalSnapshotPath)
	}
	if row.SignalCount != 3 || row.Open {
		t.Fatalf("the event itself is intact: signal_count=%d open=%v, want 3 and closed", row.SignalCount, row.Open)
	}
}

// TestSnapshotReplayedOpenFillsAPictureTheFirstAttemptLost is the write rule
// under a retry. The gate is "this row has no picture", not "this signal created
// the row": a worker that re-sends an open whose picture the first attempt lost
// — over the cap, a disk that refused the write — gets a second chance, and the
// event does not go down in history pictureless because of one bad attempt.
func TestSnapshotReplayedOpenFillsAPictureTheFirstAttemptLost(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")

	// First attempt: over the cap, so the picture is dropped and the event stands.
	oversized := signalWith(cam.ID, "snap-retry", models.SignalActionOpen, make([]byte, MaxSignalSnapshotBytes+1))
	id := postOne(t, f, oversized)
	if row := eventRow(t, f, id); row.LocalSnapshotPath != "" {
		t.Fatalf("the oversized snapshot was stored as absent, got %q", row.LocalSnapshotPath)
	}

	// The worker re-sends the same open, this time with a picture that fits.
	replay := signalWith(cam.ID, "snap-retry", models.SignalActionOpen, jpegOf(1024))
	if got := postOne(t, f, replay); got != id {
		t.Fatalf("the replay named the same event: got %s, want %s", got, id)
	}
	row := eventRow(t, f, id)
	if row.LocalSnapshotPath == "" {
		t.Fatalf("the replayed open did not fill the missing picture")
	}
	raw, err := os.ReadFile(row.LocalSnapshotPath)
	if err != nil {
		t.Fatalf("reading the filled snapshot: %v", err)
	}
	if string(raw) != string(jpegOf(1024)) {
		t.Fatalf("the filled snapshot is %d bytes, want the 1024 the replay carried", len(raw))
	}
}

// TestSnapshotReplayDoesNotOverwriteAPictureThatExists is the other half of the
// same gate. A replayed open is the worker not having heard the answer, not a
// new observation: the frame that triggered the event is already on disk, and a
// later frame is not a better witness.
func TestSnapshotReplayDoesNotOverwriteAPictureThatExists(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")

	first := jpegOf(2048)
	id := postOne(t, f, signalWith(cam.ID, "snap-no-clobber", models.SignalActionOpen, first))
	path := filepath.Join(f.SnapshotsDir, id+".jpg")

	if again := postOne(t, f, signalWith(cam.ID, "snap-no-clobber", models.SignalActionOpen, jpegOf(99))); again != id {
		t.Fatalf("the replay named the same event: got %s, want %s", again, id)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the snapshot: %v", err)
	}
	if string(raw) != string(first) {
		t.Fatalf("a replay replaced the trigger frame: got %d bytes, want the %d the first open carried",
			len(raw), len(first))
	}
}

// TestSnapshotRouteServesTheJpeg is the third exit criterion: the file is
// served, as `image/jpeg`, byte for byte, behind a session. The Range assertion
// rides along because the route uses the standard content server, and a console
// that redraws a detail drawer sends conditional requests without thinking
// about it.
func TestSnapshotRouteServesTheJpeg(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")
	frame := jpegOf(4096)
	id := postOne(t, f, signalWith(cam.ID, "snap-route", models.SignalActionOpen, frame))
	viewer := viewerTokenFor(t, f)

	status, header, raw := getRaw(t, f, "/api/v1/events/"+id+"/snapshot", viewer, nil)
	if status != http.StatusOK {
		t.Fatalf("snapshot GET: got %d %q, want 200", status, raw)
	}
	if got := header.Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type is %q, want image/jpeg", got)
	}
	if got := header.Get("Cache-Control"); !strings.HasPrefix(got, "private") {
		t.Fatalf("Cache-Control is %q, want a private cache: the frame shows someone's property", got)
	}
	if raw != string(frame) {
		t.Fatalf("the body is %d bytes, want the %d on disk", len(raw), len(frame))
	}

	status, _, partial := getRaw(t, f, "/api/v1/events/"+id+"/snapshot", viewer,
		map[string]string{"Range": "bytes=0-3"})
	if status != http.StatusPartialContent || partial != string(frame[:4]) {
		t.Fatalf("a Range request answered %d %q, want 206 and the first 4 bytes", status, partial)
	}
}

// TestSnapshotRouteRequiresASession: the bearer-only rule applies to media as
// well as JSON. A snapshot URL that works anonymously is a snapshot URL anyone
// with the link can read.
func TestSnapshotRouteRequiresASession(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")
	id := postOne(t, f, signalWith(cam.ID, "snap-auth", models.SignalActionOpen, jpegOf(512)))

	status, _, _ := getRaw(t, f, "/api/v1/events/"+id+"/snapshot", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("an anonymous snapshot GET: got %d, want 401", status)
	}
}

// TestSnapshotRouteNamesTheAbsence is the empty-snapshot case drawn as a state
// rather than an outage: the event stands, its row is intact, and the picture
// route says `snapshot not available`.
func TestSnapshotRouteNamesTheAbsence(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")

	// An open with no picture at all: the ordinary case for a camera whose
	// worker could not encode a frame.
	id := postOne(t, f, signalWith(cam.ID, "snap-absent", models.SignalActionOpen, nil))

	status, _, raw := getRaw(t, f, "/api/v1/events/"+id+"/snapshot", viewerTokenFor(t, f), nil)
	if status != http.StatusNotFound {
		t.Fatalf("a snapshot-less event: got %d %s, want 404", status, raw)
	}
	if !strings.Contains(raw, "snapshot not available") {
		t.Fatalf("the 404 does not name the absence: %s", raw)
	}

	row := eventRow(t, f, id)
	if row.SignalCount == 0 || row.CameraID != cam.ID {
		t.Fatalf("the event is still an event without its picture: %+v", row)
	}
}

// TestSnapshotRouteForAnUnknownEventIs404 keeps the two 404s distinguishable:
// "there is no such event" and "that event has no picture" are different
// sentences a console draws in different places.
func TestSnapshotRouteForAnUnknownEventIs404(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)

	status, _, raw := getRaw(t, f, "/api/v1/events/00000000-0000-0000-0000-000000000000/snapshot",
		viewerTokenFor(t, f), nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown event: got %d %s, want 404", status, raw)
	}
	if !strings.Contains(raw, "event not found") {
		t.Fatalf("the answer is %q, want it to say the event is not there", raw)
	}
}

// TestSnapshotRouteRefusesAPathOutsideTheSnapshotDir is the containment guard.
// The row's `local_snapshot_path` is a name on this server's disk; a row
// restored from another installation (or written by hand) must not turn the
// snapshot route into a general file reader.
func TestSnapshotRouteRefusesAPathOutsideTheSnapshotDir(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")
	id := postOne(t, f, signalWith(cam.ID, "snap-escape", models.SignalActionOpen, nil))

	outside := filepath.Join(t.TempDir(), "hosts.jpg")
	secret := "127.0.0.1 localhost\n"
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatalf("writing the file outside SNAPSHOTS_DIR: %v", err)
	}
	if err := f.Store.Events.SetLocalSnapshot(id, outside); err != nil {
		t.Fatalf("pointing the row at it: %v", err)
	}

	status, _, raw := getRaw(t, f, "/api/v1/events/"+id+"/snapshot", viewerTokenFor(t, f), nil)
	if status != http.StatusNotFound {
		t.Fatalf("a snapshot path outside SNAPSHOTS_DIR: got %d %q, want 404", status, raw)
	}
	if strings.Contains(raw, "localhost") {
		t.Fatalf("the route served a file from outside the snapshot dir: %q", raw)
	}
}

// TestSnapshotRouteServesAfterTheFileIsGoneWithANamedFourOhFour: the sweeper
// (S7-09) deletes local media and keeps the row, and the plan's rule for the
// clip — a named 404 for events whose media is gone — applies identically to the
// picture. A 500 here would tell the console the server is broken when what
// happened is that retention did its job.
func TestSnapshotRouteServesAfterTheFileIsGoneWithANamedFourOhFour(t *testing.T) {
	f := newDetectorFixture(t, testWorkerToken)
	cam := f.addCamera(t, "porch", true, true, models.RecordModeContinuous, "")
	id := postOne(t, f, signalWith(cam.ID, "snap-swept", models.SignalActionOpen, jpegOf(1024)))

	path := filepath.Join(f.SnapshotsDir, id+".jpg")
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the snapshot file the way the sweeper would: %v", err)
	}

	status, _, raw := getRaw(t, f, "/api/v1/events/"+id+"/snapshot", viewerTokenFor(t, f), nil)
	if status != http.StatusNotFound {
		t.Fatalf("a swept snapshot: got %d %q, want a named 404", status, raw)
	}
	if !strings.Contains(raw, "snapshot not available") {
		t.Fatalf("the answer is %q, want the absence named", raw)
	}
	if row := eventRow(t, f, id); row.ID != id {
		t.Fatalf("the event outlived its media and should still be listed: %v", row.ID)
	}
}
