// Package api is the HTTP surface: the routes, the gates in front of them, and
// the ledger rows the refusals write.
//
// The behaviour is the sibling project's (NVRWAY_V1_PLAN.md Step 2), and the
// reasons are nvrWay's. Three rules run through this package:
//
//   - The token is read from `Authorization: Bearer`, never from the URL. The
//     WebSocket is the documented exception (`/ws?token=`, ws/hub.go), and it is
//     the hub's rule, not a general allowance.
//   - A refusal is a ledger row. The gates run before any handler, so nothing
//     else can record the most useful class of event in an NVR: the person who
//     tried to do something they may not do.
//   - The role name in a token is not the authority. The role **row** carries the
//     permission set, so a token naming a role the installation does not have
//     grants nothing, and an operator's edit to a role takes effect at the next
//     issue.
package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"nvr/server/auth"
	"nvr/server/models"
	"nvr/server/store"
)

// Context keys. Prefixed so a middleware cannot collide with a route parameter
// named "id", which is what the gin convention of `c.Set("id", …)` invites.
const (
	ctxClaims   = "nvr.claims"
	ctxUserID   = "nvr.user_id"
	ctxUsername = "nvr.username"
	ctxRoles    = "nvr.roles"
)

// ClaimsFrom returns the claims the auth middleware put on this request. Nil for
// a request that never passed the gate — a handler that needs the actor uses the
// gate, not a nil check.
func ClaimsFrom(c *gin.Context) *auth.Claims {
	if v, ok := c.Get(ctxClaims); ok {
		if claims, ok := v.(*auth.Claims); ok {
			return claims
		}
	}
	return nil
}

// bearerToken reads the credential from the header. It returns the empty string
// when the header is absent or is not a Bearer scheme: the query string is never
// consulted, because a token in a URL lands in access logs, in browser history
// and in `Referer`.
func bearerToken(c *gin.Context) string {
	header := strings.TrimSpace(c.GetHeader("Authorization"))
	if header == "" {
		return ""
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// AuthMiddleware gates a route on a valid access token.
//
// A refresh token is refused here by name: it is a credential for obtaining an
// access token, and the API's gate accepts only the access half. The 401 bodies
// are distinct so the console can tell "sign in again" from "this server does not
// know your key".
func AuthMiddleware(jwt *auth.JWTAuth) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			return
		}
		claims, err := jwt.Verify(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "access token is not valid"})
			return
		}
		c.Set(ctxClaims, claims)
		c.Set(ctxUserID, claims.UserID)
		c.Set(ctxUsername, claims.Username)
		c.Set(ctxRoles, claims.Roles)
		c.Next()
	}
}

// RequireRole gates a route on one role. `audit` is the ledger the refusal
// writes: a gate that cannot write is a gate nobody can audit.
func RequireRole(role string, audit *store.AuditStore) gin.HandlerFunc {
	return RequireAnyRole(audit, role)
}

// RequireAnyRole gates a route on any of these roles.
func RequireAnyRole(audit *store.AuditStore, roles ...string) gin.HandlerFunc {
	want := append([]string(nil), roles...)
	return func(c *gin.Context) {
		claims := ClaimsFrom(c)
		if claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		if claims.HasAnyRole(want) {
			c.Next()
			return
		}
		denyRoute(c, audit, http.StatusForbidden, "insufficient role", claims,
			"requires role "+strings.Join(want, " or "))
	}
}

// RequirePermission gates a route on a permission, read from the role rows.
//
// This is the gate the route table in docs/api.md is written in (`operator`
// writes cameras, `admin` writes storage), and it is deliberately not the same
// function as RequireRole: a route that says "operator" answers to a role rename
// and a route that says "cameras:write" does not.
func RequirePermission(perm string, st *store.Store, audit *store.AuditStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims := ClaimsFrom(c)
		if claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		perms, err := permissionsByRole(st)
		if err != nil {
			// A gate that cannot read the role table cannot answer "may I?". The
			// answer is 503, not a permissive fallthrough.
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "role permissions are unreadable"})
			return
		}
		if claims.Grants(perm, perms) {
			c.Next()
			return
		}
		denyRoute(c, audit, http.StatusForbidden, "insufficient role", claims,
			"requires permission "+perm)
	}
}

// permissionsByRole reads the role table into the shape Claims.Grants wants. The
// role set is small (three built-ins plus whatever an operator adds), so reading
// it per request is cheaper than caching a copy that can be stale at the moment a
// gate matters.
func permissionsByRole(st *store.Store) (map[string][]string, error) {
	roles, err := st.Roles.ListRoles()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(roles))
	for _, role := range roles {
		out[role.Name] = role.Permissions
	}
	return out, nil
}

