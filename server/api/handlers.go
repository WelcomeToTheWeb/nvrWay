// Package api is the console's HTTP surface: the handler set every step mounts
// its routes into, the middleware that guards them, and the shapes the console
// and scripts/verify.sh read.
//
// Three things are decided here rather than discovered later:
//
//   - The bearer header is the only REST credential. `?token=` is accepted on
//     `/ws` and nowhere else, and that exception is written down in SECURITY.md
//     rather than inherited as a habit.
//   - Every refusal is a ledger row. A 403 that the operator cannot reconstruct
//     ("who tried, when, on what") is a 403 that turns into a developer reading
//     server logs at 2 a.m.
//   - The routes' response bodies are the contract. `docs/api.md` states them,
//     `scripts/verify.sh` greps them, and the console draws its screens from
//     them, so a handler's job is to answer in names, at the status the plan
//     names.
package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"nvr/server/auth"
	"nvr/server/config"
	"nvr/server/go2rtc"
	"nvr/server/media"
	"nvr/server/models"
	"nvr/server/sidecar"
	"nvr/server/sso"
	"nvr/server/store"
	"nvr/server/ws"
)

// Handlers is the mount point. Steps 3–7 add their routes to it, so every route
// in the product runs behind the same gates, the same rate limiter and the same
// ledger.
type Handlers struct {
	Cfg *config.Config
	St  *store.Store
	JWT *auth.JWTAuth
	// Hub is the live feed. nil means the `/ws` route is not mounted: the hub is
	// a listener with no recorder feeding it, and a socket that connects to
	// nothing is a console that believes it is live.
	Hub *ws.Hub
	// OAuth is S2-04's contract: the code exchange, the signed-token verification
	// and the identity gate. The routes here are thin over it.
	OAuth *sso.OAuthHandler
	// Limiter is the per-user REST budget.
	Limiter *RateLimiter

	// Go2rtc is the media sidecar client: the probe, the snapshot, and the
	// config write every camera write makes (S3-01, S3-05). nil means no sidecar
	// is configured, and the camera routes say so rather than reporting a camera
	// as probeable.
	Go2rtc *go2rtc.Client

	// Restarter makes a config write live (S3-07). go2rtc instantiates its
	// streams at startup, so a camera write that only writes the file leaves the
	// new stream out of the live map until the sidecar restarts. nil is not
	// allowed: the constructor gives the caller a Noop that says why, and the
	// console is told the stream goes live at the next boot.
	Restarter sidecar.Restarter
	// RestartCalls and LastRestart are the suite's window into the restart path:
	// "create restarts once, zone writes restart none" is a count, and a count
	// asserted from a fake is a fact, not a log line.
	RestartCalls   int
	LastRestart    sidecar.Report
	LastRestartErr error

	// Recorder is the recording supervisor (S4-01). main.go builds it, starts it,
	// and hands it here so the signal path can open a recording window on it
	// (`Recorder.OpenWindow`, the card S6-03 wires) — the recorder is one object
	// owned by the process, not one per request path.
	Recorder *media.Recorder

	// Clips is the clip assembler (S6-01). Like the recorder it is one object
	// owned by the process — it holds the armed assemblies, and one per event is
	// the whole point — so the signal path schedules onto it instead of building
	// one per request. nil means the process has no assembler: signals file
	// events, and the clips are simply never cut.
	Clips ClipScheduler

	// states is the server-side half of an SSO login in flight: the state value
	// the redirect issued, with the nonce and PKCE verifier only this process
	// knows. Short-lived and single-use (S2-07).
	states *loginStates

	// now is the clock. Every timestamp this surface writes — session expiry,
	// state expiry, ledger `created_at` — comes from here, so a test can move
	// time instead of sleeping through it.
	now func() time.Time
}

// ClipScheduler is the clip assembler as the signal path sees it: an event row
// just moved, and something has to remember to cut its clip once the post-roll
// is spent. `*media.ClipBuilder` implements it; a suite hands in a spy, because
// the deliverable here is *the call*, not an ffmpeg run.
type ClipScheduler interface {
	// Schedule arms (or re-arms) one event's assembly: the open ceiling while the
	// event is open, the post-roll deadline once it closes.
	Schedule(ev *models.Event)
	// Cancel disarms a pending assembly — the delete path's need (S6-04).
	Cancel(eventID string)
}

