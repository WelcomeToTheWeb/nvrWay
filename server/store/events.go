package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nvr/server/models"
)

// EventStore provides persistence for detected events.
type EventStore struct {
	db *gorm.DB
}

// EventInput is one detection signal, as the internal API received it.
//
// TS is the **server's** clock at receipt. The worker's own timestamp is
// validated for shape and then discarded: a detection box on a machine whose
// clock is wrong must not be able to file an event in the future, and "when did
// this happen" has exactly one answer in this installation.
type EventInput struct {
	EventKey string
	CameraID string
	Action   string // open | extend | close

	Rule     string
	ZoneID   string
	Severity int
	Classes  []string
	Objects  []models.EventObject

	// DetWidth/DetHeight name the frame `Objects` are pixels of (P-10). The
	// signal intake resolves them from the camera row with the detection
	// tap's own fallback; 0 means "not named" and lands as 0 on the row.
	// A later signal that names the frame fills columns an earlier one left
	// at 0; a signal that names nothing never erases a named frame.
	DetWidth  int
	DetHeight int

	// LocalSnapshotPath is set by the handler on an `open` signal, after it has
	// written the JPEG. The store records where the file is; writing files is
	// not a store method.
	LocalSnapshotPath string

	TS time.Time
}

// ErrEventClosed is the late signal: the event this signal names is already
// closed, and applying it would move a finished event's window. The handler
// answers 200 with {"error":"event closed"} — a worker retrying a buffered
// signal has nothing to retry, and must not be told the ingest is broken.
var ErrEventClosed = errors.New("event closed")

// UpsertByEventKey applies one signal to the event row named by its event_key,
// and answers the row, whether this call created it, and an error.
//
// The state machine, exactly:
//
//	open    → insert (started_at = ended_at = ts, open = true). A key that
//	        already exists is the worker re-sending, so it is returned
//	        unchanged: the same open arriving twice creates one event.
//	extend  → advance ended_at, union classes/zones/rules, append boxes (capped,
//	        oldest trimmed first), take max(severity).
//	close   → open = false, final ended_at.
//
// An `extend`/`close` whose open never arrived creates the row (started_at = ts)
// and applies the signal: the worker's buffer drops oldest on persistent
// failure, and losing the event because its opening signal was dropped is the
// worse outcome.
//
// A signal applied to a closed event mutates nothing and answers
// ErrEventClosed — except a duplicate `close`, which is idempotent and answers
// the row, because a closed event is exactly what a close asks for.
func (s *EventStore) UpsertByEventKey(ctx context.Context, in EventInput) (*models.Event, bool, error) {
	if !models.ValidSignalAction(in.Action) {
		return nil, false, fmt.Errorf("events: action %q is not a signal action", in.Action)
	}
	if in.CameraID == "" || in.EventKey == "" {
		return nil, false, errors.New("events: camera_id and event_key are required")
	}
	// The server clock is the only clock. A zero, a future, or a nonsense ts
	// all land on now, so the invariant "started_at is never in the future"
	// holds whatever a caller passes.
	now := time.Now().UTC()
	ts := in.TS.UTC()
	if ts.IsZero() || ts.After(now) {
		ts = now
	}

	var created bool
	var out *models.Event
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing models.Event
		findErr := tx.Where("event_key = ?", in.EventKey).First(&existing).Error
		if findErr == nil {
			if err := applySignal(tx, &existing, in, ts); err != nil {
				if errors.Is(err, ErrEventClosed) {
					// The closed row travels with the error: the handler answers
					// 200 naming the event the signal referred to, so a worker's
					// retry gets an answer and not a bare failure.
					out = &existing
				}
				return err
			}
			var updated models.Event
			if err := tx.First(&updated, "id = ?", existing.ID).Error; err != nil {
				return err
			}
			out = &updated
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		created = true
		event := &models.Event{
			ID:                uuid.NewString(),
			EventKey:          in.EventKey,
			CameraID:          in.CameraID,
			Action:            in.Action,
			StartedAt:         ts,
			EndedAt:           ts,
			Open:              true,
			ClassesJSON:       "[]",
			ZonesJSON:         "[]",
			RulesJSON:         "[]",
			ObjectsJSON:       "[]",
			Severity:          1,
			SignalCount:       0,
			LocalSnapshotPath: in.LocalSnapshotPath,
		}
		if applyErr := applySignalTo(event, in, ts); applyErr != nil {
			return applyErr
		}
		// gorm fills a zero-valued field from its column `default:` during
		// Create, so an `open = false` row created by a close whose open never
		// arrived is dropped from the INSERT and lands open. The value wanted is
		// captured before the insert and written back after it.
		wantOpen := event.Open
		if err := tx.Create(event).Error; err != nil {
			// A concurrent open of the same key lost the race on the unique
			// index. Its row is the answer, and this signal applies to it.
			if isUniqueViolation(err) {
				created = false
				var raced models.Event
				if findErr := tx.Where("event_key = ?", in.EventKey).First(&raced).Error; findErr != nil {
					return findErr
				}
				return applySignal(tx, &raced, in, ts)
			}
			return err
		}
		if !wantOpen {
			if err := tx.Model(&models.Event{}).Where("id = ?", event.ID).
				Update("open", false).Error; err != nil {
				return err
			}
		}
		// Answer the row as the database holds it, not as the caller built it:
		// the defaults gorm filled in are the values the next read will see.
		var stored models.Event
		if err := tx.First(&stored, "id = ?", event.ID).Error; err != nil {
			return err
		}
		out = &stored
		return nil
	})
	if err != nil {
		// ErrEventClosed travels with the closed row: the handler answers 200
		// naming the event the signal referred to, so a worker's retry gets an
		// answer and not a bare failure.
		if out != nil {
			return out, created, err
		}
		return nil, false, err
	}
	return out, created, nil
}

