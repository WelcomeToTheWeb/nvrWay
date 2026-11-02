package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"nvr/server/models"
	"nvr/server/sso/testidp/provider"
)

// The administrator's surface. What these tests are protecting is narrower than
// "the routes work": it is the three things an admin can do to an installation that
// an installation cannot recover from on its own — remove the last way in, leave
// someone's access live after removing it, and write a role mapping that takes the
// floor away from people who cannot ask for it back.

// usersToken is the break-glass admin's bearer token: the account every admin route
// is reached with.
func usersToken(t *testing.T, h *Handlers) (string, *models.User) {
	t.Helper()
	admin, err := h.St.Users.GetByUsername("admin")
	if err != nil {
		t.Fatalf("reading the break-glass admin: %v", err)
	}
	return accessToken(t, h.St, h.JWT, admin), admin
}

func TestUsersGateIsAdminOnly(t *testing.T) {
	h := newHandlers(t, newStore(t))
	router := routerFor(t, h)
	adminToken, _ := usersToken(t, h)

	watcher := newUser(t, h.St, "watcher-users", models.RoleViewer)
	worker := newUser(t, h.St, "operator-users", models.RoleOperator)

	writes := []request{
		{method: http.MethodGet, path: "/api/users"},
		{method: http.MethodPost, path: "/api/users", body: map[string]any{"username": "nope", "password": "a-long-enough-password"}},
		{method: http.MethodGet, path: "/api/roles"},
		{method: http.MethodPost, path: "/api/users/" + watcher.ID + "/roles", body: map[string]any{"roles": []string{models.RoleAdmin}}},
		{method: http.MethodDelete, path: "/api/users/" + watcher.ID},
	}

	for _, u := range []*models.User{watcher, worker} {
		token := accessToken(t, h.St, h.JWT, u)
		for _, r := range writes {
			status, body, _ := do(t, router, request{method: r.method, path: r.path, bearer: token, body: r.body})
			if status != http.StatusForbidden {
				t.Fatalf("%s on %s %s: got %d %v, want 403", u.Username, r.method, r.path, status, body)
			}
			if body["error"] != "insufficient role" {
				t.Errorf("%s on %s %s answered %v, want \"insufficient role\"", u.Username, r.method, r.path, body["error"])
			}
		}
	}

	// The admin reaches all of it.
	for _, r := range writes[:3] {
		status, body, _ := do(t, router, request{method: r.method, path: r.path, bearer: adminToken, body: r.body})
		if status == http.StatusForbidden || status == http.StatusUnauthorized {
			t.Fatalf("admin on %s %s: got %d %v, want the admin surface", r.method, r.path, status, body)
		}
	}

	// Each refusal is on the record, under the verb of the act it refused.
	refused := 0
	for _, action := range []string{models.AuditUserRead, models.AuditUserCreate, models.AuditRoleRead, models.AuditUserRoleChange, models.AuditUserDelete} {
		for _, row := range auditRows(t, h.St, action) {
			if row.Outcome == models.AuditOutcomeRefused {
				refused++
			}
		}
	}
	if refused < len(writes)*2 {
		t.Errorf("the admin gate wrote %d refused rows, want one per refused request (%d)", refused, len(writes)*2)
	}
}

