// The event API suite (card S6-04): the list and its filters, the detail, the
// delete, and the clip route's answer when there is no clip.
//
// What each test owns:
//
//   - **The filters are the console's screens.** camera_id / class / zone_id /
//     severity / from / to / q are the seven dials on the event wall, and each
//     one is asserted against rows the other dials would have matched — a filter
//     that returns everything is a filter that is not filtering.
//   - **`q` reaches a zone by its name.** The row stores zone *ids*; an operator
//     types "porch". The camera row holds the pair, so the resolution is asserted
//     here, including the case that makes it a bug rather than a feature: a q
//     that matches a zone on camera A must not drag camera B's events into a
//     request that named camera B.
//   - **The limit is a ceiling, and the page says whether it cut.** 50 by
//     default, 200 at most, and `truncated` is true at the ceiling as well as
//     below it — a timeline that looks complete at its edge is a lie.
//   - **The delete is one role, one ledger row, and two queue jobs.** Admin
//     only; the row is written; and both media keys go to the queue when they
//     reached a tier, because an event deleted from a database that still holds
//     its clip on S3 is an event whose footage is still being paid for.
//   - **Gone media is a named 404, not a missing event.** The event stands and
//     the clip route says what it does not have, in words the console draws.
//
// Events are filed through the store here rather than through the internal
// signals API: S5-10's suite already proves the signal path writes the row, and
// this suite is about the row read back out through the console's door.

package api

import (
	"context"
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
	"nvr/server/store"
)

// eventsFixture is the event surface with a store, three roles' tokens, and the
// two media directories the routes read.
type eventsFixture struct {
	Store        *store.Store
	Handlers     *Handlers
	Engine       http.Handler
	ClipsDir     string
	SnapshotsDir string
	Viewer       string
	Operator     string
	Admin        string
}

func newEventsFixture(t *testing.T) *eventsFixture {
	t.Helper()
	st := newStore(t)
	h := newHandlers(t, st)

	clips := t.TempDir()
	snapshots := t.TempDir()
	h.Cfg.ClipsDir = clips
	h.Cfg.SnapshotsDir = snapshots

	f := &eventsFixture{Store: st, Handlers: h, ClipsDir: clips, SnapshotsDir: snapshots}
	f.Engine = routerFor(t, h)
	f.Viewer = eventsToken(t, st, h, "wall-viewer", models.RoleViewer)
	f.Operator = eventsToken(t, st, h, "wall-operator", models.RoleOperator)
	f.Admin = eventsToken(t, st, h, "wall-admin", models.RoleAdmin)
	return f
}

// eventsToken is one role's bearer token. The names are fixed because a suite
// that needs two viewers says so with a second call, not a numbered name.
func eventsToken(t *testing.T, st *store.Store, h *Handlers, username, role string) string {
	t.Helper()
	u := newUser(t, st, username, role)
	return accessToken(t, st, h.JWT, u)
}

// addZoneCamera puts a camera row in with zones whose ids and names differ from
// every other camera's, so a zone match can be attributed to a camera.
func (f *eventsFixture) addZoneCamera(t *testing.T, slug, zoneID, zoneName string) *models.Camera {
	t.Helper()
	zones := fmt.Sprintf(`[{"id":%q,"name":%q,"polygon":[[0,0],[1,0],[1,1],[0,1]],`+
		`"classes":["person"],"rule":"loiter","dwell_seconds":30,"severity":3,`+
		`"min_confidence":0.4,"min_area_ratio":0.004,"enabled":true}]`, zoneID, zoneName)
	cam := &models.Camera{
		Slug:             slug,
		Name:             strings.ToTitle(slug),
		SourceURI:        "rtsp://cam/" + slug,
		RecordMode:       models.RecordModeContinuous,
		DetectionEnabled: true,
		DetectionFPS:     5,
		Sensitivity:      0.45,
		MinAreaRatio:     0.004,
		ZonesJSON:        zones,
		Active:           true,
	}
	if err := f.Store.Cameras.Create(cam); err != nil {
		t.Fatalf("create camera %s: %v", slug, err)
	}
	return cam
}

// eventSpec is one event as a test wants it: when it ran, how hard, and what the
// worker said was in the frame.
type eventSpec struct {
	Key      string
	Started  time.Time
	Seconds  float64
	Severity int
	Classes  []string
	ZoneID   string
	Rule     string
	Objects  []models.EventObject
}

// fileEvent writes the row through the store's upsert — an open and, when the
// spec has a length, a close. The timestamps are in the past, which the store
// accepts (only a zero or a future ts is clamped to now), so a window filter has
// something to fall inside.
func (f *eventsFixture) fileEvent(t *testing.T, cam *models.Camera, spec eventSpec) *models.Event {
	t.Helper()
	ctx := context.Background()
	open := store.EventInput{
		EventKey: spec.Key,
		CameraID: cam.ID,
		Action:   models.SignalActionOpen,
		Rule:     spec.Rule,
		ZoneID:   spec.ZoneID,
		Severity: spec.Severity,
		Classes:  spec.Classes,
		Objects:  spec.Objects,
		TS:       spec.Started,
	}
	event, created, err := f.Store.Events.UpsertByEventKey(ctx, open)
	if err != nil {
		t.Fatalf("filing %s: %v", spec.Key, err)
	}
	if !created {
		t.Fatalf("%s created a row", spec.Key)
	}
	if spec.Seconds <= 0 {
		return event
	}

	closeIn := open
	closeIn.Action = models.SignalActionClose
	closeIn.Objects = nil
	closeIn.TS = spec.Started.Add(time.Duration(spec.Seconds * float64(time.Second)))
	event, _, err = f.Store.Events.UpsertByEventKey(ctx, closeIn)
	if err != nil {
		t.Fatalf("closing %s: %v", spec.Key, err)
	}
	return event
}

