package api

import (
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvr/server/models"
	"nvr/server/store"
)

// Card S4-05 — the timeline manifest. The scrubber's whole input is this one
// route, and `scripts/verify.sh` gate 6 reads it to decide whether the recorder
// is recording at all, so the shape is an acceptance gate rather than a
// convenience: rows in start order, a window that means overlap, an empty window
// that answers [], and a limit that says when it cut the window.

// seedCamera is a camera row created the way the console creates one.
func seedCamera(t *testing.T, fx *cameraFixture, slug string) string {
	t.Helper()
	return createdCameraID(t, fx, cameraBody(slug, "rtsp://cam:554/"+slug), fx.OperatorTok)
}

// seedSegment writes one manifest row the way the recorder does, and returns it.
//
// The times are passed in because the assertions are about the window: a test
// that hid the clock behind "now" could not say which side of an edge a row fell
// on.
func seedSegment(t *testing.T, fx *cameraFixture, cameraID, slug string, generation int, start time.Time, seconds float64, kind string, uploaded bool) *models.Segment {
	t.Helper()
	start = start.UTC()
	seg := &models.Segment{
		CameraID:   cameraID,
		Generation: generation,
		Path:       filepath.Join(fx.SegmentsDir, fmt.Sprintf("%s-%d-%d.mp4", slug, generation, start.UnixNano())),
		StartTS:    start,
		EndTS:      start.Add(time.Duration(seconds * float64(time.Second))),
		Seconds:    seconds,
		Bytes:      4096,
		Kind:       kind,
	}
	if uploaded {
		at := start.Add(time.Minute)
		seg.UploadedAt = &at
		seg.RemoteKey = "nvr/" + slug + "/x/" + filepath.Base(seg.Path)
	}
	if err := fx.Store.Segments.Create(seg); err != nil {
		t.Fatalf("seeding the segment row: %v", err)
	}
	return seg
}

// listSegments is one manifest request, with the query built by url.Values so the
// RFC3339 timestamps travel escaped.
func listSegments(t *testing.T, fx *cameraFixture, bearer string, q url.Values) (int, map[string]any, string) {
	t.Helper()
	query := ""
	if q != nil {
		query = q.Encode()
	}
	return do(t, fx.Router, request{method: http.MethodGet, path: "/api/v1/segments", bearer: bearer, query: query})
}

// rowTimes reads a row's window back as times, for the ordering assertions.
func rowTimes(t *testing.T, row map[string]any) (time.Time, time.Time) {
	t.Helper()
	start, err := time.Parse(time.RFC3339, row["start_ts"].(string))
	if err != nil {
		t.Fatalf("start_ts %v is not RFC3339: %v", row["start_ts"], err)
	}
	end, err := time.Parse(time.RFC3339, row["end_ts"].(string))
	if err != nil {
		t.Fatalf("end_ts %v is not RFC3339: %v", row["end_ts"], err)
	}
	return start.UTC(), end.UTC()
}

// manifestRows decodes the collection. The response is an object, not a bare
// array — every collection in this API is `{"<thing>": [...]}` — and a suite
// that accepted either shape would not notice the convention go.
func manifestRows(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, ok := out["segments"].([]any)
	if !ok {
		t.Fatalf(`the manifest is {"segments": [...]}, got %v`, out["segments"])
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("a manifest row is an object, got %T", r)
		}
		rows = append(rows, row)
	}
	return rows
}

