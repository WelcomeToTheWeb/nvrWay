package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"nvr/server/models"
	"nvr/server/sso"
	"nvr/server/store"
)

// The OIDC routes: the redirect out, the callback back, the exchange the console
// makes with what the fragment handed it, and the queue of identities the
// contract refused to act on.
//
// Two shapes in this file are contracts with the operator's IdP and with the
// console, and neither is negotiable at runtime:
//
//   - The redirect URI is `${WEB_URL}/api/auth/sso/<provider>/callback`. The
//     operator whitelists exactly that string (`docs/installation.md` states it),
//     so the callback must send the same string it told them to register — which is
//     why it is computed in one function from the same `WEB_URL` the operator wrote
//     down, not typed in two places.
//   - The handoff back to the console is a **fragment**:
//     `${WEB_URL}/auth/callback#access_token=…&refresh_token=…`. A fragment is not
//     sent to any server, so the tokens leave the URL as soon as the console's
//     script has read them, and they never appear in the IdP's logs, in our access
//     logs, or in a `Referer` a redirect carries.

// LoginStateTTL is how long a login may be in flight. Five minutes is a person
// typing a password at an IdP's page, on a phone, once — and nothing longer,
// because the record carries the PKCE verifier that makes a stolen code useless,
// and a state that outlives the login it belongs to is a live credential with no
// owner.
const LoginStateTTL = 5 * time.Minute

type loginState struct {
	Provider    string
	State       string
	Nonce       string
	Verifier    string
	RedirectURI string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// loginStates is the server-side half of an SSO login in flight: the state value
// the redirect sent, and the three secrets only this server knows — the nonce the
// id_token must carry, the PKCE verifier the code must be spent with, and the
// redirect URI that was registered.
//
// It is in memory, deliberately. The record is not session state: it lives five
// minutes, is read exactly once, and describes a login that has not happened yet.
// A table for it would survive the restart that invalidates every session anyway
// (the signing key is ephemeral when `JWT_SECRET` is unset), and it would be
// written by every unauthenticated redirect — which is a request anyone can make.
// Single use is enforced by `Take`: the first read removes the record, so a
// replayed callback has nothing to match against.
type loginStates struct {
	mu      sync.Mutex
	byState map[string]*loginState
	ttl     time.Duration
	now     func() time.Time
}

func newLoginStates(now func() time.Time) *loginStates {
	if now == nil {
		now = time.Now
	}
	return &loginStates{byState: map[string]*loginState{}, ttl: LoginStateTTL, now: now}
}

// New records a login in flight. The verifier comes from S2-04's helper, so the
// challenge the redirect carries is the S256 one and never the verifier itself.
func (s *loginStates) New(provider, redirectURI string) (*loginState, error) {
	state, err := randomToken()
	if err != nil {
		return nil, err
	}
	nonce, err := randomToken()
	if err != nil {
		return nil, err
	}
	verifier, err := sso.NewCodeVerifier()
	if err != nil {
		return nil, err
	}

	now := s.now().UTC()
	rec := &loginState{
		Provider:    provider,
		State:       state,
		Nonce:       nonce,
		Verifier:    verifier,
		RedirectURI: redirectURI,
		CreatedAt:   now,
		ExpiresAt:   now.Add(s.ttl),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.byState[state] = rec
	return rec, nil
}

// Take consumes a state value. It answers three things, because the callback has
// to say which of them happened: the record, whether it was found, and whether it
// was found expired.
//
// An expired state is removed and refused — a login is not something that can be
// completed late — and an unknown one is refused without saying whether it was
// ever issued.
func (s *loginStates) Take(state string) (rec *loginState, found, expired bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byState[state]
	if !ok {
		return nil, false, false
	}
	delete(s.byState, state)
	return rec, true, !s.now().UTC().Before(rec.ExpiresAt)
}

// Prune drops states that expired without being used — the tidy-up a long-lived
// process does on a map that unauthenticated requests fill.
func (s *loginStates) Prune() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	var dropped int
	for key, rec := range s.byState {
		if !now.Before(rec.ExpiresAt) {
			delete(s.byState, key)
			dropped++
		}
	}
	return dropped
}

// Len is the number of logins in flight, for the tests and the readiness line.
func (s *loginStates) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byState)
}

// SSORedirectURI is the exact string the operator whitelists in their IdP.
func SSORedirectURI(webURL, provider string) string {
	return strings.TrimSuffix(strings.TrimSpace(webURL), "/") + "/api/auth/sso/" + provider + "/callback"
}