// eventsGet runs a request against the router and answers status, the decoded
// body, and the raw bytes.
func (f *eventsFixture) get(t *testing.T, path, query, bearer string) (int, map[string]any, string) {
	t.Helper()
	code, body, raw := do(t, f.Engine, request{method: http.MethodGet, path: path, query: query, bearer: bearer})
	return code, body, raw
}

// getWithHeaders is `get` for the assertions that live in headers — the media
// type, the cache promise, and what a Range request turned into.
func (f *eventsFixture) getWithHeaders(t *testing.T, path, bearer string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.Engine.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

// del is the delete route.
func (f *eventsFixture) del(t *testing.T, path, bearer string) (int, map[string]any, string) {
	t.Helper()
	return do(t, f.Engine, request{method: http.MethodDelete, path: path, bearer: bearer})
}

// eventList pulls the `events` array out of a list answer, and fails the test if
// the answer is not shaped like a list.
func eventList(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["events"]
	if !ok {
		t.Fatalf("the answer carries an events array: %v", body)
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("events is an array, got %T: %v", raw, body)
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("an event is an object, got %T", item)
		}
		out = append(out, row)
	}
	return out
}

// eventIDsInOrder is what an ordering assertion reads.
func eventIDsInOrder(rows []map[string]any) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(string)
		out = append(out, id)
	}
	return out
}

// stringList decodes one of the view's array fields.
func stringList(t *testing.T, row map[string]any, field string) []string {
	t.Helper()
	raw, ok := row[field]
	if !ok {
		t.Fatalf("%s is present: %v", field, row)
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s is an array, got %T (%v)", field, raw, row)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%s holds a string, got %T", field, item)
		}
		out = append(out, s)
	}
	return out
}

func hasString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// TestEventsListAnswersNewestFirstAndFiltersEveryColumn is the wall: seven dials,
// each asserted against rows the others would have matched.
func TestEventsListAnswersNewestFirstAndFiltersEveryColumn(t *testing.T) {
	f := newEventsFixture(t)
	porch := f.addZoneCamera(t, "porch", "z-porch", "Porch")
	drive := f.addZoneCamera(t, "driveway", "z-drive", "Driveway")

	base := time.Now().UTC().Add(-time.Hour)
	older := f.fileEvent(t, porch, eventSpec{
		Key: "wall-old", Started: base, Seconds: 10, Severity: models.SeverityHigh,
		Classes: []string{"person"}, ZoneID: "zone-01", Rule: models.ZoneRuleLoiter,
	})
	newer := f.fileEvent(t, drive, eventSpec{
		// Ten minutes long, so a window can fall strictly inside it — which is
		// what the overlap assertion at the end of this test needs.
		Key: "wall-new", Started: base.Add(20 * time.Minute), Seconds: 600, Severity: models.SeverityLow,
		Classes: []string{"car"}, ZoneID: "zone-02", Rule: models.ZoneRuleIntrusion,
	})

	code, body, raw := f.get(t, "/api/v1/events", "", f.Viewer)
	if code != http.StatusOK {
		t.Fatalf("a viewer reads the wall: got %d %s, want 200", code, raw)
	}
	rows := eventList(t, body)
	if len(rows) != 2 {
		t.Fatalf("both events: got %d (%v)", len(rows), eventIDsInOrder(rows))
	}
	// Newest first: the console's wall is "what happened last".
	if got, want := eventIDsInOrder(rows), []string{newer.ID, older.ID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order is newest first: got %v, want %v", got, want)
	}
	if body["count"] != float64(2) || body["limit"] != float64(store.EventListDefaultLimit) ||
		body["offset"] != float64(0) || body["truncated"] != false {
		t.Fatalf("the page says what it answered: got limit=%v offset=%v count=%v truncated=%v",
			body["limit"], body["offset"], body["count"], body["truncated"])
	}

	cases := []struct {
		name  string
		query string
		want  string // the event key expected, "" for none
	}{
		{"camera_id", "camera_id=" + drive.ID, "wall-new"},
		{"class", "class=person", "wall-old"},
		{"zone_id", "zone_id=zone-02", "wall-new"},
		{"severity", "severity=2", "wall-old"},
		{"window covering the newer event", "from=" + base.Add(19*time.Minute).Format(time.RFC3339), "wall-new"},
		{"window covering the older event", "to=" + base.Add(time.Minute).Format(time.RFC3339), "wall-old"},
		{"window between the two", "from=" + base.Add(5*time.Minute).Format(time.RFC3339) +
			"&to=" + base.Add(10*time.Minute).Format(time.RFC3339), ""},
		{"class that matches nothing", "class=bear", ""},
	}
	for _, tc := range cases {
		code, body, raw := f.get(t, "/api/v1/events", tc.query, f.Viewer)
		if code != http.StatusOK {
			t.Fatalf("%s: got %d %s, want 200", tc.name, code, raw)
		}
		rows := eventList(t, body)
		if tc.want == "" {
			if len(rows) != 0 {
				t.Fatalf("%s matched nothing: got %v", tc.name, eventIDsInOrder(rows))
			}
			continue
		}
		if len(rows) != 1 {
			t.Fatalf("%s matched exactly one event: got %d (%v)", tc.name, len(rows), eventIDsInOrder(rows))
		}
		if rows[0]["event_key"] != tc.want {
			t.Fatalf("%s matched %v, want %s", tc.name, rows[0]["event_key"], tc.want)
		}
	}

	// The window means overlap, not containment: an event that started before
	// `from` and ended after it is inside the window, because part of it happened
	// there. That is the clip builder's rule and the scrubber's rule.
	code, body, _ = f.get(t, "/api/v1/events",
		"from="+base.Add(25*time.Minute).Format(time.RFC3339)+"&to="+base.Add(35*time.Minute).Format(time.RFC3339),
		f.Viewer)
	if code != http.StatusOK || len(eventList(t, body)) != 1 ||
		eventList(t, body)[0]["event_key"] != "wall-new" {
		t.Fatalf("the window that falls inside the longer event answers it, got %v", body)
	}
}