// TestSegmentsManifestAnswersTheWindowInStartOrder is the scrubber's input: the
// rows overlapping the window, oldest first, carrying the fields the strip and
// the detail drawer read (`start_ts`, `end_ts`, `bytes`, `generation`, and the
// kind and slug that label them).
func TestSegmentsManifestAnswersTheWindowInStartOrder(t *testing.T) {
	fx := newCameraFixture(t)
	id := seedCamera(t, fx, "porch")
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)

	for i := 0; i < 5; i++ {
		// The one at base+30s is the first row the window answers, and it is the
		// one that has been pushed to the storage tier; the rest are still only on
		// this box.
		seedSegment(t, fx, id, "porch", 1, base.Add(time.Duration(i*30)*time.Second), 30, models.SegmentKindContinuous, i == 1)
	}

	status, out, raw := listSegments(t, fx, fx.ViewerTok, url.Values{
		"camera_id": {id},
		"from":      {base.Add(45 * time.Second).Format(time.RFC3339)},
		"to":        {base.Add(105 * time.Second).Format(time.RFC3339)},
	})
	if status != http.StatusOK {
		t.Fatalf("manifest: got %d %s, want 200", status, raw)
	}

	rows := manifestRows(t, out)
	if len(rows) != 3 {
		t.Fatalf("the window holds 3 of the 5 segments, got %d: %s", len(rows), raw)
	}
	if got, want := out["count"], 3.0; got != want {
		t.Fatalf("count is %v, want 3", got)
	}

	// Oldest first: the order a timeline draws, not the order the rows arrived.
	var previous time.Time
	for i, row := range rows {
		start, end := rowTimes(t, row)
		if !previous.IsZero() && start.Before(previous) {
			t.Fatalf("row %d starts at %s, before row %d's %s: the manifest is not in start order", i, start, i-1, previous)
		}
		previous = start
		if !end.After(start) {
			t.Fatalf("row %d's window is %s..%s, which covers nothing", i, start, end)
		}
		if want := base.Add(time.Duration((i+1)*30) * time.Second); !start.Equal(want) {
			t.Fatalf("row %d starts at %s, want %s (the window answered the wrong segments)", i, start, want)
		}
	}

	first := rows[0]
	if first["camera_id"] != id {
		t.Fatalf("row names camera %v, want %s", first["camera_id"], id)
	}
	if first["slug"] != "porch" {
		t.Fatalf("row names slug %v, want porch (the wall labels a strip with the camera's name)", first["slug"])
	}
	if first["generation"] != 1.0 {
		t.Fatalf("row names generation %v, want 1", first["generation"])
	}
	if first["bytes"] != 4096.0 {
		t.Fatalf("row names bytes %v, want 4096", first["bytes"])
	}
	if first["kind"] != models.SegmentKindContinuous {
		t.Fatalf("row names kind %v, want continuous", first["kind"])
	}
	if _, ok := first["id"].(string); !ok {
		t.Fatalf("row carries no id for playback to point at: %v", first)
	}
	// The uploaded flag is the operator's answer to "is this only on this box".
	if uploaded, _ := rows[0]["uploaded"].(bool); !uploaded {
		t.Fatalf("the pushed segment answers uploaded false: %v", rows[0])
	}
	if uploaded, _ := rows[1]["uploaded"].(bool); uploaded {
		t.Fatalf("the un-pushed segment answers uploaded true: %v", rows[1])
	}
	// A local path is a name on this server's disk; playback is server-proxied,
	// so the manifest answers an id and not a filesystem.
	if _, ok := first["path"]; ok {
		t.Fatalf("the manifest leaks the local file path: %v", first)
	}
}