func TestUsersCreateMakesAWorkingAccount(t *testing.T) {
	h := newHandlers(t, newStore(t))
	router := routerFor(t, h)
	token, _ := usersToken(t, h)

	status, body, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/users", bearer: token,
		body: map[string]any{
			"username": "nightshift", "password": "a-long-enough-password",
			"name": "Night Shift", "email": "night@example.test", "roles": []string{models.RoleOperator},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create: got %d %v, want 201", status, body)
	}
	if body["provider"] != models.ProviderLocal {
		t.Errorf("provider = %v, want local: an account made by hand authenticates by password", body["provider"])
	}
	roles, _ := body["roles"].([]any)
	if len(roles) != 1 || roles[0] != models.RoleOperator {
		t.Errorf("roles = %v, want [operator]", body["roles"])
	}
	if _, has := body["password_hash"]; has {
		t.Error("the create response carries a password hash")
	}

	// The account works: it is an account, not a row.
	status, login, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "nightshift", "password": "a-long-enough-password"},
	})
	if status != http.StatusOK {
		t.Fatalf("the created account cannot log in: got %d %v", status, login)
	}

	// And the refusal shapes: a password under the floor names the floor, an
	// unknown role names the role, a taken username is 409.
	status, body, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/users", bearer: token,
		body: map[string]any{"username": "shortie", "password": "short"},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a 5-character password: got %d %v, want 422", status, body)
	}

	status, body, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/users", bearer: token,
		body: map[string]any{"username": "typo", "password": "a-long-enough-password", "roles": []string{"operaotr"}},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown role: got %d %v, want 422", status, body)
	}
	if !strings.Contains(body["error"].(string), "operaotr") {
		t.Errorf("the refusal is %v, want it to name the role it did not recognise", body["error"])
	}

	status, body, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/users", bearer: token,
		body: map[string]any{"username": "nightshift", "password": "a-long-enough-password"},
	})
	if status != http.StatusConflict {
		t.Fatalf("a taken username: got %d %v, want 409", status, body)
	}

	if rows := auditRows(t, h.St, models.AuditUserCreate); len(rows) == 0 {
		t.Error("the account creation is not in the ledger")
	}
}

func TestUsersNoAccountArrivesWithoutARole(t *testing.T) {
	// The floor is a rule, not a default the console has to remember: an account
	// created with no roles named is a `viewer`, and an account created with no
	// roles at all is an account that logs in and sees nothing.
	h := newHandlers(t, newStore(t))
	router := routerFor(t, h)
	token, _ := usersToken(t, h)

	status, body, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/users", bearer: token,
		body: map[string]any{"username": "floor", "password": "a-long-enough-password"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create: got %d %v, want 201", status, body)
	}
	roles, _ := body["roles"].([]any)
	if len(roles) != 1 || roles[0] != models.RoleViewer {
		t.Fatalf("roles = %v, want exactly [viewer]", body["roles"])
	}
}

func TestUsersDeletingTheLastAdminIsRefused(t *testing.T) {
	h := newHandlers(t, newStore(t))
	router := routerFor(t, h)
	token, admin := usersToken(t, h)

	status, body, _ := do(t, router, request{
		method: http.MethodDelete, path: "/api/users/" + admin.ID, bearer: token,
	})
	if status != http.StatusConflict {
		t.Fatalf("deleting the last admin: got %d %v, want 409", status, body)
	}
	if !strings.Contains(body["error"].(string), "last admin") {
		t.Errorf("the refusal is %v, want it to say what is at stake", body["error"])
	}
	if _, err := h.St.Users.GetByUsername("admin"); err != nil {
		t.Fatal("the refused delete removed the admin")
	}
	rows := auditRows(t, h.St, models.AuditUserDelete)
	if len(rows) != 1 || rows[0].Outcome != models.AuditOutcomeRefused {
		t.Fatalf("the refused delete wrote %d rows, want 1 refused row", len(rows))
	}

	// The rule is about the *last* admin, not about admins: with a second admin in
	// the installation, one of them can be removed.
	status, created, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/users", bearer: token,
		body: map[string]any{"username": "second-admin", "password": "a-long-enough-password", "roles": []string{models.RoleAdmin}},
	})
	if status != http.StatusCreated {
		t.Fatalf("creating the second admin: got %d %v, want 201", status, created)
	}

	status, body, _ = do(t, router, request{
		method: http.MethodDelete, path: "/api/users/" + created["id"].(string), bearer: token,
	})
	if status != http.StatusOK {
		t.Fatalf("deleting a non-last admin: got %d %v, want 200", status, body)
	}

	// The same lockout by another route: parking the last admin as `pending` takes
	// away its login, which is the same installation with nobody who can fix it.
	status, body, _ = do(t, router, request{
		method: http.MethodPut, path: "/api/users/" + admin.ID, bearer: token,
		body: map[string]any{"status": models.UserStatusPending},
	})
	if status != http.StatusConflict {
		t.Fatalf("parking the last admin: got %d %v, want 409", status, body)
	}

	// And the account, its role grants and its sessions go together: a deleted
	// account with live sessions is a token that verifies against nobody.
	status, created, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/users", bearer: token,
		body: map[string]any{"username": "watcher-doomed", "password": "a-long-enough-password", "roles": []string{models.RoleViewer}},
	})
	watcherID := created["id"].(string)
	watcher, err := h.St.Users.GetByID(watcherID)
	if err != nil {
		t.Fatalf("reading the account: %v", err)
	}
	watcherToken := accessToken(t, h.St, h.JWT, watcher)
	refresh := refreshToken(t, h.JWT, watcher)
	recordSession(t, h.St, watcherID, refresh)

	status, _, _ = do(t, router, request{method: http.MethodDelete, path: "/api/users/" + watcherID, bearer: token})
	if status != http.StatusOK {
		t.Fatalf("deleting a plain account: got %d, want 200", status)
	}
	status, _, _ = do(t, router, request{method: http.MethodGet, path: "/api/auth/profile", bearer: watcherToken})
	if status != http.StatusUnauthorized {
		t.Fatalf("the deleted account's access token still verifies: got %d, want 401", status)
	}
	status, _, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": refresh},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("the deleted account's refresh token still rotates: got %d, want 401", status)
	}
}

