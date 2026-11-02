package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"nvr/server/config"
	"nvr/server/models"
	"nvr/server/store"
)

// The card's four exit criteria, and the shapes the console and
// scripts/verify.sh read.

func newHandlers(t *testing.T, st *store.Store) *Handlers {
	t.Helper()
	cfg := &config.Config{
		ServerPort:         ":8080",
		JWTSecret:          "unit-test-signing-secret-not-a-deployment-one",
		DataDir:            t.TempDir(),
		WebURL:             "http://localhost:3000",
		WorkerToken:        "",
		RateLimitPerMinute: 120,
		RateLimitBurst:     12,
		SegmentSeconds:     5,
		TZ:                 "UTC",
	}
	h := NewHandlers(context.Background(), st, nil, nil, cfg)
	// The break-glass admin's password, as newStore seeds it.
	return h
}

func routerFor(t *testing.T, h *Handlers) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	h.Register(r)
	return r
}

func TestAuthStatusAnswersBothButtonsFromRows(t *testing.T) {
	st := newStore(t) // seeds roles + a break-glass admin with a password
	h := newHandlers(t, st)
	r := routerFor(t, h)

	// One provider enabled, one switched off: the login screen draws one button.
	if err := st.SSOProviders.Create(&models.SSOProvider{
		Name: "corp", Type: models.SSOTypeOIDC, Issuer: "https://idp.example.test",
		ClientID: "nvrdev", ClientSecret: "devsecret", Enabled: true,
	}); err != nil {
		t.Fatalf("creating the provider: %v", err)
	}
	if err := st.SSOProviders.Create(&models.SSOProvider{
		Name: "retired", Type: models.SSOTypeOIDC, Issuer: "https://old.example.test",
		ClientID: "nvrdev", ClientSecret: "devsecret", Enabled: false,
	}); err != nil {
		t.Fatalf("creating the disabled provider: %v", err)
	}

	status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/auth/status"})
	if status != http.StatusOK {
		t.Fatalf("status: got %d %v, want 200", status, body)
	}
	if body["local_login"] != true {
		t.Errorf("local_login = %v with a seeded break-glass admin, want true", body["local_login"])
	}
	list, ok := body["oidc"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("oidc = %v, want exactly the one enabled provider", body["oidc"])
	}
	button := list[0].(map[string]any)
	if button["provider"] != "corp" || button["issuer"] != "https://idp.example.test" {
		t.Errorf("the login button is %v, want the provider's name and issuer", button)
	}
	if body["registration"] != false {
		t.Errorf("registration = %v, want false: v1 has no signup", body["registration"])
	}

	// The answer follows the rows: with every password account gone, the login
	// page has no password form. Deleting the break-glass admin is a real act.
	if _, err := st.Users.GetByUsername("admin"); err != nil {
		t.Fatalf("reading the admin: %v", err)
	}
	if err := st.Users.Delete(mustAdminID(t, st)); err != nil {
		t.Fatalf("deleting the break-glass admin: %v", err)
	}
	_, body, _ = do(t, r, request{method: http.MethodGet, path: "/api/auth/status"})
	if body["local_login"] != false {
		t.Errorf("local_login = %v after the last password account was deleted, want false", body["local_login"])
	}
}

func mustAdminID(t *testing.T, st *store.Store) string {
	t.Helper()
	users, err := st.Users.ListAll()
	if err != nil {
		t.Fatalf("listing accounts: %v", err)
	}
	for _, u := range users {
		if u.Username == "admin" {
			return u.ID
		}
	}
	t.Fatal("no admin account to delete")
	return ""
}

