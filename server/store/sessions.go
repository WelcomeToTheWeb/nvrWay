package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nvr/server/models"
)

// SessionStore provides persistence for issued refresh tokens.
type SessionStore struct {
	db *gorm.DB
}

// Create records an issued refresh token.
func (s *SessionStore) Create(session *models.Session) error {
	if session.ID == "" {
		session.ID = uuid.NewString()
	}
	if session.TokenHash == "" {
		return errors.New("sessions: token_hash is required")
	}
	return s.db.Create(session).Error
}

// GetByID fetches a session.
func (s *SessionStore) GetByID(id string) (*models.Session, error) {
	var session models.Session
	if err := s.db.First(&session, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &session, nil
}

// GetByTokenHash fetches the session a presented refresh token belongs to.
// Revoked rows are returned, not hidden: the caller has to be able to tell
// "expired" apart from "this token was already used", and only the second one
// is a reason to drop the account's live sessions.
func (s *SessionStore) GetByTokenHash(tokenHash string) (*models.Session, error) {
	var session models.Session
	if err := s.db.Where("token_hash = ?", tokenHash).First(&session).Error; err != nil {
		return nil, err
	}
	return &session, nil
}

// Rotate closes one session and opens its replacement in a single transaction,
// naming the closed row in `rotated_from`. Two workers rotating the same token
// must not both succeed: the update is guarded by the state the row was read in.
func (s *SessionStore) Rotate(old *models.Session, newTokenHash string, expiresAt time.Time) (*models.Session, error) {
	now := time.Now().UTC()
	created := &models.Session{
		ID:          uuid.NewString(),
		UserID:      old.UserID,
		TokenHash:   newTokenHash,
		RotatedFrom: old.TokenHash,
		ExpiresAt:   expiresAt,
		SourceIP:    old.SourceIP,
		UserAgent:   old.UserAgent,
		CreatedAt:   now,
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&models.Session{}).
			Where("id = ? AND revoked_at IS NULL", old.ID).
			Update("revoked_at", now)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Already revoked: this presented token is a replay.
			return ErrSessionRotated
		}
		return tx.Create(created).Error
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// ErrSessionRotated is reuse: the token presented has already been rotated, so
// whoever is presenting it is replaying an old credential.
//
// The sentinel lives in models, next to the Session row it describes: the store
// answers it when a rotation loses its guard, and auth reads it when it decides
// whether a presented token is a replay. The name is kept here for the store's
// own callers.
var ErrSessionRotated = models.ErrSessionRotated

// Revoke closes one session.
func (s *SessionStore) Revoke(id string) error {
	result := s.db.Model(&models.Session{}).Where("id = ?", id).Update("revoked_at", time.Now().UTC())
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// RevokeAllFor closes every live session of an account: the response to a
// detected replay, and to a password change.
func (s *SessionStore) RevokeAllFor(userID string) (int64, error) {
	result := s.db.Model(&models.Session{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", time.Now().UTC())
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// ListLiveFor returns an account's live sessions, for the console's "where am I
// logged in" and the audit trail's answer to it.
func (s *SessionStore) ListLiveFor(userID string, now time.Time) ([]models.Session, error) {
	var sessions []models.Session
	if err := s.db.Where("user_id = ? AND revoked_at IS NULL AND expires_at > ?", userID, now).
		Order("created_at DESC").Find(&sessions).Error; err != nil {
		return nil, err
	}
	return sessions, nil
}

// PruneExpired deletes session rows past their expiry, closing the audit trail
// on sessions that can no longer be used.
func (s *SessionStore) PruneExpired(olderThan time.Time) (int64, error) {
	result := s.db.Where("expires_at < ?", olderThan).Delete(&models.Session{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}