func TestUsersRoleChangeEndsTheSessionsItChanges(t *testing.T) {
	h := newHandlers(t, newStore(t))
	router := routerFor(t, h)
	token, _ := usersToken(t, h)

	status, created, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/users", bearer: token,
		body: map[string]any{"username": "operator-to-watch", "password": "a-long-enough-password", "roles": []string{models.RoleOperator}},
	})
	userID := created["id"].(string)
	user, err := h.St.Users.GetByID(userID)
	if err != nil {
		t.Fatalf("reading the account: %v", err)
	}
	theirAccess := accessToken(t, h.St, h.JWT, user)
	theirRefresh := refreshToken(t, h.JWT, user)
	recordSession(t, h.St, userID, theirRefresh)

	// The write, in the ledger's words: from operator to viewer, sessions dropped,
	// access tokens expired.
	status, body, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/users/" + userID + "/roles", bearer: token,
		body: map[string]any{"roles": []string{models.RoleViewer}},
	})
	if status != http.StatusOK {
		t.Fatalf("role change: got %d %v, want 200", status, body)
	}
	if dropped, _ := body["live_sessions_dropped"].(float64); dropped < 1 {
		t.Errorf("live_sessions_dropped = %v, want the session this account held", body["live_sessions_dropped"])
	}

	// The gate reads the role rows, so the next request is refused...
	status, body, _ = do(t, router, request{
		method: http.MethodGet, path: "/api/users", bearer: theirAccess,
	})
	if status == http.StatusOK {
		t.Fatal("the demoted account's access token still reaches the admin surface")
	}
	// ...and the refresh token, which mints access tokens, is closed with the
	// session. This is the difference between "removed" and "removed when they
	// next ask".
	status, _, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": theirRefresh},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("the demoted account's refresh token still rotates: got %d, want 401", status)
	}

	rows := auditRows(t, h.St, models.AuditUserRoleChange)
	if len(rows) != 1 {
		t.Fatalf("the role change wrote %d ledger rows, want 1", len(rows))
	}
	for _, want := range []string{`"from":["operator"]`, `"to":["viewer"]`, "operator-to-watch"} {
		if !strings.Contains(rows[0].Details, want) {
			t.Errorf("the role-change row is missing %s: %s", want, rows[0].Details)
		}
	}
	if rows[0].ActorUsername != "admin" {
		t.Errorf("the role-change row names %q as the actor, want the admin who made the change", rows[0].ActorUsername)
	}
	if rows[0].TargetID != userID {
		t.Errorf("the role-change row points at %q, want the account whose roles changed", rows[0].TargetID)
	}

	// The last admin cannot be demoted, by the same rule as the delete.
	_, admin := usersToken(t, h)
	status, body, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/users/" + admin.ID + "/roles", bearer: token,
		body: map[string]any{"roles": []string{models.RoleViewer}},
	})
	if status != http.StatusConflict {
		t.Fatalf("demoting the last admin: got %d %v, want 409", status, body)
	}
	if roles, _ := h.St.Users.RolesFor(admin.ID); len(roles) != 1 || roles[0] != models.RoleAdmin {
		t.Errorf("the refused demotion changed the roles to %v, want admin", roles)
	}
}

