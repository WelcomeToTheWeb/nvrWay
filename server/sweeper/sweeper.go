package sweeper

// The retention sweeps: the only thing in this installation that destroys
// footage.
//
// Everything else in the server adds — segments, events, clips, queue rows. This
// package removes, which makes it the one whose bugs are measured in lost
// recordings rather than in an error the operator can see. So the rules here are
// written as refusals:
//
//   - footage that never reached the tier is not deleted with the local window.
//     It is kept for `max(3 * local_retention_days, 7 days)` and only then
//     force-deleted, with a counted log line, because a storage outage that
//     destroys the only copy is the one failure a retention policy must never
//     have.
//   - a segment row is deleted only when the footage exists nowhere: no local
//     file, no remote key. While either remains the row is the only index to it,
//     and an object the database forgot is a bill nobody can cancel.
//   - `REMOTE_RETENTION_DAYS=0` means keep forever, and "forever" is a real
//     setting here, not a placeholder for "unset".
//   - an event's media is purged before its row goes, because a row deleted out
//     from under its media leaves objects no one is coming back for.
//
// The seven sweeps are the plan's, in the plan's order, and the order is not
// cosmetic: the remote sweep clears a row's `remote_key` as it queues the
// delete, and the local sweep refuses to delete a row that still has one. So a
// row whose last copy was *just* queued for deletion survives one more sweep —
// deliberately. The delete job has not run yet, and a row deleted in the same
// pass that gave away its last copy would orphan the object if that job failed.
//
// Single-writer rule: this package assumes it owns the media tree. Two
// installatures sharing one `SEGMENTS_DIR` would double-delete, and two
// sweepers on one database would race the retention clock. The deployment is one
// server process (no Redis, one owner of the ffmpeg supervisors), and the sweeper
// is written for that.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
	"nvr/server/store"
)

const (
	// DefaultInterval is the sweep period. Fifteen minutes is short enough that
	// a disk does not fill between passes and long enough that one pass can
	// afford to be a dozen queries.
	DefaultInterval = 15 * time.Minute

	// DefaultBatchLimit bounds what one sweep touches. A batch is not a
	// performance tweak: it is what makes a first sweep after an install (or
	// after a retention change) bounded, so the pass finishes inside the
	// interval instead of running through the next one.
	DefaultBatchLimit = 500

	// unuploadedMultiplier and unuploadedFloorDays are the protection window:
	// `max(3 * local_retention_days, 7 days)`. The multiplier gives a real upload
	// outage three times the window it was supposed to fit inside; the floor
	// matters for a short retention — a 1-day local window would otherwise
	// protect un-uploaded footage for three days, which is shorter than most
	// outages last.
	unuploadedMultiplier = 3
	unuploadedFloorDays  = 7

	// The three housekeeping windows. They are not retention policy — nobody
	// looks for a delivery row from day 29 — they are the reason the tables stay
	// small.
	DefaultWebhookDeliveryRetentionDays = 30
	DefaultAuditRetentionDays           = 365
	DefaultJobRetentionDays             = 30

	// DefaultStuckAfter is how long a job may sit claimed before the sweeper
	// decides nobody is going to settle it. A pass is bounded by five seconds, so
	// a claim still open a quarter of an hour later belongs to a worker that
	// died. `StorageJobStore.ReleaseStuck` had no other caller.
	DefaultStuckAfter = 15 * time.Minute
)