// TestEventsListQMatchesClassRuleAndZoneName is the search box. The row stores
// class names, rule names and zone **ids**; an operator searches for the zone's
// name, and the camera row is what turns "porch" into the ids that carry it. The
// zone ids here are deliberately nothing like their names, so a match on a name
// cannot be a substring match on the id wearing a disguise.
func TestEventsListQMatchesClassRuleAndZoneName(t *testing.T) {
	f := newEventsFixture(t)
	porch := f.addZoneCamera(t, "porch", "zone-01", "Porch")
	drive := f.addZoneCamera(t, "driveway", "zone-02", "Driveway")
	base := time.Now().UTC().Add(-time.Hour)

	f.fileEvent(t, porch, eventSpec{Key: "q-person", Started: base, Seconds: 10, Severity: 2,
		Classes: []string{"person"}, ZoneID: "zone-01", Rule: models.ZoneRuleLoiter})
	f.fileEvent(t, drive, eventSpec{Key: "q-car", Started: base.Add(time.Minute), Seconds: 10, Severity: 2,
		Classes: []string{"car"}, ZoneID: "zone-02", Rule: models.ZoneRuleIntrusion})

	cases := []struct {
		q    string
		want string
	}{
		{"person", "q-person"},
		{"car", "q-car"},
		{"loiter", "q-person"}, // the rule
		{"intrusion", "q-car"}, // the rule
		{"porch", "q-person"},  // the zone's name, which the row does not hold
		{"PORCH", "q-person"},  // case-insensitive, on both databases
		{"zone-02", "q-car"},   // the zone's id, which is what the row holds
		{"driveway", "q-car"},  // the other camera's zone name
		{"nothing-here", ""},
	}
	for _, tc := range cases {
		code, body, raw := f.get(t, "/api/v1/events", "q="+tc.q, f.Viewer)
		if code != http.StatusOK {
			t.Fatalf("q=%s: got %d %s, want 200", tc.q, code, raw)
		}
		rows := eventList(t, body)
		if tc.want == "" {
			if len(rows) != 0 {
				t.Fatalf("q=%s matched nothing: got %v", tc.q, eventIDsInOrder(rows))
			}
			// The empty wall is `[]`, asserted on the bytes: a `null` is a crash
			// every console has to defend against.
			if !strings.Contains(raw, `"events":[]`) {
				t.Fatalf("an empty wall answers an empty array, got %s", raw)
			}
			if body["q"] != tc.q {
				t.Fatalf("the answer echoes the filter it used: got %v", body["q"])
			}
			continue
		}
		if len(rows) != 1 || rows[0]["event_key"] != tc.want {
			t.Fatalf("q=%s matched exactly %s: got %v", tc.q, tc.want, eventIDsInOrder(rows))
		}
	}
}

// TestEventsListQIsScopedToTheCameraItWasAskedFor guards the parenthesis. The q
// clause is a chain of ORs; the camera filter is an AND. Unparenthesized, SQL
// reads `camera = B AND class LIKE q OR zones LIKE q OR rules LIKE q` as
// `(camera = B AND class LIKE q) OR zones LIKE q OR rules LIKE q`, and camera A's
// zone match answers a request that named camera B — a filter that quietly
// widens itself is worse than one that does nothing.
func TestEventsListQIsScopedToTheCameraItWasAskedFor(t *testing.T) {
	f := newEventsFixture(t)
	porch := f.addZoneCamera(t, "porch", "zone-01", "Porch")
	drive := f.addZoneCamera(t, "driveway", "zone-02", "Driveway")
	base := time.Now().UTC().Add(-time.Hour)

	f.fileEvent(t, porch, eventSpec{Key: "scope-porch", Started: base, Seconds: 10, Severity: 2,
		Classes: []string{"person"}, ZoneID: "zone-01", Rule: models.ZoneRuleLoiter})
	f.fileEvent(t, drive, eventSpec{Key: "scope-drive", Started: base.Add(time.Minute), Seconds: 10, Severity: 2,
		Classes: []string{"car"}, ZoneID: "zone-02", Rule: models.ZoneRuleIntrusion})

	// "porch" matches a zone name on the porch camera and nothing on the
	// driveway camera. Asked for the driveway camera, it must answer nothing.
	code, body, raw := f.get(t, "/api/v1/events", "camera_id="+drive.ID+"&q=porch", f.Viewer)
	if code != http.StatusOK {
		t.Fatalf("got %d %s, want 200", code, raw)
	}
	if rows := eventList(t, body); len(rows) != 0 {
		t.Fatalf("q=porch scoped to the driveway camera matched %v; the OR chain is not parenthesized",
			eventIDsInOrder(rows))
	}

	// And the same q on the camera that owns the zone still matches.
	code, body, _ = f.get(t, "/api/v1/events", "camera_id="+porch.ID+"&q=porch", f.Viewer)
	if code != http.StatusOK || len(eventList(t, body)) != 1 ||
		eventList(t, body)[0]["event_key"] != "scope-porch" {
		t.Fatalf("q=porch on the camera that owns that zone answers it, got %v", body)
	}
}

