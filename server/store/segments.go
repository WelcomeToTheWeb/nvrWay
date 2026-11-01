package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nvr/server/models"
)

// SegmentStore provides persistence for recorded segments.
type SegmentStore struct {
	db *gorm.DB
}

// Create inserts a segment row. The row **is** the manifest: the file exists
// because this row says where and for what window, and the clip builder and the
// sweeper both act on it. A segment written to disk with no row is invisible
// footage that ages out as an orphan file.
func (s *SegmentStore) Create(seg *models.Segment) error {
	if seg.ID == "" {
		seg.ID = uuid.NewString()
	}
	if seg.Kind == "" {
		seg.Kind = models.SegmentKindContinuous
	}
	return s.db.Create(seg).Error
}

// GetByID fetches a segment.
func (s *SegmentStore) GetByID(id string) (*models.Segment, error) {
	var seg models.Segment
	if err := s.db.First(&seg, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &seg, nil
}

// SegmentListMaxLimit is the manifest's ceiling: the timeline scrubber asks for
// a window, and a window that names a year should be many requests, not one
// large one.
const SegmentListMaxLimit = 2000

// ListRange returns the segments covering [from, to] — overlapping, of any kind,
// ordered by start. This is the timeline manifest and the clip builder's
// selection: a clip window that starts mid-segment needs that segment, and a
// `continuous` segment is footage exactly like an `event` one.
func (s *SegmentStore) ListRange(cameraID string, from, to time.Time, limit int) ([]models.Segment, error) {
	if limit <= 0 || limit > SegmentListMaxLimit {
		limit = SegmentListMaxLimit
	}
	query := s.db.Where("end_ts > ? AND start_ts < ?", from, to)
	if cameraID != "" {
		query = query.Where("camera_id = ?", cameraID)
	}
	var segments []models.Segment
	if err := query.Order("start_ts ASC, id ASC").Limit(limit).Find(&segments).Error; err != nil {
		return nil, err
	}
	return segments, nil
}

// ListRecentByCamera returns a camera's newest segments, newest first: the
// live tail of the timeline.
func (s *SegmentStore) ListRecentByCamera(cameraID string, limit int) ([]models.Segment, error) {
	if limit <= 0 || limit > SegmentListMaxLimit {
		limit = SegmentListMaxLimit
	}
	var segments []models.Segment
	if err := s.db.Where("camera_id = ?", cameraID).
		Order("start_ts DESC, id DESC").Limit(limit).Find(&segments).Error; err != nil {
		return nil, err
	}
	return segments, nil
}

// MaxSegmentNumber answers "what is the last segment number this camera's
// current generation wrote", which is how a restarted recorder picks its
// `-segment_start_number`: continuing, not overwriting.
func (s *SegmentStore) MaxSegmentNumber(cameraID string, generation int) (int, error) {
	if cameraID == "" {
		return 0, errors.New("segments: camera_id is required")
	}
	// The number lives in the file name (`<slug>-<generation>-<n>.mp4`), which
	// is the only place it exists between ffmpeg runs; the query is bounded by
	// the generation, so it walks this camera's segments and nothing else.
	var paths []string
	if err := s.db.Model(&models.Segment{}).
		Where("camera_id = ? AND generation = ?", cameraID, generation).
		Order("start_ts DESC").Limit(5000).Pluck("path", &paths).Error; err != nil {
		return 0, err
	}
	best := -1
	for _, path := range paths {
		if n := segmentNumberFromPath(path, generation); n >= 0 && n > best {
			best = n
		}
	}
	return best, nil
}

// segmentNumberFromPath parses the `<n>` of a `<slug>-<generation>-<n>.mp4` file
// name, and requires the generation to match so a camera that changed its
// generation cannot have the two number spaces mixed. It is not a clock:
// nothing about *when* a segment ran is read from a name, because that is what
// the segment list and the row carry.
func segmentNumberFromPath(path string, generation int) int {
	base := path
	if i := lastSlash(base); i >= 0 {
		base = base[i+1:]
	}
	const suffix = ".mp4"
	if len(base) <= len(suffix) || base[len(base)-len(suffix):] != suffix {
		return -1
	}
	stem := base[:len(base)-len(suffix)]

	// …-<n>
	a := lastRune(stem, '-')
	if a < 0 {
		return -1
	}
	digits := stem[a+1:]
	n, ok := parseUint(digits)
	if !ok {
		return -1
	}
	// …-<generation>
	stem = stem[:a]
	b := lastRune(stem, '-')
	if b < 0 {
		return -1
	}
	gen, ok := parseUint(stem[b+1:])
	if !ok || gen != generation {
		return -1
	}
	return n
}

func parseUint(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func lastRune(s string, r byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == r {
			return i
		}
	}
	return -1
}

// MarkUploaded records the remote key and the moment the copy landed. The
// local file stays: local deletion is the sweeper's job, never the uploader's.
func (s *SegmentStore) MarkUploaded(id, remoteKey string) error {
	result := s.db.Model(&models.Segment{}).Where("id = ?", id).Updates(map[string]any{
		"remote_key":  remoteKey,
		"uploaded_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// MarkLocalDeleted records that the local file is gone while the row is kept
// for the events that reference the window it covers.
func (s *SegmentStore) MarkLocalDeleted(id string) error {
	result := s.db.Model(&models.Segment{}).Where("id = ?", id).Update("local_deleted_at", time.Now().UTC())
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ListLocalOlderThan returns continuous segments past a camera's local
// retention: the sweeper's delete candidates. Uploaded only — the protected
// set is ListUnuploadedOlderThan.
func (s *SegmentStore) ListLocalOlderThan(cutoff time.Time, limit int) ([]models.Segment, error) {
	return s.list(cutoff, true, limit)
}

// ListUnuploadedOlderThan returns segments whose upload has not landed. The
// sweeper must keep these far past the local window: a storage outage that
// deletes un-uploaded footage destroys the only copy, which is the one failure
// a retention policy must never have.
func (s *SegmentStore) ListUnuploadedOlderThan(cutoff time.Time, limit int) ([]models.Segment, error) {
	return s.list(cutoff, false, limit)
}

func (s *SegmentStore) list(cutoff time.Time, uploaded bool, limit int) ([]models.Segment, error) {
	if limit <= 0 || limit > SegmentListMaxLimit {
		limit = SegmentListMaxLimit
	}
	query := s.db.Where("kind = ? AND start_ts < ? AND local_deleted_at IS NULL", models.SegmentKindContinuous, cutoff)
	if uploaded {
		query = query.Where("uploaded_at IS NOT NULL")
	} else {
		query = query.Where("uploaded_at IS NULL")
	}
	var segments []models.Segment
	// `id` breaks ties. The sweeper's batch is bounded, so this order decides
	// which rows a sweep touches: two segments sharing a start_ts across a batch
	// boundary would otherwise be picked in an order the database chooses, and a
	// sweep whose progress depends on which row the engine felt like returning is
	// not a sweep an operator can read off a log line.
	if err := query.Order("start_ts ASC, id ASC").Limit(limit).Find(&segments).Error; err != nil {
		return nil, err
	}
	return segments, nil
}

// DeleteByIDs removes segment rows. The caller deleted the files; the rows go
// with them, in one statement, so a sweep cannot report a count it did not
// achieve.
func (s *SegmentStore) DeleteByIDs(ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	result := s.db.Where("id IN ?", ids).Delete(&models.Segment{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// CountByCamera reports how many segments a camera has and how many are
// un-uploaded: the numbers `scripts/verify.sh` watches grow, and the numbers
// the storage.queue message reports.
func (s *SegmentStore) CountByCamera(cameraID string) (int64, int64, error) {
	var total, unuploaded int64
	query := s.db.Model(&models.Segment{})
	if cameraID != "" {
		query = query.Where("camera_id = ?", cameraID)
	}
	if err := query.Count(&total).Error; err != nil {
		return 0, 0, err
	}
	unup := s.db.Model(&models.Segment{})
	if cameraID != "" {
		unup = unup.Where("camera_id = ?", cameraID)
	}
	if err := unup.Where("uploaded_at IS NULL").Count(&unuploaded).Error; err != nil {
		return 0, 0, err
	}
	return total, unuploaded, nil
}

// CountSince counts segments created after a moment: "is recording live" is a
// count that increases, observed over a window.
func (s *SegmentStore) CountSince(since time.Time) (int64, error) {
	var count int64
	if err := s.db.Model(&models.Segment{}).Where("created_at > ?", since).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}