// ssoHandoffURL is the console's landing shape, fragment included.
func ssoHandoffURL(webURL, fragment string) string {
	base := strings.TrimSuffix(strings.TrimSpace(webURL), "/") + "/auth/callback"
	if fragment == "" {
		return base
	}
	return base + "#" + fragment
}

// registerSSORoutes mounts the OIDC routes.
func (h *Handlers) registerSSORoutes(public, session *gin.RouterGroup) {
	public.GET("/sso/providers", h.SSOProviders)
	public.GET("/sso/:provider/authorize", h.SSOAuthorize)
	public.GET("/sso/:provider/callback", h.SSOCallback)
	session.POST("/sso/exchange", h.SSOExchange)
}

// registerIdentityRoutes mounts the pending-identity queue. Every route here is an
// administrator's, and every decision is a ledger row.
func (h *Handlers) registerIdentityRoutes(admin *gin.RouterGroup) {
	admin.GET("/identities", h.ListIdentities)
	admin.POST("/identities/:id/approve", h.ApproveIdentity)
	admin.POST("/identities/:id/link", h.LinkIdentity)
	admin.POST("/identities/:id/refuse", h.RefuseIdentity)
}

// providerFor reads the provider the route names. A provider row that does not
// exist answers 404, and one that is switched off answers 403: the login page lists
// the enabled providers, so both shapes are reached by typing a URL.
func (h *Handlers) providerFor(c *gin.Context) (*models.SSOProvider, int, string) {
	name := strings.TrimSpace(c.Param("provider"))
	if name == "" {
		return nil, http.StatusBadRequest, "provider is required"
	}
	provider, err := h.St.SSOProviders.GetByName(name)
	if err != nil {
		if errors.Is(err, store.ErrProviderNotFound) {
			return nil, http.StatusNotFound, "no such provider"
		}
		return nil, http.StatusInternalServerError, "providers are unreadable"
	}
	if !provider.Enabled {
		return provider, http.StatusForbidden, "this provider is switched off"
	}
	return provider, 0, ""
}

// SSOProviders is the list the login page draws its buttons from, including the
// one string the operator has to put in the IdP.
//
// GET /api/auth/sso/providers
func (h *Handlers) SSOProviders(c *gin.Context) {
	providers, err := h.St.SSOProviders.ListEnabled()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "providers are unreadable")
		return
	}
	out := []gin.H{}
	for _, p := range providers {
		if p.Type != models.SSOTypeOIDC {
			continue
		}
		out = append(out, gin.H{
			"provider":       p.Name,
			"name":           p.Name,
			"issuer":         p.Issuer,
			"redirect_uri":   SSORedirectURI(h.Cfg.WebURL, p.Name),
			"link_policy":    p.LinkPolicy,
			"scopes":         p.Scope,
			"pkce_supported": p.PKCESupported,
			"discovery":      p.DiscoveryState,
		})
	}
	c.JSON(http.StatusOK, gin.H{"providers": out})
}

// SSOAuthorize starts the login: it records the state, then hands the browser to
// the provider with the state, the nonce and the S256 challenge.
//
// GET /api/auth/sso/:provider/authorize
func (h *Handlers) SSOAuthorize(c *gin.Context) {
	provider, status, msg := h.providerFor(c)
	if msg != "" {
		jsonError(c, status, msg)
		return
	}
	if strings.TrimSpace(provider.AuthURL) == "" || strings.TrimSpace(provider.ClientID) == "" {
		// Discovery writes these. An operator who typed a provider whose issuer
		// publishes no endpoints needs to be told the login cannot start, not sent
		// to a provider that will refuse it.
		jsonError(c, http.StatusUnprocessableEntity,
			"this provider has no authorize endpoint: run discovery against its issuer")
		return
	}

	// The redirect URI is the string the operator whitelisted. The router mounts
	// the callback at one path, so that path is the only string worth sending to
	// the IdP: when the row carries a different one, the operator is told the exact
	// string, in the words they copy into the IdP's form.
	redirect := SSORedirectURI(h.Cfg.WebURL, provider.Name)
	if stored := strings.TrimSpace(provider.RedirectURI); stored != "" && stored != redirect {
		jsonError(c, http.StatusUnprocessableEntity,
			"this IdP must whitelist "+redirect+" — the provider row carries "+stored)
		return
	}

	rec, err := h.states.New(provider.Name, redirect)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the login could not be started")
		return
	}

	target := h.OAuth.AuthorizeURL(provider, rec.RedirectURI, rec.State, rec.Nonce, sso.CodeChallenge(rec.Verifier))
	c.Redirect(http.StatusFound, target)
}

