package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"nvr/server/auth"
	"nvr/server/models"
)

// The local login surface. Every answer here is a shape the console draws
// buttons from and `scripts/verify.sh` gates on, so the bodies are the contract.
//
// Two rules run through this file:
//
//   - The login screen's buttons come from **rows**. `local_login` is the answer
//     to "does any account here authenticate with a password", which is a fact
//     about the accounts an operator has made, not about whether
//     `BOOTSTRAP_ADMIN_PASSWORD` was set at some boot. An installation that
//     deleted its break-glass admin has no password form, and the login page
//     must say so.
//   - A failed login is a ledger row and a single indistinguishable 401. Which
//     half was wrong is in the ledger (for the operator), not in the response
//     (for the person doing the guessing).

// AuthStatus answers what the login screen may draw.
//
// GET /api/auth/status
func (h *Handlers) AuthStatus(c *gin.Context) {
	local, err := h.St.Users.CountLocalLogin()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "accounts are unreadable")
		return
	}
	providers, err := h.St.SSOProviders.ListEnabled()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "providers are unreadable")
		return
	}

	buttons := []gin.H{}
	for _, p := range providers {
		if p.Type != models.SSOTypeOIDC {
			continue
		}
		buttons = append(buttons, gin.H{
			"provider": p.Name,
			"name":     p.Name,
			"issuer":   p.Issuer,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"local_login":  local > 0,
		"oidc":         buttons,
		"registration": false,
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login is the local (break-glass) path, and the only path that reads a
// password.
//
// POST /api/auth/login — behind the rate limiter.
func (h *Handlers) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "username and password are required")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		jsonError(c, http.StatusBadRequest, "username and password are required")
		return
	}

	failed := func(actor models.AuditActor, reason string, status int, msg string) {
		h.auditAs(c, actor, models.AuditAuthLoginFailed, status,
			AuditTarget{Type: "user", ID: actor.UserID}, map[string]any{"reason": reason})
		jsonError(c, status, msg)
	}

	user, err := h.St.Users.GetByUsername(req.Username)
	if err != nil {
		// The username is the only thing we have, and it is not a row. The
		// ledger records the attempt against the name, because "who is hammering
		// this username" is a question an operator asks.
		failed(models.AuditActor{Username: req.Username}, "no_such_account", http.StatusUnauthorized, "invalid credentials")
		return
	}
	if user.IsPending() {
		failed(actorOf(user), "account_pending", http.StatusUnauthorized, "invalid credentials")
		return
	}
	if !user.IsLocal() {
		// Distinct on purpose: an account that signs in through a provider has no
		// password to check, and the console needs to offer the provider's button
		// instead. This names the account's shape, not its existence — the login
		// page already lists the providers, and the username is the person's own
		// input.
		failed(actorOf(user), "provider_account", http.StatusForbidden, "this account signs in through its provider")
		return
	}

	ok, err := auth.VerifyPassword(req.Password, user.PasswordHash)
	if err != nil || !ok {
		failed(actorOf(user), "password_mismatch", http.StatusUnauthorized, "invalid credentials")
		return
	}

	roles := h.roleNames(user)
	access, err := h.JWT.IssueNamed(user.ID, user.Username, roles, models.AccessTokenTTL)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "session tokens could not be issued")
		return
	}
	refresh, err := h.JWT.IssueRefresh(user.ID, user.Username, roles, models.RefreshTokenTTL)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "session tokens could not be issued")
		return
	}
	session, err := h.openSession(c, user, refresh)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the session could not be recorded")
		return
	}
	_ = h.St.Users.MarkLogin(user.ID, h.now().UTC())

	h.auditAs(c, actorOf(user), models.AuditAuthLogin, http.StatusOK,
		AuditTarget{Type: "user", ID: user.ID}, map[string]any{
			"method":   "password",
			"roles":    roles,
			"session":  session.ID,
			"provider": user.Provider,
		})

	c.JSON(http.StatusOK, gin.H{
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "bearer",
		"expires_in":    int(models.AccessTokenTTL.Seconds()),
		"user":          h.view(user),
	})
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Refresh rotates a refresh token: the presented one is closed, its replacement
// names it in `rotated_from`, and the old access token is denied.
//
// POST /api/auth/refresh
//
// Reuse detection is the reason this route answers in names. A presented token
// whose row is already closed is a copy, so every live session of that account is
// dropped — the count is in the response, because it is the fact the operator
// needs ("your sessions were dropped, sign in again", and how many).
func (h *Handlers) Refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.RefreshToken) == "" {
		jsonError(c, http.StatusBadRequest, "refresh_token is required")
		return
	}

	rot, err := h.JWT.RotateRefresh(h.St.Sessions, req.RefreshToken)
	if errors.Is(err, auth.ErrRefreshReused) {
		revoked := int64(0)
		if rot != nil {
			revoked = rot.RevokedAll
		}
		h.audit(c, models.AuditAuthRefresh, http.StatusUnauthorized,
			AuditTarget{Type: "session", ID: ""}, map[string]any{
				"reason":                "refresh_token_reused",
				"live_sessions_dropped": revoked,
			})
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":                 "refresh token was already used",
			"live_sessions_dropped": revoked,
		})
		return
	}
	if err != nil {
		reason := "refresh token is not valid"
		switch {
		case errors.Is(err, auth.ErrRefreshUnknown):
			reason = "refresh token is not a known session"
		case errors.Is(err, auth.ErrTokenExpired):
			reason = "refresh token has expired"
		case errors.Is(err, auth.ErrNotRefreshToken):
			reason = "this is an access token, not a refresh token"
		}
		h.audit(c, models.AuditAuthRefresh, http.StatusUnauthorized,
			AuditTarget{Type: "session", ID: ""}, map[string]any{"reason": reason})
		jsonError(c, http.StatusUnauthorized, reason)
		return
	}

	h.audit(c, models.AuditAuthRefresh, http.StatusOK,
		AuditTarget{Type: "session", ID: rot.Session.ID}, map[string]any{
			"rotated_from": rot.Session.RotatedFrom,
			"user_id":      rot.Session.UserID,
		})

	c.JSON(http.StatusOK, gin.H{
		"access_token":  rot.Access,
		"refresh_token": rot.Refresh,
		"token_type":    "bearer",
		"expires_in":    int(models.AccessTokenTTL.Seconds()),
	})
}

