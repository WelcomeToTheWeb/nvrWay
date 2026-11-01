package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

func newUser(username string) *models.User {
	return &models.User{
		Username:     username,
		Email:        username + "@example.com",
		Name:         "Operator " + username,
		Provider:     models.ProviderLocal,
		PasswordHash: "argon2id$fixture",
		Status:       models.UserStatusActive,
	}
}

func TestUserCreateAndLookups(t *testing.T) {
	st := newTestStore(t)

	user := newUser("admin")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create: %v", err)
	}
	if user.ID == "" {
		t.Fatal("expected the store to mint the account id")
	}
	if user.Provider != models.ProviderLocal {
		t.Fatalf("an account with no provider named is a local account, got %q", user.Provider)
	}

	byName, err := st.Users.GetByUsername("admin")
	if err != nil {
		t.Fatalf("get by username: %v", err)
	}
	if byName.Email != "admin@example.com" {
		t.Fatalf("expected the account back, got %+v", byName)
	}
	if _, err := st.Users.GetByEmail("admin@example.com"); err != nil {
		t.Fatalf("get by email: %v", err)
	}
	if _, err := st.Users.GetByUsername("nobody"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}

	dup := newUser("admin")
	if err := st.Users.Create(dup); err == nil {
		t.Fatal("expected the unique username to be refused")
	}

	count, err := st.Users.Count()
	if err != nil || count != 1 {
		t.Fatalf("expected 1 account, got %d (%v)", count, err)
	}
}

