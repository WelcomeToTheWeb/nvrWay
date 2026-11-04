package storage

// S7-08 — the queue worker: claim, upload, exponential retry, failed at 20,
// requeue, enable flag.
//
// The five exit criteria are all about what the *row* says afterwards, because
// the row is what the queue page and the playback proxy read:
//
//   - claim -> upload -> uploaded, with the layout's key on the row
//   - a failure produces an exponential next_attempt_at
//   - attempts >= 20 lands in failed, visible on ?state=failed and in storage.queue
//   - a disabled backend claims nothing; in-flight jobs finish
//   - the uploader never deletes local files
//
// The world is a real sqlite database, a real directory as the tier, and a real
// file as the segment. The only substituted part is the clock, which has to be
// injected or "next attempt is now + 10 minutes" is an assertion about a duration
// rather than an instant.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nvr/server/models"
	"nvr/server/store"
)

// uploaderFixture is the smallest world the worker can run in.
type uploaderFixture struct {
	st     *store.Store
	u      *Uploader
	tier   string // the backend's RootPath
	target string // the local file the queue is asked to upload
	row    *models.StorageBackend

	segmentID string
	start     time.Time

	// now is the clock the uploader reads. ClaimNext compares next_attempt_at
	// against the real clock, so the two are deliberately separate; see
	// releaseForRetry.
	now time.Time
}

const (
	uploaderTestSlug = "front-door"
	uploaderTestBody = "an mp4, or something standing in for one"
)

