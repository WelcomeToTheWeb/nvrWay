package models

import "time"

// Account statuses. `pending` is an identity an OIDC provider asserted whose
// email belongs to an account that already exists: nvrWay does not link those
// by itself, whatever the IdP asserts. A pending account has no roles, no
// password and no access.
const (
	UserStatusActive  = "active"
	UserStatusPending = "pending"
)

// User is an account: a local one (the break-glass admin), or one JIT-provisioned
// from an OIDC assertion.
type User struct {
	ID    string `gorm:"type:uuid;primaryKey" json:"id"`
	Name  string `gorm:"not null;default:''" json:"name"`
	Email string `gorm:"index;not null;default:''" json:"email"`

	// Username is the login name, unique. An OIDC assertion that does not
	// provide a usable name gets one derived from its email, with the collision
	// handling in sso/ (`uniqueUsername`).
	Username     string `gorm:"uniqueIndex;not null" json:"username"`
	PasswordHash string `gorm:"type:text;not null;default:''" json:"-"`

	// Provider is `local` or a provider slug; ProviderID is the subject the
	// provider asserts. Together they are the JIT lookup: the same person
	// signing in twice must not create two accounts.
	Provider   string `gorm:"not null;default:'local';index" json:"provider"`
	ProviderID string `gorm:"not null;default:'';index" json:"provider_id"`

	// SSOAttributes is the last assertion's claims as JSON text, for the admin
	// who has to decide whether an account's mapping looks right.
	SSOAttributes string `gorm:"type:text;not null;default:'{}'" json:"sso_attributes"`

	Status string `gorm:"not null;default:'active';index" json:"status"`

	Roles     []Role     `gorm:"many2many:user_roles;joinForeignKey:UserID;joinReferences:RoleID" json:"roles,omitempty"`
	UserRoles []UserRole `gorm:"foreignKey:UserID" json:"-"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	LastLogin *time.Time `json:"last_login"`
}

// TableName pins the table name.
func (User) TableName() string { return "users" }

// IsPending reports the state every auth path must refuse.
func (u *User) IsPending() bool { return u != nil && u.Status == UserStatusPending }

// IsLocal reports the account that can authenticate with a password. An OIDC
// account has no password by construction, so this is the gate on the login
// handler, not a UI hint.
func (u *User) IsLocal() bool { return u != nil && u.Provider == ProviderLocal }

// ProviderLocal is the provider value a password account carries.
const ProviderLocal = "local"
