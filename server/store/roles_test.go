package store

import (
	"errors"
	"strings"
	"testing"

	"nvr/server/models"
)

func TestSeedBuiltinRolesIsIdempotentAndDoesNotClobber(t *testing.T) {
	st := newTestStore(t)

	roles, err := st.Roles.ListRoles()
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	if len(roles) != 3 {
		t.Fatalf("expected the three built-in roles, got %d %+v", len(roles), roles)
	}
	for _, role := range roles {
		if !role.IsBuiltin {
			t.Fatalf("role %q must be marked built-in", role.Name)
		}
	}

	// An operator who trimmed a role's permissions is not overwritten by a
	// boot. A role table that resets itself on restart is a policy nobody set.
	operator, err := st.Roles.GetRoleByName(models.RoleOperator)
	if err != nil {
		t.Fatalf("get operator: %v", err)
	}
	operator.Permissions = []string{models.PermCamerasRead}
	if err := st.Roles.UpdateRole(operator); err != nil {
		t.Fatalf("update role: %v", err)
	}

	if err := st.Roles.SeedBuiltinRoles(); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	after, err := st.Roles.GetRoleByName(models.RoleOperator)
	if err != nil {
		t.Fatalf("get operator: %v", err)
	}
	if len(after.Permissions) != 1 || after.Permissions[0] != models.PermCamerasRead {
		t.Fatalf("re-seeding must leave an operator's chosen permissions alone, got %v", after.Permissions)
	}

	var rows int64
	if err := st.DB.Model(&models.Role{}).Count(&rows).Error; err != nil {
		t.Fatalf("count roles: %v", err)
	}
	if rows != 3 {
		t.Fatalf("re-seeding must not add rows, got %d", rows)
	}
}

func TestRoleFloorsAreThePlansOnes(t *testing.T) {
	st := newTestStore(t)

	viewer, err := st.Roles.GetRoleByName(models.RoleViewer)
	if err != nil {
		t.Fatalf("get viewer: %v", err)
	}
	operator, err := st.Roles.GetRoleByName(models.RoleOperator)
	if err != nil {
		t.Fatalf("get operator: %v", err)
	}
	admin, err := st.Roles.GetRoleByName(models.RoleAdmin)
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}

	// viewer reads cameras, events, segments and live — and nothing else, so a
	// viewer's console has no Settings tab.
	for _, perm := range []string{models.PermCamerasRead, models.PermEventsRead, models.PermSegmentsRead, models.PermLiveRead} {
		if !viewer.HasPermission(perm) {
			t.Fatalf("viewer must hold %q", perm)
		}
	}
	for _, perm := range []string{models.PermStorageWrite, models.PermUsersWrite, models.PermSettingsWrite, models.PermCamerasWrite, models.PermZonesWrite, models.PermOIDCWrite} {
		if viewer.HasPermission(perm) {
			t.Fatalf("viewer must not hold %q", perm)
		}
	}

	// operator runs the cameras: CRUD, probes, the zone editor, and reads the
	// storage queue.
	for _, perm := range []string{models.PermCamerasWrite, models.PermCamerasProbe, models.PermZonesWrite, models.PermStorageRead} {
		if !operator.HasPermission(perm) {
			t.Fatalf("operator must hold %q", perm)
		}
	}
	// operator does not run the installation.
	for _, perm := range []string{models.PermCamerasDelete, models.PermUsersWrite, models.PermStorageWrite, models.PermSettingsWrite, models.PermOIDCWrite, models.PermEventsDelete} {
		if operator.HasPermission(perm) {
			t.Fatalf("operator must not hold %q", perm)
		}
	}

	// admin runs the installation.
	for _, perm := range []string{models.PermCamerasDelete, models.PermUsersWrite, models.PermUsersDelete,
		models.PermStorageWrite, models.PermWebhooksWrite, models.PermOIDCWrite, models.PermSettingsWrite,
		models.PermAuditRead, models.PermEventsDelete, models.PermRolesWrite} {
		if !admin.HasPermission(perm) {
			t.Fatalf("admin must hold %q", perm)
		}
	}
}

func TestRoleCRUD(t *testing.T) {
	st := newTestStore(t)

	role := &models.Role{
		Name:        "night-ops",
		Description: "Reads events, acknowledges nothing",
		Permissions: []string{models.PermEventsRead, models.PermCamerasRead},
	}
	if err := st.Roles.CreateRole(role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if role.ID == "" {
		t.Fatal("expected the store to mint the role id")
	}

	if _, err := st.Roles.GetRoleByID(role.ID); err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if _, err := st.Roles.GetRoleByName("nope"); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("expected the named not-found, got %v", err)
	}

	dup := &models.Role{Name: "night-ops"}
	if err := st.Roles.CreateRole(dup); err == nil {
		t.Fatal("expected the unique role name to be refused")
	}

	role.Permissions = []string{models.PermEventsRead, models.PermCamerasRead, models.PermSegmentsRead}
	if err := st.Roles.UpdateRole(role); err != nil {
		t.Fatalf("update role: %v", err)
	}
	loaded, err := st.Roles.GetRoleByID(role.ID)
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if len(loaded.Permissions) != 3 {
		t.Fatalf("expected the permission set to round-trip, got %v", loaded.Permissions)
	}

	if err := st.Roles.DeleteRole(role.ID); err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if _, err := st.Roles.GetRoleByID(role.ID); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("expected the role gone, got %v", err)
	}
}

func TestBuiltinRolesCannotBeDeleted(t *testing.T) {
	st := newTestStore(t)

	// Deleting `viewer` deletes the JIT floor: every account mapped to it loses
	// its access, and every future OIDC login has no role to land on.
	viewer, err := st.Roles.GetRoleByName(models.RoleViewer)
	if err != nil {
		t.Fatalf("get viewer: %v", err)
	}
	err = st.Roles.DeleteRole(viewer.ID)
	if err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("expected the built-in roles to be protected, got %v", err)
	}

	if err := st.Roles.DeleteRole("missing"); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("expected not-found for an unknown role, got %v", err)
	}
}

func TestRolePermissionsRoundTripAsJSON(t *testing.T) {
	st := newTestStore(t)

	// serializer:json stores the permission list as text; the migration test in
	// OurWay's shape asserts the same, and this is the one list column the
	// console writes whole.
	role := &models.Role{Name: "auditor", Permissions: []string{models.PermAuditRead, models.PermEventsRead}}
	if err := st.Roles.CreateRole(role); err != nil {
		t.Fatalf("create role: %v", err)
	}
	loaded, err := st.Roles.GetRoleByName("auditor")
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if len(loaded.Permissions) != 2 || !loaded.HasPermission(models.PermAuditRead) {
		t.Fatalf("expected the permissions back, got %v", loaded.Permissions)
	}
	if loaded.HasPermission("events:delete") {
		t.Fatal("the permission set gained something nobody wrote")
	}
}
