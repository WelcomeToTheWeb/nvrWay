package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"nvr/server/models"
	"nvr/server/sso"
	"nvr/server/store"
)

// The OIDC provider surface (admin only): the doors people log in through.
//
// Two rules this file carries.
//
//   - **Discovery runs at save time.** `sso/discovery.go` says it, and the reason
//     is the failure this surface exists to prevent: a provider row saved from a
//     half-discovered issuer is a login that fails at the worst possible moment,
//     with a 500 whose cause lives on someone else's server. Saving is the moment
//     the operator is standing in front of the IdP's docs, so it is the moment the
//     issuer is asked. A refusal is a 422 carrying the named discovery state — the
//     console shows `discovery_unreachable` and `issuer_mismatch` as different
//     states because they are different fixes.
//   - **The redirect URI has one author.** The server computes it
//     (`SSORedirectURI`), stores it, and answers it on every read, because the
//     string is the thing the operator whitelists in the IdP. An operator-typed
//     redirect URI is how `redirect_uri mismatch` becomes a support ticket.
//
// The client secret is write-only: the model carries it `json:"-"`, so no read
// echoes it, and the list answers `secret_set`.

// registerOIDCRoutes mounts the admin-only provider surface.
func (h *Handlers) registerOIDCRoutes(admin *gin.RouterGroup) {
	admin.GET("/settings/oidc", h.ListOIDCProviders)
	admin.POST("/settings/oidc", h.CreateOIDCProvider)
	admin.PUT("/settings/oidc/:provider", h.UpdateOIDCProvider)
	admin.DELETE("/settings/oidc/:provider", h.DeleteOIDCProvider)
}

// oidcProviderView is a provider as the admin screen reads it. No secret is in
// it, and `secret_set` is all the API concedes about one.
type oidcProviderView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Issuer         string `json:"issuer"`
	DiscoveryURL   string `json:"discovery_url"`
	ClientID       string `json:"client_id"`
	SecretSet      bool   `json:"secret_set"`
	Scope          string `json:"scope"`
	Enabled        bool   `json:"enabled"`
	RedirectURI    string `json:"redirect_uri"`
	AuthURL        string `json:"auth_url"`
	TokenURL       string `json:"token_url"`
	UserInfoURL    string `json:"user_info_url"`
	JwksURI        string `json:"jwks_uri"`
	DiscoveryState string `json:"discovery_state"`
	PKCESupported  bool   `json:"pkce_supported"`
	DiscoveredAt   string `json:"discovered_at"`
	LinkPolicy     string `json:"link_policy"`
	AllowedDomains string `json:"allowed_domains"`

	ClaimsEmail    string `json:"claims_email"`
	ClaimsUsername string `json:"claims_username"`
	ClaimsName     string `json:"claims_name"`
	ClaimsGroups   string `json:"claims_groups"`

	RequireVerifiedEmail bool `json:"require_verified_email"`
	SortOrder            int  `json:"sort_order"`
}

func (h *Handlers) oidcProviderView(p *models.SSOProvider) oidcProviderView {
	discoveredAt := ""
	if p.DiscoveredAt != nil {
		discoveredAt = p.DiscoveredAt.Format(timeLayout)
	}
	return oidcProviderView{
		ID:                   p.ID,
		Name:                 p.Name,
		Type:                 p.Type,
		Issuer:               p.Issuer,
		DiscoveryURL:         p.DiscoveryURL,
		ClientID:             p.ClientID,
		SecretSet:            p.SecretSet(),
		Scope:                p.Scope,
		Enabled:              p.Enabled,
		RedirectURI:          SSORedirectURI(h.Cfg.WebURL, p.Name),
		AuthURL:              p.AuthURL,
		TokenURL:             p.TokenURL,
		UserInfoURL:          p.UserInfoURL,
		JwksURI:              p.JwksURI,
		DiscoveryState:       p.DiscoveryState,
		PKCESupported:        p.PKCESupported,
		DiscoveredAt:         discoveredAt,
		LinkPolicy:           p.LinkPolicy,
		AllowedDomains:       p.AllowedDomains,
		ClaimsEmail:          p.ClaimsEmail,
		ClaimsUsername:       p.ClaimsUsername,
		ClaimsName:           p.ClaimsName,
		ClaimsGroups:         p.ClaimsGroups,
		RequireVerifiedEmail: p.RequireVerifiedEmail,
		SortOrder:            p.SortOrder,
	}
}