// InternalAuthMiddleware gates `/api/internal/*` on the worker token.
//
// An empty WORKER_TOKEN is the internal API switched off, which is a 403 with a
// reason an operator can act on — not a 401 that reads like the worker typed its
// token wrong. The comparison is constant-time, and the header is the only place
// it is read.
func InternalAuthMiddleware(workerToken string) gin.HandlerFunc {
	expected := []byte(strings.TrimSpace(workerToken))
	return func(c *gin.Context) {
		if len(expected) == 0 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "internal api disabled"})
			return
		}
		got := []byte(bearerToken(c))
		if len(got) == 0 || subtle.ConstantTimeCompare(got, expected) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "internal token is not valid"})
			return
		}
		c.Set(ctxInternal, true)
		c.Next()
	}
}

// ctxInternal marks a request that came through the worker gate. The internal
// handlers read it so a worker route cannot be reached by any other path.
const ctxInternal = "nvr.internal"

// InternalRequest reports whether this request passed the worker gate.
func InternalRequest(c *gin.Context) bool {
	v, ok := c.Get(ctxInternal)
	if !ok {
		return false
	}
	inside, _ := v.(bool)
	return inside
}

// denyRoute writes the ledger row for a refusal made by a gate, then answers.
//
// The gate knows the actor (the auth middleware loaded it), the route the router
// matched, the parameter the target is named by, and the status. It does not know
// the target's owner or the row's contents, because no lookup ran: that is the
// handler's knowledge, so the row says what the gate knows and nothing more.
func denyRoute(c *gin.Context, audit *store.AuditStore, status int, reason string, claims *auth.Claims, detail string) {
	action, target := lookupDeniedAction(c)
	if audit != nil {
		actor := models.AuditActor{}
		if claims != nil {
			actor = models.AuditActor{
				UserID:   claims.UserID,
				Username: claims.Username,
				Roles:    strings.Join(claims.Roles, ","),
			}
		}
		_ = audit.Record(&models.Audit{
			ID:            models.NewAuditID(),
			ActorUserID:   actor.UserID,
			ActorUsername: actor.Username,
			ActorRoles:    actor.Roles,
			Action:        action,
			TargetType:    target.Type,
			TargetID:      target.ID,
			Outcome:       models.AuditOutcomeFor(status),
			Details: store.EncodeDetails(map[string]any{
				"denied": detail,
				"route":  c.FullPath(),
				"path":   c.Request.URL.Path,
				"method": c.Request.Method,
			}),
			SourceIP:  c.ClientIP(),
			CreatedAt: time.Now().UTC(),
		})
	}
	c.AbortWithStatusJSON(status, gin.H{"error": reason})
}

// deniedActions maps a route shape to the ledger verb its refusal is recorded
// under. The registry is keyed on the router's matched path (`/api/users/:id`),
// not the request path, so the ledger groups refusals by act and the ids of the
// rows an operator is hunting stay in `target_id`.
//
// A route with no entry is recorded as `route.denied` — a new route group that
// has not declared its ledger verb is visible as one, rather than silently
// writing rows under a verb nobody searches for.
var deniedActions = map[string]string{
	"GET /api/auth/profile":   models.AuditAuthProfile,
	"PUT /api/auth/password":  models.AuditAuthPasswordChange,
	"POST /api/auth/logout":   models.AuditAuthLogout,
	"POST /api/auth/register": models.AuditAuthRegister,
	// The media surface, registered ahead of the routes (S3/S6): the verbs below
	// name the acts Steps 3–6 will mount, so a refusal on `/api/v1/cameras` is
	// recorded under `camera.create` from the day the route arrives, and the
	// ledger question an operator asks ("who tried to add a camera") is answerable
	// without knowing which card wrote the route.
	"POST /api/v1/cameras":             models.AuditCameraCreate,
	"PUT /api/v1/cameras/:id":          models.AuditCameraUpdate,
	"DELETE /api/v1/cameras/:id":       models.AuditCameraDelete,
	"POST /api/v1/cameras/:id/probe":   models.AuditCameraProbe,
	"PUT /api/v1/cameras/:id/zones":    models.AuditCameraZones,
	"DELETE /api/v1/events/:id":        models.AuditEventDelete,
	"GET /api/users":                   models.AuditUserRead,
	"POST /api/users":                  models.AuditUserCreate,
	"GET /api/users/:id":               models.AuditUserRead,
	"PUT /api/users/:id":               models.AuditUserUpdate,
	"DELETE /api/users/:id":            models.AuditUserDelete,
	"GET /api/users/:id/roles":         models.AuditRoleRead,
	"GET /api/roles":                   models.AuditRoleRead,
	"POST /api/users/:id/roles":        models.AuditUserRoleChange,
	"GET /api/identities":              models.AuditIdentityRead,
	"POST /api/identities/:id/link":    models.AuditIdentityLink,
	"POST /api/identities/:id/approve": models.AuditIdentityApprove,
	"POST /api/identities/:id/refuse":  models.AuditIdentityRefuse,
	// The role mapping decides who logs in as what, so its refusals are recorded
	// under the provider verb, not `route.denied`.
	// The OIDC provider surface (S3-06): who people log in as is decided here, so
	// its refusals are recorded under the provider verbs.
	"GET /api/settings/oidc":                        models.AuditOIDCRead,
	"POST /api/settings/oidc":                       models.AuditOIDCCreate,
	"PUT /api/settings/oidc/:provider":              models.AuditOIDCUpdate,
	"DELETE /api/settings/oidc/:provider":           models.AuditOIDCDelete,
	"PUT /api/settings/oidc/:provider/role-mapping": models.AuditOIDCUpdate,
}