// TestSegmentsWindowIsOverlapNotContainment is the clip builder's rule applied to
// the scrubber: a segment that crosses an edge is in, a segment that merely
// touches it is out. A manifest that required containment would hide the segment
// a clip starts inside of, and the operator would see a hole where the footage is.
func TestSegmentsWindowIsOverlapNotContainment(t *testing.T) {
	fx := newCameraFixture(t)
	id := seedCamera(t, fx, "drive")
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)

	// from = base+30s, to = base+120s.
	seedSegment(t, fx, id, "drive", 1, base, 30, models.SegmentKindContinuous, false)                      // ends exactly at from
	seedSegment(t, fx, id, "drive", 1, base.Add(20*time.Second), 30, models.SegmentKindContinuous, false)  // crosses from
	seedSegment(t, fx, id, "drive", 1, base.Add(100*time.Second), 30, models.SegmentKindContinuous, false) // crosses to
	seedSegment(t, fx, id, "drive", 1, base.Add(120*time.Second), 30, models.SegmentKindContinuous, false) // starts exactly at to
	seedSegment(t, fx, id, "drive", 1, base.Add(200*time.Second), 30, models.SegmentKindContinuous, false) // entirely after

	status, out, raw := listSegments(t, fx, fx.ViewerTok, url.Values{
		"camera_id": {id},
		"from":      {base.Add(30 * time.Second).Format(time.RFC3339)},
		"to":        {base.Add(120 * time.Second).Format(time.RFC3339)},
	})
	if status != http.StatusOK {
		t.Fatalf("manifest: got %d %s, want 200", status, raw)
	}

	rows := manifestRows(t, out)
	if len(rows) != 2 {
		t.Fatalf("the window holds the two crossing segments, got %d: %s", len(rows), raw)
	}
	for i, want := range []time.Duration{20 * time.Second, 100 * time.Second} {
		start, _ := rowTimes(t, rows[i])
		if !start.Equal(base.Add(want)) {
			t.Fatalf("row %d starts at %s, want %s", i, start, base.Add(want))
		}
	}
}

// TestSegmentsEmptyWindowAnswersEmptyListNotFourOhFour: "nothing was recorded
// then" is an answer about the property. A 404 makes a scrubber show an error
// where the timeline should show a gap.
func TestSegmentsEmptyWindowAnswersEmptyListNotFourOhFour(t *testing.T) {
	fx := newCameraFixture(t)
	id := seedCamera(t, fx, "quiet")
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	seedSegment(t, fx, id, "quiet", 1, base, 30, models.SegmentKindContinuous, false)

	status, out, raw := listSegments(t, fx, fx.ViewerTok, url.Values{
		"camera_id": {id},
		"from":      {base.Add(time.Hour).Format(time.RFC3339)},
		"to":        {base.Add(90 * time.Minute).Format(time.RFC3339)},
	})
	if status != http.StatusOK {
		t.Fatalf("an empty window: got %d %s, want 200", status, raw)
	}
	if got, want := out["count"], 0.0; got != want {
		t.Fatalf("count is %v, want 0", got)
	}
	if rows := manifestRows(t, out); len(rows) != 0 {
		t.Fatalf("an empty window answered %d rows", len(rows))
	}
	// `[]`, not `null`: a console drawing the strip iterates the list, and a null
	// is a crash it has to defend against.
	if !strings.Contains(raw, `"segments":[]`) {
		t.Fatalf(`an empty window answers "segments":[], got %s`, raw)
	}
}

// TestSegmentsUnknownCameraIsFourOhFour draws the line the empty window does not:
// a `camera_id` naming no camera is a request about a thing that does not exist.
// The message is the camera route's own, so one wrong id reads the same in both
// places.
func TestSegmentsUnknownCameraIsFourOhFour(t *testing.T) {
	fx := newCameraFixture(t)

	status, out, raw := listSegments(t, fx, fx.ViewerTok, url.Values{"camera_id": {"00000000-0000-4000-8000-000000000000"}})
	if status != http.StatusNotFound {
		t.Fatalf("an unknown camera id: got %d %s, want 404", status, raw)
	}
	if msg, _ := out["error"].(string); msg != "no camera with that id" {
		t.Fatalf(`the refusal says %q, want the camera route's own words (one wrong id reads the same in both places)`, msg)
	}
}

