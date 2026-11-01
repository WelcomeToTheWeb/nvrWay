package models

import "time"

// Pending identity statuses.
const (
	IdentityStatusPending  = "pending"
	IdentityStatusLinked   = "linked"
	IdentityStatusRefused  = "refused"
	IdentityStatusApproved = "approved"
)

// PendingIdentity is an identity an OIDC provider asserted that nvrWay refused
// to act on by itself: the asserted email belongs to an account that already
// exists, and matching by asserted email is exactly the thing an identity
// provider does not get to do unilaterally.
//
// The row is created with **no user**, it cannot log in, and it names the
// account the assertion points at, until an admin approves it as its own
// account at the viewer floor, merges it into that account, or refuses it.
// Each decision is an audit row.
type PendingIdentity struct {
	ID string `gorm:"type:uuid;primaryKey" json:"id"`
	// Provider is the provider slug the assertion came from; ProviderSubject is
	// the `sub` the provider asserts for this person.
	Provider        string `gorm:"not null;index" json:"provider"`
	ProviderSubject string `gorm:"not null;index" json:"provider_subject"`

	Email       string `gorm:"not null;index" json:"email"`
	Username    string `gorm:"not null;default:''" json:"username"`
	DisplayName string `gorm:"not null;default:''" json:"display_name"`

	// Claims is the assertion as received, JSON text: the record an admin
	// decides with, kept because the IdP's response is not reproducible later.
	Claims string `gorm:"type:text;not null;default:'{}'" json:"claims"`

	// MatchedUserID is the existing account the asserted email belongs to.
	MatchedUserID string `gorm:"not null;default:'';index" json:"matched_user_id"`

	Status string `gorm:"not null;default:'pending';index" json:"status"`
	// CreatedUserID is the account the decision produced (approve), if any.
	CreatedUserID string `gorm:"not null;default:''" json:"created_user_id"`

	Reason    string     `gorm:"type:text;not null;default:''" json:"reason"`
	CreatedAt time.Time  `json:"created_at"`
	DecidedAt *time.Time `json:"decided_at"`
	// DecidedBy is the admin who decided, captured as a username as well as an
	// id: accounts are deleted, and a record that resolves to "user 9f3… does
	// not exist" is not a record.
	DecidedBy         string `gorm:"not null;default:''" json:"decided_by"`
	DecidedByUsername string `gorm:"not null;default:''" json:"decided_by_username"`
}

// TableName pins the table name.
func (PendingIdentity) TableName() string { return "pending_identities" }

// IsPending reports the state the login path refuses.
func (p *PendingIdentity) IsPending() bool { return p != nil && p.Status == IdentityStatusPending }