func TestAuthLoginIssuesThePairAndRecordsTheSession(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	status, body, _ := do(t, r, request{
		method: http.MethodPost,
		path:   "/api/auth/login",
		body:   map[string]any{"username": "admin", "password": "break-glass-password"},
	})
	if status != http.StatusOK {
		t.Fatalf("login: got %d %v, want 200", status, body)
	}
	access, _ := body["access_token"].(string)
	refresh, _ := body["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("login returned no token pair: %v", body)
	}
	if body["token_type"] != "bearer" {
		t.Errorf("token_type = %v, want bearer", body["token_type"])
	}
	user, _ := body["user"].(map[string]any)
	if user["username"] != "admin" {
		t.Errorf("user = %v, want the signed-in account", user)
	}
	if _, has := body["password_hash"]; has {
		t.Error("the login response carries a password hash")
	}

	// The refresh token is bound to a session row, which is what makes rotation
	// and reuse detection possible at all.
	session, err := st.Sessions.GetByTokenHash(models.HashRefreshToken(refresh))
	if err != nil || session == nil {
		t.Fatalf("no session row for the issued refresh token: %v", err)
	}
	if session.RevokedAt != nil {
		t.Error("a freshly issued session is recorded as revoked")
	}

	// The access token is the credential the console carries.
	status, profile, _ := do(t, r, request{method: http.MethodGet, path: "/api/auth/profile", bearer: access})
	if status != http.StatusOK {
		t.Fatalf("profile with the issued token: got %d %v, want 200", status, profile)
	}
	roles, _ := profile["roles"].([]any)
	if len(roles) != 1 || roles[0] != models.RoleAdmin {
		t.Errorf("profile roles = %v, want [admin]", profile["roles"])
	}

	rows := auditRows(t, st, models.AuditAuthLogin)
	if len(rows) != 1 {
		t.Fatalf("the login wrote %d ledger rows, want 1", len(rows))
	}
	if rows[0].Outcome != models.AuditOutcomeOK || rows[0].ActorUsername != "admin" {
		t.Errorf("login row = %+v, want an ok row naming the account", rows[0])
	}
}

func TestAuthLoginRefusalsAreIndistinguishableToTheClient(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	unknown := map[string]any{"username": "nobody-at-all", "password": "break-glass-password"}
	wrong := map[string]any{"username": "admin", "password": "not-the-password"}

	statusA, bodyA, _ := do(t, r, request{method: http.MethodPost, path: "/api/auth/login", body: unknown})
	statusB, bodyB, _ := do(t, r, request{method: http.MethodPost, path: "/api/auth/login", body: wrong})

	if statusA != http.StatusUnauthorized || statusB != http.StatusUnauthorized {
		t.Fatalf("refusals answered %d and %d, want 401 and 401", statusA, statusB)
	}
	if bodyA["error"] != bodyB["error"] {
		t.Fatalf("the two refusals are distinguishable: %q vs %q", bodyA["error"], bodyB["error"])
	}
	if bodyA["error"] != "invalid credentials" {
		t.Errorf("refusal answered %q, want \"invalid credentials\"", bodyA["error"])
	}

	// The ledger keeps the distinction the client does not get.
	rows := auditRows(t, st, models.AuditAuthLoginFailed)
	if len(rows) != 2 {
		t.Fatalf("failed logins wrote %d rows, want 2", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Outcome != models.AuditOutcomeRefused {
			t.Errorf("failed login row has outcome %q, want refused", row.Outcome)
		}
		seen[row.Details] = true
	}
	if len(seen) != 2 {
		t.Errorf("the two failures recorded the same details (%d distinct), want the reason to differ", len(seen))
	}
}

func TestAuthLoginRefusesAProviderAccountWithItsOwnAnswer(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	// An OIDC-provisioned account has no password by construction.
	sso := &models.User{
		Username: "from-the-idp", Email: "from-the-idp@example.test",
		Provider: "corp", ProviderID: "sub-1", Status: models.UserStatusActive,
		PasswordHash: "",
	}
	if err := st.Users.Create(sso); err != nil {
		t.Fatalf("creating the SSO account: %v", err)
	}

	status, body, _ := do(t, r, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "from-the-idp", "password": "anything-at-all"},
	})
	if status != http.StatusForbidden {
		t.Fatalf("password login at a provider account: got %d %v, want 403", status, body)
	}
	if body["error"] != "this account signs in through its provider" {
		t.Errorf("answer = %q, want the console to know to offer the provider's button", body["error"])
	}
}