// Logout ends this session: the access token is denied by its id until it would
// have expired, and the refresh token's row is closed.
//
// POST /api/auth/logout
//
// With no `refresh_token` in the body it means "log me out everywhere": the
// account's live sessions are all closed. A console that logs out of a tab logs
// out the tab; the account owner who thinks their password leaked logs out
// everything.
func (h *Handlers) Logout(c *gin.Context) {
	claims := ClaimsFrom(c)
	if claims == nil {
		jsonError(c, http.StatusUnauthorized, "not authenticated")
		return
	}
	h.JWT.Deny(claims.ID, claims.ExpiresAt)

	var req refreshRequest
	// An empty body is the "everywhere" case, not a malformed request.
	_ = c.ShouldBindJSON(&req)

	dropped := int64(0)
	target := AuditTarget{Type: "session", ID: ""}
	if token := strings.TrimSpace(req.RefreshToken); token != "" {
		if session, err := h.St.Sessions.GetByTokenHash(models.HashRefreshToken(token)); err == nil && session != nil {
			if session.UserID == claims.UserID {
				_ = h.St.Sessions.Revoke(session.ID)
				dropped = 1
				target = AuditTarget{Type: "session", ID: session.ID}
			}
		}
	} else {
		dropped, _ = h.St.Sessions.RevokeAllFor(claims.UserID)
	}

	h.audit(c, models.AuditAuthLogout, http.StatusOK, target, map[string]any{
		"live_sessions_dropped": dropped,
		"access_token_denied":   true,
	})
	c.JSON(http.StatusOK, gin.H{"status": "logged out", "live_sessions_dropped": dropped})
}

