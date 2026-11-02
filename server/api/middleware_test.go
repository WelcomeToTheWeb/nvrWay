package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"nvr/server/models"
)

// The card's exit criteria, one test each: the internal API is switched off by
// an empty token, a role denial is a ledger row and a named 403, the rate budget
// is per user with a tenth-sized burst, and the token is read from the
// Authorization header and nowhere else.

func TestMiddlewareReadsTheBearerHeaderAndNeverTheQuery(t *testing.T) {
	st := newStore(t)
	jwt := newJWT()
	admin, err := st.Users.GetByUsername("admin")
	if err != nil {
		t.Fatalf("reading the seeded admin: %v", err)
	}
	token := accessToken(t, st, jwt, admin)
	r := protectedEcho(jwt, st, RequireAnyRole(st.Audit, models.RoleAdmin))

	// The documented way.
	status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", bearer: token})
	if status != http.StatusOK {
		t.Fatalf("Bearer header: got %d %v, want 200", status, body)
	}
	if body["user_id"] != admin.ID {
		t.Errorf("claims did not reach the handler: %v", body)
	}

	// The token in the query is not a credential on a REST route. The WebSocket
	// is the documented exception (ws/hub.go reads ?token=), and it is the hub's
	// rule, not this middleware's.
	status, body, _ = do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", query: "token=" + token})
	if status != http.StatusUnauthorized {
		t.Fatalf("?token= accepted on a REST route: got %d %v, want 401", status, body)
	}

	// No header at all.
	status, body, _ = do(t, r, request{method: http.MethodGet, path: "/api/v1/echo"})
	if status != http.StatusUnauthorized {
		t.Fatalf("missing header: got %d %v, want 401", status, body)
	}
	if body["error"] != "missing authorization header" {
		t.Errorf("missing header answered %v, want \"missing authorization header\"", body["error"])
	}

	// A refresh token is not an access token, and the gate says which one arrived.
	refresh := refreshToken(t, jwt, admin)
	status, body, _ = do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", bearer: refresh})
	if status != http.StatusUnauthorized {
		t.Fatalf("refresh token at the access gate: got %d %v, want 401", status, body)
	}
	if body["error"] != "access token is not valid" {
		t.Errorf("refresh token at the access gate answered %v", body["error"])
	}
}

func TestMiddlewareRoleDenialIsAuditedAndNamed(t *testing.T) {
	st := newStore(t)
	jwt := newJWT()
	viewer := newUser(t, st, "watcher", models.RoleViewer)
	token := accessToken(t, st, jwt, viewer)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	admin := r.Group("/api/users", AuthMiddleware(jwt), RequireRole(models.RoleAdmin, st.Audit))
	admin.GET("", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"users": []any{}}) })

	status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/users", bearer: token})
	if status != http.StatusForbidden {
		t.Fatalf("viewer on an admin route: got %d %v, want 403", status, body)
	}
	if body["error"] != "insufficient role" {
		t.Errorf("denial answered %v, want \"insufficient role\"", body["error"])
	}

	rows := auditRows(t, st, models.AuditUserRead)
	if len(rows) != 1 {
		t.Fatalf("the denial wrote %d ledger rows for %s, want 1", len(rows), models.AuditUserRead)
	}
	row := rows[0]
	if row.Outcome != models.AuditOutcomeRefused {
		t.Errorf("outcome = %q, want %q", row.Outcome, models.AuditOutcomeRefused)
	}
	if row.ActorUsername != viewer.Username || row.ActorUserID != viewer.ID {
		t.Errorf("actor = %q/%q, want the viewer who asked", row.ActorUserID, row.ActorUsername)
	}
	if row.Details == "" {
		t.Error("the denial row states no reason")
	}
}

func TestMiddlewarePermissionGateReadsTheRoleRows(t *testing.T) {
	st := newStore(t)
	jwt := newJWT()
	watcher := newUser(t, st, "watcher2", models.RoleViewer)
	operator := newUser(t, st, "shooter", models.RoleOperator)

	r := protectedEcho(jwt, st, RequirePermission(models.PermCamerasWrite, st, st.Audit))

	status, _, _ := do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", bearer: accessToken(t, st, jwt, watcher)})
	if status != http.StatusForbidden {
		t.Fatalf("viewer allowed cameras:write: got %d, want 403", status)
	}
	status, _, _ = do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", bearer: accessToken(t, st, jwt, operator)})
	if status != http.StatusOK {
		t.Fatalf("operator refused cameras:write: got %d, want 200", status)
	}

	// The claim names the role; the role row carries the authority. A token that
	// names a role the installation does not have grants nothing.
	ghost := newUser(t, st, "ghost")
	ghostToken, err := jwt.IssueNamed(ghost.ID, ghost.Username, []string{"camera-owner"}, 0)
	if err != nil {
		t.Fatalf("issuing the ghost token: %v", err)
	}
	status, _, _ = do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", bearer: ghostToken})
	if status != http.StatusForbidden {
		t.Fatalf("a token naming an unknown role was granted: got %d, want 403", status)
	}

	// `admin:*` grants everything, which is the shorthand an operator can put in
	// a custom role.
	role := &models.Role{Name: "super", Permissions: []string{models.PermAdminStar}}
	if err := st.Roles.CreateRole(role); err != nil {
		t.Fatalf("creating the custom role: %v", err)
	}
	super := newUser(t, st, "super")
	if err := st.Users.SetRoles(super.ID, []string{"super"}); err != nil {
		t.Fatalf("granting the custom role: %v", err)
	}
	status, _, _ = do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", bearer: accessToken(t, st, jwt, super)})
	if status != http.StatusOK {
		t.Fatalf("admin:* did not grant cameras:write: got %d, want 200", status)
	}
}

