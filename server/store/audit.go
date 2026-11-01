package store

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

// AuditStore is the audit log's writer and reader.
//
// The writer is a store method called by the handlers, not middleware:
// middleware cannot know the target, the actor's roles at the time, or whether
// the action succeeded — and all three are the record.
type AuditStore struct {
	db *gorm.DB
}

// NewAuditStore builds the ledger's store over an existing database, so the
// sweeper that prunes it is testable without constructing a whole installation.
func NewAuditStore(db *gorm.DB) *AuditStore { return &AuditStore{db: db} }

// AuditRecord is one act, as a handler knows it.
type AuditRecord struct {
	Action     string
	TargetType string
	TargetID   string
	Outcome    string
	Details    map[string]any
}

// Record writes one act.
//
// Actor fields are filled from the request context by the API layer's audit
// helper, which is the only caller for a request: one definition of "who is
// this request", so a row cannot log an actor the request never carried.
func (s *AuditStore) Record(row *models.Audit) error {
	if row.ID == "" {
		row.ID = models.NewAuditID()
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	return s.db.Create(row).Error
}

// EncodeDetails serialises a details map, keeping the column's shape uniform.
func EncodeDetails(details map[string]any) string {
	if len(details) == 0 {
		return ""
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// AuditFilter is the ledger's query.
type AuditFilter struct {
	ActorUserID   string
	ActorUsername string
	Action        string
	TargetID      string
	Outcome       string
	From          *time.Time
	To            *time.Time
	Limit         int
	Offset        int
}

// List returns ledger rows newest-first. The composite indexes are exactly
// these two shapes: an action (or outcome) within a time window, and one
// actor's acts within a time window.
func (s *AuditStore) List(f AuditFilter) ([]models.Audit, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	query := s.db.Order("created_at DESC").Limit(limit).Offset(f.Offset)
	if f.ActorUserID != "" {
		query = query.Where("actor_user_id = ?", f.ActorUserID)
	}
	if f.ActorUsername != "" {
		query = query.Where("actor_username = ?", f.ActorUsername)
	}
	if f.Action != "" {
		query = query.Where("action = ?", f.Action)
	}
	if f.TargetID != "" {
		query = query.Where("target_id = ?", f.TargetID)
	}
	if f.Outcome != "" {
		query = query.Where("outcome = ?", f.Outcome)
	}
	if f.From != nil {
		query = query.Where("created_at >= ?", *f.From)
	}
	if f.To != nil {
		query = query.Where("created_at <= ?", *f.To)
	}
	var rows []models.Audit
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// PruneOlderThan deletes ledger rows past the retention window. The sweeper
// runs it on the same schedule as every other sweep, and the count it returns
// is the number the operator reads.
func (s *AuditStore) PruneOlderThan(cutoff time.Time) (int64, error) {
	result := s.db.Where("created_at < ?", cutoff).Delete(&models.Audit{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// Count reports how many rows the ledger holds.
func (s *AuditStore) Count() (int64, error) {
	var count int64
	if err := s.db.Model(&models.Audit{}).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}
