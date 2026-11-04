package storage

// The queue worker: claim, put, mark.
//
// The queue is the only way to the tier (see storage.go), and this is the thing
// that walks it. One loop, one claim at a time, and a job leaves the loop in one
// of exactly three states: uploaded, pending with a next attempt, or failed.
// There is no fourth, because "we'll know from the log" is how a storage tier
// that has been broken for a week gets discovered by the footage that is missing.
//
// The division of labour with the sweeper (S7-09) is worth stating, because the
// queue has three actions and only one worker: **the sweeper decides, this
// executes.** The sweeper enqueues `upload`, `delete_local` and `delete_remote`;
// this loop claims whatever is oldest and does it. So "the uploader never deletes
// local files" is a statement about the *upload* path — a successful upload marks
// the job and leaves the file on disk for the sweeper to age out — and not about
// the loop, which will run a `delete_local` job the sweeper queued. Splitting the
// actions across two workers would mean two claims fighting over one queue.
//
// A backend is opened per job rather than cached. Every driver's constructor is
// cheap — minio and gowebdav build a client without dialling, and sftp dials per
// operation because `Backend` has no Close — so a cache would buy nothing and
// would cost a stale credential surviving an edit until restart.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
	"nvr/server/store"
)

const (
	// retryBase and retryCap are the plan's backoff: the next attempt is
	// `now + min(5min * 2^attempts, 6h)`.
	retryBase = 5 * time.Minute
	retryCap  = 6 * time.Hour

	// DefaultUploadTick is what UPLOADER_TICK_SECONDS defaults to. Ten seconds
	// is short enough that a finished segment is safe on the tier well before
	// the sweeper could consider it, and long enough that an idle installation
	// does not spin.
	DefaultUploadTick = 10 * time.Second

	// maxJobsPerTick and maxTickWork bound one pass. Without them a backlog
	// makes a tick unbounded, and the tick is what the shutdown context is
	// waiting on. The time bound matters as much as the count bound: a tier that
	// is down costs a dial timeout per job, and a pass that insists on reaching
	// 200 of them holds shutdown for minutes and starves every *healthy* backend
	// behind the dead one.
	maxJobsPerTick = 200
	maxTickWork    = 5 * time.Second

	// retryCapAttempts is the attempt count at which the doubling backoff has
	// already passed the cap (5min * 2^7 = 640min > 6h), so the loop stops
	// doubling there rather than overflowing on a row someone requeued by hand.
	retryCapAttempts = 7
)

// JobFacts resolves what a segment's key needs from the manifest.
//
// The uploader could get the slug out of the file name — the recorder's own name
// is `<slug>-<generation>-<n>.mp4`, so the slug is the prefix — but it cannot get
// the *date*, and the plan's clock rule says no time is ever read from a file
// name. So both come from the row that already has them, and the temptation to
// parse is closed rather than warned about.
type JobFacts interface {
	// SegmentFacts returns the slug of the camera that recorded the segment and
	// the segment's own start, which is what the date path is built from.
	SegmentFacts(ctx context.Context, segmentID string) (slug string, when time.Time, err error)
}

// ManifestFacts is JobFacts over the segment and camera tables.
type ManifestFacts struct {
	segments *store.SegmentStore
	cameras  *store.CameraStore
}

// NewManifestFacts wires JobFacts to the two tables it reads.
func NewManifestFacts(segments *store.SegmentStore, cameras *store.CameraStore) *ManifestFacts {
	return &ManifestFacts{segments: segments, cameras: cameras}
}

// SegmentFacts reads the segment row and its camera's slug.
func (f *ManifestFacts) SegmentFacts(ctx context.Context, segmentID string) (string, time.Time, error) {
	seg, err := f.segments.GetByID(segmentID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("storage: the segment %s this job names is not in the manifest: %w", segmentID, err)
	}
	cam, err := f.cameras.GetByID(seg.CameraID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("storage: the camera that recorded segment %s is not in the manifest: %w", segmentID, err)
	}
	return cam.Slug, seg.StartTS, nil
}

// Uploader walks the storage queue.
type Uploader struct {
	backends *store.StorageBackendStore
	jobs     *store.StorageJobStore
	settings *store.SettingStore
	sealKey  [32]byte
	facts    JobFacts

	workerID string
	tick     time.Duration
	log      *slog.Logger
	now      func() time.Time

	// open turns a row into a Backend. It is a field rather than a direct call
	// because the tier is the one dependency this loop cannot run without and
	// the one worth driving from a test; the default is the real Open.
	open func(row *models.StorageBackend) (Backend, error)
}