// applySignal loads a row, applies the signal inside the transaction, and saves
// the row back out.
func applySignal(tx *gorm.DB, existing *models.Event, in EventInput, ts time.Time) error {
	if err := applySignalTo(existing, in, ts); err != nil {
		return err
	}
	updates := map[string]any{
		"action":       existing.Action,
		"started_at":   existing.StartedAt,
		"ended_at":     existing.EndedAt,
		"open":         existing.Open,
		"classes_json": existing.ClassesJSON,
		"zones_json":   existing.ZonesJSON,
		"rules_json":   existing.RulesJSON,
		"objects_json": existing.ObjectsJSON,
		"severity":     existing.Severity,
		"signal_count": existing.SignalCount,
		"updated_at":   ts,
	}
	if existing.LocalSnapshotPath == "" && in.LocalSnapshotPath != "" {
		updates["local_snapshot_path"] = in.LocalSnapshotPath
		existing.LocalSnapshotPath = in.LocalSnapshotPath
	}
	if err := tx.Model(&models.Event{}).Where("id = ?", existing.ID).Updates(updates).Error; err != nil {
		return err
	}
	return nil
}

// applySignalTo is the state machine itself: no database, no driver, just the
// rules the event row must obey.
func applySignalTo(event *models.Event, in EventInput, ts time.Time) error {
	// A closed event is finished. A late `open`/`extend` after close changes
	// nothing and says so; a duplicate `close` is a no-op that answers the row.
	if !event.Open {
		if in.Action == models.SignalActionClose {
			return nil
		}
		return ErrEventClosed
	}

	if in.LocalSnapshotPath != "" && event.LocalSnapshotPath == "" {
		event.LocalSnapshotPath = in.LocalSnapshotPath
	}

	// A duplicate `open` for a live event is the worker re-sending a buffered
	// signal: it changes nothing. Two opens must not extend a window, count
	// twice, or move severity — the open signal is the event's birth certificate,
	// and a birth certificate presented twice is one birth.
	if in.Action == models.SignalActionOpen && event.SignalCount > 0 {
		return nil
	}

	event.SignalCount++
	event.Action = in.Action
	if in.Severity > event.Severity {
		event.Severity = in.Severity
	}

	event.ClassesJSON = unionJSON(event.ClassesJSON, in.Classes)
	if in.ZoneID != "" {
		event.ZonesJSON = unionJSON(event.ZonesJSON, []string{in.ZoneID})
	}
	if in.Rule != "" {
		event.RulesJSON = unionJSON(event.RulesJSON, []string{in.Rule})
	}
	if len(in.Objects) > 0 {
		event.ObjectsJSON = appendObjects(event.ObjectsJSON, in.Objects)
	}
	// The frame the boxes are pixels of (P-10): fill a frame the row does not
	// yet name, never overwrite one it does. A mid-event change of the tap
	// size is the one case this gets one frame wrong, and it is stated in
	// docs/api.md rather than silently papered over.
	if event.DetWidth <= 0 && event.DetHeight <= 0 && in.DetWidth > 0 && in.DetHeight > 0 {
		event.DetWidth = in.DetWidth
		event.DetHeight = in.DetHeight
	}

	switch in.Action {
	case models.SignalActionOpen:
		// An open for a live event extends its window: same key, same event.
		if ts.After(event.EndedAt) {
			event.EndedAt = ts
		}
	case models.SignalActionExtend:
		// Forward only. A replayed extend from the worker's buffer must not
		// pull a live event's window backwards.
		if ts.After(event.EndedAt) {
			event.EndedAt = ts
		}
	case models.SignalActionClose:
		if ts.After(event.EndedAt) {
			event.EndedAt = ts
		}
		event.Open = false
	}
	return nil
}