// ListOIDCProviders answers every provider, enabled or not: the admin screen
// manages the switched-off ones too, and the login page has its own list
// (`GET /api/auth/sso/providers`) that answers only the enabled ones.
//
// GET /api/settings/oidc (admin)
func (h *Handlers) ListOIDCProviders(c *gin.Context) {
	providers, err := h.St.SSOProviders.ListAll()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "providers are unreadable")
		return
	}
	out := make([]oidcProviderView, 0, len(providers))
	for i := range providers {
		out = append(out, h.oidcProviderView(&providers[i]))
	}
	h.audit(c, models.AuditOIDCRead, http.StatusOK,
		AuditTarget{Type: "collection", ID: "sso_providers"}, map[string]any{"providers": len(out)})
	c.JSON(http.StatusOK, gin.H{"providers": out})
}

// oidcProviderInput is the write payload, pointers throughout: an omitted field
// keeps its stored value (the same reason as the camera payload).
type oidcProviderInput struct {
	Name                 *string `json:"name"`
	Issuer               *string `json:"issuer"`
	DiscoveryURL         *string `json:"discovery_url"`
	ClientID             *string `json:"client_id"`
	ClientSecret         *string `json:"client_secret"`
	Scope                *string `json:"scope"`
	Enabled              *bool   `json:"enabled"`
	LinkPolicy           *string `json:"link_policy"`
	AllowedDomains       *string `json:"allowed_domains"`
	RequireVerifiedEmail *bool   `json:"require_verified_email"`
	ClaimsEmail          *string `json:"claims_email"`
	ClaimsUsername       *string `json:"claims_username"`
	ClaimsName           *string `json:"claims_name"`
	ClaimsGroups         *string `json:"claims_groups"`
	SortOrder            *int    `json:"sort_order"`
}

// CreateOIDCProvider adds a provider, after the issuer has answered discovery.
//
// POST /api/settings/oidc (admin)
func (h *Handlers) CreateOIDCProvider(c *gin.Context) {
	var in oidcProviderInput
	if err := c.BindJSON(&in); err != nil {
		jsonError(c, http.StatusBadRequest, "the provider body is not JSON this route understands")
		return
	}

	provider := &models.SSOProvider{
		Type:                 models.SSOTypeOIDC,
		Enabled:              true,
		RequireVerifiedEmail: true,
		LinkPolicy:           models.SSOLinkPolicyPending,
		ClaimsGroups:         "groups",
	}
	applyOIDCInput(provider, &in)

	if strings.TrimSpace(provider.Name) == "" {
		jsonError(c, http.StatusUnprocessableEntity, "name is required")
		return
	}
	if !models.ValidSSOLinkPolicy(provider.LinkPolicy) {
		jsonError(c, http.StatusUnprocessableEntity, "link_policy must be pending or link")
		return
	}
	if strings.TrimSpace(provider.Issuer) == "" {
		jsonError(c, http.StatusUnprocessableEntity, "issuer is required")
		return
	}
	if strings.TrimSpace(provider.ClientID) == "" {
		jsonError(c, http.StatusUnprocessableEntity, "client_id is required")
		return
	}
	if strings.TrimSpace(provider.ClientSecret) == "" {
		jsonError(c, http.StatusUnprocessableEntity, "client_secret is required")
		return
	}
	provider.RedirectURI = SSORedirectURI(h.Cfg.WebURL, provider.Name)

	if err := h.applyDiscovery(c.Request.Context(), provider); err != nil {
		h.audit(c, models.AuditOIDCCreate, http.StatusUnprocessableEntity,
			AuditTarget{Type: "provider", ID: provider.Name},
			map[string]any{"issuer": provider.Issuer, "reason": err.Error()})
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":            "provider refused by discovery",
			"discovery":        sso.DiscoveryKindOf(err),
			"discovery_reason": err.Error(),
		})
		return
	}

	if err := h.St.SSOProviders.Create(provider); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			h.audit(c, models.AuditOIDCCreate, http.StatusConflict,
				AuditTarget{Type: "provider", ID: provider.Name}, map[string]any{"name": provider.Name})
			jsonError(c, http.StatusConflict, "provider name taken: "+provider.Name)
			return
		}
		jsonError(c, http.StatusInternalServerError, "the provider row could not be written")
		return
	}

	fresh, err := h.St.SSOProviders.GetByName(provider.Name)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the provider row is unreadable after the write")
		return
	}
	h.audit(c, models.AuditOIDCCreate, http.StatusCreated,
		AuditTarget{Type: "provider", ID: fresh.Name}, map[string]any{
			"issuer":         fresh.Issuer,
			"discovery":      fresh.DiscoveryState,
			"pkce_supported": fresh.PKCESupported,
			"enabled":        fresh.Enabled,
		})
	c.JSON(http.StatusCreated, h.oidcProviderView(fresh))
}