// UploaderOptions is the loop's tuning and identity.
type UploaderOptions struct {
	// WorkerID names this process in `storage_jobs.worker_id`, which is how a
	// stuck claim is traced to the worker that died holding it.
	WorkerID string
	// Tick is UPLOADER_TICK_SECONDS; zero takes DefaultUploadTick.
	Tick time.Duration
	// Log receives one line per failure. nil takes the default logger.
	Log *slog.Logger
	// Now is the clock. nil takes time.Now, and a test supplies a fixed one so
	// the backoff can be asserted as an exact instant.
	Now func() time.Time
}

// NewUploader builds the worker. `settings` may be nil, which leaves an sftp
// backend with nowhere to pin a host key and every other kind unaffected. The
// seal key is a [32]byte because that is what config.SealKey returns, and an
// array cannot be the wrong length — the class of bug the seal has to survive
// (a truncated key from a config file) is unrepresentable here.
func NewUploader(
	backends *store.StorageBackendStore,
	jobs *store.StorageJobStore,
	settings *store.SettingStore,
	sealKey [32]byte,
	facts JobFacts,
	opts UploaderOptions,
) (*Uploader, error) {
	if backends == nil || jobs == nil {
		return nil, errors.New("storage: the uploader needs both the backend and the job store")
	}
	if opts.WorkerID == "" {
		return nil, errors.New("storage: the uploader needs a worker id to claim jobs as")
	}
	if opts.Tick <= 0 {
		opts.Tick = DefaultUploadTick
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	u := &Uploader{
		backends: backends,
		jobs:     jobs,
		settings: settings,
		sealKey:  sealKey,
		facts:    facts,
		workerID: opts.WorkerID,
		tick:     opts.Tick,
		log:      opts.Log,
		now:      opts.Now,
	}
	u.open = func(row *models.StorageBackend) (Backend, error) {
		return OpenWithSettings(context.Background(), row, u.sealKey, u.settings)
	}
	return u, nil
}

// Tick runs one pass over the queue and returns the number of jobs it settled.
//
// It drains rather than taking one job, because a backlog of a thousand segments
// at one job per tick is three hours of latency. The drain is bounded by
// `maxJobsPerTick` and by the context, so shutdown is still prompt.
//
// A job that fails does not end the pass. It is tempting to give up when a
// failure names the tier as unreachable, on the theory that the rest of the queue
// will fail the same way — but the queue is one queue across every backend, so
// the oldest job being unreachable says nothing about the next one, and a dead
// bucket would then starve a healthy one to zero jobs per tick forever. What
// bounds the damage instead is the pass's own budget: at most maxJobsPerTick jobs
// or maxTickWork of wall clock, whichever comes first.
func (u *Uploader) Tick(ctx context.Context) (int, error) {
	settled := 0
	budgetEnd := u.now().Add(maxTickWork)
	for i := 0; i < maxJobsPerTick; i++ {
		if err := ctx.Err(); err != nil {
			return settled, err
		}
		if !u.now().Before(budgetEnd) {
			return settled, nil
		}

		job, err := u.jobs.ClaimNext(ctx, u.workerID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return settled, nil
		}
		if err != nil {
			return settled, fmt.Errorf("storage: claiming the next storage job: %w", err)
		}

		if err := u.process(ctx, job); err != nil {
			u.log.Warn("storage job failed",
				"job", job.ID, "kind", job.Kind, "action", job.Action,
				"backend", job.BackendID, "attempt", job.Attempts, "error", err)
			continue
		}
		settled++
	}
	return settled, nil
}

// Run ticks until the context is done.
//
// `enabled` is the uploader's switch — STORAGE_ENABLED, and the queue-growing
// behaviour the plan attaches to it. It is a function rather than a bool because
// the setting can change while the process runs, and a loop that reads it once
// would need a restart to notice. When it is false the loop idles: the queue
// grows, the queue page shows it growing, and nothing is lost.
//
// It is called from this loop's goroutine, so it has to be safe to call
// concurrently with whoever changes it: an atomic, or something behind a mutex.
// A bare bool read across goroutines is the race the test for this behaviour
// found on its first run.
func (u *Uploader) Run(ctx context.Context, enabled func() bool) error {
	ticker := time.NewTicker(u.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if enabled != nil && !enabled() {
				continue
			}
			if _, err := u.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				u.log.Error("storage uploader tick failed", "error", err)
			}
		}
	}
}