func lookupDeniedAction(c *gin.Context) (string, AuditTarget) {
	key := c.Request.Method + " " + c.FullPath()
	action, ok := deniedActions[key]
	if !ok {
		action = models.AuditRouteDenied
	}
	target := AuditTarget{Type: "route", ID: c.FullPath()}
	if id := c.Param("id"); id != "" {
		target = AuditTarget{Type: "entity", ID: id}
	}
	return action, target
}

// RateLimiter is the per-user REST budget (RATE_LIMIT_PER_MINUTE) as a token
// bucket.
//
// A fixed-window counter lets a client spend the whole allowance in the first
// second of a window, twice at the window boundary. The bucket caps the
// instantaneous burst and refills at the sustained rate, which is the shape the
// console is designed to live inside: it polls, and a polling console should
// never be the thing that gets 429.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	perMin  int
	burst   int
	window  time.Duration
	now     func() time.Time
}

type tokenBucket struct {
	tokens float64
	seen   time.Time
}

// NewRateLimiter allows `perMinute` requests per key on a sustained basis, with
// an instantaneous burst capped at `burst`. burst <= 0 takes the documented
// default — one tenth of the budget — and never less than 1, so a small budget
// cannot produce a limiter that allows nothing.
func NewRateLimiter(perMinute, burst int) *RateLimiter {
	if burst < 1 {
		burst = perMinute / 10
	}
	if burst < 1 {
		burst = 1
	}
	if perMinute < 1 {
		perMinute = 1
	}
	return &RateLimiter{
		buckets: make(map[string]*tokenBucket),
		perMin:  perMinute,
		burst:   burst,
		window:  time.Minute,
		now:     time.Now,
	}
}

// PerMinute reports the sustained budget, for the response header that states it.
func (l *RateLimiter) PerMinute() int { return l.perMin }

// Burst reports the instantaneous ceiling an empty bucket will hand out.
func (l *RateLimiter) Burst() int { return l.burst }

// SetClock replaces the limiter's clock. A rate limit is the one part of this
// surface a test cannot wait out — a minute is a minute — so the clock is a seam,
// and the handler set hands every clock it owns to one.
func (l *RateLimiter) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// Middleware enforces the budget per account, falling back to the client address
// for requests that have no account yet (login, status). Keying the unauthenticated
// requests by address is what makes `POST /api/auth/login` sit behind the budget:
// password guessing arrives as a machine, not as a user id.
func (l *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := "ip:" + c.ClientIP()
		if claims := ClaimsFrom(c); claims != nil {
			key = "user:" + claims.UserID
		}
		if !l.Allow(key) {
			c.Header("Retry-After", "1")
			c.Header("X-RateLimit-Limit", itoa(l.perMin))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		c.Next()
	}
}

// Allow spends one token for a key.
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	bucket, ok := l.buckets[key]
	if !ok {
		// A new key starts at the burst, not at the full budget: the first
		// client to arrive cannot spend a minute's allowance in a second.
		bucket = &tokenBucket{tokens: float64(l.burst), seen: now}
		l.buckets[key] = bucket
	}
	rate := float64(l.perMin) / l.window.Seconds()
	bucket.tokens += now.Sub(bucket.seen).Seconds() * rate
	// The refill tops out at the burst, not at the budget: an idle console that
	// comes back after ten minutes gets `burst` requests at once and then the
	// sustained rate, which is what "burst caps the instantaneous burst" means.
	if bucket.tokens > float64(l.burst) {
		bucket.tokens = float64(l.burst)
	}
	bucket.seen = now
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// Prune drops buckets idle long enough to be fully refilled, which is the same
// state as a fresh bucket. A limiter that never forgets a key leaks one bucket
// per address the installation has ever seen; the console's requests come from a
// handful of keys, so the map is bounded by a normal install, not by a scanner.
func (l *RateLimiter) Prune() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	dropped := 0
	for key, bucket := range l.buckets {
		if now.Sub(bucket.seen) > l.window {
			delete(l.buckets, key)
			dropped++
		}
	}
	return dropped
}

// AgeBucket rewinds one key's clock by d, so a test can watch the sustained
// refill without sleeping through a minute. Nothing in the product calls it.
func (l *RateLimiter) AgeBucket(key string, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, ok := l.buckets[key]
	if !ok {
		return
	}
	bucket.seen = bucket.seen.Add(-d)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
