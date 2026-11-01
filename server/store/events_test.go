package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

func eventKey(cameraID, zoneID, rule string, at time.Time) string {
	return cameraID + ":" + zoneID + ":" + rule + ":" + itoa(int(at.Unix()))
}

func openInput(cameraID string, at time.Time) EventInput {
	return EventInput{
		EventKey: eventKey(cameraID, "porch", models.ZoneRuleLoiter, at),
		CameraID: cameraID,
		Action:   models.SignalActionOpen,
		Rule:     models.ZoneRuleLoiter,
		ZoneID:   "porch",
		Severity: 2,
		Classes:  []string{"person"},
		Objects:  []models.EventObject{{Class: "person", Conf: 0.81, Box: []float64{100, 50, 140, 150}}},
		TS:       at,
	}
}

func TestUpsertOpenInsertsOneEvent(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	at := minutesAgo(3)

	event, created, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, at))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !created {
		t.Fatal("expected an open to create the event")
	}
	if !event.Open || event.Action != models.SignalActionOpen {
		t.Fatalf("expected a live open event, got open=%v action=%q", event.Open, event.Action)
	}
	if !event.StartedAt.Equal(event.EndedAt) {
		t.Fatalf("an open sets the window to the signal: %v vs %v", event.StartedAt, event.EndedAt)
	}
	if event.SignalCount != 1 || event.Severity != 2 {
		t.Fatalf("expected 1 signal at severity 2, got %+v", event)
	}

	// Every JSON column holds JSON, always: readers unmarshal them, and an
	// empty string is not JSON.
	for name, raw := range map[string]string{
		"classes_json": event.ClassesJSON,
		"zones_json":   event.ZonesJSON,
		"rules_json":   event.RulesJSON,
		"objects_json": event.ObjectsJSON,
	} {
		if !json.Valid([]byte(raw)) {
			t.Fatalf("%s is not valid JSON: %q", name, raw)
		}
	}
}

func TestUpsertOpenDedupesTheSameKey(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	at := minutesAgo(3)

	first, _, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, at))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	second, created, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, at))
	if err != nil {
		t.Fatalf("replayed open: %v", err)
	}
	if created {
		t.Fatal("a replayed open must not create a second event")
	}
	if second.ID != first.ID {
		t.Fatalf("expected the same row back, got %s vs %s", second.ID, first.ID)
	}

	var count int64
	if err := st.DB.Model(&models.Event{}).Count(&count).Error; err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 event after two opens, got %d", count)
	}
	if second.SignalCount != 1 {
		t.Fatalf("a duplicate open must not count as a second signal, got %d", second.SignalCount)
	}
}

// TestUpsertOneEventAcrossThreeSignals is the dedupe rule in the shape the
// worker actually produces: open, extend, close all carry the same event_key,
// and the installation must see one event, not three.
func TestUpsertOneEventAcrossThreeSignals(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	open := openInput(camera.ID, minutesAgo(10))
	event, created, err := st.Events.UpsertByEventKey(ctx(), open)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !created {
		t.Fatal("expected the first signal to create the event")
	}

	for _, signal := range []EventInput{
		{EventKey: event.EventKey, CameraID: camera.ID, Action: models.SignalActionExtend,
			Rule: models.ZoneRuleIntrusion, ZoneID: "driveway", Severity: 3,
			Classes: []string{"person", "car"}, TS: minutesAgo(8)},
		{EventKey: event.EventKey, CameraID: camera.ID, Action: models.SignalActionClose,
			TS: minutesAgo(6)},
	} {
		if _, _, err := st.Events.UpsertByEventKey(ctx(), signal); err != nil {
			t.Fatalf("%s: %v", signal.Action, err)
		}
	}

	var count int64
	if err := st.DB.Model(&models.Event{}).Count(&count).Error; err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one event across three signals, got %d", count)
	}

	stored, err := st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.SignalCount != 3 {
		t.Fatalf("expected 3 signals on the one row, got %d", stored.SignalCount)
	}
	if !stored.StartedAt.Equal(event.StartedAt) {
		t.Fatalf("the open owns the window's start: %v", stored.StartedAt)
	}
	if stored.Open {
		t.Fatal("the close must have closed the event")
	}
}

