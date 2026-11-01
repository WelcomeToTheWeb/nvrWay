package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

func backend(name, kind string) *models.StorageBackend {
	b := &models.StorageBackend{Name: name, Kind: kind, Enabled: true}
	switch kind {
	case models.StorageKindS3:
		b.Endpoint = "minio:9000"
		b.Region = "us-east-1"
		b.Bucket = "nvr"
		b.Prefix = "media"
		b.AccessKey = "sealed-access"
		b.SecretKey = "sealed-secret"
	case models.StorageKindWebDAV:
		b.Endpoint = "https://dav.example"
		b.Prefix = "nvr"
		b.Username = "nvr"
		b.Password = "sealed"
	case models.StorageKindSFTP:
		b.Endpoint = "storage.example:22"
		b.RootPath = "/srv/nvr"
		b.Username = "nvr"
		b.PrivateKeyPath = "sealed-path"
	case models.StorageKindFS:
		b.RootPath = "/var/lib/nvr/remote"
	}
	return b
}

func TestStorageBackendCreateAndFetch(t *testing.T) {
	st := newTestStore(t)

	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.ID == "" {
		t.Fatal("expected the store to mint the backend id")
	}

	byID, err := st.StorageBackends.GetByID(b.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	byName, err := st.StorageBackends.GetByName("tier-one")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if byID.ID != byName.ID {
		t.Fatal("the two lookups must find the same row")
	}

	if _, err := st.StorageBackends.GetByID("missing"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}

	dup := backend("tier-one", models.StorageKindS3)
	if err := st.StorageBackends.Create(dup); err == nil {
		t.Fatal("expected the unique name to be refused")
	}
}

func TestStorageBackendCreatedDisabledStaysDisabled(t *testing.T) {
	st := newTestStore(t)

	b := backend("cold-tier", models.StorageKindS3)
	b.Enabled = false
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err := st.StorageBackends.GetByName("cold-tier")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Enabled {
		t.Fatal("a backend saved disabled arrived enabled (schema default won)")
	}
}

func TestStorageBackendDefaultIsExclusive(t *testing.T) {
	st := newTestStore(t)

	first := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(first); err != nil {
		t.Fatalf("create: %v", err)
	}

	// One backend: it is the default. Asking an operator who configured one
	// destination to also tick "default" is a setup step with no answer in it.
	def, err := st.StorageBackends.GetDefault()
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if def.ID != first.ID {
		t.Fatalf("expected the only enabled backend as default, got %q", def.Name)
	}

	second := backend("tier-two", models.StorageKindS3)
	if err := st.StorageBackends.Create(second); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Two destinations and no choice recorded is not a coin flip: the queue
	// says it has nowhere decided to send, and the operator picks.
	if _, err := st.StorageBackends.GetDefault(); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found with two enabled backends and no default, got %v", err)
	}

	if err := st.StorageBackends.SetDefault(second.ID); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if def, err := st.StorageBackends.GetDefault(); err != nil || def.ID != second.ID {
		t.Fatalf("expected the chosen default, got %+v (%v)", def, err)
	}

	// Exclusivity: exactly one row claims to be the default, or the queue splits
	// its output across destinations.
	var flag int64
	if err := st.DB.Model(&models.StorageBackend{}).Where("is_default = ? AND enabled = ?", true, true).
		Count(&flag).Error; err != nil {
		t.Fatalf("count defaults: %v", err)
	}
	if flag != 1 {
		t.Fatalf("expected exactly one default, got %d", flag)
	}
	var firstRow models.StorageBackend
	if err := st.DB.First(&firstRow, "id = ?", first.ID).Error; err != nil {
		t.Fatalf("reload first: %v", err)
	}
	if firstRow.IsDefault {
		t.Fatal("setting a default must clear the flag on the rest")
	}

	// A disabled backend cannot be the default: the queue would have nowhere
	// to send. With it disabled, one enabled destination is left, so it answers
	// as the default again.
	if err := st.StorageBackends.SetEnabled(second.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := st.StorageBackends.SetDefault(second.ID); err == nil {
		t.Fatal("expected a disabled backend to be refused as default")
	}
	if def, err := st.StorageBackends.GetDefault(); err != nil || def.ID != first.ID {
		t.Fatalf("expected the one enabled backend as default, got %+v (%v)", def, err)
	}

	// Nothing enabled at all: the queue has no destination, and says so.
	if err := st.StorageBackends.SetEnabled(first.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := st.StorageBackends.GetDefault(); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found with no enabled backend, got %v", err)
	}

	if err := st.StorageBackends.SetDefault("missing"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found for an unknown backend, got %v", err)
	}
}

func TestStorageBackendUpdateKeepsSecretsItWasNotGiven(t *testing.T) {
	st := newTestStore(t)

	b := backend("tier-one", models.StorageKindS3)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create: %v", err)
	}
	sealedAccess, sealedSecret := b.AccessKey, b.SecretKey

	// The console never reads a secret back, so a PUT carries the fields the
	// operator edited. A write that omits the credentials must not clear them:
	// that is how "I only renamed it" becomes an installation that cannot upload.
	bucketOnly := backend("tier-one", models.StorageKindS3)
	bucketOnly.ID = b.ID
	bucketOnly.Bucket = "nvr-archiv"
	bucketOnly.AccessKey = ""
	bucketOnly.SecretKey = ""
	if err := st.StorageBackends.Update(bucketOnly, false, false, false, false); err != nil {
		t.Fatalf("update: %v", err)
	}

	loaded, err := st.StorageBackends.GetByID(b.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Bucket != "nvr-archiv" {
		t.Fatalf("expected the edited field to land, got %q", loaded.Bucket)
	}
	if loaded.AccessKey != sealedAccess || loaded.SecretKey != sealedSecret {
		t.Fatalf("the sealed credentials must survive an edit that did not carry them: %+v", loaded)
	}

	// A write that does carry them replaces them, sealed by the caller.
	rotate := backend("tier-one", models.StorageKindS3)
	rotate.ID = b.ID
	rotate.AccessKey = "sealed-access-2"
	rotate.SecretKey = "sealed-secret-2"
	if err := st.StorageBackends.Update(rotate, false, true, true, false); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	loaded, err = st.StorageBackends.GetByID(b.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.AccessKey != "sealed-access-2" || loaded.SecretKey != "sealed-secret-2" {
		t.Fatalf("expected the rotated credentials, got %+v", loaded)
	}

	if err := st.StorageBackends.Update(&models.StorageBackend{ID: "missing"}, false, false, false, false); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestStorageBackendDelete(t *testing.T) {
	st := newTestStore(t)
	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.StorageBackends.Delete(b.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.StorageBackends.Delete(b.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found on the second delete, got %v", err)
	}
}

// --- jobs ---

func TestStorageJobEnqueueIsIdempotentPerObject(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	seg := segment(camera.ID, 1, 0, minutesAgo(10), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create segment: %v", err)
	}

	// The uploader tick runs every 30 s. Enqueue keyed on a fresh id per call
	// becomes a pile of duplicate uploads for one file, so the object's id is
	// the job's id.
	for i := 0; i < 5; i++ {
		if err := st.StorageJobs.Enqueue(JobInput{
			ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
			BackendID: b.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	counts, err := st.StorageJobs.Counts(b.ID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Pending != 1 {
		t.Fatalf("expected one job for the object after five enqueues, got %+v", counts)
	}
}

func TestStorageJobEnqueueRenewsTheObjectsObligation(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	seg := segment(camera.ID, 1, 0, minutesAgo(40), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create segment: %v", err)
	}

	if err := st.StorageJobs.Enqueue(JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: b.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// It lands, then the retention window passes and the sweeper asks for the
	// local copy back.
	claimUploaded(t, st, seg.ID)
	if err := st.StorageJobs.Enqueue(JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: b.ID, Action: models.JobActionDeleteLocal,
	}); err != nil {
		t.Fatalf("enqueue delete_local: %v", err)
	}

	job, err := st.StorageJobs.GetByID(seg.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != models.JobStatePending || job.Action != models.JobActionDeleteLocal {
		t.Fatalf("expected the object's next obligation to replace the finished one, got %+v", job)
	}

	var rows int64
	if err := st.DB.Model(&models.StorageJob{}).Count(&rows).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if rows != 1 {
		t.Fatalf("the queue is one row per object, got %d", rows)
	}
}

func TestStorageJobEnqueueRejectsAnUnknownKindOrAction(t *testing.T) {
	st := newTestStore(t)
	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create backend: %v", err)
	}

	if err := st.StorageJobs.Enqueue(JobInput{
		ID: "11111111-1111-1111-1111-111111111111", Kind: "thumbnail",
		BackendID: b.ID, Action: models.JobActionUpload,
	}); err == nil {
		t.Fatal("expected an unknown job kind to be refused")
	}
	if err := st.StorageJobs.Enqueue(JobInput{
		ID: "11111111-1111-1111-1111-111111111111", Kind: models.JobKindSegment,
		BackendID: b.ID, Action: "explode",
	}); err == nil {
		t.Fatal("expected an unknown job action to be refused")
	}
	if err := st.StorageJobs.Enqueue(JobInput{ID: "", Kind: models.JobKindSegment, BackendID: b.ID, Action: models.JobActionUpload}); err == nil {
		t.Fatal("expected a job with no object to be refused")
	}
}

func claimUploaded(t *testing.T, st *Store, jobID string) *models.StorageJob {
	t.Helper()
	claimed, err := st.StorageJobs.ClaimNext(ctx(), "worker-test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != jobID {
		t.Fatalf("expected to claim the seeded job, got %s", claimed.ID)
	}
	if err := st.StorageJobs.MarkUploaded(claimed.ID, "nvr/key.mp4", claimed.Bytes); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}
	return claimed
}

// landNextUpload claims whatever the queue offers and lands it: the shape the
// uploader's tick has, without a storage driver in the test.
func landNextUpload(t *testing.T, st *Store) *models.StorageJob {
	t.Helper()
	claimed, err := st.StorageJobs.ClaimNext(ctx(), "worker-test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.StorageJobs.MarkUploaded(claimed.ID, "nvr/key.mp4", claimed.Bytes); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}
	return claimed
}

func TestStorageJobClaimNext(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create backend: %v", err)
	}

	if _, err := st.StorageJobs.ClaimNext(ctx(), "worker-test"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("an empty queue answers not-found, got %v", err)
	}

	seg := segment(camera.ID, 1, 0, minutesAgo(5), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create segment: %v", err)
	}
	if err := st.StorageJobs.Enqueue(JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: b.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, err := st.StorageJobs.ClaimNext(ctx(), "worker-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.State != models.JobStateUploading || claimed.WorkerID != "worker-a" || claimed.Attempts != 1 {
		t.Fatalf("expected the claim to mark the job, got %+v", claimed)
	}

	// A second tick claims nothing: the job is spoken for.
	if _, err := st.StorageJobs.ClaimNext(ctx(), "worker-b"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected nothing claimable, got %v", err)
	}

	if err := st.StorageJobs.MarkUploaded(claimed.ID, "nvr/front-door/2026/11/01/cam-1-0.mp4", 1024); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}
	done, err := st.StorageJobs.GetByID(seg.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if done.State != models.JobStateUploaded || done.RemoteKey == "" {
		t.Fatalf("expected the uploaded stamp, got %+v", done)
	}
}

func TestStorageJobRetryUsesTheCallersBackoff(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	seg := segment(camera.ID, 1, 0, minutesAgo(5), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create segment: %v", err)
	}
	if err := st.StorageJobs.Enqueue(JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: b.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, err := st.StorageJobs.ClaimNext(ctx(), "worker-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	next := time.Now().UTC().Add(5 * time.Minute)
	if err := st.StorageJobs.MarkRetry(claimed.ID, "s3: connection refused", next); err != nil {
		t.Fatalf("mark retry: %v", err)
	}

	job, err := st.StorageJobs.GetByID(seg.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != models.JobStatePending {
		t.Fatalf("a failed upload goes back to pending, got %q", job.State)
	}
	if job.LastError == "" {
		t.Fatal("the queue page has to show why it is failing")
	}
	if job.NextAttemptAt == nil || !job.NextAttemptAt.Equal(next) {
		t.Fatalf("expected the caller's next-attempt time, got %v", job.NextAttemptAt)
	}

	// A job whose next attempt is in the future is not claimable: that is the
	// whole exponential backoff, expressed in the row.
	if _, err := st.StorageJobs.ClaimNext(ctx(), "worker-a"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected the backoff to hold the job, got %v", err)
	}
}

func TestStorageJobStopsAfterMaxAttempts(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	seg := segment(camera.ID, 1, 0, minutesAgo(5), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create segment: %v", err)
	}
	if err := st.StorageJobs.Enqueue(JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: b.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Walk it to the ceiling.
	for i := 0; i < models.JobMaxAttempts; i++ {
		claimed, err := st.StorageJobs.ClaimNext(ctx(), "worker-a")
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if err := st.StorageJobs.MarkRetry(claimed.ID, "boom", time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}

	job, err := st.StorageJobs.GetByID(seg.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != models.JobStateFailed {
		t.Fatalf("expected the dead-letter state at %d attempts, got %q (attempts %d)",
			models.JobMaxAttempts, job.State, job.Attempts)
	}

	counts, err := st.StorageJobs.Counts("")
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Failed != 1 {
		t.Fatalf("the failed job must be countable on the queue page, got %+v", counts)
	}
}

func TestStorageJobCountsAndQueuePage(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create backend: %v", err)
	}

	for i := 0; i < 3; i++ {
		seg := segment(camera.ID, 1, i, minutesAgo(10-i), 5, models.SegmentKindContinuous)
		if err := st.Segments.Create(seg); err != nil {
			t.Fatalf("create segment: %v", err)
		}
		if err := st.StorageJobs.Enqueue(JobInput{
			ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
			BackendID: b.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	counts, err := st.StorageJobs.Counts("")
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Pending != 3 {
		t.Fatalf("expected 3 pending, got %+v", counts)
	}

	// Land one of the three, the way a tick does: claim, then land.
	landNextUpload(t, st)

	counts, err = st.StorageJobs.Counts("")
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Uploaded != 1 || counts.Pending != 2 {
		t.Fatalf("expected 1 uploaded / 2 pending, got %+v", counts)
	}

	page, err := st.StorageJobs.ListByState(models.JobStateUploaded, 10, 0)
	if err != nil {
		t.Fatalf("list uploaded: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("expected the queue page to answer 1 row, got %d", len(page))
	}
	if page[0].RemoteKey == "" {
		t.Fatal("the queue page names the remote key")
	}
}

func TestStorageJobRequeueReShelvesOntoAnotherBackend(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	old := backend("tier-old", models.StorageKindFS)
	fresh := backend("tier-new", models.StorageKindS3)
	for _, b := range []*models.StorageBackend{old, fresh} {
		if err := st.StorageBackends.Create(b); err != nil {
			t.Fatalf("create backend: %v", err)
		}
	}

	for i := 0; i < 3; i++ {
		seg := segment(camera.ID, 1, i, minutesAgo(10-i), 5, models.SegmentKindContinuous)
		if err := st.Segments.Create(seg); err != nil {
			t.Fatalf("create segment: %v", err)
		}
		if err := st.StorageJobs.Enqueue(JobInput{
			ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
			BackendID: old.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	// Land two of them.
	for i := 0; i < 2; i++ {
		claimed, err := st.StorageJobs.ClaimNext(ctx(), "worker-a")
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := st.StorageJobs.MarkUploaded(claimed.ID, "nvr/old/key.mp4", claimed.Bytes); err != nil {
			t.Fatalf("mark uploaded: %v", err)
		}
	}

	requeued, err := st.StorageJobs.Requeue(fresh.ID, models.JobKindSegment)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if requeued != 2 {
		t.Fatalf("expected the two uploaded segments re-shelved, got %d", requeued)
	}

	counts, err := st.StorageJobs.Counts(fresh.ID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Pending != 2 {
		t.Fatalf("expected 2 pending on the new tier, got %+v", counts)
	}

	// The destination has to exist: re-shelving onto a backend that is not
	// there produces a queue failing every job against a row nobody can see.
	if _, err := st.StorageJobs.Requeue("not-a-backend", models.JobKindSegment); err == nil {
		t.Fatal("expected an unknown backend to be refused")
	}
}

func TestStorageJobReleaseStuck(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	seg := segment(camera.ID, 1, 0, minutesAgo(5), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create segment: %v", err)
	}
	if err := st.StorageJobs.Enqueue(JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: b.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, err := st.StorageJobs.ClaimNext(ctx(), "worker-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Nothing is stuck yet (the claim just happened).
	if released, err := st.StorageJobs.ReleaseStuck(time.Now().UTC().Add(-time.Hour)); err != nil || released != 0 {
		t.Fatalf("a fresh claim is not stuck: %d released, %v", released, err)
	}

	// Pretend the process died holding it: the row's stamp is old.
	if err := st.DB.Model(&models.StorageJob{}).Where("id = ?", claimed.ID).
		Update("updated_at", time.Now().UTC().Add(-2*time.Hour)).Error; err != nil {
		t.Fatalf("age the claim: %v", err)
	}
	released, err := st.StorageJobs.ReleaseStuck(time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("release stuck: %v", err)
	}
	if released != 1 {
		t.Fatalf("expected the abandoned claim released, got %d", released)
	}
	if _, err := st.StorageJobs.ClaimNext(ctx(), "worker-b"); err != nil {
		t.Fatalf("the released job must be claimable again: %v", err)
	}
}

func TestStorageJobPruneUploaded(t *testing.T) {
	st := newTestStore(t)
	camera := seedCamera(t, st, "front-door")
	b := backend("tier-one", models.StorageKindFS)
	if err := st.StorageBackends.Create(b); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	seg := segment(camera.ID, 1, 0, minutesAgo(5), 5, models.SegmentKindContinuous)
	if err := st.Segments.Create(seg); err != nil {
		t.Fatalf("create segment: %v", err)
	}
	if err := st.StorageJobs.Enqueue(JobInput{
		ID: seg.ID, Kind: models.JobKindSegment, Target: seg.Path,
		BackendID: b.ID, Action: models.JobActionUpload, Bytes: seg.Bytes,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimUploaded(t, st, seg.ID)

	if pruned, err := st.StorageJobs.PruneUploaded(time.Now().UTC().Add(-time.Hour)); err != nil || pruned != 0 {
		t.Fatalf("a fresh upload is not prunable: %d, %v", pruned, err)
	}
	pruned, err := st.StorageJobs.PruneUploaded(time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected the finished job pruned, got %d", pruned)
	}
}

func TestStorageBackendKindsAreThePlanSet(t *testing.T) {
	want := map[string]bool{models.StorageKindS3: true, models.StorageKindWebDAV: true, models.StorageKindSFTP: true, models.StorageKindFS: true}
	if len(models.StorageKinds) != len(want) {
		t.Fatalf("expected exactly the plan's four drivers, got %v", models.StorageKinds)
	}
	for _, kind := range models.StorageKinds {
		if !want[kind] {
			t.Fatalf("unexpected storage kind %q", kind)
		}
		if !models.ValidStorageKind(kind) {
			t.Fatalf("ValidStorageKind must accept %q", kind)
		}
	}
	if models.ValidStorageKind("ftp") {
		t.Fatal("ftp is not a v1 driver")
	}
	// fs is a path, s3 is a bucket: the API validates per kind from one rule.
	if !models.KindNeedsRoot(models.StorageKindFS) || models.KindNeedsRoot(models.StorageKindS3) {
		t.Fatal("the per-kind validation rule is wrong")
	}
	if !models.KindNeedsBucket(models.StorageKindS3) {
		t.Fatal("the per-kind validation rule is wrong")
	}
}
