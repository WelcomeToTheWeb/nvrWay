package store

import (
	"testing"
	"time"

	"nvr/server/auth"
	"nvr/server/models"
)

// Bootstrap is the boot that decides whether an installation has a way in. The
// assertions are the plan's rules, each one a failure an operator would discover
// at the worst moment: no admin to log in with, or an admin back after they
// deleted it.

func TestBootstrapSeedsRolesAndTheBreakGlassAdmin(t *testing.T) {
	st := newTestStore(t)

	seeded, err := st.Bootstrap("admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !seeded {
		t.Fatal("an installation with no admin and a password set must seed the break-glass admin")
	}

	for _, name := range []string{models.RoleAdmin, models.RoleOperator, models.RoleViewer} {
		if _, err := st.Roles.GetRoleByName(name); err != nil {
			t.Fatalf("role %s missing after a cold boot: %v", name, err)
		}
	}
	roles, err := st.Roles.ListRoles()
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	if len(roles) != 3 {
		t.Fatalf("builtin roles: got %d, want 3", len(roles))
	}

	admin, err := st.Users.GetByUsername("admin")
	if err != nil {
		t.Fatalf("fetch the seeded account: %v", err)
	}
	if admin.Provider != models.ProviderLocal {
		t.Fatalf("the break-glass account must be a local account, got provider %q", admin.Provider)
	}
	if admin.Status != models.UserStatusActive {
		t.Fatalf("the break-glass account must be active, got status %q", admin.Status)
	}
	if admin.PasswordHash == "" {
		t.Fatal("the break-glass account has no password, so it cannot be used to log in")
	}
	if len(admin.Roles) != 1 || admin.Roles[0].Name != models.RoleAdmin {
		t.Fatalf("the break-glass account carries %v, want exactly the admin role", roleNames(admin.Roles))
	}
	if admin.Email != "" || admin.Name != "" {
		t.Fatalf("seeding invented profile fields it was not given: name=%q email=%q", admin.Name, admin.Email)
	}

	// The password is stored as an argon2 hash, never as the password, and the
	// password from .env verifies against it.
	ok, err := auth.VerifyPassword("correct horse battery staple", admin.PasswordHash)
	if err != nil {
		t.Fatalf("verify the seeded password: %v", err)
	}
	if !ok {
		t.Fatal("the password from .env does not verify against the stored hash")
	}
	if ok, err := auth.VerifyPassword("wrong password", admin.PasswordHash); err != nil {
		t.Fatalf("verify a wrong password: %v", err)
	} else if ok {
		t.Fatal("a wrong password verified")
	}

	marker, err := st.Settings.Get(SettingBootstrapSeeded)
	if err != nil {
		t.Fatalf("read the seed marker: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, marker); err != nil {
		t.Fatalf("seed marker %q is not a timestamp: %v", marker, err)
	}
}

func TestBootstrapWithNoPasswordCreatesNoLocalAdmin(t *testing.T) {
	st := newTestStore(t)

	seeded, err := st.Bootstrap("admin", "")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if seeded {
		t.Fatal("an empty password created an account")
	}

	users, err := st.Users.ListAll()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 0 {
		t.Fatalf("no password must create no account, got %d", len(users))
	}
	if _, err := st.Settings.Get(SettingBootstrapSeeded); err == nil {
		t.Fatal("nothing was seeded, so nothing should be recorded as seeded")
	}

	// The roles are seeded on every boot, whatever the password is: this is the
	// SSO-only installation, and it still needs a role table to grant from.
	for _, name := range []string{models.RoleAdmin, models.RoleOperator, models.RoleViewer} {
		if _, err := st.Roles.GetRoleByName(name); err != nil {
			t.Fatalf("role %s missing in a passwordless boot: %v", name, err)
		}
	}
}

func TestBootstrapDoesNotResurrectADeletedBreakGlassAdmin(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.Bootstrap("admin", "first password"); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	admin, err := st.Users.GetByUsername("admin")
	if err != nil {
		t.Fatalf("fetch the seeded account: %v", err)
	}
	marker, err := st.Settings.Get(SettingBootstrapSeeded)
	if err != nil {
		t.Fatalf("read the seed marker: %v", err)
	}

	// The operator deleted it: the account, and the SSO installation has no
	// local way in. That is the state they chose.
	if err := st.Users.Delete(admin.ID); err != nil {
		t.Fatalf("delete the break-glass admin: %v", err)
	}

	seeded, err := st.Bootstrap("admin", "first password")
	if err != nil {
		t.Fatalf("bootstrap after the deletion: %v", err)
	}
	if seeded {
		t.Fatal("the boot re-created the account the operator deleted")
	}
	if _, err := st.Users.GetByUsername("admin"); err == nil {
		t.Fatal("the deleted break-glass admin is back")
	}
	count, err := st.Users.CountAdmins()
	if err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if count != 0 {
		t.Fatalf("deleting the admin left %d accounts holding admin", count)
	}
	after, err := st.Settings.Get(SettingBootstrapSeeded)
	if err != nil {
		t.Fatalf("read the seed marker again: %v", err)
	}
	if after != marker {
		t.Fatalf("the seed marker moved from %q to %q on a boot that seeded nothing", marker, after)
	}
}

func TestBootstrapSkipsWhenSomeoneAlreadyHoldsAdmin(t *testing.T) {
	st := newTestStore(t)

	// An SSO-provisioned administrator: this installation has a way in, and it
	// was not created by the break-glass path.
	if err := st.Users.Create(&models.User{
		Username:   "ida",
		Provider:   "oidc",
		ProviderID: "ida@idp.example",
		Status:     models.UserStatusActive,
	}); err != nil {
		t.Fatalf("create the SSO admin: %v", err)
	}
	ida, err := st.Users.GetByUsername("ida")
	if err != nil {
		t.Fatalf("fetch the SSO admin: %v", err)
	}
	if err := st.Users.SetRoles(ida.ID, []string{models.RoleAdmin}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}

	seeded, err := st.Bootstrap("admin", "a password")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if seeded {
		t.Fatal("an installation with an administrator got a second way in")
	}
	users, err := st.Users.ListAll()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("seeding created an account beside the existing admin: %d users", len(users))
	}
	if _, err := st.Settings.Get(SettingBootstrapSeeded); err == nil {
		t.Fatal("nothing was seeded, so nothing should be recorded as seeded")
	}

	// The marker is deliberately absent here: an installation that loses every
	// administrator must be able to get the break-glass account back. That is
	// what the marker is for on the row-deletion path, and the two rules have to
	// not contradict each other.
	deleted, err := st.Users.GetByUsername("ida")
	if err != nil {
		t.Fatalf("fetch the SSO admin: %v", err)
	}
	if err := st.Users.Delete(deleted.ID); err != nil {
		t.Fatalf("delete the SSO admin: %v", err)
	}
	seeded, err = st.Bootstrap("admin", "a password")
	if err != nil {
		t.Fatalf("bootstrap after the last admin went: %v", err)
	}
	if !seeded {
		t.Fatal("an installation with no way in did not get the break-glass account")
	}
	if count, err := st.Users.CountAdmins(); err != nil || count != 1 {
		t.Fatalf("after re-seeding: count=%d err=%v, want 1", count, err)
	}
}