func TestUsersUpdateChangesTheAccountAndResetsItsPassword(t *testing.T) {
	h := newHandlers(t, newStore(t))
	router := routerFor(t, h)
	token, _ := usersToken(t, h)

	status, created, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/users", bearer: token,
		body: map[string]any{"username": "renamable", "password": "a-long-enough-password", "roles": []string{models.RoleViewer}},
	})
	userID := created["id"].(string)
	user, err := h.St.Users.GetByID(userID)
	if err != nil {
		t.Fatalf("reading the account: %v", err)
	}
	theirAccess := accessToken(t, h.St, h.JWT, user)
	theirRefresh := refreshToken(t, h.JWT, user)
	recordSession(t, h.St, userID, theirRefresh)

	status, body, _ := do(t, router, request{
		method: http.MethodPut, path: "/api/users/" + userID, bearer: token,
		body: map[string]any{"name": "Renamed Person", "email": "renamed@example.test"},
	})
	if status != http.StatusOK {
		t.Fatalf("update: got %d %v, want 200", status, body)
	}
	updated, _ := body["user"].(map[string]any)
	if updated["name"] != "Renamed Person" || updated["email"] != "renamed@example.test" {
		t.Errorf("the update answered %+v, want the account with its new name and email", body["user"])
	}
	if dropped, _ := body["live_sessions_dropped"].(float64); dropped != 0 {
		t.Errorf("a name-only change dropped %v sessions, want 0: nothing about the credentials changed", dropped)
	}

	// An admin resetting a password ends every session of that account, exactly as
	// the account owner's own password change does.
	status, body, _ = do(t, router, request{
		method: http.MethodPut, path: "/api/users/" + userID, bearer: token,
		body: map[string]any{"password": "a-different-long-password"},
	})
	if status != http.StatusOK {
		t.Fatalf("password reset: got %d %v, want 200", status, body)
	}
	if dropped, _ := body["live_sessions_dropped"].(float64); dropped < 1 {
		t.Errorf("live_sessions_dropped = %v, want the account's sessions", body["live_sessions_dropped"])
	}
	status, _, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "renamable", "password": "a-long-enough-password"},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("the reset-overwritten password still logs in: got %d, want 401", status)
	}
	status, _, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "renamable", "password": "a-different-long-password"},
	})
	if status != http.StatusOK {
		t.Fatalf("the new password does not log in: got %d, want 200", status)
	}
	status, _, _ = do(t, router, request{method: http.MethodGet, path: "/api/auth/profile", bearer: theirAccess})
	if status != http.StatusUnauthorized {
		t.Fatalf("the pre-reset access token still verifies: got %d, want 401", status)
	}
	status, _, _ = do(t, router, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": theirRefresh},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("the pre-reset refresh token still rotates: got %d, want 401", status)
	}

	// An admin cannot put a password on a provider account from here either: the
	// account's identity is the IdP's, and a password set from the admin's side
	// would be the second door built by the person who configured the first.
	fixture := newSSOFixture(t, ssoClaims(), nil)
	fixture.walk(t)
	providerUser, err := fixture.h.St.Users.FindByProvider(ssoProviderName, "subject-ops")
	if err != nil || providerUser == nil {
		t.Fatalf("the walk provisioned no account: %v", err)
	}
	fixtureToken, _ := usersToken(t, fixture.h)
	status, body, _ = do(t, routerFor(t, fixture.h), request{
		method: http.MethodPut, path: "/api/users/" + providerUser.ID, bearer: fixtureToken,
		body: map[string]any{"password": "a-different-long-password"},
	})
	if status != http.StatusForbidden {
		t.Fatalf("setting a password on a provider account: got %d %v, want 403", status, body)
	}

	// The role list is the gate's authority, and the console's checkboxes.
	status, body, _ = do(t, router, request{method: http.MethodGet, path: "/api/roles", bearer: token})
	if status != http.StatusOK {
		t.Fatalf("roles: got %d %v, want 200", status, body)
	}
	roles, _ := body["roles"].([]any)
	if len(roles) < 3 {
		t.Fatalf("the role table is %v, want the three built-ins", body["roles"])
	}
	encoded, _ := json.Marshal(roles)
	for _, want := range []string{models.RoleViewer, models.RoleOperator, models.RoleAdmin} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("the role table is missing %s: %s", want, encoded)
		}
	}
}

