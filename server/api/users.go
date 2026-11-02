package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"nvr/server/auth"
	"nvr/server/models"
	"nvr/server/store"
)

// The administrator's surface: who the accounts are, what they are allowed to be,
// and the one thing an admin may not do — leave the installation with no way in.
//
// The rules in this file are the ones that keep an installation manageable by the
// person who is not standing in front of it:
//
//   - **The last admin cannot be deleted, demoted, or parked.** A deployment with
//     no `admin` account has no way to grant a role, approve an identity, or fix a
//     provider row. The break-glass admin is the door the operator's IdP cannot
//     close, so closing it is the one act the API refuses.
//   - **A role change ends the sessions it changes.** Roles are read from the rows,
//     so a gate reads live authority — but a live refresh token mints fresh access
//     tokens, and a token minted after a demotion carries the old roles. Revoking on
//     write is what makes "I removed their access" mean *now*.
//   - **A role mapping may not map to `viewer`.** A mapping owns the roles it names:
//     on every login it replaces them. `viewer` is the floor every new identity
//     arrives on; a mapping that owns the floor decides that the technician who
//     loses the group has no roles at all, and nobody can log in to fix it.

// registerUserRoutes mounts the admin surface.
func (h *Handlers) registerUserRoutes(admin *gin.RouterGroup) {
	admin.GET("/users", h.ListUsers)
	admin.POST("/users", h.CreateUser)
	admin.GET("/users/:id", h.GetUser)
	admin.PUT("/users/:id", h.UpdateUser)
	admin.DELETE("/users/:id", h.DeleteUser)
	admin.GET("/users/:id/roles", h.GetUserRoles)
	admin.POST("/users/:id/roles", h.SetUserRoles)

	admin.GET("/roles", h.ListRoles)

	admin.PUT("/settings/oidc/:provider/role-mapping", h.PutRoleMapping)
}

// userDetailView is the account as the admin surface answers it: the profile
// shape, plus the facts an admin acts on — the live sessions, and whether the
// account can be reached with a password at all.
type userDetailView struct {
	userView
	PasswordLogin bool          `json:"password_login"`
	Sessions      []sessionView `json:"sessions"`
	SessionsTotal int64         `json:"live_sessions"`
}

