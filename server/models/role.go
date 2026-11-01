package models

import (
	"strings"
	"time"
)

// Role is a set of permissions. v1 ships three built-in roles and no more: the
// floor an operator reads in the console is exactly this list.
type Role struct {
	ID          string   `gorm:"type:uuid;primaryKey" json:"id"`
	Name        string   `gorm:"uniqueIndex;not null" json:"name"`
	Description string   `gorm:"not null;default:''" json:"description"`
	Permissions []string `gorm:"serializer:json" json:"permissions"`
	IsBuiltin   bool     `gorm:"not null;default:false" json:"is_builtin"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the table name.
func (Role) TableName() string { return "roles" }

// HasPermission checks the permission set against this row.
func (r *Role) HasPermission(perm string) bool {
	if r == nil {
		return false
	}
	return PermissionsGrant(r.Permissions, perm)
}

// PermissionsGrant answers whether a permission list grants `perm`.
//
// A list grants `perm` when it contains `perm` verbatim, when it contains
// `admin:*`, or when it contains the resource star for `perm` (`cameras:*`
// grants `cameras:write`). The role editor and the middleware (Step 2) answer
// from this one function, so a gate cannot be enforced one way at the route
// and another way in the editor.
func PermissionsGrant(perms []string, perm string) bool {
	if perm == "" {
		return false
	}
	for _, p := range perms {
		if p == perm {
			return true
		}
		if p == PermAdminStar {
			return true
		}
		if strings.HasSuffix(p, ":*") && strings.HasPrefix(perm, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// Role names (plan Step 2's seeded set).
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// Permission names, declared here so a handler cannot invent a verb that reads
// like two other verbs, and so the role gates are auditable as a set.
const (
	PermCamerasRead   = "cameras:read"
	PermCamerasWrite  = "cameras:write"
	PermCamerasDelete = "cameras:delete"
	PermCamerasProbe  = "cameras:probe"
	PermZonesWrite    = "zones:write"

	PermEventsRead   = "events:read"
	PermEventsDelete = "events:delete"
	PermSegmentsRead = "segments:read"
	PermLiveRead     = "live:read"

	PermStorageRead  = "storage:read"
	PermStorageWrite = "storage:write"

	// The live-view gates. Every authenticated role reads live and clips — the
	// console cannot show a stream it is not allowed to read — while the admin
	// channel is the admin's alone.
	PermWSLiveRead  = "ws:live:read"
	PermWSClipsRead = "ws:clips:read"

	PermWebhooksRead  = "webhooks:read"
	PermWebhooksWrite = "webhooks:write"

	PermSettingsRead  = "settings:read"
	PermSettingsWrite = "settings:write"

	PermUsersRead   = "users:read"
	PermUsersWrite  = "users:write"
	PermUsersDelete = "users:delete"
	PermRolesRead   = "roles:read"
	PermRolesWrite  = "roles:write"

	PermOIDCRead  = "oidc:read"
	PermOIDCWrite = "oidc:write"
	PermAuditRead = "audit:read"

	// The shorthand grants an operator can write into a custom role: `admin:*`
	// implies everything, a resource star implies every verb on that resource.
	PermAdminStar    = "admin:*"
	PermCamerasStar  = "cameras:*"
	PermEventsStar   = "events:*"
	PermStorageStar  = "storage:*"
	PermWebhooksStar = "webhooks:*"
	PermOIDCStar     = "oidc:*"
	PermUsersStar    = "users:*"
	PermRolesStar    = "roles:*"
	PermSettingsStar = "settings:*"
	PermWSAdminStar  = "ws:admin:*"
)

// BuiltinRole is one built-in role with its default permissions.
type BuiltinRole struct {
	Name        string
	Description string
	Permissions []string
}

// DefaultBuiltinRoles returns v1's role set.
//
// The floors are the plan's, stated as numbers a person can read:
//   - `viewer` reads cameras, events, segments and live — and nothing else, so
//     a viewer's console has no Settings tab.
//   - `operator` adds camera CRUD, probes, the zone editor, and reading the
//     storage queue: the person who runs the cameras.
//   - `admin` adds people, storage backends, webhooks, OIDC providers and the
//     audit ledger: the person who runs the installation.
//
// `viewer` is also the JIT floor: an OIDC assertion with no mapped role gets
// viewer, never more, and role mapping can only move a person up from it.
func DefaultBuiltinRoles() []BuiltinRole {
	return []BuiltinRole{
		{
			Name:        RoleAdmin,
			Description: "Runs the installation: people, storage, webhooks, OIDC providers, audit",
			Permissions: []string{
				PermCamerasRead, PermCamerasWrite, PermCamerasDelete, PermCamerasProbe, PermZonesWrite,
				PermEventsRead, PermEventsDelete, PermSegmentsRead, PermLiveRead,
				PermStorageRead, PermStorageWrite,
				PermWSLiveRead, PermWSClipsRead, PermWSAdminStar,
				PermWebhooksRead, PermWebhooksWrite,
				PermSettingsRead, PermSettingsWrite,
				PermUsersRead, PermUsersWrite, PermUsersDelete,
				PermRolesRead, PermRolesWrite,
				PermOIDCRead, PermOIDCWrite,
				PermAuditRead,
			},
		},
		{
			Name:        RoleOperator,
			Description: "Runs the cameras: add and edit them, probe, draw zones, read the storage queue",
			Permissions: []string{
				PermCamerasRead, PermCamerasWrite, PermCamerasProbe, PermZonesWrite,
				PermEventsRead, PermSegmentsRead, PermLiveRead,
				PermStorageRead,
				PermWSLiveRead, PermWSClipsRead,
				PermWebhooksRead,
				PermSettingsRead,
				PermUsersRead,
				PermRolesRead,
			},
		},
		{
			Name:        RoleViewer,
			Description: "Read-only: live view, events, clips, snapshots, the segment timeline",
			Permissions: []string{
				PermCamerasRead,
				PermEventsRead, PermSegmentsRead, PermLiveRead,
				PermWSLiveRead, PermWSClipsRead,
			},
		},
	}
}