func TestUpsertExtendAccumulates(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	start := minutesAgo(5)

	event, _, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, start))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	extend := EventInput{
		EventKey: event.EventKey,
		CameraID: camera.ID,
		Action:   models.SignalActionExtend,
		Rule:     models.ZoneRuleIntrusion,
		ZoneID:   "driveway",
		Severity: 3,
		Classes:  []string{"person", "car"},
		Objects:  []models.EventObject{{Class: "car", Conf: 0.6, Box: []float64{0, 0, 10, 10}}},
		TS:       minutesAgo(4),
	}
	extended, _, err := st.Events.UpsertByEventKey(ctx(), extend)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}

	if !extended.EndedAt.After(event.EndedAt) {
		t.Fatalf("expected ended_at to advance: %v -> %v", event.EndedAt, extended.EndedAt)
	}
	if extended.Severity != 3 {
		t.Fatalf("expected severity max(2,3)=3, got %d", extended.Severity)
	}
	if got := models.JSONStringList(extended.ClassesJSON); strings.Join(got, ",") != "person,car" {
		t.Fatalf("expected the classes union in order, got %v", got)
	}
	if got := models.JSONStringList(extended.ZonesJSON); strings.Join(got, ",") != "porch,driveway" {
		t.Fatalf("expected the zones union, got %v", got)
	}
	if got := models.JSONStringList(extended.RulesJSON); strings.Join(got, ",") != "loiter,intrusion" {
		t.Fatalf("expected the rules union, got %v", got)
	}
	if len(ParseObjects(extended.ObjectsJSON)) != 2 {
		t.Fatalf("expected 2 boxes, got %d", len(ParseObjects(extended.ObjectsJSON)))
	}
	if extended.SignalCount != 2 {
		t.Fatalf("expected 2 signals, got %d", extended.SignalCount)
	}
	if extended.StartedAt != event.StartedAt {
		t.Fatalf("an extend moves the window's end, never its start: %v -> %v", event.StartedAt, extended.StartedAt)
	}

	// A replayed extend from the worker's buffer must not pull the window back.
	replay := extend
	replay.TS = minutesAgo(6)
	after, _, err := st.Events.UpsertByEventKey(ctx(), replay)
	if err != nil {
		t.Fatalf("replayed extend: %v", err)
	}
	if !after.EndedAt.Equal(extended.EndedAt) {
		t.Fatalf("a stale extend moved ended_at: %v -> %v", extended.EndedAt, after.EndedAt)
	}
}

// TestUpsertSeverityIsAMonotoneMax: an extend can raise severity, never lower
// it. The event console sorts by severity, and a trailing low-severity box
// must not quiet a loiter that turned into an intrusion.
func TestUpsertSeverityIsAMonotoneMax(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	open := openInput(camera.ID, minutesAgo(12))
	open.Severity = 4
	event, _, err := st.Events.UpsertByEventKey(ctx(), open)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if event.Severity != 4 {
		t.Fatalf("expected the open's severity 4, got %d", event.Severity)
	}

	quieter := EventInput{EventKey: event.EventKey, CameraID: camera.ID,
		Action: models.SignalActionExtend, Severity: 2, TS: minutesAgo(10)}
	afterQuiet, _, err := st.Events.UpsertByEventKey(ctx(), quieter)
	if err != nil {
		t.Fatalf("extend at severity 2: %v", err)
	}
	if afterQuiet.Severity != 4 {
		t.Fatalf("a lower-severity extend lowered severity to %d", afterQuiet.Severity)
	}

	louder := quieter
	louder.Severity = 5
	louder.TS = minutesAgo(9)
	afterLoud, _, err := st.Events.UpsertByEventKey(ctx(), louder)
	if err != nil {
		t.Fatalf("extend at severity 5: %v", err)
	}
	if afterLoud.Severity != 5 {
		t.Fatalf("expected severity to rise to 5, got %d", afterLoud.Severity)
	}
}

func TestUpsertCloseEndsTheEvent(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	event, _, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, minutesAgo(5)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closed, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: event.EventKey,
		CameraID: camera.ID,
		Action:   models.SignalActionClose,
		TS:       minutesAgo(4),
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.Open {
		t.Fatal("expected the event to be closed")
	}
	if closed.Action != models.SignalActionClose {
		t.Fatalf("expected action close, got %q", closed.Action)
	}

	// A duplicate close is a no-op that answers the row: a closed event is what
	// a close asks for, and the worker's buffer re-sends.
	again, created, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: event.EventKey,
		CameraID: camera.ID,
		Action:   models.SignalActionClose,
		TS:       minutesAgo(2),
	})
	if err != nil {
		t.Fatalf("a duplicate close must succeed, got %v", err)
	}
	if created || again.ID != event.ID || again.Open {
		t.Fatalf("expected the same closed row back, got created=%v %+v", created, again)
	}
}