// NewHandlers builds the handler set. Passing a nil jwtAuth is not a hole: the
// signing secret is configuration, and a handler set with no way to sign a token
// cannot answer a login.
func NewHandlers(ctx context.Context, st *store.Store, jwtAuth *auth.JWTAuth, hub *ws.Hub, cfg *config.Config) *Handlers {
	h := &Handlers{
		Cfg:     cfg,
		St:      st,
		JWT:     jwtAuth,
		Hub:     hub,
		Limiter: NewRateLimiter(cfg.RateLimitPerMinute, cfg.RateLimitBurst),
		states:  newLoginStates(time.Now),
		now:     time.Now,
	}
	if h.JWT == nil {
		h.JWT = auth.NewJWTAuth(cfg.JWTSecret)
	}
	h.OAuth = sso.NewOAuthHandler(st, h.JWT)
	return h
}

// SetClock points every timestamp this surface writes at one clock. Tests use it
// to expire a state, to let a token age, and to prove a rate limit refills.
func (h *Handlers) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	h.now = now
	h.states.now = now
	h.Limiter.SetClock(now)
}

// Register mounts the route table of `docs/api.md`.
//
// The groups are the permission shape, not a folder layout: `public` is what a
// person who is not signed in may reach (behind the rate limiter, because those
// requests are the ones someone sends without an account), `session` is anything
// that names an account, `admin` is the surface that changes who people are.
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

	session := public.Group("", AuthMiddleware(h.JWT))
	session.POST("/logout", h.Logout)
	session.GET("/profile", h.Profile)
	session.PUT("/password", h.ChangePassword)

	h.registerSSORoutes(public, session)

	// The camera surface (Step 3): the rows, the probe, the snapshot, the zone
	// rules, and the one path that writes go2rtc's config.
	h.registerCameraRoutes(r)

	// The timeline manifest (S4-05): the recorded segments in a window. This is
	// also the surface `scripts/verify.sh` gate 6 reads to see that recording is
	// actually happening, so its shape is an acceptance gate, not a convenience.
	h.registerSegmentRoutes(r)

	// The admin surface: accounts, roles, the pending-identity queue and the
	// ledger. The gate is the role check, and the denial is audited by the same
	// middleware that answers it.
	admin := r.Group("/api", AuthMiddleware(h.JWT), RequireRole(models.RoleAdmin, h.St.Audit))
	h.registerIdentityRoutes(admin)
	h.registerUserRoutes(admin)
	h.registerOIDCRoutes(admin)

	// The detection worker's API. The gate is mounted so the answer
	// "internal api disabled" is true the moment WORKER_TOKEN is empty, and the
	// two routes the worker polls and posts to are mounted behind it (S5-10).
	internal := r.Group("/api/internal/v1", InternalAuthMiddleware(h.Cfg.WorkerToken))
	h.registerDetectorRoutes(internal)
}

// AuditTarget names what an act pointed at, in the ledger's words.
type AuditTarget struct {
	Type string
	ID   string
}

// audit writes the ledger row for an act performed by whoever the request
// authenticated as.
//
// The ledger is written from the request, not from a background worker, because
// the row's whole value is that it names the actor and the source address of the
// act that happened. A failure to write is deliberately not fatal: an audit row
// that cannot be written must not turn a working installation into a 500, and a
// refusal to a request is recorded in the response the client receives.
func (h *Handlers) audit(c *gin.Context, action string, status int, target AuditTarget, details map[string]any) {
	actor := models.AuditActor{}
	if claims := ClaimsFrom(c); claims != nil {
		// The provider column comes from the account row, not the token: S2-01's
		// claims carry an id, a name and roles. A row written from claims alone
		// leaves it empty, and the rows that name a person (`auditAs` with
		// `actorOf`) fill it from the row that knows.
		actor = models.AuditActor{
			UserID:   claims.UserID,
			Username: claims.Username,
			Roles:    strings.Join(claims.Roles, ","),
		}
	}
	h.auditAs(c, actor, action, status, target, details)
}