// process settles one claimed job, in exactly one direction.
func (u *Uploader) process(ctx context.Context, job *models.StorageJob) error {
	key, size, err := u.perform(ctx, job)
	if err == nil {
		if markErr := u.jobs.MarkUploaded(job.ID, key, size); markErr != nil {
			// The bytes are on the tier and the row says otherwise, which is the
			// one direction that is safe to retry: the next attempt re-puts the
			// same object at the same key.
			return fmt.Errorf("storage: %s %s reached the tier but the job row would not take it: %w", job.Kind, key, markErr)
		}
		return nil
	}

	// A failure the row cannot outgrow stops here. Retrying a file that is not
	// on disk twenty times over four days does not fix it and buries the reason.
	if errors.Is(err, errPermanent) {
		if markErr := u.jobs.MarkFailed(job.ID, err.Error()); markErr != nil {
			return fmt.Errorf("storage: %s %s failed for good and the row would not say so: %w", job.Kind, job.ID, markErr)
		}
		return fmt.Errorf("storage: %s %s failed for good: %w", job.Kind, job.ID, err)
	}

	next := RetryAt(u.now(), job.Attempts)
	if markErr := u.jobs.MarkRetry(job.ID, err.Error(), next); markErr != nil {
		return fmt.Errorf("storage: %s %s failed and the retry would not be recorded: %w", job.Kind, job.ID, markErr)
	}
	return err
}

// perform runs the claimed job's action and returns the remote key and size the
// row should record.
func (u *Uploader) perform(ctx context.Context, job *models.StorageJob) (string, int64, error) {
	switch job.Action {
	case models.JobActionUpload:
		return u.upload(ctx, job)
	case models.JobActionDeleteLocal:
		return u.deleteLocal(job)
	case models.JobActionDeleteRemote:
		return u.deleteRemote(ctx, job)
	default:
		// An action no worker implements is a bug in whoever queued it, and
		// retrying it twenty times turns a bug into a queue jam.
		return job.RemoteKey, job.Bytes, fmt.Errorf("storage: job action %q is not implemented: %w", job.Action, errPermanent)
	}
}

// upload puts one object and marks nothing — the caller does that.
func (u *Uploader) upload(ctx context.Context, job *models.StorageJob) (string, int64, error) {
	b, err := u.backendFor(job)
	if err != nil {
		return "", 0, err
	}

	f, err := os.Open(job.Target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, fmt.Errorf("storage: the local file %q is gone, so %s %s has nothing to upload: %w",
				job.Target, job.Kind, job.ID, errPermanent)
		}
		return "", 0, fmt.Errorf("storage: opening %q: %w", job.Target, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("storage: stating %q: %w", job.Target, err)
	}
	if info.IsDir() {
		return "", 0, fmt.Errorf("storage: %q is a directory, not a %s: %w", job.Target, job.Kind, errPermanent)
	}

	key, err := u.keyFor(ctx, job)
	if err != nil {
		return "", 0, err
	}
	if err := b.Put(ctx, key, f, info.Size(), jobContentType(job.Kind)); err != nil {
		return "", 0, fmt.Errorf("storage: putting %s %s to %s: %w", job.Kind, job.ID, key, err)
	}
	return key, info.Size(), nil
}

// deleteLocal is the sweeper's `delete_local` action. The uploader's own upload
// path never reaches it, which is what keeps the source file on disk until the
// retention policy says otherwise.
func (u *Uploader) deleteLocal(job *models.StorageJob) (string, int64, error) {
	if job.Target == "" {
		return job.RemoteKey, job.Bytes, fmt.Errorf("storage: a delete_local job with no local path: %w", errPermanent)
	}
	if err := os.Remove(job.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return job.RemoteKey, job.Bytes, fmt.Errorf("storage: removing the local %s %s: %w", job.Kind, job.ID, err)
	}
	// The key and the byte count stay on the row: after the local copy is gone
	// they are the only record of where the object still is.
	return job.RemoteKey, job.Bytes, nil
}

// deleteRemote is the sweeper's retention action, and the key it deletes is
// `Target`, not `RemoteKey`.
//
// That is the model's own rule — "Target is the local path (upload) or the
// remote key (delete)" — and the two handlers that already enqueue purges
// (S3-03's camera delete, S6-04's event delete) fill `Target` with the key and
// leave `RemoteKey` alone. A worker that read only `RemoteKey` would find it
// empty on every purge those handlers queued and call the job permanently
// failed. `RemoteKey` is kept as the fallback for a job the uploader itself
// settled, where the key is the object's address rather than the thing to remove.
func (u *Uploader) deleteRemote(ctx context.Context, job *models.StorageJob) (string, int64, error) {
	key := job.Target
	if key == "" {
		key = job.RemoteKey
	}
	if key == "" {
		// The row asked for a remote delete and cannot say where the object is.
		// Retrying that twenty times is four days of agreeing with yourself.
		return "", job.Bytes, fmt.Errorf("storage: a delete_remote job with no remote key: %w", errPermanent)
	}
	b, err := u.backendFor(job)
	if err != nil {
		return key, job.Bytes, err
	}
	if err := b.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
		return key, job.Bytes, fmt.Errorf("storage: deleting %s from the tier: %w", key, err)
	}
	return key, job.Bytes, nil
}