func TestAuthLoginSitsBehindTheRateLimiter(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	// A budget of 5 with a burst of 1: the second attempt is refused.
	h.Limiter = NewRateLimiter(5, 1)
	r := routerFor(t, h)

	bad := map[string]any{"username": "admin", "password": "guess-one"}
	status, body, _ := do(t, r, request{method: http.MethodPost, path: "/api/auth/login", body: bad})
	if status != http.StatusUnauthorized {
		t.Fatalf("first guess: got %d %v, want 401", status, body)
	}
	status, body, _ = do(t, r, request{method: http.MethodPost, path: "/api/auth/login", body: bad})
	if status != http.StatusTooManyRequests {
		t.Fatalf("second guess: got %d %v, want 429 (the login route is not outside the budget)", status, body)
	}
	if body["error"] != "rate limit exceeded" {
		t.Errorf("429 answered %v, want \"rate limit exceeded\"", body["error"])
	}
}

func TestAuthRefreshRotatesAndDetectsReuse(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	_, login, _ := do(t, r, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "admin", "password": "break-glass-password"},
	})
	first, _ := login["refresh_token"].(string)
	if first == "" {
		t.Fatal("login issued no refresh token")
	}

	// Rotation: the replacement is a different token.
	status, rotated, _ := do(t, r, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": first},
	})
	if status != http.StatusOK {
		t.Fatalf("refresh: got %d %v, want 200", status, rotated)
	}
	second, _ := rotated["refresh_token"].(string)
	access2, _ := rotated["access_token"].(string)
	if second == "" || second == first {
		t.Fatalf("rotation did not issue a new refresh token: %v", rotated)
	}

	// The rotated row is closed and names what it replaced.
	old, err := st.Sessions.GetByTokenHash(models.HashRefreshToken(first))
	if err != nil || old == nil {
		t.Fatalf("the rotated row is unreadable: %v", err)
	}
	if old.RevokedAt == nil {
		t.Error("the rotated session is still live")
	}
	replacement, err := st.Sessions.GetByTokenHash(models.HashRefreshToken(second))
	if err != nil || replacement == nil {
		t.Fatalf("the replacement session row is missing: %v", err)
	}
	if replacement.RotatedFrom != old.TokenHash {
		t.Errorf("rotated_from = %q, want it to name the row it replaced", replacement.RotatedFrom)
	}

	// Reuse: presenting the spent token again is a replay, and the answer drops
	// the account's live sessions — including the honest client's new one.
	status, reused, _ := do(t, r, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": first},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("reuse: got %d %v, want 401", status, reused)
	}
	if reused["error"] != "refresh token was already used" {
		t.Errorf("reuse answered %v, want \"refresh token was already used\"", reused["error"])
	}
	if dropped, _ := reused["live_sessions_dropped"].(float64); dropped < 1 {
		t.Errorf("live_sessions_dropped = %v, want at least 1: a copy of a spent token is a stolen token", reused["live_sessions_dropped"])
	}

	// The honest client's replacement went with it, so it has to sign in again.
	// What reuse detection drops is **sessions**: the refresh credential. The
	// access token is a stateless 30-minute bearer token, and it lives out its
	// expiry — the ceiling the design accepts, and the reason the access TTL is
	// 30 minutes and not a day.
	status, _, _ = do(t, r, request{method: http.MethodGet, path: "/api/auth/profile", bearer: access2})
	if status != http.StatusOK {
		t.Fatalf("the access token stopped working on reuse: got %d, want 200 until its expiry", status)
	}
	status, _, _ = do(t, r, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": second},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("the replacement refresh token survived a reuse event: got %d, want 401", status)
	}

	rows := auditRows(t, st, models.AuditAuthRefresh)
	var refused int
	for _, row := range rows {
		if row.Outcome == models.AuditOutcomeRefused {
			refused++
		}
	}
	if refused == 0 {
		t.Error("the reuse is not in the ledger")
	}
}

