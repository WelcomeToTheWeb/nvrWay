package models

import "time"

// Link policies, per provider.
//
// `pending` is the default and the safe state: a provider that asserts the
// email of an existing local account does **not** get to log in as that
// account. The assertion parks a pending identity an admin must decide on.
//
// `link` is the match-by-email behaviour. It exists so an operator who has read
// what it means can choose it, per provider; it is not what a new provider gets.
const (
	SSOLinkPolicyPending = "pending"
	SSOLinkPolicyLink    = "link"
)

// ValidSSOLinkPolicy reports whether policy is a link policy.
func ValidSSOLinkPolicy(policy string) bool {
	return policy == SSOLinkPolicyPending || policy == SSOLinkPolicyLink
}

// SSOProviderSlug is v1's seeded provider slug: the OIDC provider configured
// from OIDC_ISSUER / OIDC_CLIENT_ID / OIDC_CLIENT_SECRET on first boot.
const SSOProviderSlug = "oidc"

// SSOProvider is one configured OIDC provider.
//
// The row is configured by **issuer**; the endpoints are what the provider
// published, captured at save time. A provider row saved from a guess is a
// login that fails at 2 a.m. with a 500, so the endpoints are discovered, not
// typed.
//
// v1 speaks OIDC only. There is no SAML column set here, and `Type` is fixed —
// a second protocol arrives as its own row kind, not as extra fields on this one.
type SSOProvider struct {
	ID   string `gorm:"type:uuid;primaryKey" json:"id"`
	Type string `gorm:"not null;default:'oidc'" json:"type"`
	// Name is a slug (google, keycloak-corp) and the string in
	// /api/auth/sso/:provider/… — unique, so a console button cannot be ambiguous.
	Name         string `gorm:"uniqueIndex;not null" json:"name"`
	ClientID     string `gorm:"not null" json:"client_id"`
	ClientSecret string `gorm:"type:text;not null;default:''" json:"-"`
	Scope        string `gorm:"not null;default:'openid email profile'" json:"scope"`
	Enabled      bool   `gorm:"not null;default:true" json:"enabled"`

	// Issuer is what an operator types. Everything below it is what the provider
	// said about itself.
	Issuer       string `gorm:"not null" json:"issuer"`
	DiscoveryURL string `gorm:"not null;default:''" json:"discovery_url"`
	AuthURL      string `gorm:"not null;default:''" json:"auth_url"`
	TokenURL     string `gorm:"not null;default:''" json:"token_url"`
	UserInfoURL  string `gorm:"not null;default:''" json:"user_info_url"`
	// JwksURI is the key set an id_token is verified against.
	JwksURI      string     `gorm:"not null;default:''" json:"jwks_uri"`
	RedirectURI  string     `gorm:"not null;default:''" json:"redirect_uri"`
	DiscoveryDoc string     `gorm:"type:text;not null;default:''" json:"-"`
	DiscoveredAt *time.Time `json:"discovered_at"`
	// DiscoveryState is the named outcome of the last discovery run:
	// ok | discovery_unreachable | discovery_document | issuer_mismatch |
	// no_code_flow. It is stored because the console draws a state, not a
	// spinner, and "why is there no login button" needs an answer in the row.
	DiscoveryState string `gorm:"not null;default:''" json:"discovery_state"`
	// PKCESupported is what discovery said about code_challenge_methods_supported,
	// recorded at save time: the authorize redirect must decide whether to send a
	// challenge without paying a round trip to the IdP to find out.
	PKCESupported bool `gorm:"not null;default:false" json:"pkce_supported"`

	// Claim mapping: which claim carries which fact. IdPs do not agree on this,
	// and the product must not guess. Empty means the OIDC standard names.
	ClaimsEmail    string `gorm:"not null;default:''" json:"claims_email"`
	ClaimsUsername string `gorm:"not null;default:''" json:"claims_username"`
	ClaimsName     string `gorm:"not null;default:''" json:"claims_name"`
	ClaimsGroups   string `gorm:"not null;default:'groups'" json:"claims_groups"`

	// The identity contract, per provider.
	// AllowedDomains is a comma-separated list; an asserted email outside it is
	// refused. RequireVerifiedEmail gates on the provider's email_verified claim.
	AllowedDomains       string `gorm:"not null;default:''" json:"allowed_domains"`
	RequireVerifiedEmail bool   `gorm:"not null;default:true" json:"require_verified_email"`
	LinkPolicy           string `gorm:"not null;default:'pending'" json:"link_policy"`
	// RoleMapping is JSON of claim value → role. Empty is not "everyone is
	// admin": it is no mapping, and the viewer floor stands.
	RoleMapping string `gorm:"type:text;not null;default:''" json:"role_mapping"`

	// The login page lists providers in this order.
	SortOrder int `gorm:"not null;default:0" json:"sort_order"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName pins the table name.
func (SSOProvider) TableName() string { return "sso_providers" }

// SSOTypeOIDC is the only provider kind v1 implements.
const SSOTypeOIDC = "oidc"

// SecretSet is what the console may know about the client secret: that it is
// set. The value never leaves the API.
func (p *SSOProvider) SecretSet() bool { return p != nil && p.ClientSecret != "" }
