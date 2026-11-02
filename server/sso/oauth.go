package sso

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"nvr/server/auth"
	"nvr/server/models"
	"nvr/server/store"
)

// The authorization-code flow (2.4-2, 2.4-3).
//
// This is the file that turns a provider row into a logged-in session, and the
// order inside `HandleCallback` is its security property:
//
//	code → token endpoint → **verify the id_token against the provider's published
//	keys** → require that userinfo and the signed token agree → contract gate →
//	account resolution → session tokens.
//
// Before 2.4 the identity came from the userinfo call alone: a bearer token whose
// subject nobody had checked against anything the provider signed. Everything
// below that order is a consequence of it — the refusal names *who tried to arrive
// as whom*, because the assertion is known before it is acted on, and nothing the
// provider asserts becomes an identity until a signature says it may.
//
// nvrWay speaks OIDC only. OurWay's SAML half of this file (`IssueSAMLIdentity`)
// and its legacy provider-type normalisation (google/microsoft claim fixups) are
// not here: `SSOProvider.Type` is fixed to `oidc`, and a provider whose claims are
// named oddly is configured with the claim-mapping columns, not coded for by name.

// OAuthHandler drives the code flow for the provider rows the operator configures.
type OAuthHandler struct {
	store *store.Store
	jwt   *auth.JWTAuth
	// client is the shared SSO HTTP policy (a timeout, so an unreachable IdP
	// produces a named failure in seconds rather than a hung login).
	client *http.Client
	// jwks is what an id_token is verified against: the key set comes from the
	// provider's published `jwks_uri`, never from a config file.
	jwks *JWKSClient
}

// NewOAuthHandler builds the handler over a store and the JWT service.
func NewOAuthHandler(st *store.Store, jwtAuth *auth.JWTAuth) *OAuthHandler {
	client := DefaultHTTPClient()
	return &OAuthHandler{
		store:  st,
		jwt:    jwtAuth,
		client: client,
		jwks:   NewJWKSClient(client),
	}
}

// SetHTTPClient replaces the HTTP client (and the JWKS client with it). Tests use
// it to point the flow at a fixture IdP; a deployment with a proxy or a mTLS
// transport uses it for the same reason.
func (h *OAuthHandler) SetHTTPClient(client *http.Client) {
	h.client = client
	h.jwks = NewJWKSClient(client)
}

// LoginOutcome is what a completed callback produces: the session tokens, the
// account the identity maps to, and the facts the audit row records.
//
// `EmailVerified` is not "the provider sent an email claim" — it is what the
// **signed** token said, which is the fact the contract acts on.
type LoginOutcome struct {
	AccessToken  string
	RefreshToken string
	User         *models.User

	Subject string
	Email   string
	// EmailVerified is the provider's `email_verified` claim as the signed token
	// asserts it.
	EmailVerified bool
	// VerifiedByIDToken: false means the provider published no `jwks_uri` (an
	// operator typed the endpoints), so the identity rests on the userinfo call
	// alone. The login record says which of the two it rests on, rather than
	// implying a verification that did not happen.
	VerifiedByIDToken bool
	// LinkedExisting is the `link_policy=link` case: the identity was stamped onto
	// an account that already existed. That is the operator's explicit per-provider
	// choice, and it is an audited act.
	LinkedExisting bool
	// CreatedJIT: the account was provisioned by this login, at the viewer floor.
	CreatedJIT bool
	// MappedRoles is the role set the provider's role mapping put on this account
	// for this login, when a mapping is configured. Empty means no mapping: the
	// recorded roles stand and the viewer floor is unchanged.
	MappedRoles []string
}

