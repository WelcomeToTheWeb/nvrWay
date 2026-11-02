package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"nvr/server/auth"
	"nvr/server/models"
	"nvr/server/store"
)

// Shared fixtures for the API suites. Every suite here runs against a sqlite
// database in memory (the plan's test shape: Postgres is the deployment target,
// sqlite is the test target) with the built-in roles and a break-glass admin
// seeded, because a role gate has nothing to gate on until the roles exist.

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New(:memory:): %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := st.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if _, err := st.Bootstrap("admin", "break-glass-password"); err != nil {
		t.Fatalf("seeding roles and the break-glass admin: %v", err)
	}
	return st
}

func newJWT() *auth.JWTAuth { return auth.NewJWTAuth("unit-test-signing-secret-not-a-deployment-one") }

// timeNow is the clock the fixtures hand to the store, so a test that wants a
// session to be expired says so with a duration, not with a sleep.
func timeNow() time.Time { return time.Now().UTC() }

// newUser creates a local account with these roles and returns it.
func newUser(t *testing.T, st *store.Store, username string, roles ...string) *models.User {
	t.Helper()
	u := &models.User{
		Username: username,
		Email:    username + "@example.test",
		Name:     username,
		Provider: models.ProviderLocal,
		Status:   models.UserStatusActive,
	}
	if err := st.Users.Create(u); err != nil {
		t.Fatalf("creating %s: %v", username, err)
	}
	if len(roles) > 0 {
		if err := st.Users.SetRoles(u.ID, roles); err != nil {
			t.Fatalf("setting roles on %s: %v", username, err)
		}
	}
	return u
}

// accessToken issues an access token carrying the account's stored roles.
func accessToken(t *testing.T, st *store.Store, jwt *auth.JWTAuth, u *models.User) string {
	t.Helper()
	roles, err := st.Users.RolesFor(u.ID)
	if err != nil {
		t.Fatalf("roles for %s: %v", u.ID, err)
	}
	token, err := jwt.IssueNamed(u.ID, u.Username, roles, 0)
	if err != nil {
		t.Fatalf("issuing an access token: %v", err)
	}
	return token
}

// refreshToken issues a refresh token for an account, for the rotation tests.
func refreshToken(t *testing.T, jwt *auth.JWTAuth, u *models.User) string {
	t.Helper()
	token, err := jwt.IssueRefresh(u.ID, u.Username, []string{models.RoleViewer}, 0)
	if err != nil {
		t.Fatalf("issuing a refresh token: %v", err)
	}
	return token
}

// recordSession opens the session row a refresh token is bound to.
func recordSession(t *testing.T, st *store.Store, userID, refreshToken string) *models.Session {
	t.Helper()
	session := &models.Session{
		UserID:    userID,
		TokenHash: models.HashRefreshToken(refreshToken),
		ExpiresAt: timeNow().Add(models.RefreshTokenTTL),
		SourceIP:  "127.0.0.1",
		UserAgent: "api-suite",
	}
	if err := st.Sessions.Create(session); err != nil {
		t.Fatalf("creating the session row: %v", err)
	}
	return session
}

type request struct {
	method string
	path   string
	body   any
	// bearer is the Authorization: Bearer value. It is deliberately a separate
	// field from query: the suites assert the token never travels in the URL.
	bearer string
	query  string
	origin string
}

// do runs one request against the router and answers status, the decoded JSON
// body, and the raw body for the assertions that need the bytes.
func do(t *testing.T, handler http.Handler, r request) (int, map[string]any, string) {
	t.Helper()
	var body *bytes.Buffer
	if r.body != nil {
		encoded, err := json.Marshal(r.body)
		if err != nil {
			t.Fatalf("encoding the request body: %v", err)
		}
		body = bytes.NewBuffer(encoded)
	} else {
		body = new(bytes.Buffer)
	}
	target := r.path
	if r.query != "" {
		target += "?" + r.query
	}
	req := httptest.NewRequest(r.method, target, body)
	req.Header.Set("Content-Type", "application/json")
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	out := map[string]any{}
	raw := rec.Body.String()
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("response is not JSON (%d %s): %q", rec.Code, r.path, raw)
		}
	}
	return rec.Code, out, raw
}

// auditRows reads the ledger for one action.
func auditRows(t *testing.T, st *store.Store, action string) []models.Audit {
	t.Helper()
	rows, err := st.Audit.List(store.AuditFilter{Action: action, Limit: 50})
	if err != nil {
		t.Fatalf("reading the ledger: %v", err)
	}
	return rows
}

// jsonBody decodes a request payload from a map, for handlers that bind a struct.
func jsonBody(v map[string]any) map[string]any { return v }

// routerFor is the smallest router that exercises the middleware: one protected
// route that echoes what the middleware put in the context.
func protectedEcho(jwt *auth.JWTAuth, st *store.Store, guard gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	group := r.Group("/api/v1", AuthMiddleware(jwt))
	group.GET("/echo", guard, func(c *gin.Context) {
		claims := ClaimsFrom(c)
		c.JSON(http.StatusOK, gin.H{
			"user_id": claims.UserID,
			"roles":   claims.Roles,
		})
	})
	return r
}