func TestUpsertAfterCloseMutatesNothing(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	event, _, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, minutesAgo(10)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: event.EventKey, CameraID: camera.ID, Action: models.SignalActionClose, TS: minutesAgo(9),
	}); err != nil {
		t.Fatalf("close: %v", err)
	}

	before, err := st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	late := openInput(camera.ID, minutesAgo(1))
	late.EventKey = event.EventKey
	late.Action = models.SignalActionExtend
	late.Severity = 3
	late.Classes = []string{"bear"}

	returned, created, err := st.Events.UpsertByEventKey(ctx(), late)
	if !errors.Is(err, ErrEventClosed) {
		t.Fatalf("expected ErrEventClosed, got %v", err)
	}
	if created {
		t.Fatal("a late signal must not create a row")
	}
	// The closed row travels with the error: the handler's 200 body names the
	// event the signal was about.
	if returned == nil || returned.ID != event.ID {
		t.Fatal("the closed row is the answer a late signal gets")
	}

	after, err := st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !after.EndedAt.Equal(before.EndedAt) || after.Severity != before.Severity ||
		after.ClassesJSON != before.ClassesJSON || after.SignalCount != before.SignalCount ||
		after.Open != before.Open {
		t.Fatalf("a post-close signal mutated the row:\nbefore %+v\nafter  %+v", before, after)
	}

	// A late open for a closed key is refused the same way.
	lateOpen := openInput(camera.ID, minutesAgo(1))
	lateOpen.EventKey = event.EventKey
	if _, _, err := st.Events.UpsertByEventKey(ctx(), lateOpen); !errors.Is(err, ErrEventClosed) {
		t.Fatalf("a late open after close: expected ErrEventClosed, got %v", err)
	}
}

func TestUpsertObjectsAreCappedOldestFirst(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	event, _, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, minutesAgo(30)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// 250 boxes over ten extends: the row keeps the newest 200, and the oldest
	// 50 are the ones that go.
	for i := 0; i < 10; i++ {
		objects := make([]models.EventObject, 0, 25)
		for j := 0; j < 25; j++ {
			serial := i*25 + j
			objects = append(objects, models.EventObject{
				Class: "person",
				Conf:  float64(serial),
				Box:   []float64{float64(serial), 0, float64(serial) + 10, 10},
			})
		}
		if _, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
			EventKey: event.EventKey, CameraID: camera.ID, Action: models.SignalActionExtend,
			Objects: objects, TS: minutesAgo(29 - i),
		}); err != nil {
			t.Fatalf("extend %d: %v", i, err)
		}
	}

	stored, err := st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	objects := ParseObjects(stored.ObjectsJSON)
	if len(objects) != models.MaxEventObjects {
		t.Fatalf("expected the cap at %d, got %d", models.MaxEventObjects, len(objects))
	}
	if last := objects[len(objects)-1]; last.Conf != 249 {
		t.Fatalf("expected the newest box to survive, got conf %v", last.Conf)
	}
	if first := objects[0]; first.Conf != 50 {
		t.Fatalf("expected trimming to drop the 50 oldest; oldest kept has conf %v", first.Conf)
	}
}

func TestUpsertServerClockOwnsEventTime(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	future := time.Now().UTC().Add(48 * time.Hour)
	event, _, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, future))
	if err != nil {
		t.Fatalf("open with a future ts: %v", err)
	}
	if event.StartedAt.After(time.Now().UTC()) {
		t.Fatalf("a worker's clock put an event in the future: %v", event.StartedAt)
	}
	if event.EndedAt.After(time.Now().UTC()) {
		t.Fatalf("a worker's clock put an event window in the future: %v", event.EndedAt)
	}

	// A zero ts is not "the beginning of the epoch" either.
	zero := openInput(camera.ID, time.Now().UTC())
	zero.TS = time.Time{}
	created, _, err := st.Events.UpsertByEventKey(ctx(), zero)
	if err != nil {
		t.Fatalf("open with a zero ts: %v", err)
	}
	if created.StartedAt.Year() < 2000 {
		t.Fatalf("a zero ts became year %d; the server clock is the only clock", created.StartedAt.Year())
	}
}