// NewCodeVerifier builds a PKCE code verifier: 32 random bytes, base64url, inside
// the 43-128 character range the spec sets.
func NewCodeVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// CodeChallenge is the S256 challenge a verifier corresponds to. `plain` is not
// sent, anywhere: the challenge is what makes an intercepted code useless.
func CodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL builds the redirect to the provider.
//
// `state` is the CSRF binding, `nonce` binds the id_token to *this* login (a
// token minted for an earlier one must not satisfy a later one), and the challenge
// is the PKCE half. The challenge is only sent when the provider said it accepts
// S256 — discovery recorded that answer on the row, so the redirect decides without
// paying a round trip to find out.
func (h *OAuthHandler) AuthorizeURL(provider *models.SSOProvider, redirectURI, state, nonce, codeChallenge string) string {
	scope := strings.TrimSpace(provider.Scope)
	if scope == "" {
		scope = "openid email profile"
	}

	params := url.Values{}
	params.Set("client_id", provider.ClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", scope)
	params.Set("state", state)
	if nonce != "" {
		params.Set("nonce", nonce)
	}
	if codeChallenge != "" {
		params.Set("code_challenge", codeChallenge)
		params.Set("code_challenge_method", "S256")
	}

	base := strings.TrimSpace(provider.AuthURL)
	if strings.Contains(base, "?") {
		return base + "&" + params.Encode()
	}
	return base + "?" + params.Encode()
}

// OAuthTokens is the token endpoint's answer.
type OAuthTokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	IDToken      string `json:"id_token"`
}

