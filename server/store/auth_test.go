package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

// The account side of the store: the rows Step 2's auth handlers read on every
// request. A session store with a hole in it is a session store that lets a
// revoked token through, so the state transitions are tested here, not in the
// handler.

func TestAccountLookupShapes(t *testing.T) {
	st := newTestStore(t)

	local := &models.User{
		Username: "watcher", Email: "watcher@example", PasswordHash: "argon2id$x",
		Provider: models.ProviderLocal, Status: models.UserStatusActive,
	}
	if err := st.Users.Create(local); err != nil {
		t.Fatalf("create: %v", err)
	}

	byUsername, err := st.Users.GetByUsername("watcher")
	if err != nil {
		t.Fatalf("get by username: %v", err)
	}
	byEmail, err := st.Users.GetByEmail("watcher@example")
	if err != nil {
		t.Fatalf("get by email: %v", err)
	}
	if byUsername.ID != byEmail.ID {
		t.Fatal("the login handler's two lookups name the same account")
	}

	// The JIT lookup: an OIDC assertion is matched on (provider, provider_id),
	// which is the pair the IdP owns and no one else can rename.
	provisioned := &models.User{
		Username: "keycloak-ops", Email: "ops@example", Provider: "keycloak", ProviderID: "subject-1",
		Status: models.UserStatusActive,
	}
	if err := st.Users.Create(provisioned); err != nil {
		t.Fatalf("create: %v", err)
	}
	found, err := st.Users.FindByProvider("keycloak", "subject-1")
	if err != nil {
		t.Fatalf("find by provider: %v", err)
	}
	if found.ID != provisioned.ID {
		t.Fatal("expected the JIT-provisioned account")
	}

	// The same subject at a different provider is a different person: the pair
	// is the identity, not the subject alone.
	if _, err := st.Users.FindByProvider("other-idp", "subject-1"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected no account for the same subject at another provider, got %v", err)
	}
	if _, err := st.Users.FindByProvider("keycloak", "subject-2"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
	if _, err := st.Users.GetByUsername("nobody"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
	if _, err := st.Users.GetByEmail("nobody@example"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}

	// A local account has no provider identity to match on, and the lookup must
	// not answer it: a JIT match on an empty subject would hand out the local
	// account to any assertion with no `sub`.
	if _, err := st.Users.FindByProvider(models.ProviderLocal, ""); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected no account for an empty subject, got %v", err)
	}
}

func TestAccountPasswordHashSurvivesProfileEdits(t *testing.T) {
	st := newTestStore(t)
	user := &models.User{
		Username: "watcher", Email: "w@example", PasswordHash: "argon2id$original",
		Provider: models.ProviderLocal, Status: models.UserStatusActive, Name: "Watch",
	}
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The profile editor sends name, email, username. It does not send a
	// password, and it must not clear one: an account whose hash is wiped by a
	// profile edit cannot log in, and the operator's only door is the one that
	// just closed.
	edit := &models.User{ID: user.ID, Username: "watcher", Email: "w@example", Name: "Watched", Status: models.UserStatusActive}
	if err := st.Users.Update(edit); err != nil {
		t.Fatalf("update: %v", err)
	}
	loaded, err := st.Users.GetByID(user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.PasswordHash != "argon2id$original" {
		t.Fatal("the profile update wiped the password hash")
	}
	if loaded.Name != "Watched" {
		t.Fatalf("expected the edit to land, got %+v", loaded)
	}

	// SetPassword is the only path that writes a hash.
	if err := st.Users.SetPassword(user.ID, "argon2id$rotated"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	loaded, err = st.Users.GetByID(user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.PasswordHash != "argon2id$rotated" {
		t.Fatal("expected the rotated hash")
	}
}

func TestAccountRoleGradesAreReplacedNotAdded(t *testing.T) {
	st := newTestStore(t)
	user := &models.User{Username: "watcher", Email: "w@example", PasswordHash: "argon2id$x", Status: models.UserStatusActive}
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := st.Users.SetRoles(user.ID, []string{models.RoleViewer}); err != nil {
		t.Fatalf("set roles: %v", err)
	}
	if err := st.Users.SetRoles(user.ID, []string{models.RoleOperator}); err != nil {
		t.Fatalf("set roles: %v", err)
	}

	roles, err := st.Users.RolesFor(user.ID)
	if err != nil {
		t.Fatalf("roles for user: %v", err)
	}
	// The role editor sets the account's roles. If it appended, a person demoted
	// to viewer would keep the operator's grants, and the console would show the
	// roles it just wrote.
	if len(roles) != 1 || roles[0] != models.RoleOperator {
		t.Fatalf("expected exactly the roles that were set, got %v", roles)
	}

	isOperator, err := st.Users.UserHasRole(user.ID, models.RoleOperator)
	if err != nil {
		t.Fatalf("user has role: %v", err)
	}
	if !isOperator {
		t.Fatal("expected the operator grant")
	}
	isAdmin, err := st.Users.UserHasRole(user.ID, models.RoleAdmin)
	if err != nil {
		t.Fatalf("user has role: %v", err)
	}
	if isAdmin {
		t.Fatal("an operator is not an admin")
	}
}

func TestAdminCountIsTheBreakGlassGate(t *testing.T) {
	st := newTestStore(t)

	// The bootstrap admin is created only when no user holds role `admin`. The
	// gate is this count, so it must count role grants and nothing else.
	count, err := st.Users.CountAdmins()
	if err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if count != 0 {
		t.Fatalf("a fresh installation has no admin, got %d", count)
	}

	operator := &models.User{Username: "ops", Email: "ops@example", PasswordHash: "argon2id$x", Status: models.UserStatusActive}
	if err := st.Users.Create(operator); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Users.SetRoles(operator.ID, []string{models.RoleOperator}); err != nil {
		t.Fatalf("set roles: %v", err)
	}
	count, err = st.Users.CountAdmins()
	if err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if count != 0 {
		t.Fatalf("an operator is not an admin, got %d", count)
	}

	root := &models.User{Username: "root", Email: "root@example", PasswordHash: "argon2id$y", Status: models.UserStatusActive}
	if err := st.Users.Create(root); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Users.SetRoles(root.ID, []string{models.RoleAdmin, models.RoleViewer}); err != nil {
		t.Fatalf("set roles: %v", err)
	}
	count, err = st.Users.CountAdmins()
	if err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one admin, got %d", count)
	}

	// An account deleted must stop counting: an installation whose only admin
	// was deleted has to be able to re-seed the break-glass account.
	if err := st.Users.Delete(root.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	count, err = st.Users.CountAdmins()
	if err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 admins after deleting the only admin, got %d", count)
	}
}

func TestAccountDeleteTakesRolesAndSessions(t *testing.T) {
	st := newTestStore(t)
	user := &models.User{Username: "watcher", Email: "w@example", PasswordHash: "argon2id$x", Status: models.UserStatusActive}
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.Users.SetRoles(user.ID, []string{models.RoleViewer}); err != nil {
		t.Fatalf("set roles: %v", err)
	}
	_, hash, err := models.NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	session := &models.Session{
		UserID: user.ID, TokenHash: hash, ExpiresAt: time.Now().UTC().Add(models.RefreshTokenTTL),
	}
	if err := st.Sessions.Create(session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// `DELETE /api/users/:id` answers one status code. A deleted account that
	// left a live session behind is a session that authenticates as an account
	// the console says does not exist.
	if err := st.Users.Delete(user.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Users.GetByID(user.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected the account gone, got %v", err)
	}
	if _, err := st.Sessions.GetByTokenHash(hash); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected the session gone with its account, got %v", err)
	}
	grants, err := st.UserRoles.ListFor(user.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("expected the role grants gone, got %d", len(grants))
	}

	if err := st.Users.Delete(user.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found on the second delete, got %v", err)
	}
}

func TestLoginStampIsTheAccountsPage(t *testing.T) {
	st := newTestStore(t)
	user := &models.User{Username: "watcher", Email: "w@example", PasswordHash: "argon2id$x", Status: models.UserStatusActive}
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.LastLogin != nil {
		t.Fatal("an account that has never logged in has no stamp")
	}

	at := time.Now().UTC()
	if err := st.Users.MarkLogin(user.ID, at); err != nil {
		t.Fatalf("mark login: %v", err)
	}
	loaded, err := st.Users.GetByID(user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.LastLogin == nil || !loaded.LastLogin.Equal(at) {
		t.Fatalf("expected the login stamp, got %v", loaded.LastLogin)
	}
}

func TestPendingAccountHasNoRolesAndNoPassword(t *testing.T) {
	st := newTestStore(t)

	// JIT provisioning parks an identity at the `pending` status when the link
	// policy refuses to claim an account: the row exists so an admin can see and
	// decide it, and it authenticates as nothing.
	pending := &models.User{
		Username: "keycloak-ops", Email: "ops@example", Provider: "keycloak", ProviderID: "subject-1",
		Status: models.UserStatusPending,
	}
	if err := st.Users.Create(pending); err != nil {
		t.Fatalf("create: %v", err)
	}
	if pending.PasswordHash != "" {
		t.Fatal("a JIT-provisioned account has no password")
	}
	roles, err := st.Users.RolesFor(pending.ID)
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	if len(roles) != 0 {
		t.Fatalf("a pending account holds no roles, got %v", roles)
	}

	count, err := st.Users.Count()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the pending account counted (the console lists it), got %d", count)
	}
}