type sessionView struct {
	ID        string `json:"id"`
	SourceIP  string `json:"source_ip"`
	UserAgent string `json:"user_agent"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

func (h *Handlers) sessionsFor(userID string) ([]sessionView, int64) {
	live, err := h.St.Sessions.ListLiveFor(userID, h.now().UTC())
	if err != nil {
		return []sessionView{}, 0
	}
	out := make([]sessionView, 0, len(live))
	for _, s := range live {
		out = append(out, sessionView{
			ID:        s.ID,
			SourceIP:  s.SourceIP,
			UserAgent: s.UserAgent,
			CreatedAt: s.CreatedAt.Format(timeLayout),
			ExpiresAt: s.ExpiresAt.Format(timeLayout),
		})
	}
	return out, int64(len(out))
}

// timeLayout is the shape every timestamp in this surface is answered in: RFC3339
// with milliseconds, so a console can render it and a log line can be compared to
// a ledger row without a second format.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// ListUsers is the account list, filtered the ways an operator asks.
//
// GET /api/users?provider=&role=&status=&q=&limit=
func (h *Handlers) ListUsers(c *gin.Context) {
	provider := strings.TrimSpace(c.Query("provider"))
	role := strings.TrimSpace(c.Query("role"))
	status := strings.TrimSpace(c.Query("status"))
	needle := strings.ToLower(strings.TrimSpace(c.Query("q")))

	users, err := h.St.Users.ListAll()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "accounts are unreadable")
		return
	}

	limit := 0
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			jsonError(c, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = n
	}

	out := []userView{}
	for _, u := range users {
		if provider != "" && u.Provider != provider {
			continue
		}
		if status != "" && u.Status != status {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(u.Username+" "+u.Name+" "+u.Email), needle) {
			continue
		}
		view := h.view(&u)
		if role != "" && !containsString(view.Roles, role) {
			continue
		}
		out = append(out, view)
		if limit > 0 && len(out) >= limit {
			break
		}
	}

	h.audit(c, models.AuditUserRead, http.StatusOK,
		AuditTarget{Type: "collection", ID: "users"}, map[string]any{"count": len(out)})
	c.JSON(http.StatusOK, gin.H{"users": out})
}

type createUserRequest struct {
	Username string   `json:"username"`
	Password string   `json:"password"`
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Roles    []string `json:"roles"`
}

// CreateUser makes an account by hand — the only way an account exists besides an
// OIDC login provisioning one at the viewer floor.
//
// POST /api/users
func (h *Handlers) CreateUser(c *gin.Context) {
	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "username and password are required")
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		jsonError(c, http.StatusBadRequest, "username is required")
		return
	}
	if len(strings.TrimSpace(req.Password)) < MinPasswordLen {
		jsonError(c, http.StatusUnprocessableEntity,
			"password must be at least "+itoa(MinPasswordLen)+" characters")
		return
	}
	roles, status, msg := h.validRoles(req.Roles)
	if msg != "" {
		jsonError(c, status, msg)
		return
	}

	hash, err := auth.HashPassword(strings.TrimSpace(req.Password))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, auth.ErrPasswordTooLong) {
			status = http.StatusUnprocessableEntity
		}
		jsonError(c, status, err.Error())
		return
	}

	user := &models.User{
		Username:     username,
		Name:         strings.TrimSpace(req.Name),
		Email:        strings.TrimSpace(req.Email),
		Provider:     models.ProviderLocal,
		Status:       models.UserStatusActive,
		PasswordHash: hash,
	}
	if err := h.St.Users.Create(user); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			h.audit(c, models.AuditUserCreate, http.StatusConflict,
				AuditTarget{Type: "user", ID: ""}, map[string]any{"reason": "username taken", "username": username})
			jsonError(c, http.StatusConflict, "that username is taken")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the account could not be created")
		return
	}
	if len(roles) == 0 {
		roles = []string{models.RoleViewer}
	}
	if err := h.St.Users.SetRoles(user.ID, roles); err != nil {
		jsonError(c, http.StatusInternalServerError, "the roles could not be assigned")
		return
	}
	user.Roles = rolesFor(roles)

	h.audit(c, models.AuditUserCreate, http.StatusOK, AuditTarget{Type: "user", ID: user.ID},
		map[string]any{"username": user.Username, "roles": roles, "provider": models.ProviderLocal})
	c.JSON(http.StatusCreated, h.view(user))
}

// GetUser is the account, its live sessions, and the door it can be reached by.
//
// GET /api/users/:id
func (h *Handlers) GetUser(c *gin.Context) {
	user, status, msg := h.userParam(c)
	if msg != "" {
		jsonError(c, status, msg)
		return
	}
	sessions, live := h.sessionsFor(user.ID)
	h.audit(c, models.AuditUserRead, http.StatusOK, AuditTarget{Type: "user", ID: user.ID}, nil)
	c.JSON(http.StatusOK, userDetailView{
		userView:      h.view(user),
		PasswordLogin: user.IsLocal() && user.PasswordHash != "",
		Sessions:      sessions,
		SessionsTotal: live,
	})
}

type updateUserRequest struct {
	Username *string `json:"username"`
	Name     *string `json:"name"`
	Email    *string `json:"email"`
	Status   *string `json:"status"`
	Password *string `json:"password"`
}

// UpdateUser changes the account, and ends the sessions a password change ends.
//
// PUT /api/users/:id
func (h *Handlers) UpdateUser(c *gin.Context) {
	claims := ClaimsFrom(c)
	user, status, msg := h.userParam(c)
	if msg != "" {
		jsonError(c, status, msg)
		return
	}

	var req updateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "the request body is not readable")
		return
	}

	changed := map[string]any{}
	dropped := int64(0)

	if req.Password != nil {
		password := strings.TrimSpace(*req.Password)
		if len(password) < MinPasswordLen {
			jsonError(c, http.StatusUnprocessableEntity,
				"password must be at least "+itoa(MinPasswordLen)+" characters")
			return
		}
		if !user.IsLocal() {
			// The same rule as the account owner's own password route: a provider
			// account's password lives at the IdP, and an admin setting one here
			// would be creating the second door from the administrator's side.
			jsonError(c, http.StatusForbidden, "this account signs in through its provider")
			return
		}
		hash, err := auth.HashPassword(password)
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
		dropped, _ = h.St.Sessions.RevokeAllFor(user.ID)
		changed["password_reset"] = true
		changed["live_sessions_dropped"] = dropped
	}

	if req.Username != nil {
		username := strings.TrimSpace(*req.Username)
		if username == "" {
			jsonError(c, http.StatusUnprocessableEntity, "username is required")
			return
		}
		if username != user.Username {
			user.Username = username
			changed["username"] = username
		}
	}
	if req.Name != nil {
		user.Name = strings.TrimSpace(*req.Name)
		changed["name"] = user.Name
	}
	if req.Email != nil {
		user.Email = strings.TrimSpace(*req.Email)
		changed["email"] = user.Email
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if status != models.UserStatusActive && status != models.UserStatusPending {
			jsonError(c, http.StatusUnprocessableEntity,
				`status is "active" or "pending"`)
			return
		}
		if status != user.Status {
			// Parking the last admin is the same lockout as deleting them: a
			// pending account has no roles and no login, so the installation has
			// nobody left who can fix the provider row.
			if userHasRole(user, models.RoleAdmin) && status == models.UserStatusPending {
				if msg := h.lastAdminRefusal(user); msg != "" {
					h.audit(c, models.AuditUserUpdate, http.StatusConflict,
						AuditTarget{Type: "user", ID: user.ID}, map[string]any{"reason": msg})
					jsonError(c, http.StatusConflict, msg)
					return
				}
			}
			user.Status = status
			changed["status"] = status
			// A parked account must not keep live sessions: its refresh token
			// would keep minting access tokens for an account that is refused at
			// every route.
			if status == models.UserStatusPending {
				h.JWT.BumpUserGen(user.ID)
				dropped, _ = h.St.Sessions.RevokeAllFor(user.ID)
				changed["live_sessions_dropped"] = dropped
			}
		}
	}

	if len(changed) == 0 {
		jsonError(c, http.StatusBadRequest, "nothing to change was named")
		return
	}

	if err := h.St.Users.Update(user); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			jsonError(c, http.StatusConflict, "that username is taken")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the account could not be stored")
		return
	}

	h.auditAs(c, models.AuditActor{
		UserID:   claims.UserID,
		Username: claims.Username,
		Roles:    strings.Join(claims.Roles, ","),
	}, models.AuditUserUpdate, http.StatusOK, AuditTarget{Type: "user", ID: user.ID}, changed)

	// The response carries what the write ended, not just what it changed: an admin
	// who resets a password is entitled to see that the account's sessions went with
	// it, because "did that take effect?" is the question they are asking.
	c.JSON(http.StatusOK, gin.H{
		"user":                  h.view(user),
		"live_sessions_dropped": dropped,
		"sign_in_required":      dropped > 0,
	})
}

// DeleteUser removes an account, its role grants and its sessions.
//
// DELETE /api/users/:id
func (h *Handlers) DeleteUser(c *gin.Context) {
	user, status, msg := h.userParam(c)
	if msg != "" {
		jsonError(c, status, msg)
		return
	}
	if msg := h.lastAdminRefusal(user); msg != "" {
		h.audit(c, models.AuditUserDelete, http.StatusConflict,
			AuditTarget{Type: "user", ID: user.ID}, map[string]any{"reason": msg, "username": user.Username})
		jsonError(c, http.StatusConflict, msg)
		return
	}

	h.JWT.BumpUserGen(user.ID)
	if err := h.St.Users.Delete(user.ID); err != nil {
		jsonError(c, http.StatusInternalServerError, "the account could not be deleted")
		return
	}

	h.audit(c, models.AuditUserDelete, http.StatusOK, AuditTarget{Type: "user", ID: user.ID},
		map[string]any{"username": user.Username, "provider": user.Provider, "access_tokens_expired": true})
	c.JSON(http.StatusOK, gin.H{"status": "deleted", "id": user.ID})
}

// GetUserRoles answers the account's roles as the rows say them.
//
// GET /api/users/:id/roles
func (h *Handlers) GetUserRoles(c *gin.Context) {
	user, status, msg := h.userParam(c)
	if msg != "" {
		jsonError(c, status, msg)
		return
	}
	roles, err := h.St.Users.RolesFor(user.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "roles are unreadable")
		return
	}
	h.audit(c, models.AuditRoleRead, http.StatusOK, AuditTarget{Type: "user", ID: user.ID},
		map[string]any{"roles": roles})
	c.JSON(http.StatusOK, gin.H{"user_id": user.ID, "roles": roles})
}

type setRolesRequest struct {
	Roles []string `json:"roles"`
}

// SetUserRoles is the act of naming what a person may do.
//
// POST /api/users/:id/roles
//
// It ends the account's sessions. The gate reads the role rows, so a demotion takes
// effect on the account's next request — and the account's refresh token is live for
// as long as it is allowed to mint access tokens. Revoking on write is the difference
// between "removed" and "removed when they next ask".
func (h *Handlers) SetUserRoles(c *gin.Context) {
	user, status, msg := h.userParam(c)
	if msg != "" {
		jsonError(c, status, msg)
		return
	}

	var req setRolesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "roles are required")
		return
	}
	roles, status, msg := h.validRoles(req.Roles)
	if msg != "" {
		jsonError(c, status, msg)
		return
	}

	// The last admin cannot be demoted to nothing, and cannot be demoted at all
	// when no other admin exists: the same lockout as deleting them, by the same
	// reason.
	if userHasRole(user, models.RoleAdmin) && !containsString(roles, models.RoleAdmin) {
		if msg := h.lastAdminRefusal(user); msg != "" {
			h.audit(c, models.AuditUserRoleChange, http.StatusConflict,
				AuditTarget{Type: "user", ID: user.ID}, map[string]any{
					"reason": msg, "from": h.roleNames(user), "to": roles,
				})
			jsonError(c, http.StatusConflict, msg)
			return
		}
	}

	from := h.roleNames(user)
	if err := h.St.Users.SetRoles(user.ID, roles); err != nil {
		jsonError(c, http.StatusInternalServerError, "the roles could not be stored")
		return
	}
	h.JWT.BumpUserGen(user.ID)
	dropped, _ := h.St.Sessions.RevokeAllFor(user.ID)

	h.audit(c, models.AuditUserRoleChange, http.StatusOK, AuditTarget{Type: "user", ID: user.ID},
		map[string]any{
			"username":              user.Username,
			"from":                  from,
			"to":                    roles,
			"live_sessions_dropped": dropped,
			"access_tokens_expired": true,
		})
	c.JSON(http.StatusOK, gin.H{
		"user_id":               user.ID,
		"roles":                 roles,
		"live_sessions_dropped": dropped,
		"sign_in_required":      true,
	})
}

// ListRoles is the role table, permissions included: the console draws the role
// checkboxes from it, and a permission gate's authority is its rows.
//
// GET /api/roles
func (h *Handlers) ListRoles(c *gin.Context) {
	roles, err := h.St.Roles.ListRoles()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "roles are unreadable")
		return
	}
	out := []gin.H{}
	for _, role := range roles {
		out = append(out, gin.H{
			"id":          role.ID,
			"name":        role.Name,
			"description": role.Description,
			"permissions": role.Permissions,
			"builtin":     role.IsBuiltin,
		})
	}
	h.audit(c, models.AuditRoleRead, http.StatusOK,
		AuditTarget{Type: "collection", ID: "roles"}, map[string]any{"count": len(out)})
	c.JSON(http.StatusOK, gin.H{"roles": out})
}

// PutRoleMapping writes a provider's role mapping, and refuses the mapping that
// would take the floor away.
//
// PUT /api/settings/oidc/:provider/role-mapping
//
// A mapping owns the roles it names on **every** login: it grants them, and it
// removes the ones it does not name. That is the point of a mapping, and it is the
// reason `viewer` may not appear in one. `viewer` is where every identity arrives;
// a mapping that owns the floor decides that a person in no mapped group has no
// roles at all — and the person who has to fix that is the admin the mapping just
// locked out.
func (h *Handlers) PutRoleMapping(c *gin.Context) {
	name := strings.TrimSpace(c.Param("provider"))
	provider, err := h.St.SSOProviders.GetByName(name)
	if err != nil {
		if errors.Is(err, store.ErrProviderNotFound) {
			jsonError(c, http.StatusNotFound, "no such provider")
			return
		}
		jsonError(c, http.StatusInternalServerError, "providers are unreadable")
		return
	}

	var req struct {
		RoleMapping json.RawMessage `json:"role_mapping"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "role_mapping is required")
		return
	}
	mapping := map[string]string{}
	if len(req.RoleMapping) > 0 {
		if err := json.Unmarshal(req.RoleMapping, &mapping); err != nil {
			jsonError(c, http.StatusUnprocessableEntity,
				`role_mapping is an object of "claim value": "role name"`)
			return
		}
	}
	for claim, role := range mapping {
		claim, role = strings.TrimSpace(claim), strings.TrimSpace(role)
		if claim == "" || role == "" {
			delete(mapping, claim)
			continue
		}
		mapping[claim] = role
	}
	if len(mapping) > 0 {
		for _, role := range mapping {
			if role == models.RoleViewer {
				h.audit(c, models.AuditOIDCUpdate, http.StatusUnprocessableEntity,
					AuditTarget{Type: "provider", ID: provider.Name},
					map[string]any{"reason": "role mapping names the viewer floor"})
				jsonError(c, http.StatusUnprocessableEntity,
					"a role mapping may not map to "+models.RoleViewer+
						": a mapping replaces roles on every login, and the floor is the only thing an identity arrives with")
				return
			}
			if _, err := h.St.Roles.GetRoleByName(role); err != nil {
				h.audit(c, models.AuditOIDCUpdate, http.StatusUnprocessableEntity,
					AuditTarget{Type: "provider", ID: provider.Name},
					map[string]any{"reason": "no such role", "role": role})
				jsonError(c, http.StatusUnprocessableEntity, "no such role: "+role)
				return
			}
		}
	}

	encoded, err := json.Marshal(mapping)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the mapping could not be stored")
		return
	}
	provider.RoleMapping = string(encoded)
	if err := h.St.SSOProviders.Update(provider, false); err != nil {
		jsonError(c, http.StatusInternalServerError, "the provider could not be stored")
		return
	}

	h.audit(c, models.AuditOIDCUpdate, http.StatusOK, AuditTarget{Type: "provider", ID: provider.Name},
		map[string]any{"role_mapping": mapping})
	c.JSON(http.StatusOK, gin.H{"provider": provider.Name, "role_mapping": mapping})
}

