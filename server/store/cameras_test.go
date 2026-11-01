package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

func TestCameraCreateMintsIDAndRespectsSlugUniqueness(t *testing.T) {
	st := newTestStore(t)

	camera := seedCamera(t, st, "front-door")
	if camera.ID == "" {
		t.Fatal("expected the store to mint the camera id")
	}

	duplicate := &models.Camera{Slug: "front-door", Name: "Copy", SourceURI: "rtsp://other", RecordMode: models.RecordModeEvents}
	err := st.Cameras.Create(duplicate)
	if !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("expected ErrSlugTaken, got %v", err)
	}
	if !strings.Contains(err.Error(), "front-door") {
		t.Fatalf("the 409 body needs the slug, got %q", err.Error())
	}
}

func TestCameraCreatedInactiveStaysInactive(t *testing.T) {
	st := newTestStore(t)

	// `active` defaults to true in the schema. A camera added switched off —
	// staged before its cable is run — must not be recording on the next boot.
	camera := &models.Camera{
		Slug: "staged-cam", Name: "Staged", SourceURI: "rtsp://staged",
		RecordMode: models.RecordModeOff, Active: false,
	}
	if err := st.Cameras.Create(camera); err != nil {
		t.Fatalf("create: %v", err)
	}

	loaded, err := st.Cameras.GetBySlug("staged-cam")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Active {
		t.Fatal("a camera created inactive arrived active (schema default won)")
	}

	active, err := st.Cameras.ListActive()
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	for _, c := range active {
		if c.Slug == "staged-cam" {
			t.Fatal("an inactive camera must not appear in go2rtc's config set")
		}
	}
}

func TestCameraGetByIDAndListAll(t *testing.T) {
	st := newTestStore(t)
	seedCamera(t, st, "front-door")
	seedCamera(t, st, "side-gate")

	if _, err := st.Cameras.GetByID("nope"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found for an unknown id, got %v", err)
	}

	all, err := st.Cameras.ListAll()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 cameras, got %d", len(all))
	}

	total, detecting, err := st.Cameras.Count()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 2 || detecting != 2 {
		t.Fatalf("expected 2 cameras, 2 detecting, got %d/%d", total, detecting)
	}
}

func TestCameraListDetectionEnabledIsTheWorkerSet(t *testing.T) {
	st := newTestStore(t)
	seedCamera(t, st, "armed-cam")

	off := &models.Camera{
		Slug: "quiet-cam", Name: "Quiet", SourceURI: "rtsp://quiet",
		RecordMode: models.RecordModeContinuous, DetectionEnabled: false, Active: true,
	}
	if err := st.Cameras.Create(off); err != nil {
		t.Fatalf("create: %v", err)
	}
	staged := &models.Camera{
		Slug: "staged-cam", Name: "Staged", SourceURI: "rtsp://staged",
		RecordMode: models.RecordModeContinuous, DetectionEnabled: true, Active: false,
	}
	if err := st.Cameras.Create(staged); err != nil {
		t.Fatalf("create: %v", err)
	}

	armed, err := st.Cameras.ListDetectionEnabled()
	if err != nil {
		t.Fatalf("list detection enabled: %v", err)
	}
	if len(armed) != 1 || armed[0].Slug != "armed-cam" {
		t.Fatalf("the worker set must be active AND detection-enabled, got %+v", armed)
	}
}

func TestCameraUpdateRejectsASlugCollision(t *testing.T) {
	st := newTestStore(t)
	seedCamera(t, st, "front-door")
	second := seedCamera(t, st, "side-gate")

	second.Slug = "front-door"
	err := st.Cameras.Update(second)
	if !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("expected ErrSlugTaken on a rename collision, got %v", err)
	}
}