// Config is the retention policy, in days, as the environment states it.
type Config struct {
	// Interval is the sweep period; zero takes DefaultInterval.
	Interval time.Duration
	// LocalRetentionDays is the installation default for the local media tree.
	// A camera's own `local_retention_days` overrides it when it is non-zero.
	LocalRetentionDays int
	// RemoteRetentionDays is the tier's window. **Zero means keep forever.**
	RemoteRetentionDays int
	// EventRetentionDays bounds the event rows, and with them the footage an
	// event was built from.
	EventRetentionDays int

	WebhookDeliveryRetentionDays int
	AuditRetentionDays           int
	JobRetentionDays             int

	// PreRollSeconds / PostRollSeconds widen an event's window when the sweeper
	// asks whether a segment is still referenced: the clip is cut from the event
	// window plus its roll margins, so a segment only the pre-roll reaches is
	// still footage that clip is built from.
	PreRollSeconds  int
	PostRollSeconds int

	// BatchLimit bounds one sweep; zero takes DefaultBatchLimit.
	BatchLimit int
	// StuckAfter is the claim age at which a job is returned to the queue.
	StuckAfter time.Duration

	// Log receives one summary line per sweep. nil takes the default logger.
	Log *slog.Logger
	// Now is the clock; nil takes time.Now.
	Now func() time.Time
}

// Report is what one sweep did, and is the payload of the `storage.queue`
// message. The operator sees disk discipline as a number, not a hope.
type Report struct {
	// LocalDeleted is local files removed inside a camera's local window. Every
	// such segment had reached the tier — that is what made it eligible — so the
	// row usually stays behind as the index to the tier copy.
	LocalDeleted int64 `json:"local_deleted"`
	// ForceDeleted is un-uploaded footage destroyed past the protection window.
	// A non-zero number here means an upload outage outlasted the protection,
	// which is the number to watch.
	ForceDeleted int64 `json:"force_deleted"`
	// EventSegmentsDeleted is footage an event was built from, retired with the
	// event's retention — the `record_mode=events` case the local sweep does not
	// cover.
	EventSegmentsDeleted int64 `json:"event_segments_deleted"`
	// SegmentRowsDeleted is rows forgotten because the footage exists nowhere:
	// no local file, no remote key, no live event that can reach the window.
	// Files and rows part company on purpose — a row outlives its file whenever
	// the tier still holds the object, and outlives the tier copy until event
	// retention, because the timeline is allowed to show what was recorded.
	SegmentRowsDeleted int64 `json:"segment_rows_deleted"`
	// EventMediaPurged is local clip and snapshot files removed with their event
	// rows.
	EventMediaPurged int64 `json:"event_media_purged"`
	EventsDeleted    int64 `json:"events_deleted"`
	// RemoteQueued is delete_remote jobs put on the queue by the tier sweep. It
	// counts what was *queued*, not what was deleted: the queue does the
	// deleting, and its own state is the number that proves it.
	RemoteQueued int64 `json:"remote_queued"`

	WebhookDeliveriesPruned int64 `json:"webhook_deliveries_pruned"`
	AuditPruned             int64 `json:"audit_pruned"`
	JobsPruned              int64 `json:"jobs_pruned"`
	StuckReleased           int64 `json:"stuck_released"`

	// Queue is the queue as the sweep left it, so the one message the storage
	// page listens to carries what the storage page shows.
	Queue *store.QueueCounts `json:"queue,omitempty"`

	// Problems are the failures that did not stop the sweep. They are on the
	// message because a sweep that quietly skipped half its candidates looks
	// exactly like a sweep with nothing to do.
	Problems []string `json:"problems,omitempty"`
}

// Sweeper runs the retention policy.
type Sweeper struct {
	st  *store.Store
	cfg Config
	log *slog.Logger
	now func() time.Time

	// broadcast publishes the report as `storage.queue`. It is a function rather
	// than a *ws.Hub so this package does not depend on the WebSocket layer, and
	// so a test can capture the payload instead of a log line.
	broadcast func(any) error
}

