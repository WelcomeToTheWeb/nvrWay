package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"nvr/server/models"
)

// SSOProviderStore provides persistence for OIDC providers.
type SSOProviderStore struct {
	db *gorm.DB
}

// ErrProviderNotFound is the named provider lookup failure.
var ErrProviderNotFound = errors.New("sso: provider not found")

// Create inserts a provider.
func (s *SSOProviderStore) Create(p *models.SSOProvider) error {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if p.Type == "" {
		p.Type = models.SSOTypeOIDC
	}
	if p.LinkPolicy == "" {
		p.LinkPolicy = models.SSOLinkPolicyPending
	}
	enabled, requireVerified := p.Enabled, p.RequireVerifiedEmail
	if err := s.db.Create(p).Error; err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("sso: provider name %q already exists", p.Name)
		}
		return err
	}
	// Two columns default to true in the schema and can be configured false: a
	// provider saved disabled, and a provider whose IdP does not assert
	// email_verified (the strict gate would lock everyone out). gorm fills both
	// defaults into the struct on insert, so the values wanted are captured
	// above and written back.
	zero := map[string]any{}
	if !enabled {
		p.Enabled = false
		zero["enabled"] = false
	}
	if !requireVerified {
		p.RequireVerifiedEmail = false
		zero["require_verified_email"] = false
	}
	return writeZeroColumns(s.db, p, zero)
}

// GetByID fetches a provider.
func (s *SSOProviderStore) GetByID(id string) (*models.SSOProvider, error) {
	var p models.SSOProvider
	if err := s.db.First(&p, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrProviderNotFound
		}
		return nil, err
	}
	return &p, nil
}

// GetByName fetches a provider by its slug, the string in
// /api/auth/sso/:provider/authorize.
func (s *SSOProviderStore) GetByName(name string) (*models.SSOProvider, error) {
	var p models.SSOProvider
	if err := s.db.First(&p, "name = ?", name).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrProviderNotFound
		}
		return nil, err
	}
	return &p, nil
}

// ListEnabled returns the providers the login page draws buttons for, in the
// order the operator chose.
func (s *SSOProviderStore) ListEnabled() ([]models.SSOProvider, error) {
	var providers []models.SSOProvider
	if err := s.db.Where("enabled = ?", true).Order("sort_order ASC, name ASC").Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// ListAll returns every provider, enabled or not: the settings page shows the
// disabled ones, because "why is there no login button" is a question about a
// row that is present and switched off.
func (s *SSOProviderStore) ListAll() ([]models.SSOProvider, error) {
	var providers []models.SSOProvider
	if err := s.db.Order("sort_order ASC, name ASC").Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// Update writes the mutable fields. ClientSecret is written only when the
// caller supplied one, so editing a provider's issuer from a console that
// never read the secret cannot erase it.
func (s *SSOProviderStore) Update(p *models.SSOProvider, sealClientSecret bool) error {
	updates := map[string]any{
		"name":                   p.Name,
		"type":                   p.Type,
		"client_id":              p.ClientID,
		"scope":                  p.Scope,
		"enabled":                p.Enabled,
		"issuer":                 p.Issuer,
		"discovery_url":          p.DiscoveryURL,
		"auth_url":               p.AuthURL,
		"token_url":              p.TokenURL,
		"user_info_url":          p.UserInfoURL,
		"jwks_uri":               p.JwksURI,
		"redirect_uri":           p.RedirectURI,
		"discovery_doc":          p.DiscoveryDoc,
		"discovered_at":          p.DiscoveredAt,
		"discovery_state":        p.DiscoveryState,
		"pkce_supported":         p.PKCESupported,
		"claims_email":           p.ClaimsEmail,
		"claims_username":        p.ClaimsUsername,
		"claims_name":            p.ClaimsName,
		"claims_groups":          p.ClaimsGroups,
		"allowed_domains":        p.AllowedDomains,
		"require_verified_email": p.RequireVerifiedEmail,
		"link_policy":            p.LinkPolicy,
		"role_mapping":           p.RoleMapping,
		"sort_order":             p.SortOrder,
		"updated_at":             time.Now().UTC(),
	}
	if sealClientSecret {
		updates["client_secret"] = p.ClientSecret
	}
	result := s.db.Model(&models.SSOProvider{}).Where("id = ?", p.ID).Updates(updates)
	if result.Error != nil {
		if isUniqueViolation(result.Error) {
			return fmt.Errorf("sso: provider name %q already exists", p.Name)
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrProviderNotFound
	}
	return nil
}

// MarkDiscovery records what discovery answered. The state is stored because
// the console draws a named state, not a spinner, and "which IdP is unhealthy"
// is a question the row answers.
func (s *SSOProviderStore) MarkDiscovery(id, state, discoveryDoc string, discoveredAt time.Time, pkceSupported bool) error {
	return s.db.Model(&models.SSOProvider{}).Where("id = ?", id).Updates(map[string]any{
		"discovery_state": state,
		"discovery_doc":   discoveryDoc,
		"discovered_at":   discoveredAt,
		"pkce_supported":  pkceSupported,
		"updated_at":      time.Now().UTC(),
	}).Error
}

// Delete removes a provider. Accounts it provisioned stay: a provider is a
// door, not the people behind it, and deleting a door does not delete them.
func (s *SSOProviderStore) Delete(id string) error {
	result := s.db.Delete(&models.SSOProvider{}, "id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrProviderNotFound
	}
	return nil
}

// PendingIdentityStore provides persistence for the identity contract's queue.
type PendingIdentityStore struct {
	db *gorm.DB
}

// Create parks an assertion the installation refused to act on.
func (s *PendingIdentityStore) Create(p *models.PendingIdentity) error {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if p.Status == "" {
		p.Status = models.IdentityStatusPending
	}
	if p.Claims == "" {
		p.Claims = "{}"
	}
	return s.db.Create(p).Error
}

// GetByID fetches a pending identity.
func (s *PendingIdentityStore) GetByID(id string) (*models.PendingIdentity, error) {
	var p models.PendingIdentity
	if err := s.db.First(&p, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// ListPending returns the queue an admin works through.
func (s *PendingIdentityStore) ListPending() ([]models.PendingIdentity, error) {
	var rows []models.PendingIdentity
	if err := s.db.Where("status = ?", models.IdentityStatusPending).
		Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListAll returns every row, decided ones included: the record of the
// decisions, not only the open queue.
func (s *PendingIdentityStore) ListAll() ([]models.PendingIdentity, error) {
	var rows []models.PendingIdentity
	if err := s.db.Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// MarkDecided records a decision. The status is validated here, because a row
// whose status no code path reads is a queue entry that looks decided and is
// not.
func (s *PendingIdentityStore) MarkDecided(id, status, decidedByUserID, decidedByUsername, reason, createdUserID string) error {
	switch status {
	case models.IdentityStatusLinked, models.IdentityStatusRefused, models.IdentityStatusApproved:
	default:
		return fmt.Errorf("identity: %q is not a decision", status)
	}
	result := s.db.Model(&models.PendingIdentity{}).Where("id = ?", id).Updates(map[string]any{
		"status":              status,
		"decided_at":          time.Now().UTC(),
		"decided_by":          decidedByUserID,
		"decided_by_username": decidedByUsername,
		"reason":              reason,
		"created_user_id":     createdUserID,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
