package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

// The methods the recorder, the clip builder, the uploader, the delivery loop
// and the auth handlers call, exercised directly. The components that call them
// land in Steps 2-7, and a method no test touches is a method that fails in the
// component — where the failure is harder to see.

func TestCameraTouchIsVisibleToTheWorkerConfigPoll(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	before, err := st.Cameras.GetByID(camera.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// The worker polls cameras on updated_at: it asks "which cameras changed" and
	// re-arms only those. Touch is how a change the store made without writing a
	// camera row becomes visible — a zone edit does it itself, and a future edit
	// that forgets to is a camera that keeps running the old rules.
	if err := st.Cameras.Touch(camera.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	after, err := st.Cameras.GetByID(camera.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("expected updated_at to move: before %v after %v", before.UpdatedAt, after.UpdatedAt)
	}

	seen, err := st.Cameras.ListUpdatedSince(before.UpdatedAt)
	if err != nil {
		t.Fatalf("list updated: %v", err)
	}
	if len(seen) != 1 || seen[0].ID != camera.ID {
		t.Fatalf("expected the touched camera in the poll's answer, got %+v", seen)
	}

	// A touch naming a camera that is not there is not a silent success: it is a
	// worker pointed at a camera an operator deleted, and the internal API should
	// answer 404.
	if err := st.Cameras.Touch("2f7a2a2a-0000-0000-0000-000000000000"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestEventLookupByEventKey(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	key := "cam:person:porch"
	event, created, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: key, CameraID: camera.ID, Action: models.SignalActionOpen,
		Severity: 2, Classes: []string{"person"}, TS: minutesAgo(1),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !created {
		t.Fatal("expected a created event")
	}

	// The event key is the worker's identity for the signal; the clip builder
	// needs the row from it.
	byKey, err := st.Events.GetByKey(key)
	if err != nil {
		t.Fatalf("get by key: %v", err)
	}
	if byKey.ID != event.ID {
		t.Fatal("expected the same row")
	}

	if _, err := st.Events.GetByKey("cam:cat:porch"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found for a key with no event, got %v", err)
	}
}

func TestClipBuilderWritesTheLocalPathsItProduced(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	event, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: "cam:car:drive", CameraID: camera.ID, Action: models.SignalActionOpen,
		Severity: 2, LocalSnapshotPath: "/var/lib/nvr/snapshots/e.jpg", TS: minutesAgo(2),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if event.LocalSnapshotPath != "/var/lib/nvr/snapshots/e.jpg" {
		t.Fatalf("an open signal carries the snapshot the handler just wrote, got %q", event.LocalSnapshotPath)
	}

	// The clip's duration is the measured duration, and the API tells the player
	// that number, not the requested window.
	if err := st.Events.SetLocalClip(event.ID, "/var/lib/nvr/clips/e.mp4", 23.5); err != nil {
		t.Fatalf("set clip: %v", err)
	}
	loaded, err := st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.LocalClipPath != "/var/lib/nvr/clips/e.mp4" || loaded.ClipSeconds != 23.5 {
		t.Fatalf("expected the clip path and its measured length, got %+v", loaded)
	}

	if err := st.Events.SetLocalSnapshot(event.ID, "/var/lib/nvr/snapshots/e2.jpg"); err != nil {
		t.Fatalf("set snapshot: %v", err)
	}
	loaded, err = st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.LocalSnapshotPath != "/var/lib/nvr/snapshots/e2.jpg" {
		t.Fatalf("expected the re-taken snapshot path, got %q", loaded.LocalSnapshotPath)
	}

	// A stamp naming a row that is not there is a 404 at the internal API, not a
	// success.
	for _, id := range []string{"2f7a2a2a-0000-0000-0000-000000000000"} {
		if err := st.Events.SetLocalClip(id, "/x", 1); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("expected not-found, got %v", err)
		}
		if err := st.Events.SetLocalSnapshot(id, "/x"); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("expected not-found, got %v", err)
		}
		if err := st.Events.SetClipAbsent(id); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("expected not-found, got %v", err)
		}
		if err := st.Events.MarkClipUploaded(id, "k"); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("expected not-found, got %v", err)
		}
		if err := st.Events.MarkSnapshotUploaded(id, "k"); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("expected not-found, got %v", err)
		}
	}
}