// New builds the sweeper. `broadcast` may be nil.
func New(st *store.Store, cfg Config, broadcast func(any) error) *Sweeper {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = DefaultBatchLimit
	}
	if cfg.WebhookDeliveryRetentionDays <= 0 {
		cfg.WebhookDeliveryRetentionDays = DefaultWebhookDeliveryRetentionDays
	}
	if cfg.AuditRetentionDays <= 0 {
		cfg.AuditRetentionDays = DefaultAuditRetentionDays
	}
	if cfg.JobRetentionDays <= 0 {
		cfg.JobRetentionDays = DefaultJobRetentionDays
	}
	if cfg.StuckAfter <= 0 {
		cfg.StuckAfter = DefaultStuckAfter
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Sweeper{st: st, cfg: cfg, log: cfg.Log, now: cfg.Now, broadcast: broadcast}
}

// Interval is the configured sweep period, for the boot line and the caller.
func (s *Sweeper) Interval() time.Duration { return s.cfg.Interval }

// Run sweeps until the context is done.
func (s *Sweeper) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep runs the seven sweeps and reports.
//
// It returns no error: a sweep that could not finish has said so in `Problems`,
// and the caller's only options are to log it (done) and to try again in fifteen
// minutes (also done).
func (s *Sweeper) Sweep(ctx context.Context) Report {
	rep := Report{}

	// First, before anything looks at the queue: a claim still open from a
	// worker that died would otherwise hold a job out of every later sweep.
	if n, err := s.st.StorageJobs.ReleaseStuck(s.now().Add(-s.cfg.StuckAfter)); err != nil {
		rep.problem("release stuck jobs", err)
	} else {
		rep.StuckReleased = n
	}

	s.sweepLocal(ctx, &rep)
	s.sweepEventSegments(ctx, &rep)
	s.sweepEvents(ctx, &rep)
	s.sweepRemote(ctx, &rep)
	s.pruneWebhookDeliveries(&rep)
	s.pruneAudit(&rep)
	s.pruneJobs(&rep)

	if counts, err := s.st.StorageJobs.Counts(""); err != nil {
		rep.problem("reading the queue counts", err)
	} else {
		rep.Queue = counts
	}

	fields := []any{
		"local_deleted", rep.LocalDeleted,
		"force_deleted", rep.ForceDeleted,
		"event_segments_deleted", rep.EventSegmentsDeleted,
		"segment_rows_deleted", rep.SegmentRowsDeleted,
		"event_media_purged", rep.EventMediaPurged,
		"events_deleted", rep.EventsDeleted,
		"remote_queued", rep.RemoteQueued,
		"webhook_deliveries_pruned", rep.WebhookDeliveriesPruned,
		"audit_pruned", rep.AuditPruned,
		"jobs_pruned", rep.JobsPruned,
		"stuck_released", rep.StuckReleased,
	}
	if rep.ForceDeleted > 0 {
		// The one count that means footage was destroyed without a copy
		// surviving, so it gets its own line at a level an operator can filter
		// on rather than hiding inside the summary.
		s.log.Warn("storage sweep force-deleted un-uploaded footage",
			"force_deleted", rep.ForceDeleted,
			"protection_window", s.protectionWindow(days(s.cfg.LocalRetentionDays)).String())
	}
	if len(rep.Problems) > 0 {
		fields = append(fields, "problems", len(rep.Problems))
		s.log.Warn("storage sweep", append(fields, "detail", rep.Problems)...)
	} else {
		s.log.Info("storage sweep", fields...)
	}

	if s.broadcast != nil {
		if err := s.broadcast(rep); err != nil {
			s.log.Warn("storage.queue broadcast failed", "error", err)
		}
	}
	return rep
}