// TestEventsListLimitDefaultsTo50AndClampsAt200 is the ceiling. A timeline
// request that asks for everything is a timeline request that reads the whole
// table, and the page that got cut says so.
func TestEventsListLimitDefaultsTo50AndClampsAt200(t *testing.T) {
	f := newEventsFixture(t)
	cam := f.addZoneCamera(t, "rowcam", "zone-rows", "Row")
	base := time.Now().UTC().Add(-4 * time.Hour)
	for i := 0; i < 250; i++ {
		f.fileEvent(t, cam, eventSpec{
			Key: fmt.Sprintf("bulk-%03d", i), Started: base.Add(time.Duration(i) * time.Minute),
			Seconds: 5, Severity: 1, Classes: []string{"person"}, ZoneID: "zone-rows",
			Rule: models.ZoneRuleIntrusion,
		})
	}

	code, body, raw := f.get(t, "/api/v1/events", "", f.Viewer)
	if code != http.StatusOK {
		t.Fatalf("got %d %s, want 200", code, raw)
	}
	if len(eventList(t, body)) != 50 || body["limit"] != float64(50) || body["truncated"] != true {
		t.Fatalf("the default page is 50 rows and says there is more: got %d rows, limit=%v truncated=%v",
			len(eventList(t, body)), body["limit"], body["truncated"])
	}

	// A request above the ceiling gets the ceiling, and the answer says the
	// number it used rather than the number that was asked for.
	code, body, raw = f.get(t, "/api/v1/events", "limit=5000", f.Viewer)
	if code != http.StatusOK {
		t.Fatalf("got %d %s, want 200", code, raw)
	}
	if len(eventList(t, body)) != store.EventListMaxLimit || body["limit"] != float64(store.EventListMaxLimit) {
		t.Fatalf("limit=5000 is clamped at %d: got %d rows, limit=%v",
			store.EventListMaxLimit, len(eventList(t, body)), body["limit"])
	}
	if body["truncated"] != true {
		t.Fatalf("a page cut at the ceiling still says truncated: got %v (250 events, 200 answered)", body["truncated"])
	}

	// The offset walks pages, and the pages do not overlap.
	code, first, _ := f.get(t, "/api/v1/events", "limit=1", f.Viewer)
	if code != http.StatusOK || len(eventList(t, first)) != 1 {
		t.Fatalf("limit=1 answers one row, got %v", first)
	}
	code, second, _ := f.get(t, "/api/v1/events", "limit=1&offset=1", f.Viewer)
	if code != http.StatusOK || len(eventList(t, second)) != 1 {
		t.Fatalf("offset=1 answers one row, got %v", second)
	}
	if eventList(t, first)[0]["id"] == eventList(t, second)[0]["id"] {
		t.Fatalf("offset=1 moved past the first row: both answered %v", eventList(t, first)[0]["id"])
	}
	if second["offset"] != float64(1) {
		t.Fatalf("the answer echoes the offset it used: got %v", second["offset"])
	}
}

// TestEventsListRefusesBadFiltersWithAName keeps a typo from reading as a quiet
// property: a filter this server cannot honour is refused in words, not clamped
// into an answer that looks like a result.
func TestEventsListRefusesBadFiltersWithAName(t *testing.T) {
	f := newEventsFixture(t)
	cam := f.addZoneCamera(t, "strict", "zone-strict", "Strict")
	f.fileEvent(t, cam, eventSpec{Key: "strict-1", Started: time.Now().UTC().Add(-time.Hour),
		Seconds: 10, Severity: 2, Classes: []string{"person"}, ZoneID: "zone-strict", Rule: "loiter"})

	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"from is not a timestamp", "from=yesterday", "from must be an RFC3339 timestamp"},
		{"to is not a timestamp", "to=yesterday", "to must be an RFC3339 timestamp"},
		{"from after to", "from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z", "from must name a moment before to"},
		{"severity is not a number", "severity=high", "severity must be 1, 2 or 3"},
		{"severity is out of range", "severity=9", "severity must be 1, 2 or 3"},
		{"limit is not a number", "limit=many", "limit must be a positive number"},
		{"limit is zero", "limit=0", "limit must be a positive number"},
		{"offset is negative", "offset=-1", "offset must be zero or a positive number"},
	}
	for _, tc := range cases {
		code, body, _ := f.get(t, "/api/v1/events", tc.query, f.Viewer)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: got %d %v, want 400", tc.name, code, body)
		}
		if body["error"] != tc.want {
			t.Fatalf("%s names the mistake: got %v, want %q", tc.name, body["error"], tc.want)
		}
	}
}

// TestEventsListUnknownCameraIsFourOhFour separates "no events" from "no
// camera": one is an answer about the property, the other is a request about a
// thing that is not there.
func TestEventsListUnknownCameraIsFourOhFour(t *testing.T) {
	f := newEventsFixture(t)
	code, body, _ := f.get(t, "/api/v1/events", "camera_id=00000000-0000-4000-8000-000000000000", f.Viewer)
	if code != http.StatusNotFound {
		t.Fatalf("got %d %v, want 404", code, body)
	}
	if body["error"] != "no camera with that id" {
		t.Fatalf("the refusal names the missing thing: got %v", body["error"])
	}
}

