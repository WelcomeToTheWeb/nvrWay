package sweeper

// The sweeper is the only thing in this installation that destroys footage, so
// these tests are mostly about what it refuses to destroy.
//
// The clock is the real one and every age is expressed relative to it, because
// the store stamps `created_at`/`updated_at` itself: an injected clock would make
// the prune sweeps (which read those columns) test a time the database never
// saw. What matters here is the *age* of a row, and `time.Now().Add(-48h)` says
// that exactly.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"nvr/server/models"
	"nvr/server/store"
)

// fixture is a media tree, a database, and a sweeper pointed at both.
type fixture struct {
	st  *store.Store
	dir string // the media tree: segments, clips and snapshots live here

	backend *models.StorageBackend
	camera  *models.Camera

	logs    *captureHandler
	reports []Report
	sweeper *Sweeper
	cfg     Config
}

func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()

	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := st.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	dir := t.TempDir()
	backend := &models.StorageBackend{
		Name: "tier", Kind: models.StorageKindFS, RootPath: t.TempDir(),
		Enabled: true, IsDefault: true,
	}
	if err := st.StorageBackends.Create(backend); err != nil {
		t.Fatalf("creating the backend: %v", err)
	}

	camera := &models.Camera{
		Slug: "front-door", Name: "Front door", SourceURI: "rtsp://cam/live",
	}
	if err := st.Cameras.Create(camera); err != nil {
		t.Fatalf("creating the camera: %v", err)
	}

	if cfg.LocalRetentionDays == 0 {
		cfg.LocalRetentionDays = 14
	}
	if cfg.EventRetentionDays == 0 {
		cfg.EventRetentionDays = 90
	}

	f := &fixture{st: st, dir: dir, backend: backend, camera: camera, cfg: cfg}
	f.logs = &captureHandler{}
	cfg.Log = slog.New(f.logs)

	f.sweeper = New(st, cfg, func(payload any) error {
		if rep, ok := payload.(Report); ok {
			f.reports = append(f.reports, rep)
		}
		return nil
	})
	return f
}

// segment seeds one segment row, with a real file when it is meant to be local.
func (f *fixture) segment(t *testing.T, age time.Duration, kind string, uploaded bool) *models.Segment {
	t.Helper()

	start := time.Now().UTC().Add(-age)
	seg := &models.Segment{
		CameraID: f.camera.ID, Generation: 1,
		Path:    filepath.Join(f.dir, fmt.Sprintf("front-door-1-%d.mp4", time.Now().UnixNano())),
		StartTS: start, EndTS: start.Add(5 * time.Second),
		Seconds: 5, Bytes: 1024, Kind: kind,
	}
	if uploaded {
		now := time.Now().UTC()
		seg.UploadedAt = &now
		seg.RemoteKey = "nvr/front-door/segment.mp4"
	}
	if err := f.st.Segments.Create(seg); err != nil {
		t.Fatalf("creating the segment: %v", err)
	}
	if err := os.WriteFile(seg.Path, []byte("mp4"), 0o644); err != nil {
		t.Fatalf("writing the segment: %v", err)
	}
	return seg
}