// TestSegmentsLimitIsEchoedAndSaysWhenTheWindowWasCut: the ceiling is
// store.SegmentListMaxLimit, the response names the limit it used, and
// `truncated` says whether the window held more than it answered. A manifest
// that was cut and looks complete is a timeline with a silent edge.
func TestSegmentsLimitIsEchoedAndSaysWhenTheWindowWasCut(t *testing.T) {
	fx := newCameraFixture(t)
	id := seedCamera(t, fx, "gate")
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	for i := 0; i < 5; i++ {
		seedSegment(t, fx, id, "gate", 1, base.Add(time.Duration(i*30)*time.Second), 30, models.SegmentKindContinuous, false)
	}

	window := url.Values{
		"camera_id": {id},
		"from":      {base.Add(-time.Minute).Format(time.RFC3339)},
		"to":        {base.Add(5 * time.Minute).Format(time.RFC3339)},
	}

	q := url.Values{}
	for k, v := range window {
		q[k] = v
	}
	q.Set("limit", "2")
	status, out, raw := listSegments(t, fx, fx.ViewerTok, q)
	if status != http.StatusOK {
		t.Fatalf("limit=2: got %d %s, want 200", status, raw)
	}
	if got, want := out["count"], 2.0; got != want {
		t.Fatalf("limit=2 answered %v rows", got)
	}
	if out["limit"] != 2.0 {
		t.Fatalf("the response does not name the limit it used: %v", out["limit"])
	}
	if truncated, _ := out["truncated"].(bool); !truncated {
		t.Fatalf("five rows answered as two, and the response does not say the window was cut: %v", out)
	}

	// A limit above the ceiling is clamped, and the clamp is what the response
	// says — a console that asked for a million and got 2000 rows should be able
	// to see the number it actually got a say in.
	q = url.Values{}
	for k, v := range window {
		q[k] = v
	}
	q.Set("limit", "1000000")
	status, out, raw = listSegments(t, fx, fx.ViewerTok, q)
	if status != http.StatusOK {
		t.Fatalf("limit=1000000: got %d %s, want 200", status, raw)
	}
	if out["limit"] != float64(store.SegmentListMaxLimit) {
		t.Fatalf("the limit is clamped to the ceiling in the response, got %v", out["limit"])
	}
	if got, want := out["count"], 5.0; got != want {
		t.Fatalf("the whole window answered %v rows, want 5", got)
	}
	if truncated, _ := out["truncated"].(bool); truncated {
		t.Fatalf("five rows under the ceiling answered as truncated: %v", out)
	}
}

// TestSegmentsRefusesABadWindowAndABadLimit: the refusals name the operand, so a
// console with its query built backwards says which half is backwards.
func TestSegmentsRefusesABadWindowAndABadLimit(t *testing.T) {
	fx := newCameraFixture(t)
	id := seedCamera(t, fx, "porch")
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)

	cases := []struct {
		name  string
		query url.Values
		want  int
	}{
		{
			name:  "from after to",
			query: url.Values{"camera_id": {id}, "from": {base.Format(time.RFC3339)}, "to": {base.Add(-time.Hour).Format(time.RFC3339)}},
			want:  http.StatusBadRequest,
		},
		{
			name:  "from equal to to",
			query: url.Values{"camera_id": {id}, "from": {base.Format(time.RFC3339)}, "to": {base.Format(time.RFC3339)}},
			want:  http.StatusBadRequest,
		},
		{
			name:  "from is not a timestamp",
			query: url.Values{"camera_id": {id}, "from": {"yesterday"}},
			want:  http.StatusBadRequest,
		},
		{
			name:  "to is not a timestamp",
			query: url.Values{"camera_id": {id}, "to": {"noon"}},
			want:  http.StatusBadRequest,
		},
		{
			name:  "limit is zero",
			query: url.Values{"camera_id": {id}, "limit": {"0"}},
			want:  http.StatusBadRequest,
		},
		{
			name:  "limit is not a number",
			query: url.Values{"camera_id": {id}, "limit": {"many"}},
			want:  http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, raw := listSegments(t, fx, fx.ViewerTok, tc.query)
			if status != tc.want {
				t.Fatalf("%s: got %d %s, want %d", tc.name, status, raw, tc.want)
			}
		})
	}
}