func newUploaderFixture(t *testing.T) *uploaderFixture {
	t.Helper()

	st := newStorageTestStore(t)

	tier := t.TempDir()
	segmentsDir := t.TempDir()
	target := filepath.Join(segmentsDir, uploaderTestSlug+"-0-7.mp4")
	if err := os.WriteFile(target, []byte(uploaderTestBody), 0o644); err != nil {
		t.Fatalf("writing the segment: %v", err)
	}

	row := &models.StorageBackend{
		Name: "tier-" + t.Name(), Kind: models.StorageKindFS,
		RootPath: tier, Enabled: true,
	}
	if err := st.StorageBackends.Create(row); err != nil {
		t.Fatalf("creating the backend: %v", err)
	}

	camera := &models.Camera{
		Slug: uploaderTestSlug, Name: "Front door", SourceURI: "rtsp://cam/live",
	}
	if err := st.Cameras.Create(camera); err != nil {
		t.Fatalf("creating the camera: %v", err)
	}

	start := time.Date(2026, 11, 4, 6, 30, 0, 0, time.UTC)
	segment := &models.Segment{
		CameraID: camera.ID, Generation: 0, Path: target,
		StartTS: start, EndTS: start.Add(5 * time.Second),
		Seconds: 5, Bytes: int64(len(uploaderTestBody)), Kind: models.SegmentKindContinuous,
	}
	if err := st.Segments.Create(segment); err != nil {
		t.Fatalf("creating the segment: %v", err)
	}

	f := &uploaderFixture{
		st: st, tier: tier, target: target, row: row,
		segmentID: segment.ID, start: start,
		// The uploader's clock sits a day ahead of the wall. ClaimNext compares
		// next_attempt_at against the *real* clock, so an injected "now" in the
		// past would make every recorded retry time already due, and one Tick
		// would drain the whole ladder. Ahead of the wall, a recorded next
		// attempt is genuinely not yet claimable — which is what releaseForRetry
		// then has to move for the next iteration to happen.
		now: time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour),
	}

	u, err := NewUploader(st.StorageBackends, st.StorageJobs, st.Settings, sealKey(t),
		NewManifestFacts(st.Segments, st.Cameras),
		UploaderOptions{
			WorkerID: "test-worker", Tick: 5 * time.Millisecond,
			Now: func() time.Time { return f.now },
			// The failure each of these tests provokes is asserted from the row,
			// which is where the queue page reads it; the log line is the same
			// fact in a place no test can check, so it is discarded rather than
			// printed forty times.
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	if err != nil {
		t.Fatalf("NewUploader: %v", err)
	}
	f.u = u
	return f
}

// enqueueSegment queues the fixture's segment.
func (f *uploaderFixture) enqueueSegment(t *testing.T) {
	t.Helper()
	if err := f.st.StorageJobs.Enqueue(store.JobInput{
		ID: f.segmentID, Kind: models.JobKindSegment, Target: f.target,
		BackendID: f.row.ID, Action: models.JobActionUpload, Bytes: int64(len(uploaderTestBody)),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

// job reloads the row, which is the only place any of the criteria are visible.
func (f *uploaderFixture) job(t *testing.T, id string) *models.StorageJob {
	t.Helper()
	job, err := f.st.StorageJobs.GetByID(id)
	if err != nil {
		t.Fatalf("reading job %s: %v", id, err)
	}
	return job
}

// releaseForRetry makes a retried row claimable again. The uploader's clock is
// injected so the recorded delay is an exact instant; ClaimNext compares
// next_attempt_at against the real one, so the row has to be moved for the next
// attempt to happen in the test.
func (f *uploaderFixture) releaseForRetry(t *testing.T, id string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Second)
	if err := f.st.DB.Model(&models.StorageJob{}).Where("id = ?", id).
		Update("next_attempt_at", past).Error; err != nil {
		t.Fatalf("releasing job %s for retry: %v", id, err)
	}
}

// setAttempts writes the attempt count directly, which is how the boundary at
// JobMaxAttempts is reached without twenty claims.
func (f *uploaderFixture) setAttempts(t *testing.T, id string, attempts int) {
	t.Helper()
	if err := f.st.DB.Model(&models.StorageJob{}).Where("id = ?", id).
		Update("attempts", attempts).Error; err != nil {
		t.Fatalf("setting attempts on job %s: %v", id, err)
	}
}

// brokenTierBackend points the backend's root at a path that cannot be a
// directory, so every Put fails for a reason the tier controls.
func (f *uploaderFixture) breakTier(t *testing.T) {
	t.Helper()
	blocker := filepath.Join(f.tier, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("writing the blocker: %v", err)
	}
	row, err := f.st.StorageBackends.GetByID(f.row.ID)
	if err != nil {
		t.Fatalf("reloading the backend: %v", err)
	}
	row.RootPath = filepath.Join(blocker, "sub")
	if err := f.st.StorageBackends.Update(row, false, false, false, false); err != nil {
		t.Fatalf("breaking the tier: %v", err)
	}
}

func TestUploaderClaimUploadAndMarksUploaded(t *testing.T) {
	f := newUploaderFixture(t)
	f.enqueueSegment(t)

	settled, err := f.u.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if settled != 1 {
		t.Fatalf("Tick settled %d jobs, want 1", settled)
	}

	job := f.job(t, f.segmentID)
	if job.State != models.JobStateUploaded {
		t.Fatalf("state is %q, want %q after a successful upload", job.State, models.JobStateUploaded)
	}
	// The key on the row is the layout's, which is what the playback proxy and
	// the sweeper both follow.
	wantKey := fmt.Sprintf("nvr/%s/2026/11/04/%s-0-7.mp4", uploaderTestSlug, uploaderTestSlug)
	if job.RemoteKey != wantKey {
		t.Errorf("remote_key is %q, want %q", job.RemoteKey, wantKey)
	}
	if job.Bytes != int64(len(uploaderTestBody)) {
		t.Errorf("bytes is %d, want %d", job.Bytes, len(uploaderTestBody))
	}
	if job.LastError != "" {
		t.Errorf("last_error is %q, want it cleared", job.LastError)
	}
	if job.WorkerID != "test-worker" {
		t.Errorf("worker_id is %q, want the claiming worker", job.WorkerID)
	}

	// And the object is really on the tier, at that key, with those bytes.
	onTier, err := os.ReadFile(filepath.Join(f.tier, filepath.FromSlash(job.RemoteKey)))
	if err != nil {
		t.Fatalf("the tier does not hold %s: %v", job.RemoteKey, err)
	}
	if string(onTier) != uploaderTestBody {
		t.Errorf("the tier holds %q, want the segment's bytes", onTier)
	}
}

func TestUploaderNeverDeletesTheLocalFile(t *testing.T) {
	// The criterion is the sweeper's boundary, and the failure mode it prevents
	// is an uploader that unlinks the source the moment the copy lands: playback
	// would then depend on the tier being reachable, and a tier that is down
	// would mean footage that used to play.
	f := newUploaderFixture(t)
	f.enqueueSegment(t)

	if _, err := f.u.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if _, err := os.Stat(f.target); err != nil {
		t.Fatalf("the local file is gone after an upload: %v", err)
	}
	if job := f.job(t, f.segmentID); job.State != models.JobStateUploaded {
		t.Fatalf("state is %q, want uploaded", job.State)
	}
}

func TestUploaderFailureProducesExponentialNextAttempt(t *testing.T) {
	// The ladder, as the exact delay recorded on the row. `attempts` is the count
	// the claim put there, so the first retry is 10 minutes and not 5.
	want := []time.Duration{
		10 * time.Minute, // attempt 1
		20 * time.Minute, // 2
		40 * time.Minute, // 3
		80 * time.Minute, // 4
		160 * time.Minute,
		320 * time.Minute,
		360 * time.Minute, // 7: the doubling has passed the 6h cap
		360 * time.Minute,
		360 * time.Minute,
	}

	f := newUploaderFixture(t)
	f.enqueueSegment(t)
	f.breakTier(t)

	for i, wantDelay := range want {
		if _, err := f.u.Tick(context.Background()); err != nil {
			t.Fatalf("Tick on attempt %d: %v", i+1, err)
		}
		job := f.job(t, f.segmentID)

		if job.Attempts != i+1 {
			t.Fatalf("attempt %d: the row says %d attempts", i+1, job.Attempts)
		}
		if job.State != models.JobStatePending {
			t.Fatalf("attempt %d: state is %q, want %q", i+1, job.State, models.JobStatePending)
		}
		if job.NextAttemptAt == nil {
			t.Fatalf("attempt %d: next_attempt_at is nil, so the job would be claimed again at once", i+1)
		}
		got := job.NextAttemptAt.Sub(f.now)
		if got != wantDelay {
			t.Fatalf("attempt %d: next_attempt_at is now + %v, want now + %v", i+1, got, wantDelay)
		}
		if job.LastError == "" {
			t.Fatalf("attempt %d: last_error is empty, and the queue page shows it", i+1)
		}
		f.releaseForRetry(t, f.segmentID)
	}
}

func TestRetryLadderIsThePlansBackoff(t *testing.T) {
	// The same ladder as a pure function, including the two properties the
	// end-to-end test cannot show: the cap holds forever, and a huge attempt
	// count does not overflow it.
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for attempts, want := range map[int]time.Duration{
		0:    5 * time.Minute,
		1:    10 * time.Minute,
		6:    320 * time.Minute,
		7:    360 * time.Minute,
		20:   360 * time.Minute,
		64:   360 * time.Minute,
		1000: 360 * time.Minute,
	} {
		got := RetryAt(base, attempts).Sub(base)
		if got != want {
			t.Errorf("RetryAt(attempts=%d) = now + %v, want now + %v", attempts, got, want)
		}
	}
}

func TestUploaderStopsAtTwentyAttemptsAndIsVisible(t *testing.T) {
	// The boundary from both sides: the claim that reaches the cap stops, and the
	// one just below it keeps going.
	for _, tc := range []struct {
		startAttempts int
		wantState     string
	}{
		{models.JobMaxAttempts - 2, models.JobStatePending},
		{models.JobMaxAttempts - 1, models.JobStateFailed},
	} {
		f := newUploaderFixture(t)
		f.enqueueSegment(t)
		f.breakTier(t)
		f.setAttempts(t, f.segmentID, tc.startAttempts)

		if _, err := f.u.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		job := f.job(t, f.segmentID)
		if job.State != tc.wantState {
			t.Fatalf("from %d recorded attempts the state is %q, want %q",
				tc.startAttempts, job.State, tc.wantState)
		}
		if tc.wantState == models.JobStateFailed {
			// A failed job is a number on the queue page, not a log line: it has
			// to be reachable by the same two reads the API endpoint uses.
			jobs, err := f.st.StorageJobs.ListByState(models.JobStateFailed, 10, 0)
			if err != nil {
				t.Fatalf("ListByState(failed): %v", err)
			}
			if len(jobs) != 1 || jobs[0].ID != f.segmentID {
				t.Fatalf("?state=failed would show %d jobs, want this one", len(jobs))
			}
			counts, err := f.st.StorageJobs.Counts("")
			if err != nil {
				t.Fatalf("Counts: %v", err)
			}
			if counts.Failed != 1 {
				t.Fatalf("storage.queue would report %d failed, want 1", counts.Failed)
			}
			if counts.LastError == "" {
				t.Fatal("storage.queue would report no last_error for a failed job")
			}
		}
	}
}

func TestUploaderFailsForeverOnAFileThatIsGone(t *testing.T) {
	// A missing source is not a transient failure. Retrying it twenty times over
	// four and a half days keeps the queue's pending number moving, which is the
	// number an operator watches to tell whether the tier is healthy.
	f := newUploaderFixture(t)
	f.enqueueSegment(t)
	if err := os.Remove(f.target); err != nil {
		t.Fatalf("removing the source: %v", err)
	}

	if _, err := f.u.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	job := f.job(t, f.segmentID)
	if job.State != models.JobStateFailed {
		t.Fatalf("state is %q after %d attempt(s), want %q", job.State, job.Attempts, models.JobStateFailed)
	}
	if job.Attempts != 1 {
		t.Errorf("the job took %d attempts to give up, want 1", job.Attempts)
	}
	if !strings.Contains(job.LastError, "gone") {
		t.Errorf("last_error is %q, want it to name the missing file", job.LastError)
	}
}

func TestDisabledBackendClaimsNothing(t *testing.T) {
	f := newUploaderFixture(t)
	f.enqueueSegment(t)

	if err := f.st.StorageBackends.SetEnabled(f.row.ID, false); err != nil {
		t.Fatalf("disabling the backend: %v", err)
	}
	settled, err := f.u.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if settled != 0 {
		t.Fatalf("a disabled backend settled %d jobs, want none", settled)
	}
	job := f.job(t, f.segmentID)
	if job.State != models.JobStatePending {
		t.Fatalf("state is %q, want it to wait in %q", job.State, models.JobStatePending)
	}
	if _, err := os.Stat(filepath.Join(f.tier, "nvr")); !os.IsNotExist(err) {
		t.Fatal("a disabled backend wrote to the tier")
	}

	// And it resumes on its own: the flag is a pause, not a cancellation.
	if err := f.st.StorageBackends.SetEnabled(f.row.ID, true); err != nil {
		t.Fatalf("re-enabling the backend: %v", err)
	}
	if _, err := f.u.Tick(context.Background()); err != nil {
		t.Fatalf("Tick after re-enabling: %v", err)
	}
	if job := f.job(t, f.segmentID); job.State != models.JobStateUploaded {
		t.Fatalf("state is %q after re-enabling, want %q", job.State, models.JobStateUploaded)
	}
}

// disableDuringPut is the tier that turns itself off while a job is in flight,
// which is the only way to test the second half of "a disabled backend claims
// nothing; in-flight jobs finish".
type disableDuringPut struct {
	Backend
	disable func()
}

func (d *disableDuringPut) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	d.disable()
	return d.Backend.Put(ctx, key, r, size, contentType)
}

func TestInFlightJobFinishesWhenTheBackendIsDisabledMidFlight(t *testing.T) {
	f := newUploaderFixture(t)
	f.enqueueSegment(t)

	once := false
	f.u.open = func(row *models.StorageBackend) (Backend, error) {
		b, err := OpenWithSettings(context.Background(), row, [32]byte{}, nil)
		if err != nil {
			return nil, err
		}
		return &disableDuringPut{Backend: b, disable: func() {
			if once {
				return
			}
			once = true
			if err := f.st.StorageBackends.SetEnabled(row.ID, false); err != nil {
				t.Errorf("disabling mid-flight: %v", err)
			}
		}}, nil
	}

	// The seal key here is the zero array, which is fine for an fs backend — it
	// has no credential to unseal — and is why this test overrides `open` rather
	// than reusing the fixture's.
	if _, err := f.u.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !once {
		t.Fatal("the tier never saw the put, so the claim never happened")
	}
	if job := f.job(t, f.segmentID); job.State != models.JobStateUploaded {
		t.Fatalf("state is %q; a backend disabled mid-flight must not strand the job in %q",
			job.State, models.JobStateUploading)
	}

	// The next pass claims nothing, because the gate is the claim.
	settled, err := f.u.Tick(context.Background())
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if settled != 0 {
		t.Fatalf("a disabled backend settled %d more jobs, want none", settled)
	}
}

func TestUploaderTickDrainsTheQueue(t *testing.T) {
	// One job per tick would be three hours of latency on a backlog, so a pass
	// drains. The events need no manifest facts: an event's media is addressed by
	// its event id, which is the job's id.
	f := newUploaderFixture(t)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("3f2b1c00-0000-4000-8000-00000000000%d", i+1)
		target := filepath.Join(filepath.Dir(f.target), fmt.Sprintf("clip-%d.mp4", i))
		if err := os.WriteFile(target, []byte("clip"), 0o644); err != nil {
			t.Fatalf("writing clip %d: %v", i, err)
		}
		kind := models.JobKindClip
		if i == 2 {
			kind = models.JobKindSnapshot
			target = filepath.Join(filepath.Dir(f.target), "snap.jpg")
			if err := os.WriteFile(target, []byte("jpeg"), 0o644); err != nil {
				t.Fatalf("writing the snapshot: %v", err)
			}
		}
		if err := f.st.StorageJobs.Enqueue(store.JobInput{
			ID: id, Kind: kind, Target: target,
			BackendID: f.row.ID, Action: models.JobActionUpload, Bytes: 4,
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	settled, err := f.u.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if settled != 3 {
		t.Fatalf("one tick settled %d of 3 jobs, want 3", settled)
	}

	counts, err := f.st.StorageJobs.Counts("")
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts.Uploaded != 3 || counts.Pending != 0 {
		t.Fatalf("counts are %+v, want three uploaded and none pending", counts)
	}

	// The event layout, on the tier, for both kinds.
	for _, want := range []string{
		"nvr/events/3f2b1c00-0000-4000-8000-000000000001/clip.mp4",
		"nvr/events/3f2b1c00-0000-4000-8000-000000000002/clip.mp4",
		"nvr/events/3f2b1c00-0000-4000-8000-000000000003/snapshot.jpg",
	} {
		if _, err := os.Stat(filepath.Join(f.tier, filepath.FromSlash(want))); err != nil {
			t.Errorf("%s is not on the tier: %v", want, err)
		}
	}
}

func TestUploaderRunIdlesWhileDisabled(t *testing.T) {
	// STORAGE_ENABLED=false stops the uploader and the queue grows — it does not
	// drain and it does not lose anything.
	f := newUploaderFixture(t)
	f.enqueueSegment(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The flag is read from the loop's goroutine, so the test flips it through an
	// atomic — a bare bool here is a data race, which is exactly the mistake the
	// real caller can make.
	var enabled atomic.Bool
	done := make(chan error, 1)
	go func() { done <- f.u.Run(ctx, enabled.Load) }()

	time.Sleep(80 * time.Millisecond)
	if job := f.job(t, f.segmentID); job.State != models.JobStatePending {
		t.Fatalf("an idled uploader moved the job to %q, want it to wait in %q",
			job.State, models.JobStatePending)
	}

	enabled.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job := f.job(t, f.segmentID); job.State == models.JobStateUploaded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job := f.job(t, f.segmentID); job.State != models.JobStateUploaded {
		t.Fatalf("after enabling, the job is %q, want %q", job.State, models.JobStateUploaded)
	}

	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

func TestUploaderRunsTheSweepersDeleteActions(t *testing.T) {
	// One queue, one worker: the sweeper decides and this executes. A
	// `delete_local` job removes the local file and keeps the remote key on the
	// row, because after that the key is the only record of where the object is.
	f := newUploaderFixture(t)
	f.enqueueSegment(t)
	if _, err := f.u.Tick(context.Background()); err != nil {
		t.Fatalf("upload Tick: %v", err)
	}
	uploaded := f.job(t, f.segmentID)

	if err := f.st.StorageJobs.Enqueue(store.JobInput{
		ID: f.segmentID, Kind: models.JobKindSegment, Target: f.target,
		BackendID: f.row.ID, Action: models.JobActionDeleteLocal,
	}); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if _, err := f.u.Tick(context.Background()); err != nil {
		t.Fatalf("delete_local Tick: %v", err)
	}
	if _, err := os.Stat(f.target); !os.IsNotExist(err) {
		t.Errorf("the local file survived its delete_local job: %v", err)
	}
	after := f.job(t, f.segmentID)
	if after.RemoteKey != uploaded.RemoteKey {
		t.Errorf("the remote key is %q after the local delete, want %q", after.RemoteKey, uploaded.RemoteKey)
	}

	// delete_remote then takes it off the tier.
	// The purge is enqueued the way S3-03's camera delete and S6-04's event
	// delete enqueue it: `Target` carries the remote key, because for a delete
	// the target *is* the thing to remove.
	if err := f.st.StorageJobs.Enqueue(store.JobInput{
		ID: f.segmentID, Kind: models.JobKindSegment, Target: uploaded.RemoteKey,
		BackendID: f.row.ID, Action: models.JobActionDeleteRemote,
	}); err != nil {
		t.Fatalf("re-enqueue for the tier: %v", err)
	}
	if got := f.job(t, f.segmentID); got.Target != uploaded.RemoteKey {
		t.Fatalf("the purge job names %q, want the remote key", got.Target)
	}
	if _, err := f.u.Tick(context.Background()); err != nil {
		t.Fatalf("delete_remote Tick: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.tier, filepath.FromSlash(uploaded.RemoteKey))); !os.IsNotExist(err) {
		t.Errorf("the object survived its delete_remote job: %v", err)
	}
}

func TestUploaderNeedsAWorkerIDAndTheStores(t *testing.T) {
	st := newStorageTestStore(t)
	if _, err := NewUploader(st.StorageBackends, st.StorageJobs, nil, [32]byte{}, nil, UploaderOptions{}); err == nil {
		t.Fatal("an uploader with no worker id was accepted; a claim with no worker cannot be traced")
	}
	if _, err := NewUploader(nil, st.StorageJobs, nil, [32]byte{}, nil, UploaderOptions{WorkerID: "w"}); err == nil {
		t.Fatal("an uploader with no backend store was accepted")
	}
}

// TestUploaderDeleteRemoteFollowsThePurgeJobsTheAPIAlreadyEnqueues is the
// integration half of the Target-is-the-key rule. The camera purge (S3-03) and
// the event delete (S6-04) fill Target with the key and leave RemoteKey empty;
// a worker that read only RemoteKey would fail every purge those handlers
// queued, and the object would sit on the tier forever with nothing pointing at
// it.
func TestUploaderDeleteRemoteFollowsThePurgeJobsTheAPIAlreadyEnqueues(t *testing.T) {
	f := newUploaderFixture(t)

	key := "nvr/front-door/2026/11/04/front-door-0-7.mp4"
	if err := os.MkdirAll(filepath.Join(f.tier, "nvr/front-door/2026/11/04"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.tier, filepath.FromSlash(key)), []byte(uploaderTestBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// The job exactly as the handler writes it: no RemoteKey, no upload first.
	if err := f.st.StorageJobs.Enqueue(store.JobInput{
		ID: f.segmentID, Kind: models.JobKindSegment, Target: key,
		BackendID: f.row.ID, Action: models.JobActionDeleteRemote,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := f.u.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.tier, filepath.FromSlash(key))); !os.IsNotExist(err) {
		t.Fatalf("the tier still holds %s after the purge job: %v", key, err)
	}
	if job := f.job(t, f.segmentID); job.State != models.JobStateUploaded {
		t.Fatalf("the purge job is %q with %q, want it settled", job.State, job.LastError)
	}
}

// TestUploaderKeysAnEventJobThatNamesItsPiece states the queue's id convention
// against the layout. An event has two objects and a job row is keyed by its
// object, so the queue holds `<event_id>:clip` and `<event_id>:snapshot` — and
// the layout must still be nvr/events/<event_id>/clip.mp4, not a directory named
// after the piece.
func TestUploaderKeysAnEventJobThatNamesItsPiece(t *testing.T) {
	f := newUploaderFixture(t)
	eventID := "3f2b1c00-0000-4000-8000-0000000000aa"

	for _, tc := range []struct {
		jobID, kind, want string
	}{
		{eventID + ":clip", models.JobKindClip, "nvr/events/" + eventID + "/clip.mp4"},
		{eventID + ":snapshot", models.JobKindSnapshot, "nvr/events/" + eventID + "/snapshot.jpg"},
		{eventID, models.JobKindClip, "nvr/events/" + eventID + "/clip.mp4"},
	} {
		target := filepath.Join(filepath.Dir(f.target), strings.ReplaceAll(tc.jobID, ":", "-"))
		if err := os.WriteFile(target, []byte("bytes"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", target, err)
		}
		if err := f.st.StorageJobs.Enqueue(store.JobInput{
			ID: tc.jobID, Kind: tc.kind, Target: target,
			BackendID: f.row.ID, Action: models.JobActionUpload, Bytes: 5,
		}); err != nil {
			t.Fatalf("enqueue %s: %v", tc.jobID, err)
		}
		if _, err := f.u.Tick(context.Background()); err != nil {
			t.Fatalf("Tick %s: %v", tc.jobID, err)
		}
		if _, err := os.Stat(filepath.Join(f.tier, filepath.FromSlash(tc.want))); err != nil {
			t.Errorf("%s landed wrong: %s is not on the tier: %v", tc.jobID, tc.want, err)
		}
		if job := f.job(t, tc.jobID); job.RemoteKey != tc.want {
			t.Errorf("job %s recorded %q, want %q", tc.jobID, job.RemoteKey, tc.want)
		}
	}
}