// TestEventsDetailCarriesTheBoxesAndTheZoneNames is the drawer: the row, the
// boxes the model saw, and the zone by the name the operator gave it.
func TestEventsDetailCarriesTheBoxesAndTheZoneNames(t *testing.T) {
	f := newEventsFixture(t)
	cam := f.addZoneCamera(t, "drawer", "zone-walk", "Front Walk")
	event := f.fileEvent(t, cam, eventSpec{
		Key: "detail-1", Started: time.Now().UTC().Add(-time.Hour), Seconds: 42, Severity: 3,
		Classes: []string{"person", "dog"}, ZoneID: "zone-walk", Rule: models.ZoneRuleLoiter,
		Objects: []models.EventObject{
			{Class: "person", Conf: 0.81, Box: []float64{100, 50, 140, 150}},
			{Class: "dog", Conf: 0.44, Box: []float64{200, 90, 240, 130}},
		},
	})

	code, body, raw := f.get(t, "/api/v1/events/"+event.ID, "", f.Viewer)
	if code != http.StatusOK {
		t.Fatalf("got %d %s, want 200", code, raw)
	}
	if body["id"] != event.ID || body["camera_id"] != cam.ID || body["camera_slug"] != "drawer" {
		t.Fatalf("the detail names the event and its camera: got %v", body)
	}
	if body["open"] != false || body["action"] != models.SignalActionClose {
		t.Fatalf("the detail carries the state machine: got open=%v action=%v", body["open"], body["action"])
	}
	if body["seconds"] != float64(42) {
		t.Fatalf("seconds is the window the event ran for: got %v, want 42", body["seconds"])
	}
	if got := stringList(t, body, "classes"); !hasString(got, "person") || !hasString(got, "dog") {
		t.Fatalf("classes carries the union: got %v", got)
	}
	if got := stringList(t, body, "zones"); !hasString(got, "Front Walk") {
		t.Fatalf("zones answers the zone's **name**: got %v", got)
	}
	if got := stringList(t, body, "zone_ids"); !hasString(got, "zone-walk") {
		t.Fatalf("zone_ids carries the id the `zone_id=` filter takes: got %v", got)
	}
	if got := stringList(t, body, "rules"); !hasString(got, models.ZoneRuleLoiter) {
		t.Fatalf("rules carries the rule that fired: got %v", got)
	}

	boxes, ok := body["objects"].([]any)
	if !ok {
		t.Fatalf("the detail carries the boxes: got %T", body["objects"])
	}
	if len(boxes) != 2 || body["object_count"] != float64(2) {
		t.Fatalf("two boxes: got %d, object_count=%v", len(boxes), body["object_count"])
	}
	first, _ := boxes[0].(map[string]any)
	if first["class"] != "person" {
		t.Fatalf("the boxes are the worker's, in order: got %v", boxes)
	}

	// The media flags are the row's claim: this event has neither, and the two
	// routes say so.
	if body["has_snapshot"] != false || body["has_clip"] != false {
		t.Fatalf("an event with no media says so: got snapshot=%v clip=%v", body["has_snapshot"], body["has_clip"])
	}
}

// TestEventsListCarriesNoBoxesButCountsThem keeps the wall small. Fifty events at
// two hundred boxes each is a payload no wall draws, and a card that shows "3
// boxes" needs the count, not the boxes.
func TestEventsListCarriesNoBoxesButCountsThem(t *testing.T) {
	f := newEventsFixture(t)
	cam := f.addZoneCamera(t, "wall", "zone-wall", "Wall")
	f.fileEvent(t, cam, eventSpec{
		Key: "boxes-1", Started: time.Now().UTC().Add(-time.Hour), Seconds: 5, Severity: 2,
		Classes: []string{"person"}, ZoneID: "zone-wall", Rule: "intrusion",
		Objects: []models.EventObject{
			{Class: "person", Conf: 0.7, Box: []float64{1, 2, 3, 4}},
			{Class: "person", Conf: 0.8, Box: []float64{5, 6, 7, 8}},
			{Class: "person", Conf: 0.9, Box: []float64{9, 10, 11, 12}},
		},
	})

	code, body, _ := f.get(t, "/api/v1/events", "", f.Viewer)
	if code != http.StatusOK {
		t.Fatalf("got %d %v, want 200", code, body)
	}
	row := eventList(t, body)[0]
	if row["object_count"] != float64(3) {
		t.Fatalf("the wall counts the boxes: got %v", row["object_count"])
	}
	if boxes, ok := row["objects"].([]any); !ok || len(boxes) != 0 {
		t.Fatalf("the wall carries no boxes: got %v", row["objects"])
	}
}

// TestEventsDetailUnknownEventIsFourOhFour is the shared lookup's answer.
func TestEventsDetailUnknownEventIsFourOhFour(t *testing.T) {
	f := newEventsFixture(t)
	code, body, _ := f.get(t, "/api/v1/events/00000000-0000-4000-8000-000000000000", "", f.Viewer)
	if code != http.StatusNotFound || body["error"] != "event not found" {
		t.Fatalf("got %d %v, want 404 event not found", code, body)
	}
}