// sweepLocal is sweep (1): the local media tree, past each camera's window.
//
// The query runs at the shortest window in the installation and each row is then
// checked against its own camera's, because retention is per camera and the
// store's list is per cutoff. One query, exact per-camera semantics.
func (s *Sweeper) sweepLocal(ctx context.Context, rep *Report) {
	windows, shortest := s.localWindows()
	if shortest <= 0 {
		return
	}

	segments, err := s.st.Segments.ListLocalOlderThan(s.now().Add(-shortest), s.cfg.BatchLimit)
	if err != nil {
		rep.problem("listing local segments past retention", err)
		return
	}

	var gone []string
	for _, seg := range segments {
		if err := ctx.Err(); err != nil {
			rep.problem("local sweep interrupted", err)
			return
		}
		window, ok := windows[seg.CameraID]
		if !ok {
			// The camera row is gone — a deleted camera's segments outlive it
			// until the purge takes them — so the installation default governs.
			window = shortest
		}
		if !seg.StartTS.Before(s.now().Add(-window)) {
			continue
		}

		if s.stillReferenced(seg) {
			// An event can still rebuild its clip from this window, so the local
			// window does not apply to it at all: the event's own retention
			// decides, in sweep (2). The check comes **before** the file is
			// touched, because protecting the row and deleting the footage is
			// not protection.
			continue
		}
		if s.removeLocalFile(seg.Path, rep) {
			rep.LocalDeleted++
		}
		if seg.RemoteKey != "" {
			// The tier still holds it, and the row is the index to that copy.
			if err := s.st.Segments.MarkLocalDeleted(seg.ID); err != nil {
				rep.problem(fmt.Sprintf("marking segment %s locally deleted", seg.ID), err)
			}
			continue
		}
		gone = append(gone, seg.ID)
	}

	if n, err := s.st.Segments.DeleteByIDs(gone); err != nil {
		rep.problem("deleting retired segment rows", err)
	} else {
		rep.SegmentRowsDeleted += n
	}

	// The protected set: footage that never reached the tier. It is kept far
	// past the window, and what is removed past *that* is counted separately,
	// because a non-zero number is an outage report.
	protected := s.protectionWindow(shortest)
	unuploaded, err := s.st.Segments.ListUnuploadedOlderThan(s.now().Add(-protected), s.cfg.BatchLimit)
	if err != nil {
		rep.problem("listing un-uploaded segments", err)
		return
	}

	gone = gone[:0]
	for _, seg := range unuploaded {
		window, ok := windows[seg.CameraID]
		if !ok {
			window = shortest
		}
		if !seg.StartTS.Before(s.now().Add(-s.protectionWindow(window))) {
			continue
		}
		if s.stillReferenced(seg) {
			// Even past the protection window, footage an event can still reach
			// waits for the event's own sweep. Destroying it here would take the
			// only copy of an event the console is still showing.
			continue
		}
		if s.removeLocalFile(seg.Path, rep) {
			rep.ForceDeleted++
		}
		gone = append(gone, seg.ID)
	}
	if n, err := s.st.Segments.DeleteByIDs(gone); err != nil {
		rep.problem("deleting force-retired segment rows", err)
	} else {
		rep.SegmentRowsDeleted += n
	}
}

// sweepEventSegments is sweep (2): the footage an event was built from, retired
// with the event's retention.
//
// This is the sweep that catches `record_mode=events` installations. The local
// sweep is continuous-only, so an event-triggered camera's segments appear in no
// other list; without this pass their rows and files would only ever grow.
func (s *Sweeper) sweepEventSegments(ctx context.Context, rep *Report) {
	if s.cfg.EventRetentionDays <= 0 {
		return
	}
	cutoff := s.now().Add(-days(s.cfg.EventRetentionDays))

	segments, err := s.st.Segments.ListPastRetention(cutoff, s.cfg.BatchLimit)
	if err != nil {
		rep.problem("listing segments past event retention", err)
		return
	}

	var gone []string
	for _, seg := range segments {
		if err := ctx.Err(); err != nil {
			rep.problem("event segment sweep interrupted", err)
			return
		}
		if s.stillReferenced(seg) {
			// The event row is still here, so this sweep cannot take the
			// footage it was built from. Sweep (3) removes that row moments
			// later, and the next pass takes the footage. One extra pass is the
			// price of never deleting what the database still points at.
			continue
		}
		if seg.LocalDeletedAt == nil && s.removeLocalFile(seg.Path, rep) {
			rep.EventSegmentsDeleted++
		}
		if seg.RemoteKey != "" {
			if seg.LocalDeletedAt == nil {
				if err := s.st.Segments.MarkLocalDeleted(seg.ID); err != nil {
					rep.problem(fmt.Sprintf("marking segment %s locally deleted", seg.ID), err)
				}
			}
			continue
		}
		// Nothing left: no file, no remote key, no event that can reach it.
		gone = append(gone, seg.ID)
	}
	if n, err := s.st.Segments.DeleteByIDs(gone); err != nil {
		rep.problem("deleting event segment rows", err)
	} else {
		rep.SegmentRowsDeleted += n
	}
}

