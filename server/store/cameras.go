package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nvr/server/models"
)

// CameraStore provides persistence for cameras.
type CameraStore struct {
	db *gorm.DB
}

// Create inserts a camera, minting its id when the caller did not supply one.
//
// Slug uniqueness is the constraint the whole media path depends on: the slug is
// the go2rtc stream name and the segment file prefix, so two cameras sharing one
// slug record into each other. The unique index is the gate; this returns a
// named error so the handler answers 409 rather than a driver string.
func (s *CameraStore) Create(camera *models.Camera) error {
	if camera.ID == "" {
		camera.ID = uuid.NewString()
	}
	active := camera.Active
	if err := s.db.Create(camera).Error; err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: slug %q", ErrSlugTaken, camera.Slug)
		}
		return err
	}
	// `active` defaults to true in the schema, and gorm fills the struct with
	// that default on insert, so the value wanted is captured above and written
	// back: a camera added switched off must stay switched off.
	if !active {
		camera.Active = false
		return writeZeroColumns(s.db, camera, map[string]any{"active": false})
	}
	return nil
}

// ErrSlugTaken is the slug collision, named for the handler's 409.
var ErrSlugTaken = errors.New("slug already in use")

// GetByID fetches a camera by id.
func (s *CameraStore) GetByID(id string) (*models.Camera, error) {
	var camera models.Camera
	if err := s.db.First(&camera, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &camera, nil
}

// GetBySlug fetches a camera by its stream name.
func (s *CameraStore) GetBySlug(slug string) (*models.Camera, error) {
	var camera models.Camera
	if err := s.db.First(&camera, "slug = ?", slug).Error; err != nil {
		return nil, err
	}
	return &camera, nil
}

// ListAll returns every camera, newest first.
func (s *CameraStore) ListAll() ([]models.Camera, error) {
	var cameras []models.Camera
	if err := s.db.Order("created_at DESC").Find(&cameras).Error; err != nil {
		return nil, err
	}
	return cameras, nil
}

// ListActive returns the cameras that are switched on: the set go2rtc's config
// is rendered from, and the set the recorder supervises.
func (s *CameraStore) ListActive() ([]models.Camera, error) {
	var cameras []models.Camera
	if err := s.db.Where("active = ?", true).Order("slug ASC").Find(&cameras).Error; err != nil {
		return nil, err
	}
	return cameras, nil
}

// ListDetectionEnabled returns the cameras the detection worker is pointed at:
// active and detection-enabled. The internal config endpoint answers only these,
// so a camera switched off stops being inferred within one config tick.
func (s *CameraStore) ListDetectionEnabled() ([]models.Camera, error) {
	var cameras []models.Camera
	if err := s.db.Where("active = ? AND detection_enabled = ?", true, true).Order("slug ASC").Find(&cameras).Error; err != nil {
		return nil, err
	}
	return cameras, nil
}

// ListUpdatedSince returns cameras whose row changed after `since`.
//
// This is the recorder's config poll (every 10 s): a recorder that never notices
// a camera edit is a recorder keeping the old recording mode after the operator
// changed it.
func (s *CameraStore) ListUpdatedSince(since time.Time) ([]models.Camera, error) {
	var cameras []models.Camera
	if err := s.db.Where("updated_at > ?", since).Order("slug ASC").Find(&cameras).Error; err != nil {
		return nil, err
	}
	return cameras, nil
}

// Update writes the mutable fields of a camera row. The field list is explicit:
// a whole-row Save would let a PUT that omitted a column silently zero it —
// retention days, and the detection floors, are exactly the columns a partial
// client payload tends to omit.
func (s *CameraStore) Update(camera *models.Camera) error {
	result := s.db.Model(&models.Camera{}).Where("id = ?", camera.ID).Updates(map[string]any{
		"slug":                  camera.Slug,
		"name":                  camera.Name,
		"source_uri":            camera.SourceURI,
		"record_mode":           camera.RecordMode,
		"detection_enabled":     camera.DetectionEnabled,
		"detection_fps":         camera.DetectionFPS,
		"detection_width":       camera.DetectionWidth,
		"detection_height":      camera.DetectionHeight,
		"sensitivity":           camera.Sensitivity,
		"min_area_ratio":        camera.MinAreaRatio,
		"local_retention_days":  camera.LocalRetentionDays,
		"remote_retention_days": camera.RemoteRetentionDays,
		"active":                camera.Active,
		"updated_at":            time.Now().UTC(),
	})
	if result.Error != nil {
		if isUniqueViolation(result.Error) {
			return fmt.Errorf("%w: slug %q", ErrSlugTaken, camera.Slug)
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// UpdateZones writes the zone rule set. The API validates it (3–32 vertices,
// coordinates in 0..1) and audits the write before calling this; the store does
// not re-parse, so a caller cannot land a payload the validators refused.
func (s *CameraStore) UpdateZones(id, zonesJSON string) error {
	result := s.db.Model(&models.Camera{}).Where("id = ?", id).Updates(map[string]any{
		"zones_json": zonesJSON,
		"updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// Touch bumps updated_at, so the recorder's config poll notices a change the
// store made without a camera row write (a zone edit already does this itself).
// A camera id that matches no row is an error: it is a worker pointed at a
// camera an operator deleted, and answering success would hide that.
func (s *CameraStore) Touch(id string) error {
	result := s.db.Model(&models.Camera{}).Where("id = ?", id).
		Update("updated_at", time.Now().UTC())
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// Delete removes a camera. Media cleanup is not this method's job: the handler
// enqueues the purge jobs first, because a camera deleted with its rows still
// referenced by events would lose the events' media bookkeeping.
func (s *CameraStore) Delete(id string) error {
	result := s.db.Delete(&models.Camera{}, "id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// Count reports how many cameras exist, and how many are armed for detection:
// the numbers the boot line and the health endpoint state.
func (s *CameraStore) Count() (int64, int64, error) {
	var total, detecting int64
	if err := s.db.Model(&models.Camera{}).Count(&total).Error; err != nil {
		return 0, 0, err
	}
	if err := s.db.Model(&models.Camera{}).
		Where("active = ? AND detection_enabled = ?", true, true).Count(&detecting).Error; err != nil {
		return 0, 0, err
	}
	return total, detecting, nil
}

// isUniqueViolation recognises a unique-index rejection on both drivers. The
// point is the 409 the handler can answer: "slug taken" is a fact the operator
// acts on, and a raw driver message is not.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{"UNIQUE constraint failed", "duplicate key value", "constraint unique", "SQLSTATE 23505"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