// TestEventsDeleteIsAdminOnlyAndWritesTheLedger is the act. An operator changes
// what gets recorded; only an admin removes footage that was recorded, and the
// ledger names who did it.
func TestEventsDeleteIsAdminOnlyAndWritesTheLedger(t *testing.T) {
	f := newEventsFixture(t)
	cam := f.addZoneCamera(t, "ledger", "zone-ledger", "Ledger")
	event := f.fileEvent(t, cam, eventSpec{Key: "del-audit", Started: time.Now().UTC().Add(-time.Hour),
		Seconds: 10, Severity: 2, Classes: []string{"person"}, ZoneID: "zone-ledger", Rule: "loiter"})
	path := "/api/v1/events/" + event.ID

	for name, bearer := range map[string]string{"viewer": f.Viewer, "operator": f.Operator} {
		code, _, raw := f.del(t, path, bearer)
		if code != http.StatusForbidden {
			t.Fatalf("%s may not delete an event: got %d %s, want 403", name, code, raw)
		}
	}
	if _, err := f.Store.Events.GetByID(event.ID); err != nil {
		t.Fatalf("a refused delete changed nothing: %v", err)
	}

	code, body, raw := f.del(t, path, f.Admin)
	if code != http.StatusOK {
		t.Fatalf("an admin deletes an event: got %d %s, want 200", code, raw)
	}
	if body["deleted"] != event.ID {
		t.Fatalf("the answer names what it removed: got %v", body)
	}
	if _, err := f.Store.Events.GetByID(event.ID); err == nil {
		t.Fatalf("the row is gone")
	}
	if code, body, _ := f.get(t, path, "", f.Viewer); code != http.StatusNotFound || body["error"] != "event not found" {
		t.Fatalf("the deleted event answers 404: got %d %v", code, body)
	}

	rows := auditRows(t, f.Store, models.AuditEventDelete)
	var ok, refused []models.Audit
	for _, row := range rows {
		if row.Outcome == models.AuditOutcomeOK {
			ok = append(ok, row)
		} else {
			refused = append(refused, row)
		}
	}
	if len(ok) != 1 {
		t.Fatalf("one ledger row for the delete: got %d of %d rows", len(ok), len(rows))
	}
	// The two refusals are in the same ledger: a 403 the operator cannot
	// reconstruct is a 403 that turns into someone reading server logs.
	if len(refused) != 2 {
		t.Fatalf("both refusals are recorded too: got %d of %d rows", len(refused), len(rows))
	}

	row := ok[0]
	if row.TargetID != event.ID || row.TargetType != "event" {
		t.Fatalf("the row names the event: got type=%s id=%s", row.TargetType, row.TargetID)
	}
	if row.ActorUsername != "wall-admin" || row.Outcome != models.AuditOutcomeOK {
		t.Fatalf("the row names who did it and how it went: got actor=%s outcome=%s",
			row.ActorUsername, row.Outcome)
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(row.Details), &details); err != nil {
		t.Fatalf("the row's details are JSON (%q): %v", row.Details, err)
	}
	if details["camera_id"] != cam.ID || details["event_key"] != "del-audit" {
		t.Fatalf("the row says what was destroyed: got %v", details)
	}
}

// TestEventsDeleteEnqueuesPurgeJobsForBothMediaKeys is the exit criterion in the
// queue's shape: two jobs, one per key, each naming its own backend and key, so
// they survive the row they came from.
func TestEventsDeleteEnqueuesPurgeJobsForBothMediaKeys(t *testing.T) {
	f := newEventsFixture(t)
	backend := &models.StorageBackend{
		Name: "the-tier", Kind: models.StorageKindFS, RootPath: t.TempDir(),
		Enabled: true, IsDefault: true,
	}
	if err := f.Store.StorageBackends.Create(backend); err != nil {
		t.Fatalf("creating the storage backend: %v", err)
	}

	cam := f.addZoneCamera(t, "purge", "zone-purge", "Purge")
	event := f.fileEvent(t, cam, eventSpec{Key: "del-jobs", Started: time.Now().UTC().Add(-time.Hour),
		Seconds: 10, Severity: 2, Classes: []string{"person"}, ZoneID: "zone-purge", Rule: "loiter"})

	// Both media are on the tier, under the plan's keys.
	if err := f.Store.Events.MarkSnapshotUploaded(event.ID, "nvr/events/"+event.ID+"/snapshot.jpg"); err != nil {
		t.Fatalf("marking the snapshot uploaded: %v", err)
	}
	if err := f.Store.Events.MarkClipUploaded(event.ID, "nvr/events/"+event.ID+"/clip.mp4"); err != nil {
		t.Fatalf("marking the clip uploaded: %v", err)
	}

	code, body, raw := f.del(t, "/api/v1/events/"+event.ID, f.Admin)
	if code != http.StatusOK {
		t.Fatalf("got %d %s, want 200", code, raw)
	}
	if body["remote_jobs"] != float64(2) {
		t.Fatalf("both media keys went to the queue: got %v (%s)", body["remote_jobs"], raw)
	}
	if body["queue_warning"] != "" {
		t.Fatalf("the queue accepted both jobs: got warning %v", body["queue_warning"])
	}

	for _, tc := range []struct {
		kind, key string
	}{
		{models.JobKindClip, "nvr/events/" + event.ID + "/clip.mp4"},
		{models.JobKindSnapshot, "nvr/events/" + event.ID + "/snapshot.jpg"},
	} {
		job, err := f.Store.StorageJobs.GetByID(event.ID + ":" + tc.kind)
		if err != nil {
			t.Fatalf("the %s purge job is queued under <event_id>:%s: %v", tc.kind, tc.kind, err)
		}
		if job.Kind != tc.kind || job.Action != models.JobActionDeleteRemote {
			t.Fatalf("the job says what to do: got kind=%s action=%s", job.Kind, job.Action)
		}
		if job.Target != tc.key {
			t.Fatalf("the job names the key to remove: got %q, want %q", job.Target, tc.key)
		}
		if job.BackendID != backend.ID || job.State != models.JobStatePending {
			t.Fatalf("the job names its backend and waits: got backend=%s state=%s", job.BackendID, job.State)
		}
	}
}