// UpdateOIDCProvider changes a provider, and re-runs discovery: an IdP that moved
// its endpoints, rotated its JWKS host, or stopped publishing the code flow is
// caught at the save, by the person who just changed it.
//
// PUT /api/settings/oidc/:provider (admin)
func (h *Handlers) UpdateOIDCProvider(c *gin.Context) {
	provider, ok := h.oidcProviderByName(c)
	if !ok {
		return
	}
	before := *provider

	var in oidcProviderInput
	if err := c.BindJSON(&in); err != nil {
		jsonError(c, http.StatusBadRequest, "the provider body is not JSON this route understands")
		return
	}
	sealSecret := applyOIDCInput(provider, &in)

	if strings.TrimSpace(provider.Name) == "" {
		jsonError(c, http.StatusUnprocessableEntity, "name is required")
		return
	}
	if !models.ValidSSOLinkPolicy(provider.LinkPolicy) {
		jsonError(c, http.StatusUnprocessableEntity, "link_policy must be pending or link")
		return
	}
	if strings.TrimSpace(provider.Issuer) == "" {
		jsonError(c, http.StatusUnprocessableEntity, "issuer is required")
		return
	}
	provider.RedirectURI = SSORedirectURI(h.Cfg.WebURL, provider.Name)

	if err := h.applyDiscovery(c.Request.Context(), provider); err != nil {
		h.audit(c, models.AuditOIDCUpdate, http.StatusUnprocessableEntity,
			AuditTarget{Type: "provider", ID: provider.Name},
			map[string]any{"issuer": provider.Issuer, "reason": err.Error()})
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":            "provider refused by discovery",
			"discovery":        sso.DiscoveryKindOf(err),
			"discovery_reason": err.Error(),
		})
		return
	}

	if err := h.St.SSOProviders.Update(provider, sealSecret); err != nil {
		if errors.Is(err, store.ErrProviderNotFound) {
			jsonError(c, http.StatusNotFound, "no such provider")
			return
		}
		if strings.Contains(err.Error(), "already exists") {
			jsonError(c, http.StatusConflict, "provider name taken: "+provider.Name)
			return
		}
		jsonError(c, http.StatusInternalServerError, "the provider row could not be written")
		return
	}

	fresh, err := h.St.SSOProviders.GetByID(provider.ID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "the provider row is unreadable after the write")
		return
	}
	h.audit(c, models.AuditOIDCUpdate, http.StatusOK,
		AuditTarget{Type: "provider", ID: fresh.Name}, map[string]any{
			"issuer_before":  before.Issuer,
			"issuer":         fresh.Issuer,
			"enabled_before": before.Enabled,
			"enabled":        fresh.Enabled,
			"discovery":      fresh.DiscoveryState,
			"secret_changed": sealSecret,
		})
	c.JSON(http.StatusOK, h.oidcProviderView(fresh))
}

// DeleteOIDCProvider removes a provider. The accounts it provisioned stay: a
// provider is a door, not the people behind it.
//
// DELETE /api/settings/oidc/:provider (admin)
func (h *Handlers) DeleteOIDCProvider(c *gin.Context) {
	provider, ok := h.oidcProviderByName(c)
	if !ok {
		return
	}

	if err := h.St.SSOProviders.Delete(provider.ID); err != nil {
		if errors.Is(err, store.ErrProviderNotFound) {
			jsonError(c, http.StatusNotFound, "no such provider")
			return
		}
		jsonError(c, http.StatusInternalServerError, "the provider row could not be deleted")
		return
	}
	h.audit(c, models.AuditOIDCDelete, http.StatusOK,
		AuditTarget{Type: "provider", ID: provider.Name}, map[string]any{
			"issuer":              provider.Issuer,
			"login_buttons_after": h.loginButtonCount(c),
		})
	c.JSON(http.StatusOK, gin.H{"deleted": provider.Name, "issuer": provider.Issuer})
}