// event seeds one event row with its local media and its remote keys.
func (f *fixture) event(t *testing.T, age time.Duration, withMedia bool) *models.Event {
	t.Helper()

	started := time.Now().UTC().Add(-age)
	ev := &models.Event{
		// A direct DB.Create does not generate the id the way the store's
		// upsert does, and an empty primary key turns the fixture's later Save
		// into a second insert.
		ID:       uuid.NewString(),
		EventKey: fmt.Sprintf("%s:zone:person:%d", f.camera.ID, time.Now().UnixNano()),
		CameraID: f.camera.ID, Action: "open",
		StartedAt: started, EndedAt: started.Add(20 * time.Second),
		Open: false, Severity: 2,
	}
	if withMedia {
		ev.LocalClipPath = filepath.Join(f.dir, "clip.mp4")
		ev.LocalSnapshotPath = filepath.Join(f.dir, "snapshot.jpg")
		ev.ClipKey = "nvr/events/" + "placeholder" + "/clip.mp4"
		ev.SnapshotKey = "nvr/events/" + "placeholder" + "/snapshot.jpg"
		now := time.Now().UTC()
		ev.ClipUploadedAt, ev.SnapshotUploadedAt = &now, &now
	}
	if err := f.st.DB.Create(ev).Error; err != nil {
		t.Fatalf("creating the event: %v", err)
	}
	// The keys name the event, which is only known after the row exists.
	if withMedia {
		ev.ClipKey = "nvr/events/" + ev.ID + "/clip.mp4"
		ev.SnapshotKey = "nvr/events/" + ev.ID + "/snapshot.jpg"
		if err := f.st.DB.Save(ev).Error; err != nil {
			t.Fatalf("filing the event's remote keys: %v", err)
		}
		for _, path := range []string{ev.LocalClipPath, ev.LocalSnapshotPath} {
			if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
				t.Fatalf("writing the event media: %v", err)
			}
		}
	}
	return ev
}

// age moves a row's own timestamp into the past, for the sweeps that read
// `created_at`/`updated_at` rather than the media's start.
func (f *fixture) age(t *testing.T, model any, column string, id string, age time.Duration) {
	t.Helper()
	past := time.Now().UTC().Add(-age)
	if err := f.st.DB.Model(model).Where("id = ?", id).Update(column, past).Error; err != nil {
		t.Fatalf("aging %T: %v", model, err)
	}
}

func (f *fixture) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (f *fixture) segmentRow(t *testing.T, id string) *models.Segment {
	t.Helper()
	seg, err := f.st.Segments.GetByID(id)
	if err != nil {
		return nil
	}
	return seg
}

func (f *fixture) jobs(t *testing.T) []models.StorageJob {
	t.Helper()
	var jobs []models.StorageJob
	if err := f.st.DB.Order("created_at ASC").Find(&jobs).Error; err != nil {
		t.Fatalf("reading the queue: %v", err)
	}
	return jobs
}

// TestUnuploadedFootageSurvivesTheWindowAndIsForceDeletedAfterIt is the card's
// first criterion, with all three cases in one sweep: footage that reached the
// tier goes on its camera's window, footage that did not is protected, and
// footage that did not and has been waiting past the protection window goes —
// counted.
func TestUnuploadedFootageSurvivesTheWindowAndIsForceDeletedAfterIt(t *testing.T) {
	// A one-day local window makes the protection window the 7-day floor:
	// max(3 * 1 day, 7 days).
	f := newFixture(t, Config{LocalRetentionDays: 1})

	uploaded := f.segment(t, 8*24*time.Hour, models.SegmentKindContinuous, true)
	protected := f.segment(t, 5*24*time.Hour, models.SegmentKindContinuous, false)
	stale := f.segment(t, 9*24*time.Hour, models.SegmentKindContinuous, false)

	rep := f.sweeper.Sweep(context.Background())

	if f.exists(uploaded.Path) {
		t.Error("footage that reached the tier is still local past its window")
	}
	// The row stays: the tier still holds the footage, and the row is the only
	// index to it. Deleting the row with the file would orphan the object.
	got := f.segmentRow(t, uploaded.ID)
	if got == nil {
		t.Error("the uploaded segment's row went with its local file, while the tier still holds it")
	} else {
		if got.RemoteKey == "" {
			t.Error("the row forgot where the tier holds the footage")
		}
		if got.LocalDeletedAt == nil {
			t.Error("the row says the local file is still here, and it is not")
		}
	}
	if !f.exists(protected.Path) {
		t.Error("un-uploaded footage 5 days old was destroyed inside a 7-day protection window")
	}
	if f.segmentRow(t, protected.ID) == nil {
		t.Error("the protected segment's row went with a file that did not")
	}
	if f.exists(stale.Path) {
		t.Error("un-uploaded footage past the protection window was kept")
	}

	if rep.ForceDeleted != 1 {
		t.Errorf("ForceDeleted = %d, want 1: the force-delete has to be counted", rep.ForceDeleted)
	}
	if rep.LocalDeleted != 1 {
		t.Errorf("LocalDeleted = %d, want 1 local file reclaimed", rep.LocalDeleted)
	}
	if rep.SegmentRowsDeleted != 1 {
		t.Errorf("SegmentRowsDeleted = %d, want 1: only the force-deleted footage exists nowhere", rep.SegmentRowsDeleted)
	}
}