// TestEventsDeleteRemovesLocalMediaAndSaysWhatItCouldNot is the local half, and
// the local-only installation. A row deleted with its files still on disk is
// bytes nothing will ever reclaim, and an installation with no tier has nothing
// remote to purge — which is a sentence, not an error.
func TestEventsDeleteRemovesLocalMediaAndSaysWhatItCouldNot(t *testing.T) {
	f := newEventsFixture(t)
	cam := f.addZoneCamera(t, "local", "zone-local", "Local")
	event := f.fileEvent(t, cam, eventSpec{Key: "del-local", Started: time.Now().UTC().Add(-time.Hour),
		Seconds: 10, Severity: 2, Classes: []string{"person"}, ZoneID: "zone-local", Rule: "loiter"})

	clip := filepath.Join(f.ClipsDir, event.ID+".mp4")
	snapshot := filepath.Join(f.SnapshotsDir, event.ID+".jpg")
	if err := os.WriteFile(clip, []byte("a clip of the event"), 0o640); err != nil {
		t.Fatalf("writing the clip: %v", err)
	}
	if err := os.WriteFile(snapshot, []byte("a picture of the event"), 0o640); err != nil {
		t.Fatalf("writing the snapshot: %v", err)
	}
	if err := f.Store.Events.SetLocalClip(event.ID, clip, 10); err != nil {
		t.Fatalf("stamping the clip: %v", err)
	}
	if err := f.Store.Events.SetLocalSnapshot(event.ID, snapshot); err != nil {
		t.Fatalf("stamping the snapshot: %v", err)
	}

	code, body, raw := f.del(t, "/api/v1/events/"+event.ID, f.Admin)
	if code != http.StatusOK {
		t.Fatalf("got %d %s, want 200", code, raw)
	}
	if body["local_files"] != float64(2) {
		t.Fatalf("both files went with the row: got %v", body["local_files"])
	}
	for _, path := range []string{clip, snapshot} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s was deleted: %v", path, err)
		}
	}

	// Nothing was uploaded, so there is nothing remote to purge — and with no
	// backend configured the answer says that in words instead of failing.
	if body["remote_jobs"] != float64(0) {
		t.Fatalf("un-uploaded media queues no remote job: got %v", body["remote_jobs"])
	}
	warning, _ := body["queue_warning"].(string)
	if !strings.Contains(warning, "no storage tier is configured") {
		t.Fatalf("a local-only installation is stated, not improvised: got %q", warning)
	}
}

// TestEventsDeleteCancelsAnArmedAssembly is the one thing the delete owes the
// clip builder. An assembly that fires after the row is gone cuts a clip for an
// event nobody can ask for and leaves a file no row points at.
func TestEventsDeleteCancelsAnArmedAssembly(t *testing.T) {
	f := newEventsFixture(t)
	spy := &clipSpy{}
	f.Handlers.Clips = spy

	cam := f.addZoneCamera(t, "armed", "zone-armed", "Armed")
	event := f.fileEvent(t, cam, eventSpec{Key: "del-cancel", Started: time.Now().UTC().Add(-time.Minute),
		Seconds: 10, Severity: 2, Classes: []string{"person"}, ZoneID: "zone-armed", Rule: "loiter"})

	if _, _, raw := f.del(t, "/api/v1/events/"+event.ID, f.Admin); !strings.Contains(raw, `"deleted"`) {
		t.Fatalf("the delete answered: %s", raw)
	}
	if got := spy.cancelCount(); got != 1 {
		t.Fatalf("the pending assembly was cancelled once: got %d", got)
	}
	if len(spy.cancelled) != 1 || spy.cancelled[0] != event.ID {
		t.Fatalf("the cancel named this event: got %v", spy.cancelled)
	}
}

// TestEventsClipServesTheLocalFileAndAnswers206ForARange is the delivery this
// build has: the file on this disk, seekable. A scrubber that cannot seek has to
// download a clip to find the second of it it wants.
func TestEventsClipServesTheLocalFileAndAnswers206ForARange(t *testing.T) {
	f := newEventsFixture(t)
	cam := f.addZoneCamera(t, "clip", "zone-clip", "Clip")
	event := f.fileEvent(t, cam, eventSpec{Key: "clip-serve", Started: time.Now().UTC().Add(-time.Hour),
		Seconds: 10, Severity: 2, Classes: []string{"person"}, ZoneID: "zone-clip", Rule: "loiter"})

	body := strings.Repeat("mp4bytes", 512)
	path := filepath.Join(f.ClipsDir, event.ID+".mp4")
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatalf("writing the clip: %v", err)
	}
	if err := f.Store.Events.SetLocalClip(event.ID, path, 10); err != nil {
		t.Fatalf("stamping the clip: %v", err)
	}

	code, headers, got := f.getWithHeaders(t, "/api/v1/events/"+event.ID+"/clip", f.Viewer, nil)
	if code != http.StatusOK {
		t.Fatalf("got %d, want 200", code)
	}
	if got != body {
		t.Fatalf("the bytes are the file: got %d bytes, want %d", len(got), len(body))
	}
	if ct := headers.Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("Content-Type is video/mp4: got %q", ct)
	}
	if cc := headers.Get("Cache-Control"); cc != "private, max-age=3600" {
		t.Fatalf("the cache promise is stated: got %q", cc)
	}
	if ranges := headers.Get("Accept-Ranges"); ranges != "bytes" {
		t.Fatalf("the route says it can be seeked: got %q", ranges)
	}

	code, headers, got = f.getWithHeaders(t, "/api/v1/events/"+event.ID+"/clip", f.Viewer,
		map[string]string{"Range": "bytes=0-99999"})
	if code != http.StatusPartialContent {
		t.Fatalf("a Range request answers 206: got %d", code)
	}
	if len(got) != len(body) {
		t.Fatalf("a Range wider than the file returns the file: got %d bytes, want %d", len(got), len(body))
	}
	if cr := headers.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-") {
		t.Fatalf("the partial answer says what it sent: got %q", cr)
	}

	code, headers, got = f.getWithHeaders(t, "/api/v1/events/"+event.ID+"/clip", f.Viewer,
		map[string]string{"Range": "bytes=8-15"})
	if code != http.StatusPartialContent || got != body[8:16] {
		t.Fatalf("a Range inside the file answers that slice: got %d %q", code, got)
	}
}