// SSOCallback is the half the operator's IdP posts to.
//
// GET /api/auth/sso/:provider/callback
//
// The state is consumed before anything else happens: one callback per login. The
// refusals are ledger rows carrying the reason the contract named, because "SSO
// login failed" is a message nobody can act on.
func (h *Handlers) SSOCallback(c *gin.Context) {
	provider, status, msg := h.providerFor(c)
	if status != 0 {
		// The provider row is switched off or absent, on a route the IdP redirected
		// to. The ledger records it under the SSO verb, because an operator watching
		// a login that never arrives is looking here.
		h.audit(c, models.AuditSSOLoginRefused, status,
			AuditTarget{Type: "provider", ID: strings.TrimSpace(c.Param("provider"))},
			map[string]any{"reason": "provider_unavailable", "detail": msg})
		jsonError(c, status, msg)
		return
	}

	rec, found, expired := h.states.Take(strings.TrimSpace(c.Query("state")))
	if !found {
		h.ssoRefusal(c, provider, sso.ReasonStateMismatch, "", "", "the login state is not recognised")
		return
	}
	if expired {
		h.ssoRefusal(c, provider, sso.ReasonStateExpired, rec.Nonce, "",
			"this login was started more than "+itoa(int(LoginStateTTL.Minutes()))+" minutes ago")
		return
	}
	if rec.Provider != provider.Name {
		// A state issued for one provider arriving at another provider's callback is
		// not a login anyone started deliberately.
		h.ssoRefusal(c, provider, sso.ReasonStateMismatch, "", "", "the login state belongs to a different provider")
		return
	}

	code := strings.TrimSpace(c.Query("code"))
	if code == "" {
		// The IdP sent the browser back without a code — usually its own refusal
		// (consent denied, unknown client). Its words are kept: the person in front
		// of the browser is the one who has to act on them.
		detail := "the provider returned no code"
		if idpErr := strings.TrimSpace(c.Query("error")); idpErr != "" {
			detail = "the provider said: " + idpErr
			if desc := strings.TrimSpace(c.Query("error_description")); desc != "" {
				detail += " — " + desc
			}
		}
		h.ssoRefusal(c, provider, "provider_returned_no_code", rec.Nonce, "", detail)
		return
	}

	outcome, err := h.OAuth.HandleCallback(c.Request.Context(), provider, code, rec.RedirectURI, rec.Nonce, rec.Verifier)
	if err != nil {
		h.ssoFailure(c, provider, err)
		return
	}

	session, err := h.openSession(c, outcome.User, outcome.RefreshToken)
	if err != nil {
		// The identity is sound and the tokens are issued; a session that cannot be
		// recorded is a session that cannot be rotated, which is the half of the
		// design that survives a stolen token. Refuse the login and say why.
		h.ssoRefusal(c, provider, "session_record_failed", rec.Nonce, outcome.Email,
			"the session could not be recorded")
		return
	}

	h.auditAs(c, actorOf(outcome.User), models.AuditSSOLogin, http.StatusOK,
		AuditTarget{Type: "user", ID: outcome.User.ID}, map[string]any{
			"provider":             provider.Name,
			"subject":              outcome.Subject,
			"email":                outcome.Email,
			"email_verified":       outcome.EmailVerified,
			"verified_by_id_token": outcome.VerifiedByIDToken,
			"created_jit":          outcome.CreatedJIT,
			"linked_existing":      outcome.LinkedExisting,
			"mapped_roles":         outcome.MappedRoles,
			"roles":                h.roleNames(outcome.User),
			"session":              session.ID,
		})

	// The tokens go to the console in the fragment. Nothing here is logged: the
	// redirect target is our own page, and a fragment is not a request.
	c.Redirect(http.StatusFound, ssoHandoffURL(h.Cfg.WebURL,
		"access_token="+url.QueryEscape(outcome.AccessToken)+
			"&refresh_token="+url.QueryEscape(outcome.RefreshToken)))
}