// applyDiscovery asks the issuer for its configuration and copies the answer onto
// the row. The failure kinds are the console's states, so they travel unchanged.
func (h *Handlers) applyDiscovery(ctx context.Context, provider *models.SSOProvider) error {
	client := &http.Client{Timeout: 10 * time.Second}
	doc, err := sso.Discover(client, provider.Issuer, provider.DiscoveryURL)
	if err != nil {
		provider.DiscoveryState = sso.DiscoveryKindOf(err)
		return err
	}
	provider.AuthURL = doc.AuthorizationEndpoint
	provider.TokenURL = doc.TokenEndpoint
	provider.UserInfoURL = doc.UserInfoEndpoint
	provider.JwksURI = doc.JwksURI
	provider.DiscoveryState = sso.DiscoveryOK
	provider.DiscoveryDoc = doc.Raw
	now := h.now().UTC()
	provider.DiscoveredAt = &now
	for _, method := range doc.PKCEMethods {
		if method == "S256" {
			provider.PKCESupported = true
			break
		}
	}
	return nil
}

func (h *Handlers) oidcProviderByName(c *gin.Context) (*models.SSOProvider, bool) {
	name := strings.TrimSpace(c.Param("provider"))
	provider, err := h.St.SSOProviders.GetByName(name)
	if err != nil {
		if errors.Is(err, store.ErrProviderNotFound) {
			jsonError(c, http.StatusNotFound, "no such provider")
			return nil, false
		}
		jsonError(c, http.StatusInternalServerError, "providers are unreadable")
		return nil, false
	}
	return provider, true
}

func (h *Handlers) loginButtonCount(c *gin.Context) int {
	providers, err := h.St.SSOProviders.ListEnabled()
	if err != nil {
		return 0
	}
	return len(providers)
}

// applyOIDCInput merges the payload onto the row and reports whether a client
// secret arrived (the store seals it only when one was supplied, so a PUT that
// omits the secret keeps the stored one).
func applyOIDCInput(provider *models.SSOProvider, in *oidcProviderInput) bool {
	sealSecret := false
	if in.Name != nil {
		provider.Name = strings.TrimSpace(*in.Name)
	}
	if in.Issuer != nil {
		provider.Issuer = strings.TrimSpace(*in.Issuer)
	}
	if in.DiscoveryURL != nil {
		provider.DiscoveryURL = strings.TrimSpace(*in.DiscoveryURL)
	}
	if in.ClientID != nil {
		provider.ClientID = strings.TrimSpace(*in.ClientID)
	}
	if in.ClientSecret != nil && strings.TrimSpace(*in.ClientSecret) != "" {
		provider.ClientSecret = strings.TrimSpace(*in.ClientSecret)
		sealSecret = true
	}
	if in.Scope != nil {
		provider.Scope = strings.TrimSpace(*in.Scope)
	}
	if in.Enabled != nil {
		provider.Enabled = *in.Enabled
	}
	if in.LinkPolicy != nil {
		provider.LinkPolicy = strings.TrimSpace(*in.LinkPolicy)
	}
	if in.AllowedDomains != nil {
		provider.AllowedDomains = strings.TrimSpace(*in.AllowedDomains)
	}
	if in.RequireVerifiedEmail != nil {
		provider.RequireVerifiedEmail = *in.RequireVerifiedEmail
	}
	if in.ClaimsEmail != nil {
		provider.ClaimsEmail = strings.TrimSpace(*in.ClaimsEmail)
	}
	if in.ClaimsUsername != nil {
		provider.ClaimsUsername = strings.TrimSpace(*in.ClaimsUsername)
	}
	if in.ClaimsName != nil {
		provider.ClaimsName = strings.TrimSpace(*in.ClaimsName)
	}
	if in.ClaimsGroups != nil {
		provider.ClaimsGroups = strings.TrimSpace(*in.ClaimsGroups)
	}
	if in.SortOrder != nil {
		provider.SortOrder = *in.SortOrder
	}
	return sealSecret
}
