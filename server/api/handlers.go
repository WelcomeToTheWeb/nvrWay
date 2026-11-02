package api

import (
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"nvr/server/auth"
	"nvr/server/config"
	"nvr/server/models"
	"nvr/server/sso"
	"nvr/server/store"
	"nvr/server/ws"
)

// Handlers is the mounted HTTP surface, with the things the routes need.
//
// The constructor takes the store, the JWT service, the hub and the config —
// the plan's boot order (Step 2's `api.NewHandlers(ctx, st, jwtAuth, hub, cfg)`)
// — because a handler that reaches for a global is a handler that cannot be
// built twice, and every suite in this package builds one.
//
// `ctx` is the server lifecycle context. It is carried, not used yet: the
// recorder (Step 4), the uploader and the sweeper (Step 7) start from the
// routes' work, and they must stop when the process stops.
type Handlers struct {
	Cfg   *config.Config
	St    *store.Store
	JWT   *auth.JWTAuth
	Hub   *ws.Hub
	OAuth *sso.OAuthHandler

	// Limiter is the REST budget (RATE_LIMIT_PER_MINUTE). The login route sits
	// behind it, which is why it is a field: the console's own polling and a
	// password guesser are separated by the limiter's key, not by the route.
	Limiter *RateLimiter

	now func() time.Time
}

// NewHandlers builds the surface. Nothing here talks to the network: discovery
// runs when an operator saves a provider, not at boot.
func NewHandlers(ctx context.Context, st *store.Store, jwtAuth *auth.JWTAuth, hub *ws.Hub, cfg *config.Config) *Handlers {
	if cfg == nil {
		cfg = config.Load()
	}
	if jwtAuth == nil {
		jwtAuth = auth.NewJWTAuth(cfg.JWTSecret)
	}
	return &Handlers{
		Cfg:     cfg,
		St:      st,
		JWT:     jwtAuth,
		Hub:     hub,
		OAuth:   sso.NewOAuthHandler(st, jwtAuth),
		Limiter: NewRateLimiter(cfg.RateLimitPerMinute, cfg.RateLimitBurst),
		now:     time.Now,
	}
}

// SetClock points every timestamp this surface writes at one clock. Tests use
// it; the product runs on the wall clock.
func (h *Handlers) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	h.now = now
}

// Register mounts every route Step 2 owns.
//
// The public auth group carries the rate limiter: password guessing arrives as a
// machine, not as a user id, so the unauthenticated requests are keyed by
// address.
func (h *Handlers) Register(r *gin.Engine) {
	// The live feed. The hub reads `?token=` on the URL, which is the documented
	// exception to the bearer-only rule (SECURITY.md names it).
	if h.Hub != nil {
		r.GET(config.WSPath, h.Hub.Handle)
	}

	public := r.Group("/api/auth", h.Limiter.Middleware())
	public.GET("/status", h.AuthStatus)
	public.POST("/login", h.Login)
	public.POST("/refresh", h.Refresh)
	public.POST("/register", h.RegisterDisabled)
	// The SSO routes (authorize / callback / exchange) are mounted by card S2-07
	// in `sso_routes.go`, which owns the state record they stand on.

	session := public.Group("", AuthMiddleware(h.JWT))
	session.POST("/logout", h.Logout)
	session.GET("/profile", h.Profile)
	session.PUT("/password", h.ChangePassword)

	// The user/role routes (S2-09) and the pending-identity routes (S2-07) are
	// mounted by their own cards, in `users.go` and `sso_routes.go`.

	// The detection worker's API. The gate is mounted now so the answer
	// "internal api disabled" is true the moment WORKER_TOKEN is empty, before
	// any internal route exists (the routes arrive with S5-10).
	r.Group("/api/internal/v1", InternalAuthMiddleware(h.Cfg.WorkerToken))
}

// AuditTarget names what an act pointed at, in the ledger's words.
type AuditTarget struct {
	Type string
	ID   string
}

// audit writes one ledger row for an act performed by the actor on this request.
//
// The writer is called by the handlers, not by middleware: middleware cannot know
// the target or whether the act succeeded, and those two facts are the record.
func (h *Handlers) audit(c *gin.Context, action string, status int, target AuditTarget, details map[string]any) {
	h.auditAs(c, actorFrom(c), action, status, target, details)
}