func TestProtectionWindowIsThreeTimesRetentionWithASevenDayFloor(t *testing.T) {
	f := newFixture(t, Config{LocalRetentionDays: 14})
	cases := []struct {
		local time.Duration
		want  time.Duration
	}{
		{14 * 24 * time.Hour, 42 * 24 * time.Hour},
		{1 * 24 * time.Hour, 7 * 24 * time.Hour},
		{2 * 24 * time.Hour, 7 * 24 * time.Hour},
		{3 * 24 * time.Hour, 9 * 24 * time.Hour},
		{30 * 24 * time.Hour, 90 * 24 * time.Hour},
	}
	for _, tc := range cases {
		got := f.sweeper.protectionWindow(tc.local)
		if got != tc.want {
			t.Errorf("protectionWindow(%v) = %v, want %v", tc.local, got, tc.want)
		}
	}
}

func TestPerCameraRetentionBeatsTheInstallationDefault(t *testing.T) {
	// The installation keeps 14 days; this camera keeps one day. The candidate
	// query runs at the loosest window in the installation and each row is then
	// checked against its own camera's, so both numbers have to be honoured.
	f := newFixture(t, Config{LocalRetentionDays: 14})

	tight := &models.Camera{
		Slug: "porch", Name: "Porch", SourceURI: "rtsp://porch/live",
		LocalRetentionDays: 1,
	}
	if err := f.st.Cameras.Create(tight); err != nil {
		t.Fatalf("creating the second camera: %v", err)
	}

	start := time.Now().UTC().Add(-3 * 24 * time.Hour)
	seg := &models.Segment{
		CameraID: tight.ID, Generation: 1, Path: filepath.Join(f.dir, "porch-1-1.mp4"),
		StartTS: start, EndTS: start.Add(5 * time.Second), Seconds: 5, Bytes: 10,
		Kind: models.SegmentKindContinuous,
	}
	now := time.Now().UTC()
	seg.UploadedAt, seg.RemoteKey = &now, "nvr/porch/porch-1-1.mp4"
	if err := f.st.Segments.Create(seg); err != nil {
		t.Fatalf("creating the segment: %v", err)
	}
	if err := os.WriteFile(seg.Path, []byte("mp4"), 0o644); err != nil {
		t.Fatal(err)
	}

	kept := f.segment(t, 3*24*time.Hour, models.SegmentKindContinuous, true)

	f.sweeper.Sweep(context.Background())

	if f.exists(seg.Path) {
		t.Error("a camera with a one-day window kept three days of footage")
	}
	if !f.exists(kept.Path) {
		t.Error("the installation default camera lost footage a day early")
	}
}