// TestEventsClipGoneMediaAnswersNamedNotFound is the exit criterion. Three states
// end here — no clip was ever cut, the local file is gone, and the copy that
// exists is on a tier this build cannot stream yet — and the console draws all
// three the same way, so they share one answer. The event itself is not missing,
// and saying so is the difference between a broken clip and a broken event.
func TestEventsClipGoneMediaAnswersNamedNotFound(t *testing.T) {
	f := newEventsFixture(t)
	cam := f.addZoneCamera(t, "gone", "zone-gone", "Gone")

	// 1. Never cut: a camera in record_mode=off, or a window with no segments.
	never := f.fileEvent(t, cam, eventSpec{Key: "gone-none", Started: time.Now().UTC().Add(-time.Hour),
		Seconds: 10, Severity: 2, Classes: []string{"person"}, ZoneID: "zone-gone", Rule: "loiter"})
	code, body, raw := f.get(t, "/api/v1/events/"+never.ID+"/clip", "", f.Viewer)
	if code != http.StatusNotFound {
		t.Fatalf("an event with no clip: got %d %s, want 404", code, raw)
	}
	if raw != `{"error":"clip not available"}` {
		t.Fatalf("the body is the plan's words exactly: got %s", raw)
	}
	if body["error"] != "clip not available" {
		t.Fatalf("the body is the plan's words: got %v", body["error"])
	}
	// The event itself is still there, and still readable.
	if code, _, _ := f.get(t, "/api/v1/events/"+never.ID, "", f.Viewer); code != http.StatusOK {
		t.Fatalf("the event survives its media: got %d, want 200", code)
	}

	// 2. On the tier, not on this disk: the local copy was aged out (or the
	// volume came back without it). Streaming from the backend is S6-05.
	remote := f.fileEvent(t, cam, eventSpec{Key: "gone-remote", Started: time.Now().UTC().Add(-2 * time.Hour),
		Seconds: 10, Severity: 2, Classes: []string{"person"}, ZoneID: "zone-gone", Rule: "loiter"})
	if err := f.Store.Events.MarkClipUploaded(remote.ID, "nvr/events/"+remote.ID+"/clip.mp4"); err != nil {
		t.Fatalf("marking the clip uploaded: %v", err)
	}
	code, body, raw = f.get(t, "/api/v1/events/"+remote.ID+"/clip", "", f.Viewer)
	if code != http.StatusNotFound || body["error"] != "clip not available" {
		t.Fatalf("a clip that exists only on the tier: got %d %s, want 404 clip not available", code, raw)
	}

	// 3. The row names a file outside CLIPS_DIR — a row from another
	// installation. Serving it would make this route a general file reader.
	elsewhere := filepath.Join(t.TempDir(), "somebody-elses-clip.mp4")
	if err := os.WriteFile(elsewhere, []byte("bytes"), 0o640); err != nil {
		t.Fatalf("writing the outside clip: %v", err)
	}
	outside := f.fileEvent(t, cam, eventSpec{Key: "gone-outside", Started: time.Now().UTC().Add(-3 * time.Hour),
		Seconds: 10, Severity: 2, Classes: []string{"person"}, ZoneID: "zone-gone", Rule: "loiter"})
	if err := f.Store.Events.SetLocalClip(outside.ID, elsewhere, 10); err != nil {
		t.Fatalf("stamping the outside clip: %v", err)
	}
	if code, body, _ := f.get(t, "/api/v1/events/"+outside.ID+"/clip", "", f.Viewer); code != http.StatusNotFound ||
		body["error"] != "clip not available" {
		t.Fatalf("a clip outside CLIPS_DIR is not served: got %d %v", code, body)
	}

	// 4. The row names a file the disk does not have: the sweeper's local delete
	// raced this request.
	dangling := f.fileEvent(t, cam, eventSpec{Key: "gone-dangling", Started: time.Now().UTC().Add(-4 * time.Hour),
		Seconds: 10, Severity: 2, Classes: []string{"person"}, ZoneID: "zone-gone", Rule: "loiter"})
	missing := filepath.Join(f.ClipsDir, dangling.ID+".mp4")
	if err := f.Store.Events.SetLocalClip(dangling.ID, missing, 10); err != nil {
		t.Fatalf("stamping the missing clip: %v", err)
	}
	if code, body, _ := f.get(t, "/api/v1/events/"+dangling.ID+"/clip", "", f.Viewer); code != http.StatusNotFound ||
		body["error"] != "clip not available" {
		t.Fatalf("a clip the disk does not have: got %d %v", code, body)
	}

	// And the route shares the lookup: an id that names no event is the event's
	// own 404, not the media's.
	if code, body, _ := f.get(t, "/api/v1/events/00000000-0000-4000-8000-000000000001/clip", "", f.Viewer); code != http.StatusNotFound ||
		body["error"] != "event not found" {
		t.Fatalf("no such event is the event's 404: got %d %v", code, body)
	}
}

// TestEventsRoutesAreBehindTheAuthGate keeps the surface from growing a hole: no
// token, no event list.
func TestEventsRoutesAreBehindTheAuthGate(t *testing.T) {
	f := newEventsFixture(t)
	for _, path := range []string{"/api/v1/events", "/api/v1/events/00000000-0000-4000-8000-000000000002"} {
		code, _, raw := f.get(t, path, "", "")
		if code != http.StatusUnauthorized {
			t.Fatalf("%s without a token: got %d %s, want 401", path, code, raw)
		}
	}
}