// auditAs writes a row for a named actor — the case a login refusal has, where
// nobody is authenticated and the ledger still has to say who was tried.
func (h *Handlers) auditAs(c *gin.Context, actor models.AuditActor, action string, status int, target AuditTarget, details map[string]any) {
	if h.St == nil || h.St.Audit == nil {
		return
	}
	if details == nil {
		details = map[string]any{}
	}
	details["method"] = c.Request.Method
	details["route"] = c.FullPath()

	_ = h.St.Audit.Record(&models.Audit{
		ID:            models.NewAuditID(),
		ActorUserID:   actor.UserID,
		ActorUsername: actor.Username,
		ActorRoles:    actor.Roles,
		ActorProvider: actor.Provider,
		Action:        action,
		TargetType:    target.Type,
		TargetID:      target.ID,
		Outcome:       models.AuditOutcomeFor(status),
		Details:       store.EncodeDetails(details),
		SourceIP:      c.ClientIP(),
		CreatedAt:     h.now().UTC(),
	})
}

// actorFrom captures who this request is, from the claims the auth middleware
// loaded. A refusal before the gate has no actor, and the row says so with empty
// columns — which is why the ledger's actor columns are text, not uuid.
func actorFrom(c *gin.Context) models.AuditActor {
	claims := ClaimsFrom(c)
	if claims == nil {
		return models.AuditActor{}
	}
	return models.AuditActor{
		UserID:   claims.UserID,
		Username: claims.Username,
		Roles:    strings.Join(claims.Roles, ","),
	}
}

// actorOf captures an account we loaded from the database (a login that arrived
// at a real account, and failed there).
func actorOf(u *models.User) models.AuditActor {
	if u == nil {
		return models.AuditActor{}
	}
	out := models.AuditActor{UserID: u.ID, Username: u.Username, Provider: u.Provider}
	for _, role := range u.Roles {
		out.Roles += role.Name + ","
	}
	out.Roles = strings.TrimSuffix(out.Roles, ",")
	return out
}

// roleNames reads an account's roles from the rows, which is what a session is
// issued with. An account with no role rows gets the viewer floor and nothing
// more, so a missing row never widens access.
func (h *Handlers) roleNames(u *models.User) []string {
	if u == nil {
		return nil
	}
	names, err := h.St.Users.RolesFor(u.ID)
	if err != nil || len(names) == 0 {
		return []string{models.RoleViewer}
	}
	return names
}

// userView is the account as the API shows it. PasswordHash is `json:"-"` on the
// model, so there is no path from here to a hash.
type userView struct {
	ID        string     `json:"id"`
	Username  string     `json:"username"`
	Name      string     `json:"name"`
	Email     string     `json:"email"`
	Provider  string     `json:"provider"`
	Status    string     `json:"status"`
	Roles     []string   `json:"roles"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	LastLogin *time.Time `json:"last_login"`
}

func (h *Handlers) view(u *models.User) userView {
	return userView{
		ID:        u.ID,
		Username:  u.Username,
		Name:      u.Name,
		Email:     u.Email,
		Provider:  u.Provider,
		Status:    u.Status,
		Roles:     h.roleNames(u),
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
		LastLogin: u.LastLogin,
	}
}

// openSession records the issued refresh token as a session row, which is what
// makes rotation and reuse detection observable.
//
// The row carries the SHA-256 of the token, never the token: a database read must
// not be able to hand out a live session.
func (h *Handlers) openSession(c *gin.Context, u *models.User, refreshToken string) (*models.Session, error) {
	session := &models.Session{
		UserID:    u.ID,
		TokenHash: models.HashRefreshToken(refreshToken),
		ExpiresAt: h.now().UTC().Add(models.RefreshTokenTTL),
		SourceIP:  c.ClientIP(),
		UserAgent: truncate(c.GetHeader("User-Agent"), 200),
		CreatedAt: h.now().UTC(),
	}
	if err := h.St.Sessions.Create(session); err != nil {
		return nil, err
	}
	return session, nil
}

// truncate bounds a client-supplied string before it lands in a column.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// jsonError answers the one error shape the console and scripts/verify.sh read.
func jsonError(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": msg})
}