func TestUploadedStampsLandOnTheEvent(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	event, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: "cam:person:porch", CameraID: camera.ID, Action: models.SignalActionOpen,
		Severity: 2, TS: minutesAgo(2),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if event.SnapshotUploaded() || event.ClipUploaded() {
		t.Fatal("nothing is uploaded yet, and the sweeper must not delete un-uploaded media")
	}

	if err := st.Events.MarkSnapshotUploaded(event.ID, "nvr/events/e/snapshot.jpg"); err != nil {
		t.Fatalf("mark snapshot: %v", err)
	}
	if err := st.Events.MarkClipUploaded(event.ID, "nvr/events/e/clip.mp4"); err != nil {
		t.Fatalf("mark clip: %v", err)
	}
	loaded, err := st.Events.GetByID(event.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !loaded.SnapshotUploaded() || !loaded.ClipUploaded() {
		t.Fatalf("expected both upload stamps, got %+v", loaded)
	}
	if loaded.SnapshotKey != "nvr/events/e/snapshot.jpg" || loaded.ClipKey != "nvr/events/e/clip.mp4" {
		t.Fatalf("the remote keys are what the sweeper deletes, got %+v", loaded)
	}
}

func TestSegmentsListRecentByCameraIsTheRestartManifest(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	for i := 0; i < 3; i++ {
		seg := &models.Segment{
			CameraID: camera.ID, Generation: 1, Kind: models.SegmentKindContinuous,
			Path:    "/var/lib/nvr/segments/" + camera.Slug + "-1-" + itoa(i+1) + ".mp4",
			StartTS: minutesAgo(30 - 10*i), EndTS: minutesAgo(25 - 10*i), Seconds: 5,
		}
		if err := st.Segments.Create(seg); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	other := seedCamera(t, st, "side-gate")
	if err := st.Segments.Create(&models.Segment{
		CameraID: other.ID, Generation: 1, Kind: models.SegmentKindContinuous,
		Path: "/var/lib/nvr/segments/other.mp4", StartTS: minutesAgo(5), EndTS: minutesAgo(0), Seconds: 5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The recorder supervisor reads the camera's recent segments to continue
	// numbering after a restart, so the query is per camera, newest-first, bounded.
	recent, err := st.Segments.ListRecentByCamera(camera.ID, 2)
	if err != nil {
		t.Fatalf("list recent: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("expected the limit honoured, got %d", len(recent))
	}
	for _, seg := range recent {
		if seg.CameraID != camera.ID {
			t.Fatal("expected the query to stay scoped to the camera")
		}
	}
	if recent[0].StartTS.Before(recent[1].StartTS) {
		t.Fatal("expected newest-first, so the supervisor reads the tail of the run")
	}

	all, err := st.Segments.ListRecentByCamera(camera.ID, 10)
	if err != nil {
		t.Fatalf("list recent: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected the camera's three segments, got %d", len(all))
	}
}

func TestSegmentsRangeEdges(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	if err := st.Segments.Create(&models.Segment{
		CameraID: camera.ID, Generation: 1, Kind: models.SegmentKindContinuous,
		Path: "/var/lib/nvr/segments/1.mp4", StartTS: minutesAgo(60), EndTS: minutesAgo(55), Seconds: 5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The timeline's query is "what overlaps this window": a window ending at a
	// segment's end touches it, a window starting at its end does not.
	hits, err := st.Segments.ListRange(camera.ID, minutesAgo(70), minutesAgo(55), 0)
	if err != nil {
		t.Fatalf("list range: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected the segment inside the window, got %d", len(hits))
	}
	misses, err := st.Segments.ListRange(camera.ID, minutesAgo(55), minutesAgo(50), 0)
	if err != nil {
		t.Fatalf("list range: %v", err)
	}
	if len(misses) != 0 {
		t.Fatalf("expected the window that starts at the segment's end to be empty, got %d", len(misses))
	}
	if _, err := st.Segments.GetByID("2f7a2a2a-0000-0000-0000-000000000000"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestListAllAnswersTheSettingsPages(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	// The settings pages list the disabled rows too: an operator who parked a
	// backend, an endpoint and a provider has to see all three.
	if err := st.StorageBackends.Create(&models.StorageBackend{
		Name: "tier-one", Kind: models.StorageKindFS, RootPath: "/mnt/nas", Enabled: true,
	}); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	if err := st.StorageBackends.Create(&models.StorageBackend{
		Name: "parked", Kind: models.StorageKindS3, Bucket: "b", Enabled: false,
	}); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	backends, err := st.StorageBackends.ListAll()
	if err != nil {
		t.Fatalf("list backends: %v", err)
	}
	if len(backends) != 2 {
		t.Fatalf("expected both backends, got %d", len(backends))
	}

	if err := st.Webhooks.Create(&models.Webhook{
		Name: "oncall", URL: "https://hooks.example/nvr", Enabled: true,
	}); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	if err := st.Webhooks.Create(&models.Webhook{
		Name: "parked", URL: "https://hooks.example/parked", Enabled: false,
	}); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	hooks, err := st.Webhooks.ListAll()
	if err != nil {
		t.Fatalf("list webhooks: %v", err)
	}
	if len(hooks) != 2 {
		t.Fatalf("expected both endpoints, got %d", len(hooks))
	}

	if err := st.SSOProviders.Create(&models.SSOProvider{
		Name: "keycloak", ClientID: "nvr", Issuer: "https://id.example", Enabled: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	provider, err := st.SSOProviders.GetByName("keycloak")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	byID, err := st.SSOProviders.GetByID(provider.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if byID.Name != "keycloak" {
		t.Fatalf("expected the same row, got %+v", byID)
	}
	if _, err := st.SSOProviders.GetByID("2f7a2a2a-0000-0000-0000-000000000000"); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("expected the provider store's not-found, got %v", err)
	}

	users, err := st.Users.ListAll()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 0 {
		t.Fatalf("a fresh installation has no accounts, got %d", len(users))
	}
	u := &models.User{Username: "watcher", Email: "w@example", PasswordHash: "argon2id$x", Status: models.UserStatusActive}
	if err := st.Users.Create(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	users, err = st.Users.ListAll()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 || users[0].Username != "watcher" {
		t.Fatalf("expected the account, got %+v", users)
	}

	roles, err := st.Roles.ListRoles()
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	role, err := st.Roles.GetRoleByID(roles[0].ID)
	if err != nil {
		t.Fatalf("get role by id: %v", err)
	}
	if role.ID != roles[0].ID {
		t.Fatal("expected the same role")
	}
	if _, err := st.Roles.GetRoleByID("2f7a2a2a-0000-0000-0000-000000000000"); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("expected the role store's not-found, got %v", err)
	}

	_, hash, err := models.NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	session := &models.Session{
		UserID: u.ID, TokenHash: hash, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := st.Sessions.Create(session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	live, err := st.Sessions.ListLiveFor(u.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("expected the live session, got %d", len(live))
	}
	if _, err := st.Sessions.GetByID(session.ID); err != nil {
		t.Fatalf("get session: %v", err)
	}
	if _, err := st.Sessions.GetByID("2f7a2a2a-0000-0000-0000-000000000000"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}

	// A camera delete leaves nothing behind in the tables the console lists.
	if err := st.Cameras.Delete(camera.ID); err != nil {
		t.Fatalf("delete camera: %v", err)
	}
	if _, err := st.Cameras.GetByID(camera.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected the camera row gone, got %v", err)
	}
}

func TestPendingIdentityLookup(t *testing.T) {
	st := newTestStore(t)

	pending := &models.PendingIdentity{
		Provider:        "keycloak",
		ProviderSubject: "subject-1",
		Email:           "ops@example",
		Status:          models.IdentityStatusPending,
	}
	if err := st.PendingIdentities.Create(pending); err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err := st.PendingIdentities.GetByID(pending.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !loaded.IsPending() {
		t.Fatal("expected the parked assertion to read as pending")
	}
	if _, err := st.PendingIdentities.GetByID("2f7a2a2a-0000-0000-0000-000000000000"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestStoreErrorsSurfaceAsErrors(t *testing.T) {
	// The database is missing tables: an installation whose migration failed
	// partway, or a data directory pointed at an older release's database. Every
	// read must answer an error the handler turns into a 500, and no call may
	// panic. The console's "500 on the cameras page" is then traceable, not a
	// mystery.
	st := newTestStore(t)
	for _, model := range []any{
		&models.Camera{}, &models.Segment{}, &models.Event{}, &models.Webhook{},
		&models.User{}, &models.Session{}, &models.Setting{}, &models.Audit{},
	} {
		if err := st.DB.Migrator().DropTable(model); err != nil {
			t.Fatalf("drop %T: %v", model, err)
		}
	}

	checks := []struct {
		name string
		call func() error
	}{
		{"cameras list", func() error { _, err := st.Cameras.ListAll(); return err }},
		{"cameras active", func() error { _, err := st.Cameras.ListActive(); return err }},
		{"cameras detection", func() error { _, err := st.Cameras.ListDetectionEnabled(); return err }},
		{"cameras count", func() error { _, _, err := st.Cameras.Count(); return err }},
		{"cameras get", func() error { _, err := st.Cameras.GetByID("x"); return err }},
		{"camera create", func() error { return st.Cameras.Create(&models.Camera{Slug: "x", Name: "x"}) }},
		{"camera touch", func() error { return st.Cameras.Touch("x") }},
		{"camera delete", func() error { return st.Cameras.Delete("x") }},
		{"segments list", func() error { _, err := st.Segments.ListRange("x", time.Now(), time.Now(), 0); return err }},
		{"segments recent", func() error { _, err := st.Segments.ListRecentByCamera("x", 5); return err }},
		{"segments count", func() error { _, _, err := st.Segments.CountByCamera("x"); return err }},
		{"segments max number", func() error { _, err := st.Segments.MaxSegmentNumber("x", 1); return err }},
		{"segments create", func() error {
			return st.Segments.Create(&models.Segment{CameraID: "x", Generation: 1, Path: "/x", StartTS: time.Now(), EndTS: time.Now()})
		}},
		{"events list", func() error { _, err := st.Events.List(EventFilter{}, 0); return err }},
		{"events open", func() error { _, err := st.Events.ListOpen(); return err }},
		{"event upsert", func() error {
			_, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{EventKey: "k", CameraID: "x", Action: models.SignalActionOpen, TS: time.Now()})
			return err
		}},
		{"webhooks list", func() error { _, err := st.Webhooks.ListAll(); return err }},
		{"users list", func() error { _, err := st.Users.ListAll(); return err }},
		{"sessions list", func() error { _, err := st.Sessions.ListLiveFor("x", time.Now()); return err }},
		{"settings get", func() error { _, err := st.Settings.Get("k"); return err }},
		{"settings set", func() error { return st.Settings.Set("k", "v") }},
		{"audit list", func() error { _, err := st.Audit.List(AuditFilter{}); return err }},
		{"audit count", func() error { _, err := st.Audit.Count(); return err }},
		{"audit record", func() error {
			return st.Audit.Record(&models.Audit{Action: models.AuditCameraCreate, Outcome: models.AuditOutcomeOK})
		}},
	}
	for _, check := range checks {
		if err := check.call(); err == nil {
			t.Fatalf("%s: a missing table must answer an error", check.name)
		}
	}
}

func TestEventSignalActionIsCheckedAtTheStore(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")

	// The internal API's action whitelist is enforced where the row is written,
	// so a worker that invents an action cannot reach the state machine.
	if _, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: "cam:person:porch", CameraID: camera.ID, Action: "expire", TS: time.Now().UTC(),
	}); err == nil {
		t.Fatal("expected an unknown signal action to be refused")
	}
	if _, _, err := st.Events.UpsertByEventKey(ctx(), EventInput{
		EventKey: "cam:person:porch", Action: models.SignalActionOpen,
	}); err == nil {
		t.Fatal("expected a signal naming no camera to be refused")
	}
}
