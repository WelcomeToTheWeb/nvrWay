package models

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// ErrSessionRotated is reuse: the presented refresh token has already been
// rotated, so the row it names is closed. It lives here because it is a fact
// about a Session row — the store answers it when a rotation loses its guard,
// and the auth package reads it when deciding whether a presented token is a
// replay. Every live session of that account goes when it is answered.
var ErrSessionRotated = errors.New("session already rotated")

// Session is one issued refresh token.
//
// Access tokens are stateless (HS256 JWTs, 30 min) and are not stored. The
// refresh token is: rotation and reuse detection need a row to rotate.
//
//	issue      → a row with token_hash and no rotated_from
//	rotate     → the row is revoked and a new row names it in rotated_from
//	reuse      → a presented token whose row is already revoked is a replay, and
//	           the account's live sessions are dropped. A stolen refresh token
//	           is only useful once; the honest client is the one that rotates it.
type Session struct {
	ID     string `gorm:"type:uuid;primaryKey" json:"id"`
	UserID string `gorm:"type:uuid;not null;index" json:"user_id"`

	// TokenHash is the SHA-256 (hex) of the refresh token. The token itself
	// never lands in the database, so a DB read cannot hand out a live session.
	TokenHash string `gorm:"uniqueIndex;not null" json:"-"`
	// RotatedFrom is the token_hash this row replaced. Empty on the first issue.
	RotatedFrom string `gorm:"index;not null;default:''" json:"-"`

	ExpiresAt time.Time  `gorm:"not null;index" json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`

	SourceIP string `gorm:"not null;default:''" json:"source_ip"`
	// UserAgent is the console's, truncated. It is the fact an operator reads
	// when asking "which of my sessions is this".
	UserAgent string    `gorm:"not null;default:''" json:"user_agent"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName pins the table name.
func (Session) TableName() string { return "sessions" }

// IsLive reports the state a presented token must be in to log someone in:
// not revoked, not expired.
func (s *Session) IsLive(now time.Time) bool {
	return s != nil && s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// NewRefreshToken returns a fresh random refresh token and the hash to store
// for it. The token is what the client keeps; the hash is what the server keeps.
func NewRefreshToken() (token string, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(b)
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken returns the hex SHA-256 of a refresh token.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Token TTLs. Access is short because it is the credential the console carries
// in memory; refresh is long because re-authenticating against an IdP is not
// something a console should do every half hour.
const (
	AccessTokenTTL  = 30 * time.Minute
	RefreshTokenTTL = 14 * 24 * time.Hour
)