func TestRemoteDeleteOnlyPastRemoteRetention(t *testing.T) {
	f := newFixture(t, Config{RemoteRetentionDays: 7})

	old := f.segment(t, 10*24*time.Hour, models.SegmentKindContinuous, true)
	// Its local file is already gone, which is the state a row is in once the
	// local sweep has run: the tier copy is all that is left.
	if err := os.Remove(old.Path); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Segments.MarkLocalDeleted(old.ID); err != nil {
		t.Fatal(err)
	}
	young := f.segment(t, 2*24*time.Hour, models.SegmentKindContinuous, true)

	rep := f.sweeper.Sweep(context.Background())

	if rep.RemoteQueued != 1 {
		t.Fatalf("RemoteQueued = %d, want 1", rep.RemoteQueued)
	}
	jobs := f.jobs(t)
	if len(jobs) != 1 {
		t.Fatalf("the queue holds %d jobs, want the one purge", len(jobs))
	}
	if jobs[0].Action != models.JobActionDeleteRemote || jobs[0].Target != old.RemoteKey {
		t.Errorf("the queued job is %s %q, want a delete_remote of %q",
			jobs[0].Action, jobs[0].Target, old.RemoteKey)
	}

	// The row that was queued forgets where the tier held it; the young one does
	// not, and neither loses its `uploaded_at`.
	if got := f.segmentRow(t, old.ID); got == nil || got.RemoteKey != "" {
		t.Errorf("the swept row is %+v, want it kept with remote_key cleared", got)
	}
	if got := f.segmentRow(t, old.ID); got != nil && got.UploadedAt == nil {
		t.Error("clearing the remote key also cleared the fact that it reached the tier")
	}
	if got := f.segmentRow(t, young.ID); got == nil || got.RemoteKey == "" {
		t.Errorf("footage inside the remote window lost its key: %+v", got)
	}
}

func TestRemoteRetentionZeroKeepsForever(t *testing.T) {
	// Zero is a real value, not an absent one. The sweep that reads it as "no
	// window configured" deletes the tier.
	f := newFixture(t, Config{RemoteRetentionDays: 0})

	ancient := f.segment(t, 400*24*time.Hour, models.SegmentKindContinuous, true)
	if err := os.Remove(ancient.Path); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Segments.MarkLocalDeleted(ancient.ID); err != nil {
		t.Fatal(err)
	}

	rep := f.sweeper.Sweep(context.Background())

	if rep.RemoteQueued != 0 {
		t.Errorf("RemoteQueued = %d with REMOTE_RETENTION_DAYS=0, want 0", rep.RemoteQueued)
	}
	if len(f.jobs(t)) != 0 {
		t.Errorf("a keep-forever installation queued %d remote deletes", len(f.jobs(t)))
	}
	got := f.segmentRow(t, ancient.ID)
	if got == nil {
		t.Fatal("keep-forever deleted the row that remembered the tier copy")
	}
	if got.RemoteKey == "" {
		t.Error("keep-forever cleared the remote key")
	}
}

func TestARowOutlivesTheSweepThatGaveAwayItsLastCopy(t *testing.T) {
	// The seven sweeps run in the plan's order, and this is why that order is
	// not cosmetic: the remote sweep clears the key as it queues, and the local
	// sweep refuses to delete a row that still has one. A row whose last copy was
	// queued for deletion in this pass goes in the *next* one, because the delete
	// job has not run yet and a row deleted now would orphan the object if it
	// failed.
	f := newFixture(t, Config{LocalRetentionDays: 1, RemoteRetentionDays: 1, EventRetentionDays: 1})

	seg := f.segment(t, 3*24*time.Hour, models.SegmentKindContinuous, true)
	if err := os.Remove(seg.Path); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Segments.MarkLocalDeleted(seg.ID); err != nil {
		t.Fatal(err)
	}

	first := f.sweeper.Sweep(context.Background())
	if first.RemoteQueued != 1 {
		t.Fatalf("the first sweep queued %d remote deletes, want 1", first.RemoteQueued)
	}
	if f.segmentRow(t, seg.ID) == nil {
		t.Fatal("the row went in the same sweep that gave away its last copy")
	}

	second := f.sweeper.Sweep(context.Background())
	if f.segmentRow(t, seg.ID) != nil {
		t.Error("the row survived the sweep after its last copy was queued")
	}
	if second.SegmentRowsDeleted != 1 {
		t.Errorf("the second sweep reported SegmentRowsDeleted=%d, want 1", second.SegmentRowsDeleted)
	}
}