// sweepEvents is sweep (3): event rows past EVENT_RETENTION_DAYS, media first.
//
// The media purge is what makes the order matter. An event row is the only thing
// that names its clip and snapshot keys, so deleting the row first would leave
// two objects on the tier with nothing pointing at them.
func (s *Sweeper) sweepEvents(ctx context.Context, rep *Report) {
	if s.cfg.EventRetentionDays <= 0 {
		return
	}
	cutoff := s.now().Add(-days(s.cfg.EventRetentionDays))

	for {
		if err := ctx.Err(); err != nil {
			rep.problem("event sweep interrupted", err)
			return
		}
		events, err := s.st.Events.ListOlderThan(cutoff, s.cfg.BatchLimit)
		if err != nil {
			rep.problem("listing events past retention", err)
			return
		}
		if len(events) == 0 {
			return
		}

		backend, err := s.st.StorageBackends.GetDefault()
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			rep.problem("reading the default backend for the event purge", err)
		}

		ids := make([]string, 0, len(events))
		for i := range events {
			ev := events[i]
			for _, piece := range []struct {
				key   string
				local string
			}{{ev.ClipKey, ev.LocalClipPath}, {ev.SnapshotKey, ev.LocalSnapshotPath}} {
				if piece.local == "" {
					continue
				}
				if err := os.Remove(piece.local); err != nil && !errors.Is(err, os.ErrNotExist) {
					rep.problem(fmt.Sprintf("removing event media %s", piece.local), err)
					continue
				}
				rep.EventMediaPurged++
			}

			// Two jobs, two ids — `<event_id>:clip` and `<event_id>:snapshot` —
			// because a job row is keyed by its object and an event has two of
			// them. This is the shape S3-03's camera purge and S6-04's event
			// delete already enqueue, and `Target` carries the key for the same
			// reason they do.
			if backend != nil {
				for _, piece := range []struct {
					jobID, kind, key string
				}{
					{ev.ID + ":clip", models.JobKindClip, ev.ClipKey},
					{ev.ID + ":snapshot", models.JobKindSnapshot, ev.SnapshotKey},
				} {
					if piece.key == "" {
						continue
					}
					if err := s.st.StorageJobs.Enqueue(store.JobInput{
						ID: piece.jobID, Kind: piece.kind, Target: piece.key,
						BackendID: backend.ID, Action: models.JobActionDeleteRemote,
					}); err != nil {
						rep.problem(fmt.Sprintf("queueing the purge of %s", piece.jobID), err)
					}
				}
			}
			ids = append(ids, ev.ID)
		}

		n, err := s.st.Events.DeleteByIDs(ids)
		if err != nil {
			rep.problem("deleting event rows", err)
			return
		}
		rep.EventsDeleted += n
		if len(events) < s.cfg.BatchLimit {
			return
		}
	}
}