func TestUsersGetAnswersTheSessionsAndTheDoor(t *testing.T) {
	h := newHandlers(t, newStore(t))
	router := routerFor(t, h)
	token, admin := usersToken(t, h)

	status, body, _ := do(t, router, request{method: http.MethodGet, path: "/api/users/" + admin.ID, bearer: token})
	if status != http.StatusOK {
		t.Fatalf("get: got %d %v, want 200", status, body)
	}
	if body["password_login"] != true {
		t.Errorf("password_login = %v, want true: this is the break-glass account, and its door is the one the IdP cannot close", body["password_login"])
	}
	if _, has := body["password_hash"]; has {
		t.Error("the admin surface carries a password hash")
	}
	if _, has := body["last_login"]; !has {
		t.Error("the admin surface has no last_login: it is the fact an operator reads about a break-glass account")
	}

	// A live session is named with what an operator uses to pick it out: the
	// address and the user agent.
	refresh := refreshToken(t, h.JWT, admin)
	recordSession(t, h.St, admin.ID, refresh)
	status, body, _ = do(t, router, request{method: http.MethodGet, path: "/api/users/" + admin.ID, bearer: token})
	sessions, _ := body["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %v, want the one live session", body["sessions"])
	}
	session := sessions[0].(map[string]any)
	if session["source_ip"] == "" || session["user_agent"] == "" {
		t.Errorf("the session is %v, want its address and user agent", session)
	}
	if live, _ := body["live_sessions"].(float64); live != 1 {
		t.Errorf("live_sessions = %v, want 1", body["live_sessions"])
	}

	// A provider account has no password door, and the admin surface says so. The
	// JIT account arrives from the walk, so the walk is what puts the account here.
	fixture := newSSOFixture(t, ssoClaims(), nil)
	fixture.walk(t)
	providerUser, err := fixture.h.St.Users.FindByProvider(ssoProviderName, "subject-ops")
	if err != nil || providerUser == nil {
		t.Fatalf("the walk provisioned no account: %v", err)
	}
	fixtureToken, _ := usersToken(t, fixture.h)
	status, body, _ = do(t, routerFor(t, fixture.h), request{
		method: http.MethodGet, path: "/api/users/" + providerUser.ID, bearer: fixtureToken,
	})
	if status != http.StatusOK {
		t.Fatalf("reading a provider account: got %d %v, want 200", status, body)
	}
	if body["password_login"] != false {
		t.Errorf("password_login = %v for a provider account, want false", body["password_login"])
	}
}