func TestEventSweepPurgesMediaBeforeDeletingTheRow(t *testing.T) {
	f := newFixture(t, Config{EventRetentionDays: 30})

	ev := f.event(t, 45*24*time.Hour, true)
	clipKey, snapshotKey := ev.ClipKey, ev.SnapshotKey

	rep := f.sweeper.Sweep(context.Background())

	if rep.EventsDeleted != 1 {
		t.Errorf("EventsDeleted = %d, want 1", rep.EventsDeleted)
	}
	if rep.EventMediaPurged != 2 {
		t.Errorf("EventMediaPurged = %d, want 2 (the clip and the snapshot)", rep.EventMediaPurged)
	}
	if _, err := f.st.Events.GetByID(ev.ID); err == nil {
		t.Error("the event row survived its retention")
	}

	// Two jobs, two ids: a job row is keyed by its object and an event has two,
	// so a clip purge that replaced a snapshot purge would leave one object on
	// the tier forever.
	jobs := f.jobs(t)
	if len(jobs) != 2 {
		t.Fatalf("the queue holds %d jobs, want the two purges", len(jobs))
	}
	wantIDs := map[string]string{
		ev.ID + ":clip":     clipKey,
		ev.ID + ":snapshot": snapshotKey,
	}
	for _, job := range jobs {
		wantKey, ok := wantIDs[job.ID]
		if !ok {
			t.Errorf("the queue holds a job for %q, want the clip and the snapshot", job.ID)
			continue
		}
		if job.Action != models.JobActionDeleteRemote || job.Target != wantKey {
			t.Errorf("job %s is %s %q, want a delete_remote of %q",
				job.ID, job.Action, job.Target, wantKey)
		}
	}
}

func TestEventKindSegmentsAreRetiredWithEventRetention(t *testing.T) {
	// `record_mode=events` writes segments of kind `event`, which appear in no
	// other retention list: without this sweep their rows and files only ever
	// grow.
	f := newFixture(t, Config{EventRetentionDays: 30, LocalRetentionDays: 365})

	eventSeg := f.segment(t, 45*24*time.Hour, models.SegmentKindEvent, false)
	young := f.segment(t, 5*24*time.Hour, models.SegmentKindEvent, false)

	rep := f.sweeper.Sweep(context.Background())

	if rep.EventSegmentsDeleted != 1 {
		t.Errorf("EventSegmentsDeleted = %d, want 1", rep.EventSegmentsDeleted)
	}
	if f.exists(eventSeg.Path) {
		t.Error("an event segment past event retention kept its file")
	}
	if f.segmentRow(t, eventSeg.ID) != nil {
		t.Error("an event segment past event retention kept its row")
	}
	if !f.exists(young.Path) {
		t.Error("an event segment inside event retention lost its file")
	}
}

func TestALiveEventKeepsTheFootageItsClipIsBuiltFrom(t *testing.T) {
	// The reference is by time, not by a foreign key, so the sweeper asks the
	// same question the clip builder answers with: does any live event's window,
	// widened by its roll margins, overlap this segment?
	f := newFixture(t, Config{LocalRetentionDays: 1, PreRollSeconds: 8, PostRollSeconds: 15})

	seg := f.segment(t, 3*24*time.Hour, models.SegmentKindContinuous, true)
	f.event(t, 3*24*time.Hour, false)

	f.sweeper.Sweep(context.Background())

	if !f.exists(seg.Path) {
		t.Error("the sweeper deleted footage a live event can still rebuild a clip from")
	}
	got := f.segmentRow(t, seg.ID)
	if got == nil {
		t.Fatal("the referenced segment's row went too")
	}
	if got.LocalDeletedAt != nil {
		t.Error("the row claims its file is gone, and the file is still here")
	}
}