// ssoRefusal records a refusal whose facts we know, and sends the browser home to
// the console's own failure state.
func (h *Handlers) ssoRefusal(c *gin.Context, provider *models.SSOProvider, reason, subject, email, detail string) {
	h.audit(c, models.AuditSSOLoginRefused, http.StatusForbidden,
		AuditTarget{Type: "provider", ID: provider.Name}, map[string]any{
			"reason":  reason,
			"subject": subject,
			"email":   email,
			"detail":  detail,
		})
	c.Redirect(http.StatusFound, ssoHandoffURL(h.Cfg.WebURL,
		"error="+url.QueryEscape(reason)+"&error_description="+url.QueryEscape(detail)+
			"&provider="+url.QueryEscape(provider.Name)))
}

// ssoFailure turns the contract's refusal into the console's state and the
// ledger's row. The reason string is the contract's own vocabulary (S2-04), so a
// pending identity, a domain the operator did not list, and an IdP that asserts two
// different subjects in one login are three different screens — and three different
// fixes.
func (h *Handlers) ssoFailure(c *gin.Context, provider *models.SSOProvider, err error) {
	var iderr *sso.IdentityError
	if !errors.As(err, &iderr) {
		h.ssoRefusal(c, provider, "sso_login_failed", "", "", err.Error())
		return
	}

	details := map[string]any{
		"reason":  iderr.Reason,
		"subject": iderr.Subject,
		"email":   iderr.Email,
		"detail":  iderr.Err.Error(),
	}
	if iderr.TargetUserID != "" {
		details["target_user_id"] = iderr.TargetUserID
	}
	if iderr.PendingUserID != "" {
		details["pending_identity_id"] = iderr.PendingUserID
	}
	h.audit(c, models.AuditSSOLoginRefused, http.StatusForbidden,
		AuditTarget{Type: "provider", ID: provider.Name}, details)

	fragment := "error=" + url.QueryEscape(iderr.Reason) +
		"&provider=" + url.QueryEscape(provider.Name) +
		"&error_description=" + url.QueryEscape(iderr.Err.Error())
	if iderr.PendingUserID != "" {
		fragment += "&pending_identity=" + url.QueryEscape(iderr.PendingUserID)
	}
	c.Redirect(http.StatusFound, ssoHandoffURL(h.Cfg.WebURL, fragment))
}

// SSOExchange is the console's first authenticated call: the token the fragment
// handed it is presented, and the account it names comes back with its roles.
//
// POST /api/auth/sso/exchange
//
// It exists so the console has one call that says "this token is mine, and this is
// what I am allowed to do", rather than decoding the token in JavaScript and
// drawing tabs from a claim it did not sign.
func (h *Handlers) SSOExchange(c *gin.Context) {
	claims := ClaimsFrom(c)
	if claims == nil {
		jsonError(c, http.StatusUnauthorized, "not authenticated")
		return
	}
	user, err := h.St.Users.GetByID(claims.UserID)
	if err != nil {
		jsonError(c, http.StatusUnauthorized, "this account no longer exists")
		return
	}
	if user.IsPending() {
		jsonError(c, http.StatusForbidden, "this account is waiting for an administrator's decision")
		return
	}
	h.auditAs(c, actorOf(user), models.AuditAuthProfile, http.StatusOK,
		AuditTarget{Type: "user", ID: user.ID}, map[string]any{
			"step":  "exchange",
			"roles": h.roleNames(user),
		})
	c.JSON(http.StatusOK, h.view(user))
}

// identityView is the queue row an admin decides from. The claims are included
// verbatim: the row is the record of an assertion the IdP made once, and it is not
// reproducible later.
func identityView(p *models.PendingIdentity) gin.H {
	claims := map[string]any{}
	_ = json.Unmarshal([]byte(p.Claims), &claims)
	return gin.H{
		"id":                  p.ID,
		"provider":            p.Provider,
		"subject":             p.ProviderSubject,
		"email":               p.Email,
		"username":            p.Username,
		"display_name":        p.DisplayName,
		"status":              p.Status,
		"matched_user_id":     p.MatchedUserID,
		"created_user_id":     p.CreatedUserID,
		"claims":              claims,
		"created_at":          p.CreatedAt,
		"decided_at":          p.DecidedAt,
		"decided_by":          p.DecidedBy,
		"decided_by_username": p.DecidedByUsername,
		"reason":              p.Reason,
	}
}