// --- shared helpers ----------------------------------------------------------

// userParam resolves the :id the admin routes point at.
func (h *Handlers) userParam(c *gin.Context) (*models.User, int, string) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		return nil, http.StatusBadRequest, "user id is required"
	}
	user, err := h.St.Users.GetByID(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, http.StatusNotFound, "no such account"
		}
		return nil, http.StatusInternalServerError, "accounts are unreadable"
	}
	return user, 0, ""
}

// lastAdminRefusal answers with the sentence a UI must show, when `user` is the
// installation's last admin.
//
// "Last" is read from the rows — accounts holding the `admin` role — at the moment
// of the write, which is the only moment the answer means anything.
func (h *Handlers) lastAdminRefusal(user *models.User) string {
	if !userHasRole(user, models.RoleAdmin) {
		return ""
	}
	count, err := h.St.Users.CountAdmins()
	if err != nil {
		// The refusal is the safe answer when the count is unreadable: an API that
		// guessed here would delete the last admin on a database hiccup.
		return "the admin accounts are unreadable, and removing an admin is refused until they are readable"
	}
	if count <= 1 {
		return "this is the last admin account: an installation with no admin has no way to grant a role, approve an identity, or fix its provider rows"
	}
	return ""
}

// validRoles validates a role list against the role rows. A name that is not a role
// is refused by name, because "roles: [operaotr]" answered with a silent viewer
// account is a privilege question nobody can answer later.
func (h *Handlers) validRoles(roles []string) ([]string, int, string) {
	seen := map[string]bool{}
	out := []string{}
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" || seen[role] {
			continue
		}
		if _, err := h.St.Roles.GetRoleByName(role); err != nil {
			return nil, http.StatusUnprocessableEntity, "no such role: " + role
		}
		seen[role] = true
		out = append(out, role)
	}
	return out, 0, ""
}

func rolesFor(names []string) []models.Role {
	out := make([]models.Role, 0, len(names))
	for _, name := range names {
		out = append(out, models.Role{Name: name})
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// userHasRole reads the role set the store loaded with the account. The store
// preloads roles on every account fetch, so this is a list scan, not a query.
func userHasRole(u *models.User, want string) bool {
	for _, role := range u.Roles {
		if role.Name == want {
			return true
		}
	}
	return false
}