func TestHousekeepingPrunesDeliveriesAuditAndQueue(t *testing.T) {
	f := newFixture(t, Config{})

	delivery := &models.WebhookDelivery{
		WebhookID: "00000000-0000-4000-8000-000000000001",
		Event:     "event.new", Payload: "{}", Status: models.DeliveryStatusDelivered,
	}
	if err := f.st.WebhookDeliveries.Create(delivery); err != nil {
		t.Fatalf("creating the delivery: %v", err)
	}
	f.age(t, &models.WebhookDelivery{}, "created_at", delivery.ID, 45*24*time.Hour)

	audit := &models.Audit{Action: "camera.create", TargetType: "camera", TargetID: "c1"}
	if err := f.st.Audit.Record(audit); err != nil {
		t.Fatalf("recording the audit row: %v", err)
	}
	f.age(t, &models.Audit{}, "created_at", audit.ID, 400*24*time.Hour)

	seg := f.segment(t, 2*24*time.Hour, models.SegmentKindContinuous, true)
	if err := f.st.StorageJobs.Enqueue(store.JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: f.backend.ID, Action: models.JobActionUpload,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := f.st.StorageJobs.MarkUploaded(seg.ID, seg.RemoteKey, seg.Bytes); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}
	f.age(t, &models.StorageJob{}, "updated_at", seg.ID, 45*24*time.Hour)

	rep := f.sweeper.Sweep(context.Background())

	if rep.WebhookDeliveriesPruned != 1 {
		t.Errorf("WebhookDeliveriesPruned = %d, want 1", rep.WebhookDeliveriesPruned)
	}
	if rep.AuditPruned != 1 {
		t.Errorf("AuditPruned = %d, want 1", rep.AuditPruned)
	}
	if rep.JobsPruned != 1 {
		t.Errorf("JobsPruned = %d, want 1", rep.JobsPruned)
	}
}