func TestAuthLogoutDeniesTheAccessToken(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	_, login, _ := do(t, r, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "admin", "password": "break-glass-password"},
	})
	access, _ := login["access_token"].(string)
	refresh, _ := login["refresh_token"].(string)

	status, out, _ := do(t, r, request{
		method: http.MethodPost, path: "/api/auth/logout",
		bearer: access, body: map[string]any{"refresh_token": refresh},
	})
	if status != http.StatusOK {
		t.Fatalf("logout: got %d %v, want 200", status, out)
	}
	if dropped, _ := out["live_sessions_dropped"].(float64); dropped != 1 {
		t.Errorf("live_sessions_dropped = %v, want the one session this console held", out["live_sessions_dropped"])
	}

	status, _, _ = do(t, r, request{method: http.MethodGet, path: "/api/auth/profile", bearer: access})
	if status != http.StatusUnauthorized {
		t.Fatalf("the access token survived logout: got %d, want 401", status)
	}
	status, _, _ = do(t, r, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": refresh},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("the logged-out refresh token still rotates: got %d, want 401", status)
	}

	if rows := auditRows(t, st, models.AuditAuthLogout); len(rows) != 1 {
		t.Fatalf("logout wrote %d ledger rows, want 1", len(rows))
	}
}

func TestAuthLogoutWithoutARefreshTokenEndsEverySession(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	_, login, _ := do(t, r, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "admin", "password": "break-glass-password"},
	})
	access, _ := login["access_token"].(string)
	refresh, _ := login["refresh_token"].(string)

	// A second device: a second session of the same account.
	second, err := h.JWT.IssueRefresh("placeholder", "admin", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issuing the second device's token: %v", err)
	}
	user, err := st.Users.GetByUsername("admin")
	if err != nil {
		t.Fatalf("reading the admin: %v", err)
	}
	if err := st.Sessions.Create(&models.Session{
		UserID: user.ID, TokenHash: models.HashRefreshToken(second),
		ExpiresAt: timeNow().Add(models.RefreshTokenTTL), UserAgent: "second-device",
	}); err != nil {
		t.Fatalf("recording the second session: %v", err)
	}

	status, out, _ := do(t, r, request{method: http.MethodPost, path: "/api/auth/logout", bearer: access})
	if status != http.StatusOK {
		t.Fatalf("logout: got %d %v, want 200", status, out)
	}
	if dropped, _ := out["live_sessions_dropped"].(float64); dropped < 2 {
		t.Fatalf("live_sessions_dropped = %v, want every live session of the account", out["live_sessions_dropped"])
	}
	status, _, _ = do(t, r, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": refresh},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("the other device kept working: got %d, want 401", status)
	}
}

func TestAuthChangePasswordStopsEveryOldCredential(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	_, login, _ := do(t, r, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "admin", "password": "break-glass-password"},
	})
	access, _ := login["access_token"].(string)
	refresh, _ := login["refresh_token"].(string)

	status, out, _ := do(t, r, request{
		method: http.MethodPut, path: "/api/auth/password", bearer: access,
		body: map[string]any{"current_password": "break-glass-password", "new_password": "a-longer-new-password"},
	})
	if status != http.StatusOK {
		t.Fatalf("password change: got %d %v, want 200", status, out)
	}

	// The access token this request was made with is dead: its generation is
	// behind the account's.
	status, _, _ = do(t, r, request{method: http.MethodGet, path: "/api/auth/profile", bearer: access})
	if status != http.StatusUnauthorized {
		t.Fatalf("the pre-change access token still verifies: got %d, want 401", status)
	}
	// So is the refresh token, and with it every other device.
	status, _, _ = do(t, r, request{
		method: http.MethodPost, path: "/api/auth/refresh",
		body: map[string]any{"refresh_token": refresh},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("the pre-change refresh token still rotates: got %d, want 401", status)
	}

	// The new password is the way in, and the old one is not.
	status, _, _ = do(t, r, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "admin", "password": "break-glass-password"},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("the old password still logs in: got %d, want 401", status)
	}
	status, _, _ = do(t, r, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "admin", "password": "a-longer-new-password"},
	})
	if status != http.StatusOK {
		t.Fatalf("the new password does not log in: got %d, want 200", status)
	}

	if rows := auditRows(t, st, models.AuditAuthPasswordChange); len(rows) == 0 {
		t.Error("the password change is not in the ledger")
	}
}