func TestUpsertCloseWithoutOpenCreatesTheRow(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	// The worker's buffer drops oldest on persistent failure. Losing the event
	// because its opening signal was dropped is worse than reconstructing it
	// from the close.
	input := EventInput{
		EventKey: eventKey(camera.ID, "porch", models.ZoneRuleLoiter, minutesAgo(2)),
		CameraID: camera.ID,
		Action:   models.SignalActionClose,
		Rule:     models.ZoneRuleLoiter,
		ZoneID:   "porch",
		Severity: 2,
		TS:       minutesAgo(1),
	}
	event, created, err := st.Events.UpsertByEventKey(ctx(), input)
	if err != nil {
		t.Fatalf("close without open: %v", err)
	}
	if !created {
		t.Fatal("expected the close to create the row")
	}
	if event.Open {
		t.Fatal("a row created by a close is closed, not live")
	}
	if !event.StartedAt.Equal(event.EndedAt) {
		t.Fatalf("the reconstructed window is the signal: %v vs %v", event.StartedAt, event.EndedAt)
	}

	// And it is closed in the database, not just in the returned struct.
	loaded, err := st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Open {
		t.Fatal("the stored row must carry the close (schema default vs explicit write)")
	}
}

func TestUpsertRejectsAnUnknownAction(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	if _, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: "k", CameraID: camera.ID, Action: "expired", TS: time.Now().UTC(),
	}); err == nil || !strings.Contains(err.Error(), "not a signal action") {
		t.Fatalf("expected a named action rejection, got %v", err)
	}
	if _, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: "k", CameraID: "", Action: models.SignalActionOpen, TS: time.Now().UTC(),
	}); err == nil {
		t.Fatal("expected a rejection for a signal with no camera")
	}
}

func TestEventListFilters(t *testing.T) {
	st := newTestStore(t)
	porch := seedCamera(t, st, "porch-cam")
	drive := seedCamera(t, st, "drive-cam")

	// porch: a closed, high-severity person loiter on the porch.
	event, _, err := st.Events.UpsertByEventKey(ctx(), openInput(porch.ID, minutesAgo(30)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err = st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: event.EventKey, CameraID: porch.ID, Action: models.SignalActionExtend,
		Severity: 3, Classes: []string{"person"}, TS: minutesAgo(25),
	}); err != nil {
		t.Fatalf("extend: %v", err)
	}
	if _, _, err = st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: event.EventKey, CameraID: porch.ID, Action: models.SignalActionClose, TS: minutesAgo(24),
	}); err != nil {
		t.Fatalf("close: %v", err)
	}

	// drive: an open car intrusion on the driveway.
	car := openInput(drive.ID, minutesAgo(20))
	car.Classes = []string{"car"}
	car.Rule = models.ZoneRuleIntrusion
	car.ZoneID = "driveway"
	car.Severity = 1
	if _, _, err := st.Events.UpsertByEventKey(ctx(), car); err != nil {
		t.Fatalf("open car: %v", err)
	}

	if got, err := st.Events.List(EventFilter{CameraID: porch.ID}, 0); err != nil || len(got) != 1 {
		t.Fatalf("camera filter: %d events, %v", len(got), err)
	}
	if got, _ := st.Events.List(EventFilter{Class: "car"}, 0); len(got) != 1 {
		t.Fatalf("class filter: %d events", len(got))
	}
	if got, _ := st.Events.List(EventFilter{ZoneID: "driveway"}, 0); len(got) != 1 {
		t.Fatalf("zone filter: %d events", len(got))
	}
	severity := 3
	if got, _ := st.Events.List(EventFilter{Severity: &severity}, 0); len(got) != 1 {
		t.Fatalf("severity filter: %d events", len(got))
	}
	if got, _ := st.Events.List(EventFilter{OpenOnly: true}, 0); len(got) != 1 {
		t.Fatalf("open-only filter: %d events", len(got))
	}
	if got, _ := st.Events.List(EventFilter{Q: "loiter"}, 0); len(got) != 1 {
		t.Fatalf("q filter on a rule name: %d events", len(got))
	}

	// `from` is a window edge: both events end inside it.
	from := minutesAgo(25)
	if got, _ := st.Events.List(EventFilter{From: &from}, 0); len(got) != 2 {
		t.Fatalf("from filter: %d events", len(got))
	}

	// The limit clamp: a timeline request that asks for everything is a request
	// that reads the whole table.
	got, err := st.Events.List(EventFilter{Limit: 5000}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) > EventListMaxLimit {
		t.Fatalf("expected the limit clamp at %d, got %d", EventListMaxLimit, len(got))
	}
	if got, _ := st.Events.List(EventFilter{}, 1); len(got) != 1 {
		t.Fatalf("offset must page the list, got %d", len(got))
	}
}