func TestStuckClaimsAreReleasedBeforeTheSweepsLook(t *testing.T) {
	f := newFixture(t, Config{StuckAfter: 15 * time.Minute})

	seg := f.segment(t, 2*24*time.Hour, models.SegmentKindContinuous, true)
	if err := f.st.StorageJobs.Enqueue(store.JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: f.backend.ID, Action: models.JobActionUpload,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// A claim whose worker died: `uploading`, and not updated since.
	if err := f.st.DB.Model(&models.StorageJob{}).Where("id = ?", seg.ID).
		Updates(map[string]any{"state": models.JobStateUploading, "worker_id": "dead-worker"}).Error; err != nil {
		t.Fatalf("claiming: %v", err)
	}
	f.age(t, &models.StorageJob{}, "updated_at", seg.ID, time.Hour)

	rep := f.sweeper.Sweep(context.Background())

	if rep.StuckReleased != 1 {
		t.Errorf("StuckReleased = %d, want 1", rep.StuckReleased)
	}
	var job models.StorageJob
	if err := f.st.DB.First(&job, "id = ?", seg.ID).Error; err != nil {
		t.Fatalf("reading the job: %v", err)
	}
	if job.State != models.JobStatePending || job.WorkerID != "" {
		t.Errorf("the stuck claim is %q by %q, want it back in the queue", job.State, job.WorkerID)
	}
}

func TestSweepLogsOneSummaryLineAndBroadcastsTheCounts(t *testing.T) {
	// The card's third criterion. The operator sees disk discipline as a number,
	// so there is one line to read and one message to listen to, and both carry
	// the counts rather than a count of lines.
	f := newFixture(t, Config{LocalRetentionDays: 1})

	uploaded := f.segment(t, 8*24*time.Hour, models.SegmentKindContinuous, true)

	rep := f.sweeper.Sweep(context.Background())

	var summary *slog.Record
	for i := range f.logs.records {
		if strings.HasPrefix(f.logs.records[i].Message, "storage sweep") {
			summary = &f.logs.records[i]
		}
	}
	if summary == nil {
		t.Fatalf("the sweep logged %d records, none of them the summary: %v",
			len(f.logs.records), f.logs.messages())
	}

	want := []string{
		"local_deleted", "force_deleted", "event_segments_deleted", "segment_rows_deleted",
		"event_media_purged",
		"events_deleted", "remote_queued", "webhook_deliveries_pruned", "audit_pruned",
		"jobs_pruned", "stuck_released",
	}
	got := map[string]bool{}
	summary.Attrs(func(a slog.Attr) bool {
		got[a.Key] = true
		return true
	})
	for _, key := range want {
		if !got[key] {
			t.Errorf("the summary line has no %s: %v", key, f.logs.messages())
		}
	}
	if got["local_deleted"] {
		var found int64 = -1
		summary.Attrs(func(a slog.Attr) bool {
			if a.Key == "local_deleted" {
				found = a.Value.Int64()
			}
			return true
		})
		if found != 1 {
			t.Errorf("the summary line reports local_deleted=%d, want 1", found)
		}
	}

	// And the same numbers reach the console as one `storage.queue` payload.
	if len(f.reports) != 1 {
		t.Fatalf("the sweep broadcast %d messages, want 1", len(f.reports))
	}
	broadcast := f.reports[0]
	if broadcast.LocalDeleted != rep.LocalDeleted || broadcast.ForceDeleted != rep.ForceDeleted {
		t.Errorf("the broadcast counts (%+v) disagree with the sweep's (%+v)", broadcast, rep)
	}
	if broadcast.Queue == nil {
		t.Error("the storage.queue payload carries no queue counts, which is what the storage page shows")
	}
	if f.exists(uploaded.Path) {
		t.Error("the sweep did not run, so the counts it reports are meaningless")
	}
}

func TestSweepReportsAProblemInsteadOfGoingQuiet(t *testing.T) {
	// A sweep that could not read the tier has to say so: an empty report looks
	// exactly like a healthy installation with nothing to do.
	f := newFixture(t, Config{RemoteRetentionDays: 1})

	if err := f.st.DB.Model(&models.StorageBackend{}).Where("id = ?", f.backend.ID).
		Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	seg := f.segment(t, 3*24*time.Hour, models.SegmentKindContinuous, true)
	if err := os.Remove(seg.Path); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Segments.MarkLocalDeleted(seg.ID); err != nil {
		t.Fatal(err)
	}

	rep := f.sweeper.Sweep(context.Background())
	if rep.RemoteQueued != 0 {
		t.Errorf("a disabled backend queued %d remote deletes", rep.RemoteQueued)
	}
	// The row keeps its key: nothing queued the delete, so nothing may forget
	// where the object is.
	if got := f.segmentRow(t, seg.ID); got == nil || got.RemoteKey == "" {
		t.Errorf("the row lost its remote key without a delete being queued: %+v", got)
	}
}

func TestSweeperDefaultsAreThePlansNumbers(t *testing.T) {
	f := newFixture(t, Config{})
	if got := f.sweeper.Interval(); got != DefaultInterval {
		t.Errorf("the sweep interval is %v, want %v", got, DefaultInterval)
	}
	if f.sweeper.cfg.BatchLimit != DefaultBatchLimit {
		t.Errorf("the batch limit is %d, want %d", f.sweeper.cfg.BatchLimit, DefaultBatchLimit)
	}
	if f.sweeper.cfg.AuditRetentionDays != DefaultAuditRetentionDays {
		t.Errorf("audit retention is %d days, want %d", f.sweeper.cfg.AuditRetentionDays, DefaultAuditRetentionDays)
	}
	if f.sweeper.cfg.WebhookDeliveryRetentionDays != DefaultWebhookDeliveryRetentionDays {
		t.Errorf("webhook delivery retention is %d days, want %d",
			f.sweeper.cfg.WebhookDeliveryRetentionDays, DefaultWebhookDeliveryRetentionDays)
	}
	if f.sweeper.cfg.JobRetentionDays != DefaultJobRetentionDays {
		t.Errorf("queue retention is %d days, want %d", f.sweeper.cfg.JobRetentionDays, DefaultJobRetentionDays)
	}
}

// captureHandler keeps every record so a test can assert on the summary line
// rather than on a string an operator reads.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.records))
	for _, r := range h.records {
		out = append(out, r.Message)
	}
	return out
}