// auditAs writes a row for an actor the handler knows and the request does not —
// the login path, where the account is established by the act being recorded, and
// the failed logins, where the only thing known is a username someone typed.
func (h *Handlers) auditAs(c *gin.Context, actor models.AuditActor, action string, status int, target AuditTarget, details map[string]any) {
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

// actorOf is the ledger's actor for an account that exists.
func actorOf(u *models.User) models.AuditActor {
	return models.AuditActor{
		UserID:   u.ID,
		Username: u.Username,
		Roles:    strings.Join(roleNamesOf(u), ","),
		Provider: u.Provider,
	}
}

func roleNamesOf(u *models.User) []string {
	if u == nil {
		return nil
	}
	names := make([]string, 0, len(u.Roles))
	for _, role := range u.Roles {
		names = append(names, role.Name)
	}
	return names
}

// roleNames reads an account's roles from the rows. The store preloads them on
// every account fetch, so this is not a query; it is the loaded list.
//
// It matters where the roles come from: a token carries the roles its account had
// when it was minted, and the ledger should record what the account is granted
// now.
func (h *Handlers) roleNames(u *models.User) []string {
	names := roleNamesOf(u)
	if len(names) > 0 {
		return names
	}
	roles, err := h.St.Users.RolesFor(u.ID)
	if err != nil {
		return nil
	}
	return roles
}

// userView is an account as the API answers it. No password hash, ever; the
// provider fields are present because "how does this account sign in" is a fact
// the console has to show.
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

// openSession records the session row a refresh token belongs to.
//
// Without this row a refresh token has nothing to be rotated against, and reuse
// detection has nothing to notice: the row is the difference between a refresh
// token that can be revoked and a bearer token that cannot.
func (h *Handlers) openSession(c *gin.Context, u *models.User, refreshToken string) (*models.Session, error) {
	now := h.now().UTC()
	session := &models.Session{
		ID:        uuid.NewString(),
		UserID:    u.ID,
		TokenHash: models.HashRefreshToken(refreshToken),
		ExpiresAt: now.Add(models.RefreshTokenTTL),
		SourceIP:  c.ClientIP(),
		UserAgent: truncate(c.Request.UserAgent(), 200),
		CreatedAt: now,
	}
	if err := h.St.Sessions.Create(session); err != nil {
		return nil, err
	}
	return session, nil
}

// truncate bounds a client-supplied string at the column's width. A user agent
// longer than the column is not a reason a login cannot be recorded.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// randomToken is 32 random bytes, base64url: the shape the SSO state value, the
// nonce and the CSRF-ish secrets are built from. The PKCE verifier itself comes
// from S2-04's helper, so its length rules live in one place.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// jsonError is the plan's error shape: {"error":"<human readable>"}.
func jsonError(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": msg})
}

// uniqueUsername turns an asserted identity into a username nobody holds.
//
// The IdP's words are the first candidates — its username claim, then the local
// part of its email — and the provider's name is the suffix that makes two
// providers asserting `jsmith` produce two accounts. A suffix that collides gets a
// number, because "user exists" is not an answer to give a person standing at an
// IdP's login page.
func (h *Handlers) uniqueUsername(provider, username, email string) string {
	candidate := strings.TrimSpace(username)
	if candidate == "" {
		candidate = strings.TrimSpace(strings.SplitN(email, "@", 2)[0])
	}
	candidate = sanitizeUsername(candidate)
	if candidate == "" {
		candidate = "user"
	}
	base := candidate
	if !h.usernameFree(candidate) {
		candidate = sanitizeUsername(candidate + "-" + provider)
	}
	for n := 2; !h.usernameFree(candidate); n++ {
		candidate = sanitizeUsername(base + "-" + provider + "-" + itoa(n))
	}
	return candidate
}

func sanitizeUsername(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			out = append(out, r)
		case r == ' ':
			out = append(out, '-')
		}
	}
	s = strings.Trim(string(out), "-._")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// usernameFree asks the only question the answer depends on.
func (h *Handlers) usernameFree(candidate string) bool {
	_, err := h.St.Users.GetByUsername(candidate)
	return err != nil
}