// ListIdentities is the queue, and the record of the decisions.
//
// GET /api/identities?status=pending|all
func (h *Handlers) ListIdentities(c *gin.Context) {
	var (
		rows []models.PendingIdentity
		err  error
	)
	status := strings.ToLower(strings.TrimSpace(c.DefaultQuery("status", "pending")))
	if status == "all" {
		rows, err = h.St.PendingIdentities.ListAll()
	} else {
		status = "pending"
		rows, err = h.St.PendingIdentities.ListPending()
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "identities are unreadable")
		return
	}
	out := []gin.H{}
	for i := range rows {
		out = append(out, identityView(&rows[i]))
	}
	h.audit(c, models.AuditIdentityRead, http.StatusOK,
		AuditTarget{Type: "collection", ID: "pending_identities"},
		map[string]any{"count": len(out), "status": status})
	c.JSON(http.StatusOK, gin.H{"identities": out, "status": status})
}

// identityDecision reads the row every decision route needs, and answers the two
// shapes that are not decisions: a row that does not exist, and a row someone has
// already decided.
func (h *Handlers) identityDecision(c *gin.Context) (*models.PendingIdentity, int, string) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		return nil, http.StatusBadRequest, "identity id is required"
	}
	row, err := h.St.PendingIdentities.GetByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, http.StatusNotFound, "no such identity"
		}
		return nil, http.StatusInternalServerError, "identities are unreadable"
	}
	if !row.IsPending() {
		return row, http.StatusConflict, "this identity was already decided as " + row.Status
	}
	return row, 0, ""
}

// ApproveIdentity creates the account the assertion describes, at the viewer floor.
//
// POST /api/identities/:id/approve
//
// The floor is the reason approving an identity cannot grant authority. Roles are a
// separate, audited act by an admin naming a person — not an IdP asserting a string.
func (h *Handlers) ApproveIdentity(c *gin.Context) {
	row, status, msg := h.identityDecision(c)
	if msg != "" {
		h.audit(c, models.AuditIdentityApprove, status, AuditTarget{Type: "identity", ID: c.Param("id")},
			map[string]any{"reason": msg})
		jsonError(c, status, msg)
		return
	}

	claims := ClaimsFrom(c)
	user := &models.User{
		Username:   h.uniqueUsername(row.Provider, row.Username, row.Email),
		Name:       row.DisplayName,
		Email:      row.Email,
		Provider:   row.Provider,
		ProviderID: row.ProviderSubject,
		Status:     models.UserStatusActive,
	}
	if err := h.St.Users.Create(user); err != nil {
		jsonError(c, http.StatusConflict, "an account for this identity already exists: "+err.Error())
		return
	}
	if err := h.St.Users.SetRoles(user.ID, []string{models.RoleViewer}); err != nil {
		jsonError(c, http.StatusInternalServerError, "the account could not be given the viewer floor")
		return
	}
	if err := h.St.PendingIdentities.MarkDecided(row.ID, models.IdentityStatusApproved,
		claims.UserID, claims.Username, "approved as its own account at the viewer floor", user.ID); err != nil {
		jsonError(c, http.StatusInternalServerError, "the decision could not be recorded")
		return
	}

	h.audit(c, models.AuditIdentityApprove, http.StatusOK, AuditTarget{Type: "user", ID: user.ID},
		map[string]any{
			"identity": row.ID,
			"provider": row.Provider,
			"subject":  row.ProviderSubject,
			"email":    row.Email,
			"username": user.Username,
			"roles":    h.roleNames(user),
			"floor":    models.RoleViewer,
		})
	c.JSON(http.StatusOK, gin.H{
		"identity": identityView(row),
		"user":     h.view(user),
		"roles":    h.roleNames(user),
	})
}