// backendFor opens the row the job names.
//
// Note what it does *not* do: re-check `enabled`. The claim is the gate —
// ClaimNext joins the backend row and refuses a disabled one — and a worker that
// checked again after claiming would strand a job in `uploading` with the bytes
// already in flight, which is the one state that needs the stuck-job release to
// recover. The plan's "a disabled backend claims nothing; in-flight jobs finish"
// is that ordering, and this is the half of it that lives here.
func (u *Uploader) backendFor(job *models.StorageJob) (Backend, error) {
	row, err := u.backends.GetByID(job.BackendID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("storage: backend %q is gone, so %s %s cannot reach a tier: %w",
				job.BackendID, job.Kind, job.ID, errPermanent)
		}
		return nil, fmt.Errorf("storage: loading backend %q: %w", job.BackendID, err)
	}
	b, err := u.open(row)
	if err != nil {
		return nil, fmt.Errorf("storage: opening backend %q (%s): %w", row.Name, row.Kind, err)
	}
	return b, nil
}

// keyFor is the layout, called with the facts the job names.
func (u *Uploader) keyFor(ctx context.Context, job *models.StorageJob) (string, error) {
	ref := ObjectRef{Kind: job.Kind, LocalPath: job.Target}
	switch job.Kind {
	case models.JobKindSegment:
		if u.facts == nil {
			return "", fmt.Errorf("storage: a segment key needs the manifest, and this uploader has no JobFacts: %w", errPermanent)
		}
		slug, when, err := u.facts.SegmentFacts(ctx, job.ID)
		if err != nil {
			return "", err
		}
		ref.Slug, ref.When = slug, when
	case models.JobKindClip, models.JobKindSnapshot:
		ref.EventID = eventIDFromJobID(job.ID)
	default:
		return "", fmt.Errorf("storage: %q is not a storage job kind: %w", job.Kind, errPermanent)
	}
	return KeyFor(ref)
}

// eventIDFromJobID recovers the event from a job id.
//
// An event has two objects — its clip and its snapshot — and a job row is keyed
// by its object, so the queue holds them as `<event_id>:clip` and
// `<event_id>:snapshot`. That is the shape S3-03's camera purge and S6-04's
// event delete already enqueue, and the key layout cannot use the job id
// verbatim: `nvr/events/<id>:clip/clip.mp4` is not the plan's layout. A UUID has
// no colon, so cutting at the first one recovers the event whether or not the
// piece suffix is there.
func eventIDFromJobID(id string) string {
	eventID, _, _ := strings.Cut(id, ":")
	return eventID
}

// jobContentType is the type the tier stores, derived from the job's kind rather
// than from the file: the recorder writes mp4 and the snapshot builder writes
// JPEG, and guessing from an extension is guessing about a file name this loop
// does not control. (The fs driver has its own `contentTypeFor`, which reads the
// key — that one answers "what is this file", this one answers "what did the
// queue say".)
func jobContentType(kind string) string {
	switch kind {
	case models.JobKindSnapshot:
		return "image/jpeg"
	default:
		return "video/mp4"
	}
}

// RetryAt is the plan's backoff as a pure function, so the sequence is asserted
// as a list of instants rather than waited out.
//
// `attempts` is the count the claim put on the row, which is why the *first*
// retry is 10 minutes and not 5: the claim that failed is attempt one, and the
// exponent counts attempts already made. The full ladder is 10, 20, 40, 80,
// 160, 320 minutes and then 6 hours for the rest, so a job that never succeeds
// is retried for roughly four and a half days before it lands in `failed`.
func RetryAt(now time.Time, attempts int) time.Time {
	if attempts >= retryCapAttempts {
		return now.Add(retryCap)
	}
	delay := retryBase
	for i := 0; i < attempts; i++ {
		delay *= 2
	}
	if delay > retryCap {
		delay = retryCap
	}
	return now.Add(delay)
}

// errPermanent marks a failure that no retry can fix, so the job stops at
// `failed` now instead of after twenty attempts.
//
// The three cases are all "the row and the world disagree": the local file is
// gone, the backend row is gone, or the job names something no worker
// implements. Each is a state an operator has to change, and each would otherwise
// spend four and a half days failing before it said so.
var errPermanent = errors.New("permanent, will not be retried")