func TestAuthChangePasswordRefusesTheWeakAndTheWrong(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	_, login, _ := do(t, r, request{
		method: http.MethodPost, path: "/api/auth/login",
		body: map[string]any{"username": "admin", "password": "break-glass-password"},
	})
	access, _ := login["access_token"].(string)

	status, out, _ := do(t, r, request{
		method: http.MethodPut, path: "/api/auth/password", bearer: access,
		body: map[string]any{"current_password": "break-glass-password", "new_password": "short"},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a 5-character password: got %d %v, want 422", status, out)
	}
	if msg, _ := out["error"].(string); msg == "" || len(msg) < 10 {
		t.Errorf("422 answered %v, want the refusal to state the floor", out["error"])
	}

	status, out, _ = do(t, r, request{
		method: http.MethodPut, path: "/api/auth/password", bearer: access,
		body: map[string]any{"current_password": "not-the-password", "new_password": "a-longer-new-password"},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("changing a password without the current one: got %d %v, want 401", status, out)
	}

	// An account whose password lives at a provider has none to change here.
	sso := &models.User{Username: "provider-only", Email: "provider-only@example.test",
		Provider: "corp", ProviderID: "sub-2", Status: models.UserStatusActive}
	if err := st.Users.Create(sso); err != nil {
		t.Fatalf("creating the provider account: %v", err)
	}
	token := accessToken(t, st, h.JWT, sso)
	status, out, _ = do(t, r, request{
		method: http.MethodPut, path: "/api/auth/password", bearer: token,
		body: map[string]any{"current_password": "whatever", "new_password": "a-longer-new-password"},
	})
	if status != http.StatusForbidden {
		t.Fatalf("password set on a provider account: got %d %v, want 403", status, out)
	}
}

func TestAuthRegistrationIsAdminOnly(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	status, body, _ := do(t, r, request{
		method: http.MethodPost, path: "/api/auth/register",
		body: map[string]any{"username": "self-registered", "password": "a-longer-new-password", "email": "x@example.test"},
	})
	if status != http.StatusForbidden {
		t.Fatalf("registration: got %d %v, want 403", status, body)
	}
	if body["error"] != "registration is admin-only" {
		t.Errorf("registration answered %v, want \"registration is admin-only\"", body["error"])
	}

	// The attempt is on the record: "someone tried to create an account" is a
	// question the ledger should answer.
	rows := auditRows(t, st, models.AuditAuthRegister)
	if len(rows) != 1 || rows[0].Outcome != models.AuditOutcomeRefused {
		t.Fatalf("the registration attempt wrote %d rows (%+v), want 1 refused row", len(rows), rows)
	}

	// And no account appeared.
	if count, _ := st.Users.Count(); count != 1 {
		t.Errorf("accounts after the refused registration = %d, want the one seeded admin", count)
	}
}

func TestAuthProfileRefusesAnAccountThatNoLongerExists(t *testing.T) {
	st := newStore(t)
	h := newHandlers(t, st)
	r := routerFor(t, h)

	watcher := newUser(t, st, "watcher", models.RoleViewer)
	token := accessToken(t, st, h.JWT, watcher)

	if err := st.Users.Delete(watcher.ID); err != nil {
		t.Fatalf("deleting the account: %v", err)
	}

	status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/auth/profile", bearer: token})
	if status != http.StatusUnauthorized {
		t.Fatalf("profile for a deleted account: got %d %v, want 401", status, body)
	}
	if body["error"] != "this account no longer exists" {
		t.Errorf("answer = %v, want the reason, not a silent 401", body["error"])
	}
}