// sweepRemote is sweep (4): the tier's own retention.
//
// `REMOTE_RETENTION_DAYS=0` means keep forever, and the sweep says so by doing
// nothing — not by treating zero as "no window configured" and deleting
// everything, which is the mistake the zero-is-a-real-value rule exists to
// prevent.
func (s *Sweeper) sweepRemote(ctx context.Context, rep *Report) {
	if s.cfg.RemoteRetentionDays <= 0 {
		return
	}
	cutoff := s.now().Add(-days(s.cfg.RemoteRetentionDays))

	backend, err := s.st.StorageBackends.GetDefault()
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			rep.problem("reading the default backend for the remote sweep", err)
		}
		return
	}

	segments, err := s.st.Segments.ListRemoteOlderThan(cutoff, s.cfg.BatchLimit)
	if err != nil {
		rep.problem("listing segments past remote retention", err)
		return
	}

	ids := make([]string, 0, len(segments))
	for _, seg := range segments {
		if err := ctx.Err(); err != nil {
			rep.problem("remote sweep interrupted", err)
			return
		}
		if err := s.st.StorageJobs.Enqueue(store.JobInput{
			ID: seg.ID, Kind: models.JobKindSegment, Target: seg.RemoteKey,
			BackendID: backend.ID, Action: models.JobActionDeleteRemote, Bytes: seg.Bytes,
		}); err != nil {
			rep.problem(fmt.Sprintf("queueing the remote delete of segment %s", seg.ID), err)
			continue
		}
		ids = append(ids, seg.ID)
	}
	if n, err := s.st.Segments.ClearRemoteKeys(ids); err != nil {
		rep.problem("clearing remote keys", err)
	} else {
		rep.RemoteQueued += n
	}

	// Event media follows the same window, so `REMOTE_RETENTION_DAYS` means the
	// same thing for a clip as for a segment. The consequence is worth stating
	// plainly: an event older than the tier's window is a record of what
	// happened, not something that plays, and its row says so by having no upload
	// stamps.
	events, err := s.st.Events.ListOlderThan(cutoff, s.cfg.BatchLimit)
	if err != nil {
		rep.problem("listing events past remote retention", err)
		return
	}
	ids = ids[:0]
	for _, ev := range events {
		if ev.ClipKey == "" && ev.SnapshotKey == "" {
			continue
		}
		for _, piece := range []struct {
			jobID, kind, key string
		}{
			{ev.ID + ":clip", models.JobKindClip, ev.ClipKey},
			{ev.ID + ":snapshot", models.JobKindSnapshot, ev.SnapshotKey},
		} {
			if piece.key == "" {
				continue
			}
			if err := s.st.StorageJobs.Enqueue(store.JobInput{
				ID: piece.jobID, Kind: piece.kind, Target: piece.key,
				BackendID: backend.ID, Action: models.JobActionDeleteRemote,
			}); err != nil {
				rep.problem(fmt.Sprintf("queueing the purge of %s", piece.jobID), err)
			}
		}
		ids = append(ids, ev.ID)
	}
	if _, err := s.st.Events.ClearRemoteMedia(ids); err != nil {
		rep.problem("clearing event media stamps", err)
	}
}

// pruneWebhookDeliveries is sweep (5).
func (s *Sweeper) pruneWebhookDeliveries(rep *Report) {
	n, err := s.st.WebhookDeliveries.PruneOlderThan(s.now().Add(-days(s.cfg.WebhookDeliveryRetentionDays)))
	if err != nil {
		rep.problem("pruning webhook deliveries", err)
		return
	}
	rep.WebhookDeliveriesPruned = n
}

// pruneAudit is sweep (6).
func (s *Sweeper) pruneAudit(rep *Report) {
	n, err := s.st.Audit.PruneOlderThan(s.now().Add(-days(s.cfg.AuditRetentionDays)))
	if err != nil {
		rep.problem("pruning the audit ledger", err)
		return
	}
	rep.AuditPruned = n
}