func TestMiddlewareInternalAPIIsDisabledByAnEmptyToken(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	group := r.Group("/api/internal/v1", InternalAuthMiddleware(""))
	group.GET("/cameras", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"cameras": []any{}}) })

	for _, bearer := range []string{"", "anything-at-all", "the-token-the-worker-will-use"} {
		status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/internal/v1/cameras", bearer: bearer})
		if status != http.StatusForbidden {
			t.Fatalf("empty WORKER_TOKEN with bearer %q: got %d %v, want 403", bearer, status, body)
		}
		if body["error"] != "internal api disabled" {
			t.Errorf("empty WORKER_TOKEN answered %v, want \"internal api disabled\"", body["error"])
		}
	}
}

func TestMiddlewareInternalTokenGateAcceptsOnlyTheWorkerToken(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	group := r.Group("/api/internal/v1", InternalAuthMiddleware("worker-token-value"))
	group.GET("/cameras", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	status, _, _ := do(t, r, request{method: http.MethodGet, path: "/api/internal/v1/cameras", bearer: "worker-token-value"})
	if status != http.StatusOK {
		t.Fatalf("the worker's own token: got %d, want 200", status)
	}

	for _, bearer := range []string{"", "worker-token-valu", "worker-token-value2", "ADMIN-TOKEN"} {
		status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/internal/v1/cameras", bearer: bearer})
		if status != http.StatusUnauthorized {
			t.Fatalf("bearer %q accepted on the internal API: got %d %v, want 401", bearer, status, body)
		}
		if body["error"] != "internal token is not valid" {
			t.Errorf("bearer %q answered %v, want \"internal token is not valid\"", bearer, body["error"])
		}
	}
}

func TestMiddlewareRateLimitIsPerUserWithATenthSizedBurst(t *testing.T) {
	st := newStore(t)
	jwt := newJWT()
	first := newUser(t, st, "first", models.RoleViewer)
	second := newUser(t, st, "second", models.RoleViewer)

	// 10 requests a minute, burst 1.
	limiter := NewRateLimiter(10, 1)
	r := protectedEcho(jwt, st, limiter.Middleware())
	firstToken := accessToken(t, st, jwt, first)
	secondToken := accessToken(t, st, jwt, second)

	// The burst is the ceiling of an empty bucket: one request lands, the next is
	// refused before the refill.
	status, body, _ := do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", bearer: firstToken})
	if status != http.StatusOK {
		t.Fatalf("first request of a fresh bucket: got %d %v, want 200", status, body)
	}
	status, body, _ = do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", bearer: firstToken})
	if status != http.StatusTooManyRequests {
		t.Fatalf("burst of 1 let a second request through: got %d %v", status, body)
	}
	if body["error"] != "rate limit exceeded" {
		t.Errorf("429 answered %v, want \"rate limit exceeded\"", body["error"])
	}

	// A rate-limited account cannot spend a second account's budget.
	status, _, _ = do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", bearer: secondToken})
	if status != http.StatusOK {
		t.Fatalf("the second account was charged for the first: got %d, want 200", status)
	}

	// The bucket refills at the sustained rate, not all at once.
	limiter.AgeBucket("user:"+first.ID, time.Minute)
	status, _, _ = do(t, r, request{method: http.MethodGet, path: "/api/v1/echo", bearer: firstToken})
	if status != http.StatusOK {
		t.Fatalf("the bucket did not refill with the window: got %d, want 200", status)
	}
}

func TestMiddlewareRateLimitBurstZeroIsATenthOfTheBudget(t *testing.T) {
	// config.Load resolves RATE_LIMIT_BURST=0 to a tenth of the budget, and a
	// limiter built from an unset config must resolve it the same way.
	limiter := NewRateLimiter(120, 0)
	if got := limiter.Burst(); got != 12 {
		t.Errorf("burst = %d, want 12 (a tenth of 120)", got)
	}
	if got := limiter.PerMinute(); got != 120 {
		t.Errorf("per minute = %d, want 120", got)
	}

	// burst 0 on a budget under 10 is 1, not 0: a limiter that allows nothing
	// is not a limiter, it is an outage.
	if got := NewRateLimiter(5, 0).Burst(); got != 1 {
		t.Errorf("burst on a budget of 5 = %d, want 1", got)
	}
}