func TestUserPasswordHashIsNotOnTheUpdatePath(t *testing.T) {
	st := newTestStore(t)
	user := newUser("admin")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create: %v", err)
	}
	hash := user.PasswordHash

	// A profile edit that carries no hash must not blank the password: the
	// console reads a user row, edits the name, and writes it back.
	user.Name = "Renamed"
	user.PasswordHash = ""
	if err := st.Users.Update(user); err != nil {
		t.Fatalf("update: %v", err)
	}

	loaded, err := st.Users.GetByID(user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Name != "Renamed" {
		t.Fatalf("expected the edit to land, got %+v", loaded)
	}
	if loaded.PasswordHash != hash {
		t.Fatal("a profile edit wiped the password hash")
	}

	if err := st.Users.SetPassword(user.ID, "argon2id$rotated"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	loaded, err = st.Users.GetByID(user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.PasswordHash != "argon2id$rotated" {
		t.Fatal("expected the rotation to land")
	}
	if err := st.Users.SetPassword("missing", "x"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestUserRolesAreReplacedNotAdded(t *testing.T) {
	st := newTestStore(t)
	user := newUser("operator-one")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := st.Users.SetRoles(user.ID, []string{models.RoleAdmin}); err != nil {
		t.Fatalf("set roles: %v", err)
	}
	if err := st.Users.SetRoles(user.ID, []string{models.RoleOperator}); err != nil {
		t.Fatalf("set roles: %v", err)
	}

	names, err := st.Users.RolesFor(user.ID)
	if err != nil {
		t.Fatalf("roles for: %v", err)
	}
	if len(names) != 1 || names[0] != models.RoleOperator {
		t.Fatalf(`"make this person operator" must not leave admin attached, got %v`, names)
	}

	hasAdmin, err := st.Users.UserHasRole(user.ID, models.RoleAdmin)
	if err != nil {
		t.Fatalf("has role: %v", err)
	}
	if hasAdmin {
		t.Fatal("the account is still admin")
	}

	// The JIT floor and the console's button are both read from the same query.
	preloaded, err := st.Users.GetByID(user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(preloaded.Roles) != 1 || preloaded.Roles[0].Name != models.RoleOperator {
		t.Fatalf("expected the roles preloaded on the row, got %+v", preloaded.Roles)
	}

	// An account created with no roles cannot act: this is the pending identity's
	// state, and the login path refuses it.
	bare := newUser("pending-one")
	bare.Status = models.UserStatusPending
	bare.PasswordHash = ""
	if err := st.Users.Create(bare); err != nil {
		t.Fatalf("create pending: %v", err)
	}
	if names, err := st.Users.RolesFor(bare.ID); err != nil || len(names) != 0 {
		t.Fatalf("a pending identity has no roles, got %v (%v)", names, err)
	}
	if !bare.IsPending() {
		t.Fatal("IsPending must report the state the auth paths refuse")
	}

	if err := st.Users.SetRoles(user.ID, []string{"not-a-role"}); err == nil {
		t.Fatal("expected an unknown role to be refused, not silently skipped")
	}
}

func TestUserCountAdminsIsTheBreakGlassGate(t *testing.T) {
	st := newTestStore(t)

	// The break-glass seeding is gated on "does anyone hold admin", so the
	// answer must come from the role grant, not from a username.
	count, err := st.Users.CountAdmins()
	if err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if count != 0 {
		t.Fatalf("a fresh install has no admin, got %d", count)
	}

	user := newUser("admin")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Users.SetRoles(user.ID, []string{models.RoleAdmin}); err != nil {
		t.Fatalf("set roles: %v", err)
	}
	if count, err := st.Users.CountAdmins(); err != nil || count != 1 {
		t.Fatalf("expected 1 admin, got %d (%v)", count, err)
	}

	// Deleting the account opens the gate again — which is why seeding is gated
	// on a settings row as well, and why Step 2 writes that row.
	if err := st.Users.Delete(user.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if count, err := st.Users.CountAdmins(); err != nil || count != 0 {
		t.Fatalf("expected 0 admins after the delete, got %d (%v)", count, err)
	}
}

func TestUserDeleteTakesRolesAndSessionsWithIt(t *testing.T) {
	st := newTestStore(t)
	user := newUser("admin")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Users.SetRoles(user.ID, []string{models.RoleAdmin}); err != nil {
		t.Fatalf("set roles: %v", err)
	}
	token, hash, err := models.NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	_ = token
	if err := st.Sessions.Create(&models.Session{
		UserID: user.ID, TokenHash: hash, ExpiresAt: time.Now().UTC().Add(models.RefreshTokenTTL),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := st.Users.Delete(user.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var grants, sessions int64
	if err := st.DB.Model(&models.UserRole{}).Where("user_id = ?", user.ID).Count(&grants).Error; err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grants != 0 {
		t.Fatalf("role grants must go with the account, got %d", grants)
	}
	if err := st.DB.Model(&models.Session{}).Where("user_id = ?", user.ID).Count(&sessions).Error; err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Fatal("an account deleted with live sessions leaves tokens that verify against a person who no longer exists")
	}

	if err := st.Users.Delete(user.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found on the second delete, got %v", err)
	}
}

func TestUserFindByProviderIsTheJITLookup(t *testing.T) {
	st := newTestStore(t)

	// The same person signing in twice must land on one account.
	provisioned := &models.User{
		Username:   "ada@id.example",
		Email:      "ada@id.example",
		Provider:   "okta",
		ProviderID: "00u1abc",
		Status:     models.UserStatusActive,
	}
	if err := st.Users.Create(provisioned); err != nil {
		t.Fatalf("create: %v", err)
	}

	found, err := st.Users.FindByProvider("okta", "00u1abc")
	if err != nil {
		t.Fatalf("find by provider: %v", err)
	}
	if found.ID != provisioned.ID {
		t.Fatal("the JIT lookup must find the account it provisioned")
	}
	if _, err := st.Users.FindByProvider("okta", "other"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found for an unknown subject, got %v", err)
	}

	// An OIDC account has no password, so the login handler's gate is the
	// provider, not a UI hint.
	if provisioned.IsLocal() {
		t.Fatal("an OIDC-provisioned account is not a password account")
	}
	local, err := st.Users.GetByUsername("ada@id.example")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if local.IsLocal() {
		t.Fatalf("the row is provider %q; IsLocal must be false", local.Provider)
	}
}

func TestUserMarkLogin(t *testing.T) {
	st := newTestStore(t)
	user := newUser("admin")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.Users.MarkLogin(user.ID, now); err != nil {
		t.Fatalf("mark login: %v", err)
	}
	loaded, err := st.Users.GetByID(user.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.LastLogin == nil || !loaded.LastLogin.Equal(now) {
		t.Fatalf("expected the login stamp, got %v", loaded.LastLogin)
	}
}

func TestUserRolesStoreGrantAndRevoke(t *testing.T) {
	st := newTestStore(t)
	user := newUser("operator-two")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create: %v", err)
	}
	role, err := st.Roles.GetRoleByName(models.RoleOperator)
	if err != nil {
		t.Fatalf("get role: %v", err)
	}

	if err := st.UserRoles.Grant(user.ID, role.ID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Granting twice is one grant: the ledger says what changed, not how often
	// the button was pressed.
	if err := st.UserRoles.Grant(user.ID, role.ID); err != nil {
		t.Fatalf("grant twice: %v", err)
	}
	grants, err := st.UserRoles.ListFor(user.ID)
	if err != nil {
		t.Fatalf("list for: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected one grant, got %d", len(grants))
	}
	if grants[0].ID == "" {
		t.Fatal("the grant row needs its own id: 'who gave this person operator, when' is a row")
	}

	if err := st.UserRoles.Revoke(user.ID, role.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := st.UserRoles.Revoke(user.ID, role.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found revoking a grant that is gone, got %v", err)
	}
}