// pruneJobs is sweep (7): the queue is the upload log while it is running, and
// history after a month.
func (s *Sweeper) pruneJobs(rep *Report) {
	n, err := s.st.StorageJobs.PruneUploaded(s.now().Add(-days(s.cfg.JobRetentionDays)))
	if err != nil {
		rep.problem("pruning uploaded queue rows", err)
		return
	}
	rep.JobsPruned = n
}

// removeLocalFile deletes one file and reports whether a file was actually
// taken, which is what the counts measure: "how much did the sweep reclaim",
// not "how many rows did it look at".
//
// A file that is already gone is not a failure. The row is the thing that is
// wrong in that case, and the sweep fixes the row on this pass.
func (s *Sweeper) removeLocalFile(path string, rep *Report) bool {
	if path == "" {
		return false
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		rep.problem(fmt.Sprintf("removing %s", path), err)
		return false
	}
	return true
}

// stillReferenced asks whether a live event can still reach this segment's
// window, widened by the clip's roll margins.
func (s *Sweeper) stillReferenced(seg models.Segment) bool {
	from := seg.StartTS
	to := seg.EndTS
	// The event window is what the clip is cut from, so a segment is referenced
	// by any event whose window plus its margins overlaps it.
	from = from.Add(-time.Duration(s.cfg.PostRollSeconds) * time.Second)
	to = to.Add(time.Duration(s.cfg.PreRollSeconds) * time.Second)

	hit, err := s.st.Events.OverlapsWindow(seg.CameraID, from, to)
	if err != nil {
		// An unreadable answer protects the footage. The sweeper's one rule is
		// that it does not destroy what it could not check.
		return true
	}
	return hit
}

// localWindows returns each camera's local window and the **shortest** in the
// installation, which is the cutoff the candidate query runs at.
//
// Shortest, not longest: the query is "what could be eligible anywhere", and a
// camera with a one-day window makes footage eligible a day later. A query run
// at the longest window would never return that camera's rows at all, and the
// per-camera check downstream could not fix a candidate list that never
// contained them.
func (s *Sweeper) localWindows() (map[string]time.Duration, time.Duration) {
	def := days(s.cfg.LocalRetentionDays)
	windows := map[string]time.Duration{}
	shortest := def

	cameras, err := s.st.Cameras.ListAll()
	if err != nil {
		// The default still governs. A camera whose row cannot be read is not a
		// reason to delete nothing at all, and it is not a reason to delete
		// everything either: the installation window is the honest fallback.
		s.log.Warn("per-camera retention unreadable; using the installation default", "error", err)
		return windows, shortest
	}
	for _, cam := range cameras {
		window := def
		if cam.LocalRetentionDays > 0 {
			window = days(cam.LocalRetentionDays)
		}
		windows[cam.ID] = window
		if window < shortest {
			shortest = window
		}
	}
	return windows, shortest
}

// UnuploadedProtectionDays is the un-uploaded protection window in whole days:
// `max(3 * localDays, 7)`. It is exported because the Settings page reports the
// window the sweep will actually honour, and a second copy of the formula there
// would make two authors of a promise this package keeps.
func UnuploadedProtectionDays(localDays int) int {
	protected := localDays * unuploadedMultiplier
	if protected < unuploadedFloorDays {
		return unuploadedFloorDays
	}
	return protected
}

// protectionWindow is how long un-uploaded footage is kept past its own window:
// `max(3 * local_retention_days, 7 days)`. Every caller passes a window built
// by `days`, so the whole-day arithmetic of `UnuploadedProtectionDays` is
// exact, and the formula lives in exactly one place.
func (s *Sweeper) protectionWindow(local time.Duration) time.Duration {
	return days(UnuploadedProtectionDays(int(local / (24 * time.Hour))))
}

func days(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * 24 * time.Hour
}

// problem records a failure that did not stop the sweep.
func (r *Report) problem(what string, err error) {
	if len(r.Problems) >= 16 {
		return
	}
	r.Problems = append(r.Problems, fmt.Sprintf("%s: %v", what, err))
}