func TestUsersRoleMappingRefusesTheViewerFloor(t *testing.T) {
	fixture := newSSOFixture(t, ssoClaims(), nil)
	h := fixture.h
	router := routerFor(t, h)
	token, _ := usersToken(t, h)

	// A mapping the operator is allowed to write, on the provider row the fixture
	// carries.
	status, body, _ := do(t, router, request{
		method: http.MethodPut, path: "/api/settings/oidc/" + ssoProviderName + "/role-mapping",
		bearer: token, body: map[string]any{"role_mapping": map[string]string{"nvr-operators": models.RoleOperator}},
	})
	if status != http.StatusOK {
		t.Fatalf("a mapping to operator: got %d %v, want 200", status, body)
	}
	row, err := h.St.SSOProviders.GetByName(ssoProviderName)
	if err != nil {
		t.Fatalf("reading the provider row: %v", err)
	}
	if !strings.Contains(row.RoleMapping, models.RoleOperator) {
		t.Errorf("the provider row carries %q, want the mapping the admin wrote", row.RoleMapping)
	}

	// The floor may not be mapped. A mapping replaces roles on every login, so a
	// mapping that owns `viewer` decides that anyone in no mapped group has no
	// roles — and the admin who has to undo it is the one the mapping just locked
	// out, through the only door that provider's users have.
	status, body, _ = do(t, router, request{
		method: http.MethodPut, path: "/api/settings/oidc/" + ssoProviderName + "/role-mapping",
		bearer: token, body: map[string]any{"role_mapping": map[string]string{"everyone": models.RoleViewer}},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a mapping to viewer: got %d %v, want 422", status, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, models.RoleViewer) {
		t.Errorf("the refusal is %q, want it to name the floor", msg)
	}
	if !strings.Contains(msg, "login") {
		t.Errorf("the refusal is %q, want it to say why the floor may not be mapped", msg)
	}
	if rows := auditRows(t, h.St, models.AuditOIDCUpdate); len(rows) == 0 || rows[0].Outcome != models.AuditOutcomeRefused {
		t.Errorf("the refused mapping is not on the ledger: %+v", rows)
	}

	// A role that does not exist is refused by name: a mapping saved with a typo
	// grants nothing and looks configured.
	status, body, _ = do(t, router, request{
		method: http.MethodPut, path: "/api/settings/oidc/" + ssoProviderName + "/role-mapping",
		bearer: token, body: map[string]any{"role_mapping": map[string]string{"ops": "operators-squad"}},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a mapping to a role that does not exist: got %d %v, want 422", status, body)
	}
	if !strings.Contains(body["error"].(string), "operators-squad") {
		t.Errorf("the refusal is %v, want it to name the role", body["error"])
	}

	// Clearing the mapping is allowed: no mapping is the floor standing, not
	// everyone becoming an admin.
	status, body, _ = do(t, router, request{
		method: http.MethodPut, path: "/api/settings/oidc/" + ssoProviderName + "/role-mapping",
		bearer: token, body: map[string]any{"role_mapping": map[string]string{}},
	})
	if status != http.StatusOK {
		t.Fatalf("clearing the mapping: got %d %v, want 200", status, body)
	}

	// The route is an admin route, and a provider that does not exist is 404.
	status, body, _ = do(t, router, request{
		method: http.MethodPut, path: "/api/settings/oidc/no-such-provider/role-mapping",
		bearer: token, body: map[string]any{"role_mapping": map[string]string{"ops": models.RoleOperator}},
	})
	if status != http.StatusNotFound {
		t.Fatalf("a mapping on a missing provider: got %d %v, want 404", status, body)
	}
	watcher := newUser(t, h.St, "watcher-mapping", models.RoleViewer)
	status, body, _ = do(t, router, request{
		method: http.MethodPut, path: "/api/settings/oidc/" + ssoProviderName + "/role-mapping",
		bearer: accessToken(t, h.St, h.JWT, watcher), body: map[string]any{"role_mapping": map[string]string{"ops": models.RoleOperator}},
	})
	if status != http.StatusForbidden {
		t.Fatalf("a viewer writing the mapping: got %d %v, want 403", status, body)
	}
}

func TestUsersListFiltersAndNeverEchoesSecrets(t *testing.T) {
	h := newHandlers(t, newStore(t))
	router := routerFor(t, h)
	token, _ := usersToken(t, h)

	status, created, _ := do(t, router, request{
		method: http.MethodPost, path: "/api/users", bearer: token,
		body: map[string]any{"username": "filter-me", "password": "a-long-enough-password", "roles": []string{models.RoleOperator}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create: got %d %v", status, created)
	}

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"provider", "provider=" + models.ProviderLocal},
		{"role", "role=" + models.RoleOperator},
		{"status", "status=" + models.UserStatusActive},
		{"query", "q=filter"},
	} {
		status, body, _ := do(t, router, request{method: http.MethodGet, path: "/api/users", bearer: token, query: tc.query})
		if status != http.StatusOK {
			t.Fatalf("%s filter: got %d %v, want 200", tc.name, status, body)
		}
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "password_hash") {
			t.Errorf("the %s filter leaked a password hash: %s", tc.name, encoded)
		}
		if strings.Contains(string(encoded), "a-long-enough-password") {
			t.Errorf("the %s filter leaked a password: %s", tc.name, encoded)
		}
	}

	// The role filter is the one an operator uses to find "who are the admins", so
	// it has to select, not just answer.
	status, body, _ := do(t, router, request{method: http.MethodGet, path: "/api/users", bearer: token, query: "role=" + models.RoleAdmin})
	admins, _ := body["users"].([]any)
	if len(admins) != 1 {
		t.Fatalf("the admin filter returned %d accounts, want the one admin", len(admins))
	}

	// An unparseable limit is refused rather than read as a number.
	status, body, _ = do(t, router, request{method: http.MethodGet, path: "/api/users", bearer: token, query: "limit=0"})
	if status != http.StatusBadRequest {
		t.Fatalf("limit=0: got %d %v, want 400", status, body)
	}
}

func TestUsersRoutesAreBearerOnly(t *testing.T) {
	h := newHandlers(t, newStore(t))
	router := routerFor(t, h)
	token, _ := usersToken(t, h)

	// The admin surface is exactly where a token in the URL becomes a leak: these
	// are the URLs that get pasted into a chat to ask "why can't this person in?".
	status, body, _ := do(t, router, request{method: http.MethodGet, path: "/api/users", query: "token=" + token})
	if status != http.StatusUnauthorized {
		t.Fatalf("a list read with ?token= answered %d %v, want 401", status, body)
	}
	if rows := auditRows(t, h.St, models.AuditUserRead); len(rows) != 0 {
		t.Errorf("an unauthenticated list read wrote %d ledger rows, want 0", len(rows))
	}
}

// TestUsersProviderAccountCannotReceiveAPasswordFromItsOwnRoute closes the pair of
// S2-06's rule and S2-09's: neither the account owner nor the admin can put a
// password on an account whose identity an IdP asserts.
func TestUsersProviderAccountCannotReceiveAPassword(t *testing.T) {
	fixture := newSSOFixture(t, provider.Claims{
		Subject: "subject-no-password", Email: "nopass@nvrway.test", EmailVerified: true,
		Name: "No Password", Username: "nopass",
	}, nil)
	handoff, _ := fixture.walk(t)
	_, fragment := handoffParts(t, handoff)
	access := fragment.Get("access_token")

	status, body, _ := do(t, routerFor(t, fixture.h), request{
		method: http.MethodPut, path: "/api/auth/password", bearer: access,
		body: map[string]any{"current_password": "whatever-it-was", "new_password": "a-different-long-password"},
	})
	if status != http.StatusForbidden {
		t.Fatalf("the owner setting a password on a provider account: got %d %v, want 403", status, body)
	}
}