// unionJSON merges values into a JSON array column, preserving the order
// already stored.
func unionJSON(current string, values []string) string {
	have := models.JSONStringList(current)
	merged := models.AppendUnique(have, values)
	if len(merged) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return current
	}
	return string(encoded)
}

// appendObjects appends boxes to the capped list, trimming oldest-first.
func appendObjects(current string, objects []models.EventObject) string {
	have := parseObjects(current)
	merged := append(have, objects...)
	if len(merged) > models.MaxEventObjects {
		merged = merged[len(merged)-models.MaxEventObjects:]
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return current
	}
	return string(encoded)
}

// ParseObjects decodes an event's ObjectsJSON column.
func ParseObjects(raw string) []models.EventObject {
	return parseObjects(raw)
}

func parseObjects(raw string) []models.EventObject {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var out []models.EventObject
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil
	}
	return out
}

// EventFilter is the event list's query. Zone ids are what the row stores;
// zone **names** resolve through the camera row, which is where the console's
// `q` filter gets its match on a zone name.
type EventFilter struct {
	CameraID string
	From     *time.Time
	To       *time.Time
	Class    string
	ZoneID   string
	Severity *int
	// Q matches class, zone and rule names inside the row's JSON columns.
	Q string
	// QZoneIDs are the zone **ids** whose **name** matched Q. The row stores ids
	// (`zones_json` is the union of the zone ids that contributed), so a search
	// for a zone called "porch" reaches the row only through the ids that name
	// it — and the id↔name pair lives on the camera row, which is why the
	// handler resolves it and hands the ids here (S6-04). An empty list leaves
	// `q` matching classes, ids and rules as it did before.
	QZoneIDs []string
	Limit    int
	// OpenOnly is the clip builder's query: live events need their clip
	// scheduled at close.
	OpenOnly bool
}

// EventListLimit is the default and the ceiling: a timeline request that asks
// for everything is a timeline request that reads the whole table.
const (
	EventListDefaultLimit = 50
	EventListMaxLimit     = 200
)