// TestSegmentsDefaultWindowIsTheServersHour is `scripts/verify.sh` gate 6's
// request: a camera id and nothing else. The server owns the window it answered
// and says so, so the caller never has to guess how far back "the manifest" goes.
func TestSegmentsDefaultWindowIsTheServersHour(t *testing.T) {
	fx := newCameraFixture(t)
	id := seedCamera(t, fx, "porch")
	pinned := time.Now().UTC().Truncate(time.Second)
	fx.Handlers.SetClock(func() time.Time { return pinned })

	seedSegment(t, fx, id, "porch", 1, pinned.Add(-10*time.Minute), 30, models.SegmentKindContinuous, false)
	seedSegment(t, fx, id, "porch", 1, pinned.Add(-2*time.Hour), 30, models.SegmentKindContinuous, false)

	status, out, raw := listSegments(t, fx, fx.ViewerTok, url.Values{"camera_id": {id}})
	if status != http.StatusOK {
		t.Fatalf("manifest with no window: got %d %s, want 200", status, raw)
	}
	if got, want := out["count"], 1.0; got != want {
		t.Fatalf("the default window answered %v rows, want 1 (the hour before now): %v", got, out)
	}
	from, err := time.Parse(time.RFC3339, out["from"].(string))
	if err != nil {
		t.Fatalf("from is not RFC3339: %v", out["from"])
	}
	to, err := time.Parse(time.RFC3339, out["to"].(string))
	if err != nil {
		t.Fatalf("to is not RFC3339: %v", out["to"])
	}
	if !to.Equal(pinned) {
		t.Fatalf("the window ends at %s, want the server's now (%s)", to, pinned)
	}
	if got := to.Sub(from); got != DefaultManifestWindow {
		t.Fatalf("the default window is %s, want %s", got, DefaultManifestWindow)
	}
}

// TestSegmentsCountGrowsWhileRecording is the gate's second half: the count
// strictly increases while the recorder is writing. A manifest that answered the
// same rows forever — because the recorder wrote files but no rows — would pass
// every other test in this file.
func TestSegmentsCountGrowsWhileRecording(t *testing.T) {
	fx := newCameraFixture(t)
	id := seedCamera(t, fx, "porch")
	now := time.Now().UTC()

	for i := 0; i < 3; i++ {
		seedSegment(t, fx, id, "porch", 1, now.Add(-time.Duration(3-i)*time.Minute), 30, models.SegmentKindContinuous, false)
	}

	status, first, raw := listSegments(t, fx, fx.ViewerTok, url.Values{"camera_id": {id}})
	if status != http.StatusOK {
		t.Fatalf("manifest: got %d %s, want 200", status, raw)
	}
	before, _ := first["count"].(float64)
	if before < 3 {
		t.Fatalf("three segments recorded, the manifest answers %v: %v", before, first)
	}

	seedSegment(t, fx, id, "porch", 1, now.Add(-10*time.Second), 30, models.SegmentKindContinuous, false)

	_, second, raw := listSegments(t, fx, fx.ViewerTok, url.Values{"camera_id": {id}})
	after, _ := second["count"].(float64)
	if after <= before {
		t.Fatalf("the manifest did not grow with the recording: %v then %v (%s)", before, after, raw)
	}
}

// TestSegmentsRoleFloorIsViewer: reading the timeline is the lowest thing a
// signed-in account can do, and an anonymous request is refused like every other
// `/api/v1` route.
func TestSegmentsRoleFloorIsViewer(t *testing.T) {
	fx := newCameraFixture(t)
	id := seedCamera(t, fx, "porch")
	now := time.Now().UTC()
	seedSegment(t, fx, id, "porch", 1, now.Add(-time.Minute), 30, models.SegmentKindContinuous, false)

	for name, bearer := range map[string]string{
		"viewer":   fx.ViewerTok,
		"operator": fx.OperatorTok,
		"admin":    fx.AdminTok,
	} {
		status, _, raw := listSegments(t, fx, bearer, url.Values{"camera_id": {id}})
		if status != http.StatusOK {
			t.Fatalf("%s reading the manifest: got %d %s, want 200", name, status, raw)
		}
	}

	status, _, raw := listSegments(t, fx, "", url.Values{"camera_id": {id}})
	if status != http.StatusUnauthorized {
		t.Fatalf("an anonymous manifest read: got %d %s, want 401", status, raw)
	}
}
