package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nvr/server/models"
)

// WebhookStore provides persistence for outbound notification endpoints.
type WebhookStore struct {
	db *gorm.DB
}

// Create inserts a webhook.
func (s *WebhookStore) Create(w *models.Webhook) error {
	if w.Name == "" {
		// The name is what the settings page lists and what the delivery history
		// is read from: an endpoint without one cannot be shown, edited, or
		// diagnosed, and a row that cannot be reached from the UI is a row an
		// operator cannot turn off.
		return errors.New("webhooks: name is required")
	}
	if w.ID == "" {
		w.ID = uuid.NewString()
	}
	enabled := w.Enabled
	if err := s.db.Create(w).Error; err != nil {
		if isUniqueViolation(err) {
			return errors.New("webhooks: name already exists")
		}
		return err
	}
	// `enabled` defaults to true in the schema and gorm fills the struct with
	// it, so the value wanted is captured above.
	if !enabled {
		w.Enabled = false
		return writeZeroColumns(s.db, w, map[string]any{"enabled": false})
	}
	return nil
}

// GetByID fetches a webhook.
func (s *WebhookStore) GetByID(id string) (*models.Webhook, error) {
	var w models.Webhook
	if err := s.db.First(&w, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &w, nil
}

// ListAll returns every webhook.
func (s *WebhookStore) ListAll() ([]models.Webhook, error) {
	var hooks []models.Webhook
	if err := s.db.Order("created_at ASC").Find(&hooks).Error; err != nil {
		return nil, err
	}
	return hooks, nil
}

// ListEnabledForCamera returns the endpoints that receive this camera's events:
// the delivery loop's set, resolved in one query so a close does not scan the
// table per signal. An empty camera filter is the global endpoint.
func (s *WebhookStore) ListEnabledForCamera(cameraID string) ([]models.Webhook, error) {
	var hooks []models.Webhook
	if err := s.db.Where("enabled = ? AND (camera_id = '' OR camera_id = ?)", true, cameraID).
		Order("created_at ASC").Find(&hooks).Error; err != nil {
		return nil, err
	}
	return hooks, nil
}

// Update writes the mutable fields. The secret is written only when the caller
// supplied one, so a PUT that omits the signing secret (the console never reads
// it back) cannot un-sign the endpoint's deliveries.
func (s *WebhookStore) Update(w *models.Webhook, sealSecret bool) error {
	updates := map[string]any{
		"name":       w.Name,
		"url":        w.URL,
		"camera_id":  w.CameraID,
		"events":     w.Events,
		"enabled":    w.Enabled,
		"updated_at": time.Now().UTC(),
	}
	if sealSecret {
		updates["secret"] = w.Secret
	}
	result := s.db.Model(&models.Webhook{}).Where("id = ?", w.ID).Updates(updates)
	if result.Error != nil {
		if isUniqueViolation(result.Error) {
			return errors.New("webhooks: name already exists")
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// MarkDelivered records the last delivery outcome on the endpoint, so the
// console answers "is this webhook working" from the row it already has.
func (s *WebhookStore) MarkDelivered(id, lastError string, deliveredAt time.Time) error {
	return s.db.Model(&models.Webhook{}).Where("id = ?", id).Updates(map[string]any{
		"last_error":        lastError,
		"last_delivered_at": deliveredAt,
	}).Error
}

// Delete removes a webhook.
func (s *WebhookStore) Delete(id string) error {
	result := s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Delete(&models.Webhook{}, "id = ?", id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		// The delivery history goes with the endpoint: rows naming an endpoint
		// that no longer exists are a history nobody can act on.
		return tx.Where("webhook_id = ?", id).Delete(&models.WebhookDelivery{}).Error
	})
	return result
}

// WebhookDeliveryStore provides persistence for delivery attempts. Every
// attempt is a row: the operator's question is "did it get through", and the
// answer is the sequence, not the last entry.
type WebhookDeliveryStore struct {
	db *gorm.DB
}

// Create records one attempt.
func (s *WebhookDeliveryStore) Create(d *models.WebhookDelivery) error {
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	if d.Status == "" {
		d.Status = models.DeliveryStatusPending
	}
	if d.Attempt <= 0 {
		d.Attempt = 1
	}
	return s.db.Create(d).Error
}

// ListForWebhook returns one endpoint's attempts, newest first.
//
// `status` narrows the answer to the attempts that failed, or the ones that
// succeeded; empty is every attempt. The console's question is "why did my hook
// fail", and the answer is the sequence of attempts rather than the last one.
//
// `limit` is clamped, and the clamp sits *above* the highest limit the API asks for
// on purpose: the route asks for one row more than it shows so it can say
// `truncated: true` honestly, and a clamp at the API's own ceiling would answer
// "there is nothing more" exactly at the point where there is.
//
// The `id` tiebreaker is not decoration. Two attempts of one delivery can land in the
// same millisecond (the ladder's first waits are milliseconds), and an offset page
// over a tie that the database resolves differently per query skips a row on one page
// and repeats it on the next.
func (s *WebhookDeliveryStore) ListForWebhook(webhookID, status string, limit, offset int) ([]models.WebhookDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	q := s.db.Where("webhook_id = ?", webhookID)
	if status != "" {
		q = q.Where("status = ?", status)
	}

	var deliveries []models.WebhookDelivery
	if err := q.Order("created_at DESC, id DESC").Limit(limit).Offset(offset).
		Find(&deliveries).Error; err != nil {
		return nil, err
	}
	return deliveries, nil
}

// CountForWebhook is how many attempts an endpoint has. The delete route reports it,
// because the history goes with the endpoint and an operator who just deleted one is
// entitled to know how much of it went with it.
func (s *WebhookDeliveryStore) CountForWebhook(webhookID string) (int64, error) {
	var n int64
	if err := s.db.Model(&models.WebhookDelivery{}).Where("webhook_id = ?", webhookID).
		Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// PruneOlderThan deletes delivery rows past a window. The delivery log is a
// debugging tool with a shelf life; the plan's sweep prunes it on the same
// schedule as the audit ledger, with its own counters.
func (s *WebhookDeliveryStore) PruneOlderThan(cutoff time.Time) (int64, error) {
	result := s.db.Where("created_at < ?", cutoff).Delete(&models.WebhookDelivery{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}