// List returns events newest-first.
func (s *EventStore) List(f EventFilter, offset int) ([]models.Event, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = EventListDefaultLimit
	}
	if limit > EventListMaxLimit {
		limit = EventListMaxLimit
	}

	// `id` breaks ties: two events opened by the same signal tick share a
	// started_at, and a page boundary that lands between them moves rows onto two
	// pages (or drops them). The console walks pages, so the order must be
	// stable, not merely sorted.
	query := s.db.Order("started_at DESC, id DESC").Limit(limit).Offset(offset)
	if f.CameraID != "" {
		query = query.Where("camera_id = ?", f.CameraID)
	}
	if f.From != nil {
		query = query.Where("ended_at >= ?", *f.From)
	}
	if f.To != nil {
		query = query.Where("started_at <= ?", *f.To)
	}
	if f.Class != "" {
		query = query.Where("classes_json LIKE ? ESCAPE '\\'", "%"+escapeLike(f.Class)+"%")
	}
	if f.ZoneID != "" {
		query = query.Where("zones_json LIKE ? ESCAPE '\\'", "%"+escapeLike(f.ZoneID)+"%")
	}
	if f.Severity != nil {
		query = query.Where("severity >= ?", *f.Severity)
	}
	if f.OpenOnly {
		query = query.Where("open = ?", true)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		like := "%" + escapeLike(q) + "%"
		clause := "classes_json LIKE ? ESCAPE '\\' OR zones_json LIKE ? ESCAPE '\\' OR rules_json LIKE ? ESCAPE '\\'"
		args := []any{like, like, like}
		for _, zoneID := range f.QZoneIDs {
			if zoneID == "" {
				continue
			}
			clause += " OR zones_json LIKE ? ESCAPE '\\'"
			args = append(args, "%"+escapeLike(zoneID)+"%")
		}
		// Parenthesized by hand. GORM joins separate `Where` calls with `AND`, and
		// an unparenthesized `a = ? AND x OR y` reads as `(a AND x) OR y` — which
		// would let one camera's zone match answer for every camera in the table.
		query = query.Where("("+clause+")", args...)
	}

	var events []models.Event
	if err := query.Find(&events).Error; err != nil {
		return nil, err
	}
	return events, nil
}

// ListOpen returns the events still open: the clip builder schedules each one's
// assembly `POST_ROLL_SECONDS` after its window ends, and it needs the list
// after a restart, not only the events it saw while running.
func (s *EventStore) ListOpen() ([]models.Event, error) {
	var events []models.Event
	if err := s.db.Where("open = ?", true).Order("started_at ASC, id ASC").Find(&events).Error; err != nil {
		return nil, err
	}
	return events, nil
}