// UserInfo is the identity the provider asserts.
//
// `Groups` is the mapped group claim, which is what the role mapping reads; it is
// carried from both identity sources, never invented from a name.
type UserInfo struct {
	Subject       string   `json:"sub"`
	Name          string   `json:"name"`
	GivenName     string   `json:"given_name"`
	FamilyName    string   `json:"family_name"`
	Username      string   `json:"preferred_username"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Picture       string   `json:"picture"`
	Groups        []string `json:"groups,omitempty"`
}

// ExchangeCode trades the authorization code for tokens. The verifier is sent when
// the authorization request carried a challenge: an IdP that was given one must
// refuse a token request without it, which is the point of sending it.
func (h *OAuthHandler) ExchangeCode(provider *models.SSOProvider, code, redirectURI, codeVerifier string) (*OAuthTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", provider.ClientID)
	form.Set("client_secret", provider.ClientSecret)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}

	resp, err := h.client.PostForm(provider.TokenURL, form)
	if err != nil {
		return nil, fmt.Errorf("token request to %s failed: %w", provider.TokenURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint %s answered %d", provider.TokenURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the token response from %s: %w", provider.TokenURL, err)
	}
	var tokens OAuthTokens
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("token endpoint %s did not answer with JSON: %w", provider.TokenURL, err)
	}
	return &tokens, nil
}

// GetUserInfo calls the provider's userinfo endpoint.
//
// The body is another machine's answer about a person: size-capped, and kept,
// because the mapped group claim is read out of it as published.
func (h *OAuthHandler) GetUserInfo(provider *models.SSOProvider, accessToken string) (*UserInfo, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, provider.UserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("userinfo request to %s failed: %w", provider.UserInfoURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo endpoint %s answered %d", provider.UserInfoURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading userinfo from %s: %w", provider.UserInfoURL, err)
	}
	var info UserInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("userinfo endpoint %s did not answer with JSON: %w", provider.UserInfoURL, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err == nil {
		info.Groups = claimStrings(raw, groupsClaim(provider))
		if info.Username == "" {
			info.Username = claimString(raw, usernameClaim(provider))
		}
	}
	return &info, nil
}

// HandleCallback completes the login.
//
// The ordering is the exit criterion, and it is the reason the function reads as a
// sequence of refusals: exchange, verify the signed token, require the two identity
// sources to agree, apply the contract, and only then touch accounts. `nonce` and
// `codeVerifier` come from the server-side state record the redirect created.
func (h *OAuthHandler) HandleCallback(ctx context.Context, provider *models.SSOProvider, code, redirectURI, nonce, codeVerifier string) (*LoginOutcome, error) {
	// The switch is honored at the callback too, not only at the redirect: a login
	// in flight when an operator flips it must not complete.
	if provider == nil || !provider.Enabled {
		return nil, &IdentityError{Reason: ReasonProviderDisabled,
			Err: fmt.Errorf("provider is not enabled")}
	}

	tokens, err := h.ExchangeCode(provider, code, redirectURI, codeVerifier)
	if err != nil {
		return nil, &IdentityError{Reason: ReasonCodeExchange, Err: err}
	}

	// A provider with an issuer and a key set is an OIDC provider, and the rule is
	// that an identity claim is trusted only after `iss`/`aud`/`exp` and the
	// signature have been checked against the keys that issuer publishes. No
	// `jwks_uri` means the operator typed the endpoints by hand: there is no key set
	// to check against, and the outcome says so instead of pretending.
	verifies := strings.TrimSpace(provider.Issuer) != "" && strings.TrimSpace(provider.JwksURI) != ""

	// userinfo is *called* before verification, and trusted after it. Nothing
	// asserted here becomes an identity until the signed token has been checked, and
	// the reason a refusal can name who tried to arrive as whom is that the
	// assertion is known at refusal time.
	var info *UserInfo
	var userinfoErr error
	if strings.TrimSpace(provider.UserInfoURL) != "" && tokens.AccessToken != "" {
		info, userinfoErr = h.GetUserInfo(provider, tokens.AccessToken)
	}

	refuse := func(reason string, cause error, claims map[string]any) (*LoginOutcome, error) {
		verr := &IdentityError{Reason: reason, Err: cause}
		if info != nil {
			verr.Subject, verr.Email = info.Subject, info.Email
		}
		if verr.Subject == "" && claims != nil {
			verr.Subject = claimString(claims, "sub")
			verr.Email = claimString(claims, emailClaim(provider))
		}
		return nil, verr
	}

	var claims map[string]any
	if verifies {
		if tokens.IDToken == "" {
			return refuse(ReasonMissingIDToken,
				fmt.Errorf("provider %s published a jwks_uri and answered the token request with no id_token", provider.Name), nil)
		}
		verified, err := h.jwks.VerifyIDToken(provider.JwksURI, provider.Issuer, provider.ClientID, tokens.IDToken)
		if err != nil {
			return refuse(ReasonIDTokenVerification, err, nil)
		}
		claims = verified
		if nonce != "" {
			if got := claimString(claims, "nonce"); got != nonce {
				return refuse(ReasonNonceMismatch,
					fmt.Errorf("id_token nonce is %q, this login sent %q", got, nonce), claims)
			}
		}
	}

	if info == nil && userinfoErr != nil && claims == nil {
		return refuse(ReasonUserInfoFailed, userinfoErr, nil)
	}
	if info == nil {
		// No userinfo endpoint, or the call failed and the verified id_token stands
		// on its own — which is what the OIDC code flow allows, and the reason the
		// token is verified before anything is trusted.
		if claims == nil {
			return refuse(ReasonNoEmailClaim,
				fmt.Errorf("provider %s published no usable identity: userinfo %v", provider.Name, userinfoErr), nil)
		}
		info = userInfoFromClaims(provider, claims)
	} else if claims != nil {
		// Both sources name a subject, and they must name the same one. A provider
		// whose userinfo says one person and whose signed token says another is
		// misconfigured or under someone's control, and nvrWay does not pick which.
		tokenSubject := claimString(claims, "sub")
		if info.Subject == "" {
			info.Subject = tokenSubject
		} else if tokenSubject != "" && info.Subject != tokenSubject {
			return nil, &IdentityError{
				Reason:  ReasonClaimsDisagree,
				Subject: info.Subject,
				Email:   info.Email,
				Err:     fmt.Errorf("userinfo says %q, the signed id_token says %q", info.Subject, tokenSubject),
			}
		}
		// The signed token is the stronger source for the email facts: userinfo is a
		// call, the id_token is a claim the provider signed.
		if email := claimString(claims, emailClaim(provider)); email != "" {
			info.Email = email
		}
		info.EmailVerified = claimBool(claims, "email_verified")
		if info.Name == "" {
			info.Name = claimString(claims, nameClaim(provider))
		}
		if info.Username == "" {
			info.Username = claimString(claims, usernameClaim(provider))
		}
		if len(info.Groups) == 0 {
			info.Groups = claimStrings(claims, groupsClaim(provider))
		}
	}

	// The subject is the only stable handle an IdP offers: without one an assertion
	// cannot be provisioned, linked, or audited. This is also the store's second
	// line — `FindByProvider` refuses an empty half of the JIT lookup, so a local
	// row (provider_id = "") never answers an assertion with no subject.
	if strings.TrimSpace(info.Subject) == "" {
		return nil, &IdentityError{Reason: ReasonSubjectMissing, Email: info.Email,
			Err: fmt.Errorf("provider %s asserted an identity with no sub", provider.Name)}
	}
	if info.Name == "" {
		info.Name = info.Username
	}

	// The contract's door, applied to the asserted identity before any account is
	// touched: the domain allow-list and the verified-email requirement.
	if gateErr := contractGate(provider, info); gateErr != nil {
		return nil, gateErr
	}

	user, account, err := h.resolveAccount(ctx, provider, info)
	if err != nil {
		if iderr, ok := err.(*IdentityError); ok {
			iderr.Subject, iderr.Email = info.Subject, info.Email
		}
		return nil, err
	}

	roles := h.rolesOf(user)
	access, err := h.jwt.IssueNamed(user.ID, user.Username, roles, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to issue the access token: %w", err)
	}
	refresh, err := h.jwt.IssueRefresh(user.ID, user.Username, roles, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to issue the refresh token: %w", err)
	}
	if err := h.store.Users.MarkLogin(user.ID, time.Now().UTC()); err != nil {
		// A login that happened is not undone by a bookkeeping failure, and the
		// session tokens are already issued by here.
		_ = err
	}

	return &LoginOutcome{
		AccessToken:       access,
		RefreshToken:      refresh,
		User:              user,
		Subject:           info.Subject,
		Email:             info.Email,
		EmailVerified:     info.EmailVerified,
		VerifiedByIDToken: claims != nil,
		LinkedExisting:    account.LinkedExisting,
		CreatedJIT:        account.CreatedJIT,
		MappedRoles:       account.MappedRoles,
	}, nil
}

// contractGate is the contract's door, per provider: an IdP may only assert
// identities the operator listed, and an unverified email is not an identity when
// the operator requires one. An empty allow-list is no restriction; a list is a wall.
func contractGate(provider *models.SSOProvider, info *UserInfo) *IdentityError {
	if strings.TrimSpace(provider.AllowedDomains) != "" {
		if strings.TrimSpace(info.Email) == "" {
			return &IdentityError{Reason: ReasonNoEmailClaim, Subject: info.Subject,
				Err: fmt.Errorf("provider %s lists allowed domains and asserted no email", provider.Name)}
		}
		if !domainAllowed(provider.AllowedDomains, info.Email) {
			return &IdentityError{Reason: ReasonDomainNotAllowed, Subject: info.Subject, Email: info.Email,
				Err: fmt.Errorf("%s is outside %s's allowed domains (%s)", info.Email, provider.Name, provider.AllowedDomains)}
		}
	}
	if provider.RequireVerifiedEmail && !info.EmailVerified {
		return &IdentityError{Reason: ReasonEmailUnverified, Subject: info.Subject, Email: info.Email,
			Err: fmt.Errorf("provider %s requires a verified email and %s was asserted unverified", provider.Name, info.Email)}
	}
	return nil
}

// rolesOf is the role set a session is issued with. An account with no role rows is
// the viewer floor and nothing more: the floor is recorded on JIT accounts, and a
// missing row must never widen access.
func (h *OAuthHandler) rolesOf(user *models.User) []string {
	if len(user.Roles) == 0 {
		return []string{models.RoleViewer}
	}
	roles := make([]string, 0, len(user.Roles))
	for _, role := range user.Roles {
		roles = append(roles, role.Name)
	}
	return roles
}

// AccountOutcome is what the contract decided about the account space, so the API
// layer can write it into the ledger: an account linked by explicit policy, an
// identity parked as pending, the roles the mapping owned.
type AccountOutcome struct {
	LinkedExisting bool
	CreatedJIT     bool
	// Pending is the pending-identity row the refusal created, when it created one.
	Pending *models.PendingIdentity
	// MappedRoles is the role set the provider's mapping applied for this login.
	MappedRoles []string
}

// resolveAccount turns an asserted identity into an account under the contract.
// Three cases, and the middle one is the whole point:
//
//  1. the identity is already linked to an account → that account, with the role
//     mapping re-applied (a mapping that owns role assignment owns it on every
//     login, not just the first);
//  2. the asserted email belongs to an account that already exists → **not** that
//     account, by default. `link_policy=pending` parks the assertion as a pending
//     identity and refuses the login; `link_policy=link` is the operator's explicit
//     choice to keep match-by-email, and it is audited;
//  3. nobody holds this identity → JIT provisioning at the viewer floor.
//
// A permissive IdP that lets anyone assert `admin@corp.example` therefore produces
// a ledger row and a pending identity. It does not produce the admin's session, and
// it does not touch the admin's roles.
func (h *OAuthHandler) resolveAccount(ctx context.Context, provider *models.SSOProvider, info *UserInfo) (*models.User, *AccountOutcome, error) {
	outcome := &AccountOutcome{}

	// 1. An identity this provider already provisioned.
	if user, err := h.store.Users.FindByProvider(provider.Name, info.Subject); err == nil && user != nil {
		if user.IsPending() {
			// A pending account is not a login: it has no roles and no decision on
			// it yet. The answer is the contract's, so the console shows the same
			// state as the assertion that created it.
			return nil, outcome, &IdentityError{Reason: ReasonAccountPending, TargetUserID: user.ID,
				Err: fmt.Errorf("this identity is awaiting an administrator's decision")}
		}
		mapped, err := h.applyRoleMapping(provider, user, info)
		if err != nil {
			return nil, outcome, err
		}
		outcome.MappedRoles = mapped
		return user, outcome, nil
	}

	// 2. The asserted email belongs to an account that already exists.
	var existing *models.User
	if strings.TrimSpace(info.Email) != "" {
		if user, err := h.store.Users.GetByEmail(strings.TrimSpace(info.Email)); err == nil && user != nil {
			existing = user
		}
	}
	if existing != nil {
		if provider.LinkPolicy == models.SSOLinkPolicyLink {
			// Match-by-email, because the operator named it for this provider. The
			// account's roles are not touched here: linking says who the identity is,
			// and the role mapping (if configured) decides roles.
			existing.Provider = provider.Name
			existing.ProviderID = info.Subject
			existing.SSOAttributes = ssoAttributeSummary(provider.Name, info)
			if err := h.store.Users.Update(existing); err != nil {
				return nil, outcome, fmt.Errorf("failed to link the account: %w", err)
			}
			mapped, err := h.applyRoleMapping(provider, existing, info)
			if err != nil {
				return nil, outcome, err
			}
			outcome.LinkedExisting = true
			outcome.MappedRoles = mapped
			return existing, outcome, nil
		}

		// The default, and the safe state: park the assertion, touch nothing that
		// exists. The row carries the assertion as received, because the IdP's
		// response is not reproducible when the admin reads it a week later.
		pending := &models.PendingIdentity{
			ID:              uuid.NewString(),
			Provider:        provider.Name,
			ProviderSubject: info.Subject,
			Email:           strings.TrimSpace(info.Email),
			Username:        h.uniqueUsername(provider, info),
			DisplayName:     strings.TrimSpace(info.Name),
			Claims:          ssoAttributeSummary(provider.Name, info),
			MatchedUserID:   existing.ID,
			Status:          models.IdentityStatusPending,
			Reason: fmt.Sprintf("%s belongs to an existing account; the identity is pending an administrator's decision",
				strings.TrimSpace(info.Email)),
		}
		if err := h.store.PendingIdentities.Create(pending); err != nil {
			return nil, outcome, fmt.Errorf("failed to record the pending identity: %w", err)
		}
		outcome.Pending = pending
		return nil, outcome, &IdentityError{
			Reason:        ReasonIdentityPending,
			Subject:       info.Subject,
			Email:         info.Email,
			TargetUserID:  existing.ID,
			PendingUserID: pending.ID,
			Err: fmt.Errorf("%s belongs to an existing account; the provider may not link it by asserting it",
				strings.TrimSpace(info.Email)),
		}
	}

	// 3. A new identity: JIT provisioning at the floor.
	user := &models.User{
		ID: uuid.NewString(),
		// The JIT account has no password by construction: an OIDC account
		// authenticates with the provider, never with a hash here.
		PasswordHash:  "",
		Name:          strings.TrimSpace(info.Name),
		Email:         strings.TrimSpace(info.Email),
		Username:      h.uniqueUsername(provider, info),
		Provider:      provider.Name,
		ProviderID:    info.Subject,
		SSOAttributes: ssoAttributeSummary(provider.Name, info),
		Status:        models.UserStatusActive,
	}
	if user.Email == "" {
		// No email asserted: the account needs a unique, non-colliding address, and
		// `@<provider>.sso` is visibly not a person's mailbox.
		user.Email = info.Subject + "@" + provider.Name + ".sso"
	}
	if err := h.store.Users.Create(user); err != nil {
		return nil, outcome, fmt.Errorf("failed to create the JIT account: %w", err)
	}
	if err := h.grantFloorRole(user.ID); err != nil {
		return nil, outcome, fmt.Errorf("failed to record the JIT floor role: %w", err)
	}
	outcome.CreatedJIT = true

	mapped, err := h.applyRoleMapping(provider, user, info)
	if err != nil {
		return nil, outcome, err
	}
	outcome.MappedRoles = mapped

	// Reload, so the caller's account carries the roles the login just recorded.
	created, err := h.store.Users.GetByID(user.ID)
	if err != nil {
		return user, outcome, nil
	}
	return created, outcome, nil
}

// uniqueUsername derives a login name from the asserted identity, and never
// overwrites one. `users.username` is a unique index, and an assertion whose name is
// taken — a local account with the same local part, an identity from another
// provider — is exactly the case the contract is about, so the account gets a
// distinct name and the admin sees the same person under a name that does not
// collide.
func (h *OAuthHandler) uniqueUsername(provider *models.SSOProvider, info *UserInfo) string {
	base := strings.TrimSpace(info.Username)
	if base == "" {
		email := strings.TrimSpace(info.Email)
		if idx := strings.Index(email, "@"); idx > 0 {
			base = email[:idx]
		} else {
			base = email
		}
	}
	if base == "" {
		base = strings.TrimSpace(info.Subject)
	}
	if base == "" {
		base = "sso-user"
	}

	candidate := base
	for n := 2; n < 100; n++ {
		if h.usernameFree(candidate) {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
	return fmt.Sprintf("%s-%s", base, uuid.NewString()[:8])
}

// usernameFree reports that no account holds this login name (case-insensitively:
// the unique index is byte-exact, and `Ops` and `ops` must not both arrive).
func (h *OAuthHandler) usernameFree(candidate string) bool {
	var count int64
	if err := h.store.DB.Model(&models.User{}).
		Where("LOWER(username) = ?", strings.ToLower(candidate)).
		Count(&count).Error; err != nil {
		return false
	}
	return count == 0
}

// applyRoleMapping is the contract's fourth rule: role mapping is opt-in, and when
// it is configured it **owns** role assignment for the accounts that arrive through
// this provider. No mapping is not "everyone is admin" — it is no mapping, and the
// recorded roles and the viewer floor stand unchanged.
//
// Owning it means removing what the claim no longer asserts: an operator who maps
// `rmm-admins` to admin expects the technician who leaves that group to stop being
// admin. Roles outside the mapping's own set are never touched, so a mapping cannot
// quietly strip a role the operator never put in it.
func (h *OAuthHandler) applyRoleMapping(provider *models.SSOProvider, user *models.User, info *UserInfo) ([]string, error) {
	mapping := parseRoleMapping(provider.RoleMapping)
	if len(mapping) == 0 {
		return nil, nil
	}

	// The floor is always wanted: a mapping that owns role assignment must not
	// leave an account with nothing.
	want := map[string]bool{models.RoleViewer: true}
	for _, group := range info.Groups {
		if role, ok := mapping[strings.TrimSpace(group)]; ok {
			want[role] = true
		}
	}

	current := map[string]string{} // role name → grant id
	if grants, err := h.store.UserRoles.ListFor(user.ID); err == nil {
		for _, grant := range grants {
			role, err := h.store.Roles.GetRoleByID(grant.RoleID)
			if err != nil {
				continue
			}
			current[role.Name] = grant.RoleID
		}
	}

	owned := mappingOwnsRoles(mapping)
	for name, roleID := range current {
		if want[name] || !owned[name] {
			continue
		}
		if err := h.store.UserRoles.Revoke(user.ID, roleID); err != nil {
			return nil, fmt.Errorf("failed to remove the role the mapping no longer asserts: %w", err)
		}
		delete(current, name)
	}

	granted := make([]string, 0, len(want))
	for _, name := range sortedNames(want) {
		if _, ok := current[name]; ok {
			granted = append(granted, name)
			continue
		}
		role, err := h.store.Roles.GetRoleByName(name)
		if err != nil {
			// A mapping naming a role that does not exist grants nothing. It must
			// not invent one, and it must not fail the login open.
			continue
		}
		if err := h.store.UserRoles.Grant(user.ID, role.ID); err != nil {
			return nil, fmt.Errorf("failed to grant the mapped role: %w", err)
		}
		granted = append(granted, name)
	}
	return granted, nil
}

// ssoAttributeSummary is the claim record the product keeps: who the provider
// asserted, and what it said about the email. Structured JSON, so a reader can tell
// a verified email from an asserted one — and the raw id_token is **not** stored:
// it is an access credential to another system, and a dump of `users` must not hand
// it to anyone holding a database read.
func ssoAttributeSummary(providerName string, info *UserInfo) string {
	type summary struct {
		Provider      string   `json:"provider"`
		ProviderID    string   `json:"provider_id"`
		Name          string   `json:"name,omitempty"`
		Username      string   `json:"username,omitempty"`
		Email         string   `json:"email,omitempty"`
		EmailVerified bool     `json:"email_verified"`
		Groups        []string `json:"groups,omitempty"`
	}
	encoded, err := json.Marshal(summary{
		Provider:      providerName,
		ProviderID:    info.Subject,
		Name:          info.Name,
		Username:      info.Username,
		Email:         info.Email,
		EmailVerified: info.EmailVerified,
		Groups:        info.Groups,
	})
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// grantFloorRole gives a JIT account the viewer role: the access the console has
// always given a new SSO account, recorded on the account itself rather than
// implied at login time.
func (h *OAuthHandler) grantFloorRole(userID string) error {
	role, err := h.store.Roles.GetRoleByName(models.RoleViewer)
	if err != nil {
		return err
	}
	return h.store.UserRoles.Grant(userID, role.ID)
}

// emailClaim, nameClaim, usernameClaim and groupsClaim are the claim mapping the
// provider row carries. IdPs do not agree on the claim names — Entra's UPN is not
// Google's `email`, and a home IdP's group claim is whatever its admin typed — so
// the operator names them, and the default is the OIDC standard name.
func emailClaim(provider *models.SSOProvider) string {
	if v := strings.TrimSpace(provider.ClaimsEmail); v != "" {
		return v
	}
	return "email"
}

func nameClaim(provider *models.SSOProvider) string {
	if v := strings.TrimSpace(provider.ClaimsName); v != "" {
		return v
	}
	return "name"
}

func usernameClaim(provider *models.SSOProvider) string {
	if v := strings.TrimSpace(provider.ClaimsUsername); v != "" {
		return v
	}
	return "preferred_username"
}

func groupsClaim(provider *models.SSOProvider) string {
	if v := strings.TrimSpace(provider.ClaimsGroups); v != "" {
		return v
	}
	return "groups"
}

// claimStrings reads a claim that is a string array (a group claim), tolerating the
// single-string shape some IdPs publish for a one-member group.
func claimStrings(claims map[string]any, key string) []string {
	if key == "" {
		return nil
	}
	switch v := claims[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}

func claimString(claims map[string]any, key string) string {
	if key == "" {
		return ""
	}
	if v, ok := claims[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func claimBool(claims map[string]any, key string) bool {
	v, ok := claims[key]
	if !ok {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// userInfoFromClaims builds the identity record from a **verified** id_token, for
// the providers that answer the code flow with a token and no (or a failed) userinfo
// call. `email_verified` is read from the claims the provider signed.
func userInfoFromClaims(provider *models.SSOProvider, claims map[string]any) *UserInfo {
	info := &UserInfo{
		Subject:       claimString(claims, "sub"),
		Name:          claimString(claims, nameClaim(provider)),
		Username:      claimString(claims, usernameClaim(provider)),
		Email:         claimString(claims, emailClaim(provider)),
		EmailVerified: claimBool(claims, "email_verified"),
		Groups:        claimStrings(claims, groupsClaim(provider)),
	}
	if info.Name == "" {
		info.Name = info.Username
	}
	return info
}

// sortedNames keeps the granted-role list deterministic, so a test (and an audit
// row) sees the same order on every run.
func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}