// Profile is the account the bearer token names, roles included — the route the
// console reads to decide which tabs to draw.
//
// GET /api/auth/profile
func (h *Handlers) Profile(c *gin.Context) {
	claims := ClaimsFrom(c)
	if claims == nil {
		jsonError(c, http.StatusUnauthorized, "not authenticated")
		return
	}
	user, err := h.St.Users.GetByID(claims.UserID)
	if err != nil {
		// The token is valid and the account is not: someone deleted it under a
		// live session. The answer is 401, and it says why.
		jsonError(c, http.StatusUnauthorized, "this account no longer exists")
		return
	}
	if user.IsPending() {
		jsonError(c, http.StatusForbidden, "this account is waiting for an administrator's decision")
		return
	}
	c.JSON(http.StatusOK, h.view(user))
}

// MinPasswordLen is the floor the password form enforces. It is stated in the
// refusal, because a console that refuses a password without saying why teaches
// the operator nothing.
const MinPasswordLen = 12

type passwordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword changes the local account's password and ends every session it
// has.
//
// PUT /api/auth/password
//
// Both halves of "the old tokens must stop working" are done here: the token
// generation is bumped (so every access token issued before this call stops
// verifying, for as long as it would have lived), and the account's live sessions
// are closed (so no refresh token can quietly mint a new one). Changing a password
// because a device was lost is exactly the moment a half-measure costs an access.
func (h *Handlers) ChangePassword(c *gin.Context) {
	claims := ClaimsFrom(c)
	if claims == nil {
		jsonError(c, http.StatusUnauthorized, "not authenticated")
		return
	}
	var req passwordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "current_password and new_password are required")
		return
	}

	user, err := h.St.Users.GetByID(claims.UserID)
	if err != nil {
		jsonError(c, http.StatusUnauthorized, "this account no longer exists")
		return
	}
	if !user.IsLocal() {
		// An OIDC account's password lives at the provider. Handing this route a
		// password would create a second way into an account whose first way is an
		// IdP, and the operator who configured the IdP is the person who must do
		// that.
		jsonError(c, http.StatusForbidden, "this account signs in through its provider")
		return
	}

	ok, err := auth.VerifyPassword(req.CurrentPassword, user.PasswordHash)
	if err != nil || !ok {
		h.audit(c, models.AuditAuthPasswordChange, http.StatusUnauthorized,
			AuditTarget{Type: "user", ID: user.ID}, map[string]any{"reason": "current_password_mismatch"})
		jsonError(c, http.StatusUnauthorized, "current password is not correct")
		return
	}

	newPassword := strings.TrimSpace(req.NewPassword)
	if len(newPassword) < MinPasswordLen {
		jsonError(c, http.StatusUnprocessableEntity,
			"new password must be at least "+itoa(MinPasswordLen)+" characters")
		return
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, auth.ErrPasswordTooLong) {
			status = http.StatusUnprocessableEntity
		}
		jsonError(c, status, err.Error())
		return
	}

	if err := h.St.Users.SetPassword(user.ID, hash); err != nil {
		jsonError(c, http.StatusInternalServerError, "the password could not be stored")
		return
	}
	h.JWT.BumpUserGen(user.ID)
	dropped, _ := h.St.Sessions.RevokeAllFor(user.ID)

	h.audit(c, models.AuditAuthPasswordChange, http.StatusOK,
		AuditTarget{Type: "user", ID: user.ID}, map[string]any{
			"live_sessions_dropped": dropped,
			"access_tokens_expired": true,
		})
	c.JSON(http.StatusOK, gin.H{
		"status":                "password changed",
		"live_sessions_dropped": dropped,
		"access_tokens_expired": true,
		"sign_in_required":      true,
	})
}

// RegisterDisabled is the decision, not an omission.
//
// POST /api/auth/register → 403 {"error":"registration is admin-only"}
//
// An NVR records people. v1 has no signup flow, no email verification and no
// invite, so the route exists and answers the refusal — a 404 would read as "this
// deployment forgot to wire something up", and the console would guess.
// Accounts arrive two ways: an admin creates one (`POST /api/users`), or an OIDC
// login provisions one at the viewer floor.
func (h *Handlers) RegisterDisabled(c *gin.Context) {
	h.audit(c, models.AuditAuthRegister, http.StatusForbidden,
		AuditTarget{Type: "route", ID: "/api/auth/register"}, map[string]any{
			"reason": "registration is admin-only in v1: accounts are created by an admin or provisioned by an OIDC login at the viewer floor",
		})
	jsonError(c, http.StatusForbidden, "registration is admin-only")
}