func TestEventListEscapeLikeIsLiteral(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "porch-cam")

	input := openInput(camera.ID, minutesAgo(3))
	input.Classes = []string{"a_b"}
	if _, _, err := st.Events.UpsertByEventKey(ctx(), input); err != nil {
		t.Fatalf("open: %v", err)
	}
	if got, _ := st.Events.List(EventFilter{Q: "a_b"}, 0); len(got) != 1 {
		t.Fatalf("expected the literal match, got %d", len(got))
	}
	if got, _ := st.Events.List(EventFilter{Q: "axb"}, 0); len(got) != 0 {
		t.Fatalf("the underscore must not act as a wildcard, matched %d", len(got))
	}
}

func TestEventMediaStamps(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "porch-cam")

	input := openInput(camera.ID, minutesAgo(3))
	input.LocalSnapshotPath = "/var/lib/nvr/snapshots/pending.jpg"
	event, _, err := st.Events.UpsertByEventKey(ctx(), input)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if event.LocalSnapshotPath == "" {
		t.Fatal("the open signal's snapshot path must land on the row")
	}

	if err := st.Events.SetLocalClip(event.ID, "/var/lib/nvr/clips/"+event.ID+".mp4", 23.5); err != nil {
		t.Fatalf("set clip: %v", err)
	}
	loaded, err := st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.ClipSeconds != 23.5 || loaded.LocalClipPath == "" {
		t.Fatalf("expected the clip stamp, got %+v", loaded)
	}
	if loaded.ClipUploaded() || loaded.SnapshotUploaded() {
		t.Fatal("nothing is uploaded yet: the sweeper must not delete un-uploaded media")
	}

	if err := st.Events.MarkSnapshotUploaded(event.ID, "nvr/events/"+event.ID+"/snapshot.jpg"); err != nil {
		t.Fatalf("mark snapshot: %v", err)
	}
	loaded, err = st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !loaded.SnapshotUploaded() || loaded.SnapshotKey == "" {
		t.Fatalf("expected the snapshot upload stamp, got %+v", loaded)
	}
	if loaded.ClipUploadedAt != nil {
		t.Fatal("the clip is still local-only")
	}

	// The no-segments outcome the clip builder reports: the event keeps its
	// snapshot, and snapshot-only is a stated outcome, not a missing event.
	if err := st.Events.SetClipAbsent(event.ID); err != nil {
		t.Fatalf("clip absent: %v", err)
	}
	absent, err := st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if absent.ClipKey != "" || absent.LocalClipPath != "" || absent.ClipSeconds != 0 {
		t.Fatalf("expected an event with no clip, got %+v", absent)
	}
	if absent.LocalSnapshotPath == "" {
		t.Fatal("a no-clip event keeps its snapshot")
	}

	if err := st.Events.Delete("missing"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("deleting an unknown event: %v", err)
	}
	if err := st.Events.Delete(absent.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestEventListOpenIsTheClipBuildersQuery(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "porch-cam")

	live, _, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, minutesAgo(1)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, minutesAgo(40))); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: live.EventKey, CameraID: camera.ID, Action: models.SignalActionClose, TS: minutesAgo(20),
	}); err != nil {
		t.Fatalf("close: %v", err)
	}

	open, err := st.Events.ListOpen()
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected exactly the live event, got %d", len(open))
	}
}

func TestEventDeleteOlderThanIsTheRetentionSweep(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "porch-cam")

	old := openInput(camera.ID, daysAgo(120))
	if _, _, err := st.Events.UpsertByEventKey(ctx(), old); err != nil {
		t.Fatalf("open: %v", err)
	}
	// The server clock owns event time, so an old signal is filed as now; the
	// sweep test therefore moves the row's clock, exactly as a running
	// installation's rows age.
	if err := st.DB.Model(&models.Event{}).Where("event_key = ?", old.EventKey).
		Updates(map[string]any{"started_at": daysAgo(120), "ended_at": daysAgo(120)}).Error; err != nil {
		t.Fatalf("age the row: %v", err)
	}
	if _, _, err := st.Events.UpsertByEventKey(ctx(), openInput(camera.ID, minutesAgo(5))); err != nil {
		t.Fatalf("open: %v", err)
	}

	deleted, err := st.Events.DeleteOlderThan(daysAgo(90))
	if err != nil {
		t.Fatalf("delete older than: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected exactly the old row deleted, got %d", deleted)
	}
}