// LinkIdentity merges an assertion into an account that already exists.
//
// POST /api/identities/:id/link
//
// This is the operator's answer to `link_policy=pending`: the assertion is parked,
// the operator decides, and the decision is a ledger row naming the account the
// assertion pointed at and the admin who made the call. The account's roles are not
// touched — merging an identity is not a way to grant authority either.
func (h *Handlers) LinkIdentity(c *gin.Context) {
	row, status, msg := h.identityDecision(c)
	if msg != "" {
		h.audit(c, models.AuditIdentityLink, status, AuditTarget{Type: "identity", ID: c.Param("id")},
			map[string]any{"reason": msg})
		jsonError(c, status, msg)
		return
	}

	var req struct {
		UserID string `json:"user_id"`
	}
	_ = c.ShouldBindJSON(&req)
	target := strings.TrimSpace(req.UserID)
	if target == "" {
		// The row names the account the asserted email belongs to. That is the
		// default, and the reason the queue exists: the assertion pointed at
		// someone, and an admin is confirming it.
		target = strings.TrimSpace(row.MatchedUserID)
	}
	if target == "" {
		jsonError(c, http.StatusUnprocessableEntity,
			"user_id is required: this identity names no existing account")
		return
	}

	user, err := h.St.Users.GetByID(target)
	if err != nil {
		h.audit(c, models.AuditIdentityLink, http.StatusNotFound, AuditTarget{Type: "identity", ID: row.ID},
			map[string]any{"reason": "no such account", "user_id": target})
		jsonError(c, http.StatusNotFound, "no such account")
		return
	}
	if strings.TrimSpace(user.ProviderID) != "" && user.Provider != row.Provider {
		// An account already belongs to a provider. Attaching a second identity
		// would mean two logins, two assertions, and one account whose authority came
		// from whichever IdP arrives first.
		jsonError(c, http.StatusConflict, "this account already signs in through "+user.Provider)
		return
	}
	if user.Provider == row.Provider && user.ProviderID == row.ProviderSubject {
		jsonError(c, http.StatusConflict, "this account already carries that identity")
		return
	}

	if err := h.St.Users.LinkIdentity(user.ID, row.Provider, row.ProviderSubject); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrIdentityTaken) {
			status = http.StatusConflict
		}
		h.audit(c, models.AuditIdentityLink, status, AuditTarget{Type: "user", ID: user.ID},
			map[string]any{"identity": row.ID, "reason": "the identity is already attached to an account"})
		jsonError(c, status, "that identity is already attached to an account")
		return
	}

	claims := ClaimsFrom(c)
	if err := h.St.PendingIdentities.MarkDecided(row.ID, models.IdentityStatusLinked,
		claims.UserID, claims.Username, "linked to an existing account", user.ID); err != nil {
		jsonError(c, http.StatusInternalServerError, "the decision could not be recorded")
		return
	}

	// The linked account's live sessions are closed and its access tokens
	// invalidated: the account has just become one that signs in through a provider,
	// and every credential issued before that statement is a credential nobody can
	// say who held.
	h.JWT.BumpUserGen(user.ID)
	dropped, _ := h.St.Sessions.RevokeAllFor(user.ID)

	h.audit(c, models.AuditIdentityLink, http.StatusOK, AuditTarget{Type: "user", ID: user.ID},
		map[string]any{
			"identity":              row.ID,
			"provider":              row.Provider,
			"subject":               row.ProviderSubject,
			"email":                 row.Email,
			"live_sessions_dropped": dropped,
			"access_tokens_expired": true,
		})
	c.JSON(http.StatusOK, gin.H{
		"identity":              identityView(row),
		"user":                  h.view(user),
		"live_sessions_dropped": dropped,
	})
}

// RefuseIdentity closes the queue row without creating or touching an account.
//
// POST /api/identities/:id/refuse
//
// The reason is required, not optional: "refused" without a reason is a decision
// nobody can defend later, and the person it refused is the one who will ask.
func (h *Handlers) RefuseIdentity(c *gin.Context) {
	row, status, msg := h.identityDecision(c)
	if msg != "" {
		h.audit(c, models.AuditIdentityRefuse, status, AuditTarget{Type: "identity", ID: c.Param("id")},
			map[string]any{"reason": msg})
		jsonError(c, status, msg)
		return
	}

	var req struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Reason) == "" {
		jsonError(c, http.StatusBadRequest, "reason is required")
		return
	}

	claims := ClaimsFrom(c)
	if err := h.St.PendingIdentities.MarkDecided(row.ID, models.IdentityStatusRefused,
		claims.UserID, claims.Username, strings.TrimSpace(req.Reason), ""); err != nil {
		jsonError(c, http.StatusInternalServerError, "the decision could not be recorded")
		return
	}

	h.audit(c, models.AuditIdentityRefuse, http.StatusOK, AuditTarget{Type: "identity", ID: row.ID},
		map[string]any{
			"provider": row.Provider,
			"subject":  row.ProviderSubject,
			"email":    row.Email,
			"reason":   strings.TrimSpace(req.Reason),
		})
	c.JSON(http.StatusOK, gin.H{"identity": identityView(row)})
}