// GetByID fetches an event.
func (s *EventStore) GetByID(id string) (*models.Event, error) {
	var event models.Event
	if err := s.db.First(&event, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &event, nil
}

// GetByKey fetches an event by its worker-assigned key.
func (s *EventStore) GetByKey(eventKey string) (*models.Event, error) {
	var event models.Event
	if err := s.db.Where("event_key = ?", eventKey).First(&event).Error; err != nil {
		return nil, err
	}
	return &event, nil
}

// The event's media stamps. All five answer gorm.ErrRecordNotFound for an id
// that matches no row, the same convention the segment stamps use: these are
// called by the clip builder and the uploader through the internal API, and a
// stamp that silently succeeded against an event an operator deleted is a file
// on disk that no row points at.

// SetLocalSnapshot records the snapshot file the handler wrote.
func (s *EventStore) SetLocalSnapshot(id, path string) error {
	return s.stamp(id, map[string]any{"local_snapshot_path": path})
}

// SetLocalClip records an assembled clip. `ClipSeconds` is the measured duration
// the player is told about, not the requested window.
func (s *EventStore) SetLocalClip(id, path string, seconds float64) error {
	return s.stamp(id, map[string]any{
		"local_clip_path": path,
		"clip_seconds":    seconds,
	})
}

// SetClipAbsent records that the window had no segments to assemble from. The
// event stays visible with its snapshot: "no clip" is a stated outcome, not a
// missing event.
func (s *EventStore) SetClipAbsent(id string) error {
	return s.stamp(id, map[string]any{
		"local_clip_path": "",
		"clip_key":        "",
		"clip_seconds":    0,
	})
}

// MarkSnapshotUploaded / MarkClipUploaded record the remote key and the moment
// the copy landed. The sweeper reads these, and so does the playback proxy when
// the local file is gone: nil means "not safe remotely", which is the state the
// sweeper must not delete from.
func (s *EventStore) MarkSnapshotUploaded(id, remoteKey string) error {
	return s.stamp(id, map[string]any{
		"snapshot_key":         remoteKey,
		"snapshot_uploaded_at": time.Now().UTC(),
	})
}

func (s *EventStore) MarkClipUploaded(id, remoteKey string) error {
	return s.stamp(id, map[string]any{
		"clip_key":         remoteKey,
		"clip_uploaded_at": time.Now().UTC(),
	})
}

func (s *EventStore) stamp(id string, columns map[string]any) error {
	result := s.db.Model(&models.Event{}).Where("id = ?", id).Updates(columns)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// OverlapsWindow reports whether any live event's window covers part of
// [from, to].
//
// Events reference segments by **time, not by a foreign key**: the clip builder
// selects whatever segments cover an event's window (`ListRange`), so "is this
// segment still referenced" can only be answered the same way. The caller passes
// the window already widened by the clip's roll margins — a segment that only
// the pre-roll reaches is still footage the clip is built from.
func (s *EventStore) OverlapsWindow(cameraID string, from, to time.Time) (bool, error) {
	var count int64
	err := s.db.Model(&models.Event{}).
		Where("camera_id = ? AND started_at <= ? AND ended_at >= ?", cameraID, to, from).
		Limit(1).Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ListOlderThan returns event rows started before the cutoff, oldest first, up
// to limit. The sweeper's order: it purges an event's media and then deletes
// that batch, so the batch has to terminate on its own and the oldest events are
// the ones whose media is least likely to be wanted.
func (s *EventStore) ListOlderThan(cutoff time.Time, limit int) ([]models.Event, error) {
	if limit <= 0 || limit > EventListMaxLimit {
		limit = EventListMaxLimit
	}
	var events []models.Event
	err := s.db.Where("started_at < ?", cutoff).Order("started_at ASC, id ASC").
		Limit(limit).Find(&events).Error
	if err != nil {
		return nil, err
	}
	return events, nil
}

// DeleteByIDs removes event rows in one statement, after the caller has queued
// their media purge. Counting the rows it actually removed is the point: a sweep
// that reports a number the database did not achieve is a lie on the queue page.
func (s *EventStore) DeleteByIDs(ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	result := s.db.Where("id IN ?", ids).Delete(&models.Event{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// ClearRemoteMedia forgets that these events' clip and snapshot reached the
// tier, once the sweeper has queued their delete_remote jobs. Clearing the
// upload stamps is what makes the playback proxy stop promising a clip that is
// on its way off the bucket: an event past remote retention is a record of what
// happened, not something that plays.
func (s *EventStore) ClearRemoteMedia(ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	result := s.db.Model(&models.Event{}).Where("id IN ?", ids).Updates(map[string]any{
		"clip_key":             "",
		"snapshot_key":         "",
		"clip_uploaded_at":     nil,
		"snapshot_uploaded_at": nil,
		"updated_at":           time.Now().UTC(),
	})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// Delete removes an event row. The handler enqueues the media purge jobs first
// and audits the act; a row deleted out from under its media leaves files no
// one is coming back for.
func (s *EventStore) Delete(id string) error {
	result := s.db.Delete(&models.Event{}, "id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// DeleteOlderThan removes event rows past the event retention window.
func (s *EventStore) DeleteOlderThan(cutoff time.Time) (int64, error) {
	result := s.db.Where("started_at < ?", cutoff).Delete(&models.Event{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// escapeLike neutralises the LIKE wildcards in a caller-supplied filter, so a
// search for "a_b" matches "a_b" and not every three-character string. Every
// LIKE in this package spells out `ESCAPE \\`: sqlite treats a backslash as
// literal unless the escape character is declared, Postgres defaults to it, and
// one spelling that works on both is the only one worth having.
func escapeLike(s string) string {
	out := strings.ReplaceAll(s, "\\", "\\\\")
	out = strings.ReplaceAll(out, "%", "\\%")
	out = strings.ReplaceAll(out, "_", "\\_")
	return out
}