func TestBootstrapRefusesAnUsernameThatIsAlreadyTaken(t *testing.T) {
	st := newTestStore(t)

	// A local account an operator made, named admin, holding viewer. It has a
	// password the break-glass path has never seen.
	if err := st.Users.Create(&models.User{
		Username:     "admin",
		Provider:     models.ProviderLocal,
		Status:       models.UserStatusActive,
		PasswordHash: "an-existing-argon2-hash",
	}); err != nil {
		t.Fatalf("create the operator's account: %v", err)
	}

	seeded, err := st.Bootstrap("admin", "a password")
	if err == nil {
		t.Fatal("seeding took over a username that was already taken")
	}
	if seeded {
		t.Fatal("seeding reported success beside an error")
	}
	if got := err.Error(); got == "" {
		t.Fatal("the refusal says nothing")
	}

	account, err := st.Users.GetByUsername("admin")
	if err != nil {
		t.Fatalf("fetch the operator's account: %v", err)
	}
	if account.PasswordHash != "an-existing-argon2-hash" {
		t.Fatal("seeding replaced the password of an account it did not create")
	}
	if len(account.Roles) != 0 {
		t.Fatalf("seeding granted roles to an account it did not create: %v", roleNames(account.Roles))
	}
	if _, err := st.Settings.Get(SettingBootstrapSeeded); err == nil {
		t.Fatal("nothing was seeded, so nothing should be recorded as seeded")
	}
	// The roles are seeded regardless: the refusal is about the account.
	if _, err := st.Roles.GetRoleByName(models.RoleViewer); err != nil {
		t.Fatalf("role seeding: %v", err)
	}
}

func TestBootstrapIsIdempotentAcrossBoots(t *testing.T) {
	st := newTestStore(t)

	first, err := st.Bootstrap("admin", "a password")
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if !first {
		t.Fatal("the first boot must seed the account")
	}
	marker, err := st.Settings.Get(SettingBootstrapSeeded)
	if err != nil {
		t.Fatalf("read the seed marker: %v", err)
	}

	for boot := 2; boot <= 3; boot++ {
		seeded, err := st.Bootstrap("admin", "a password")
		if err != nil {
			t.Fatalf("bootstrap on boot %d: %v", boot, err)
		}
		if seeded {
			t.Fatalf("boot %d seeded an account again", boot)
		}
	}

	users, err := st.Users.ListAll()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("repeated boots produced %d accounts, want 1", len(users))
	}
	if count, err := st.Users.CountAdmins(); err != nil || count != 1 {
		t.Fatalf("repeated boots produced count=%d admins (err %v), want 1", count, err)
	}
	after, err := st.Settings.Get(SettingBootstrapSeeded)
	if err != nil {
		t.Fatalf("read the seed marker: %v", err)
	}
	if after != marker {
		t.Fatalf("the seed marker moved from %q to %q across boots that seeded nothing", marker, after)
	}
}

func TestBootstrapRunsOnAStoreBuiltWithoutSeeding(t *testing.T) {
	// The role seeding is Bootstrap's own job, not only store.New's: a Store
	// assembled from a database that was never seeded through buildStore still
	// ends the boot with a role table an operator can grant from.
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := NewWithDB(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := db.Where("1 = 1").Delete(&models.Role{}).Error; err != nil {
		t.Fatalf("clear roles: %v", err)
	}
	if _, err := st.Roles.GetRoleByName(models.RoleOperator); err == nil {
		t.Fatal("the fixture left a builtin role in place")
	}

	seeded, err := st.Bootstrap("admin", "a password")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !seeded {
		t.Fatal("bootstrap did not seed the account")
	}
	for _, name := range []string{models.RoleAdmin, models.RoleOperator, models.RoleViewer} {
		if _, err := st.Roles.GetRoleByName(name); err != nil {
			t.Fatalf("role %s was not seeded by Bootstrap: %v", name, err)
		}
	}
}

// roleNames names a user's roles, so a failure says which role moved.
func roleNames(roles []models.Role) []string {
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, role.Name)
	}
	return names
}