func TestCameraUpdateLeavesTheZoneSetAlone(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	before := camera.ZonesJSON

	camera.Name = "Renamed Doorbell"
	camera.RecordMode = models.RecordModeEvents
	if err := st.Cameras.Update(camera); err != nil {
		t.Fatalf("update: %v", err)
	}

	loaded, err := st.Cameras.GetByID(camera.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Name != "Renamed Doorbell" || loaded.RecordMode != models.RecordModeEvents {
		t.Fatalf("update did not land: %+v", loaded)
	}
	if loaded.ZonesJSON != before {
		t.Fatalf("a camera edit must not touch the zone rule set: %q -> %q", before, loaded.ZonesJSON)
	}

	if err := st.Cameras.Update(&models.Camera{ID: "missing"}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found on an update of an unknown camera, got %v", err)
	}
}

func TestCameraUpdateZones(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	zones := `[{"id":"drive","name":"driveway","polygon":[[0,0],[1,0],[1,1],[0,1]],"classes":["car"],"rule":"intrusion","dwell_seconds":0,"severity":2,"min_confidence":0.4,"min_area_ratio":0.004,"enabled":true}]`
	if err := st.Cameras.UpdateZones(camera.ID, zones); err != nil {
		t.Fatalf("update zones: %v", err)
	}
	loaded, err := st.Cameras.GetByID(camera.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.ZonesJSON != zones {
		t.Fatalf("expected the zone set verbatim, got %q", loaded.ZonesJSON)
	}
	parsed, err := models.ParseZones(loaded.ZonesJSON)
	if err != nil {
		t.Fatalf("the stored zone set must parse: %v", err)
	}
	if len(parsed) != 1 || parsed[0].ID != "drive" {
		t.Fatalf("expected the one zone back, got %+v", parsed)
	}

	if err := st.Cameras.UpdateZones("missing", zones); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestCameraListUpdatedSinceIsTheRecordersPoll(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	// Before the edit: nothing changed after "now".
	if rows, err := st.Cameras.ListUpdatedSince(time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("list updated: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("expected no changes in the future, got %d", len(rows))
	}

	poll := time.Now().UTC().Add(-time.Second)
	if rows, err := st.Cameras.ListUpdatedSince(poll); err != nil {
		t.Fatalf("list updated: %v", err)
	} else if len(rows) != 1 {
		t.Fatalf("a camera written a moment ago must appear in the poll, got %d rows", len(rows))
	}

	// A zone edit bumps updated_at, which is how the recorder notices it.
	if err := st.Cameras.UpdateZones(camera.ID, "[]"); err != nil {
		t.Fatalf("update zones: %v", err)
	}
	rows, err := st.Cameras.ListUpdatedSince(poll)
	if err != nil {
		t.Fatalf("list updated: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("a zone write must be visible to the config poll, got %d rows", len(rows))
	}
}

func TestCameraDelete(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	if err := st.Cameras.Delete(camera.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.Cameras.Delete(camera.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("deleting twice must answer not-found, got %v", err)
	}

	total, detecting, err := st.Cameras.Count()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 0 || detecting != 0 {
		t.Fatalf("expected the camera gone from both counts, got %d/%d", total, detecting)
	}

	// The slug is free again: the stream name is the slug, and a deleted
	// camera's slug reappearing is a normal deployment move.
	if err := st.Cameras.Create(&models.Camera{
		Slug: "front-door", Name: "Replacement", SourceURI: "rtsp://new", RecordMode: models.RecordModeContinuous,
	}); err != nil {
		t.Fatalf("re-using a deleted slug: %v", err)
	}
}

func TestCameraEffectiveRetention(t *testing.T) {
	// Zero on the camera row means "not set", so the installation default
	// answers; a camera that set its own number is honoured exactly.
	blank := &models.Camera{}
	if got := blank.EffectiveLocalRetention(14); got != 14 {
		t.Fatalf("expected the default 14, got %d", got)
	}
	if got := blank.EffectiveRemoteRetention(30); got != 30 {
		t.Fatalf("expected the default 30, got %d", got)
	}

	set := &models.Camera{LocalRetentionDays: 3, RemoteRetentionDays: 0}
	if got := set.EffectiveLocalRetention(14); got != 3 {
		t.Fatalf("expected the camera's 3, got %d", got)
	}
	if got := set.EffectiveRemoteRetention(0); got != 0 {
		t.Fatalf("a camera with no remote window must take the installation's 0 (keep forever), got %d", got)
	}
}
