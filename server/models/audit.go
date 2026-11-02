package models

import (
	"time"

	"github.com/google/uuid"
)

// Audit is the record of a mutating act: who did it, to what, and what the
// server answered.
//
// Two rules carried over from the sibling project this repo's conventions come
// from, because both are what make a ledger usable:
//
//   - The **outcome is a column** and refusals are rows. A log that records
//     only successes is a list of what the installation allowed; the events an
//     operator wants are the ones it refused (`refused:403` on a viewer trying
//     to delete a camera is the row that settles an argument).
//   - `actor_user_id`, `actor_username` and `target_id` are **text**, not
//     `uuid`, and Postgres is what makes the reason visible: a uuid column
//     rejects the empty string, and these columns are legitimately empty. A
//     failed login has no authenticated user id; a delete refused before any
//     target resolved has no entity id.
//
// The writer is a store method called by the handlers, not middleware:
// middleware cannot know the target or whether the action succeeded, and those
// are the record.
type Audit struct {
	ID string `gorm:"type:uuid;primaryKey" json:"id"`

	ActorUserID   string `gorm:"index;not null;default:''" json:"actor_user_id"`
	ActorUsername string `gorm:"not null;default:''" json:"actor_username"`
	ActorRoles    string `gorm:"not null;default:''" json:"actor_roles"`
	// ActorProvider is where the account came from — `local`, or a provider
	// slug. Stored for the same reason the username is: providers are deleted,
	// and "who removed a camera" must keep its answer after the directory they
	// signed in through is gone.
	ActorProvider string `gorm:"not null;default:''" json:"actor_provider"`

	// Action is a verb, not a sentence: `camera.delete`, `auth.login_failed`.
	// Stable strings, so a filter is a filter.
	Action     string `gorm:"not null;index" json:"action"`
	TargetType string `gorm:"not null;default:''" json:"target_type"`
	TargetID   string `gorm:"not null;default:'';index" json:"target_id"`

	// Outcome is `ok`, `refused`, `not_found` or `error`, rendered from the
	// status code (AuditOutcomeFor), so a filter cannot be a free-text reason.
	Outcome string `gorm:"not null;default:'';index" json:"outcome"`

	// Details is JSON: what changed, and how much of it.
	Details  string `gorm:"type:text;not null;default:''" json:"details"`
	SourceIP string `gorm:"not null;default:''" json:"source_ip"`

	// idx_audit_action_created is the ledger's own query ("every failed login,
	// this week"); idx_audit_actor_created is "everything this person did,
	// newest first". A single-column index on created_at serves neither.
	CreatedAt time.Time `gorm:"not null;index:idx_audit_action_created;index:idx_audit_actor_created" json:"created_at"`
}

// TableName pins the table name.
func (Audit) TableName() string { return "audit" }

// AuditActor is who performed an act, captured at the moment of it. It is a
// plain struct, not a table: the writer copies it onto the row's flat actor_*
// columns, so the record stays readable when the account is gone.
type AuditActor struct {
	UserID   string
	Username string
	Roles    string
	Provider string
}

// Audit outcomes.
const (
	AuditOutcomeOK       = "ok"
	AuditOutcomeRefused  = "refused"
	AuditOutcomeNotFound = "not_found"
	AuditOutcomeError    = "error"
)

// AuditRetentionDays is how long the ledger keeps rows. It is a number an
// operator can read, and the sweeper that prunes it uses the same value.
const AuditRetentionDays = 365

// Audit action names. Declared here so a handler cannot invent a verb that
// reads like two other verbs.
const (
	AuditAuthLogin       = "auth.login"
	AuditAuthLoginFailed = "auth.login_failed"
	AuditAuthLogout      = "auth.logout"
	// A refresh is on the record because its failure is the interesting one: a
	// reused refresh token is a copied credential, and the row is how an operator
	// learns that a session was dropped.
	AuditAuthRefresh        = "auth.refresh"
	AuditAuthPasswordChange = "auth.password_change"
	AuditAuthRegister       = "auth.register"
	AuditAuthProfile        = "auth.profile"

	// The SSO login ledger. A refusal at the callback names its reason in
	// `details`, and the reason vocabulary lives in sso/contract.go, so the
	// action says which door was refused and the details say why.
	AuditSSOLogin        = "sso.login"
	AuditSSOLoginRefused = "sso.login_refused"
	// Stamping an SSO identity onto an existing account is reachable only when
	// the operator chose link_policy=link for that provider. It gives an
	// existing account's access to a new door, so it is a row.
	AuditSSOLink      = "sso.link"
	AuditIdentityRead = "identity.read"
	// Approving a pending identity creates an account at the viewer floor, which
	// is an act with consequences, not a queue being cleared.
	AuditIdentityApprove = "identity.approve"
	AuditIdentityLink    = "identity.link"
	AuditIdentityMerge   = "identity.merge"
	AuditIdentityRefuse  = "identity.refuse"

	AuditCameraCreate = "camera.create"
	AuditCameraUpdate = "camera.update"
	AuditCameraDelete = "camera.delete"
	// Drawing a zone is arming a rule that records people, so every zone write
	// is on the record.
	AuditCameraZones = "camera.zones"
	AuditCameraProbe = "camera.probe"

	AuditEventDelete = "event.delete"

	AuditStorageCreate  = "storage.backend_create"
	AuditStorageUpdate  = "storage.backend_update"
	AuditStorageDelete  = "storage.backend_delete"
	AuditStorageDefault = "storage.backend_default"
	AuditStorageEnable  = "storage.backend_enable"
	AuditStorageTest    = "storage.backend_test"
	AuditStorageRequeue = "storage.requeue"

	AuditWebhookCreate = "webhook.create"
	AuditWebhookUpdate = "webhook.update"
	AuditWebhookDelete = "webhook.delete"
	AuditWebhookTest   = "webhook.test"

	AuditOIDCRead   = "oidc.read"
	AuditOIDCCreate = "oidc.create"
	AuditOIDCUpdate = "oidc.update"
	AuditOIDCDelete = "oidc.delete"
	AuditOIDCTest   = "oidc.test"

	AuditUserCreate     = "user.create"
	AuditUserUpdate     = "user.update"
	AuditUserDelete     = "user.delete"
	AuditUserRead       = "user.read"
	AuditUserRoleChange = "user.role_change"

	AuditRoleCreate = "role.create"
	AuditRoleUpdate = "role.update"
	AuditRoleDelete = "role.delete"
	AuditRoleRead   = "role.read"

	// AuditRouteDenied is the verb a gate uses for a route that has not declared
	// one. A new route group that never registered its ledger verb shows up here
	// instead of writing refusals under a name nobody searches for.
	AuditRouteDenied = "route.denied"

	AuditSettingUpdate = "setting.update"
)

// NewAuditID mints an audit row id.
func NewAuditID() string { return uuid.NewString() }

// AuditOutcomeFor renders the `outcome` column's value for a status code, so
// the stored outcome is comparable and not a free-text reason.
func AuditOutcomeFor(status int) string {
	switch {
	case status >= 200 && status < 300:
		return AuditOutcomeOK
	case status == 404:
		return AuditOutcomeNotFound
	case status >= 400 && status < 500:
		return AuditOutcomeRefused
	default:
		return AuditOutcomeError
	}
}
