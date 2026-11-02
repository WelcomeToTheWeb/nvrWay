package sso

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"nvr/server/auth"
	"nvr/server/models"
	"nvr/server/store"
)

// The fixtures.
//
// `fakeIDP` is a provider that answers the three calls the code flow makes —
// token, userinfo, JWKS — with bodies the test controls, and it counts the calls
// so a test can say "the userinfo call was never made". The signing keys come from
// jwks_test.go's `idpKeys`, which publishes them at a real endpoint.

type fakeIDP struct {
	server *httptest.Server

	// idToken is what the token endpoint returns as `id_token`; empty means the
	// provider answered with no id_token.
	idToken string
	// userinfo is the userinfo body; nil means the endpoint answers 404.
	userinfo map[string]any

	tokenStatus    int
	userinfoStatus int

	tokenRequests    int
	userinfoRequests int
	lastTokenForm    url.Values
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	idp := &fakeIDP{tokenStatus: http.StatusOK, userinfoStatus: http.StatusOK}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		idp.tokenRequests++
		_ = r.ParseForm()
		idp.lastTokenForm = r.PostForm
		if idp.tokenStatus != http.StatusOK {
			w.WriteHeader(idp.tokenStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "provider-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idp.idToken,
		})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, _ *http.Request) {
		idp.userinfoRequests++
		if idp.userinfoStatus != http.StatusOK || idp.userinfo == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(idp.userinfo)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	idp.server = server
	return idp
}

func (f *fakeIDP) tokenURL() string    { return f.server.URL + "/token" }
func (f *fakeIDP) userInfoURL() string { return f.server.URL + "/userinfo" }

// mint signs an id_token with the provider's key, with the claims a conformant
// provider puts in it.
func mint(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(claims))
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// newTestSSOStore is the account space: an optional seeded account, and the
// built-in roles every boot seeds.
func newTestSSOStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := st.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return st
}

func seedAccount(t *testing.T, st *store.Store, username, email string, roles ...string) *models.User {
	t.Helper()
	user := &models.User{
		Username:     username,
		Email:        email,
		PasswordHash: "$argon2id$local-password",
		Provider:     models.ProviderLocal,
		Status:       models.UserStatusActive,
	}
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if len(roles) > 0 {
		if err := st.Users.SetRoles(user.ID, roles); err != nil {
			t.Fatalf("seed roles: %v", err)
		}
	}
	return user
}

// ssoProvider is a provider row as discovery would have left it: endpoints from
// the document, contract fields at the defaults the migration carries.
func ssoProvider(idp *fakeIDP, keys *idpKeys) *models.SSOProvider {
	return &models.SSOProvider{
		Type:                 models.SSOTypeOIDC,
		Name:                 "corp",
		ClientID:             "nvrway",
		ClientSecret:         "shared-secret",
		Scope:                "openid email profile",
		Enabled:              true,
		Issuer:               "https://idp.example/realnvrway",
		AuthURL:              "https://idp.example/authorize",
		TokenURL:             idp.tokenURL(),
		UserInfoURL:          idp.userInfoURL(),
		JwksURI:              keys.jwksURI(),
		RedirectURI:          "https://nvrway.example/api/auth/sso/corp/callback",
		PKCESupported:        true,
		RequireVerifiedEmail: true,
		LinkPolicy:           models.SSOLinkPolicyPending,
		DiscoveryState:       DiscoveryOK,
	}
}

// verifiedClaims are id_token claims that pass every check jwks.go makes.
func verifiedClaims(now time.Time, issuer, clientID, sub, email, nonce string, verified bool, groups []string) map[string]any {
	claims := map[string]any{
		"iss":            issuer,
		"aud":            clientID,
		"sub":            sub,
		"email":          email,
		"email_verified": verified,
		"name":           "Ada Ops",
		"nonce":          nonce,
		"iat":            now.Add(-time.Minute).Unix(),
		"exp":            now.Add(10 * time.Minute).Unix(),
	}
	if len(groups) > 0 {
		claims["groups"] = groups
	}
	return claims
}

// TestPKCEVerifierChallengePair asserts the S256 pairing and the ranges the spec
// sets: a challenge is a hash, not the verifier, and a verifier inside the length
// window.
func TestPKCEVerifierChallengePair(t *testing.T) {
	verifier, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("verifier length %d is outside the 43-128 range", len(verifier))
	}

	challenge := CodeChallenge(verifier)
	if challenge == verifier {
		t.Fatal("the challenge must not be the verifier: sending it plain defeats PKCE")
	}
	if _, err := base64.RawURLEncoding.DecodeString(challenge); err != nil {
		t.Fatalf("challenge is not base64url: %v", err)
	}
	if want := CodeChallenge(verifier); challenge != want {
		t.Fatal("the challenge is not a deterministic function of the verifier")
	}

	other, _ := NewCodeVerifier()
	if CodeChallenge(other) == challenge {
		t.Fatal("two verifiers produced the same challenge")
	}
}

// TestAuthorizeURLCarriesStateNonceAndS256Challenge is the redirect contract: the
// state is the CSRF binding, the nonce binds the id_token to this login, and the
// challenge is sent as S256 (never `plain`).
func TestAuthorizeURLCarriesStateNonceAndS256Challenge(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	verifier, err := NewCodeVerifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	raw := h.AuthorizeURL(provider, provider.RedirectURI, "state-1", "nonce-1", CodeChallenge(verifier))
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := parsed.Query()

	for key, want := range map[string]string{
		"client_id":             provider.ClientID,
		"redirect_uri":          provider.RedirectURI,
		"response_type":         "code",
		"scope":                 "openid email profile",
		"state":                 "state-1",
		"nonce":                 "nonce-1",
		"code_challenge":        CodeChallenge(verifier),
		"code_challenge_method": "S256",
	} {
		if got := q.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if parsed.Path != "/authorize" {
		t.Fatalf("redirect went to %q", parsed.Path)
	}
}

// TestTokenRequestCarriesTheCodeVerifier: a challenge sent at the redirect is
// useless unless the token request presents the verifier it corresponds to.
func TestTokenRequestCarriesTheCodeVerifier(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	if _, err := h.ExchangeCode(provider, "code-1", provider.RedirectURI, "verifier-1"); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := idp.lastTokenForm.Get("code_verifier"); got != "verifier-1" {
		t.Fatalf("code_verifier = %q, want the verifier that matches the challenge", got)
	}
	if got := idp.lastTokenForm.Get("grant_type"); got != "authorization_code" {
		t.Fatalf("grant_type = %q", got)
	}
	if got := idp.lastTokenForm.Get("redirect_uri"); got != provider.RedirectURI {
		t.Fatalf("redirect_uri = %q, want the redirect the code was minted for", got)
	}
}

// TestProviderDisabledIsRefusedAtCallback: the switch is honored at the callback
// too, so a login in flight when an operator flips it does not complete. It is
// refused before any request leaves for the IdP.
func TestProviderDisabledIsRefusedAtCallback(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.Enabled = false
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if outcome != nil {
		t.Fatal("a disabled provider produced a session")
	}
	if reason := reasonOf(t, err); reason != ReasonProviderDisabled {
		t.Fatalf("reason = %q, want %q", reason, ReasonProviderDisabled)
	}
	if idp.tokenRequests != 0 {
		t.Fatalf("the disabled provider cost %d IdP requests", idp.tokenRequests)
	}
}

// TestHandleCallbackRefusesACodeExchangeFailure names the flow failure, so the
// console shows "code exchange failed" rather than a 500.
func TestHandleCallbackRefusesACodeExchangeFailure(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	idp.tokenStatus = http.StatusBadRequest
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	outcome, err := h.HandleCallback(context.Background(), provider, "expired-code", provider.RedirectURI, "nonce-1", "verifier-1")
	if outcome != nil {
		t.Fatal("a failed code exchange produced a session")
	}
	if reason := reasonOf(t, err); reason != ReasonCodeExchange {
		t.Fatalf("reason = %q, want %q", reason, ReasonCodeExchange)
	}
}

// TestHandleCallbackVerifiesTheIDTokenBeforeTrust is the exit criterion, and the
// headline of the card: the userinfo call and the signed token must name the same
// subject. A provider that concatenates a token minted for one person with a
// profile belonging to another is refused, and the refusal records *who tried to
// arrive as whom*.
func TestHandleCallbackVerifiesTheIDTokenBeforeTrust(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	// The signed token — the strong source — names an attacker's subject...
	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "attacker-9", "ida@corp.example", "nonce-1", true, nil))
	// ...while the userinfo call, which a stolen bearer token controls, names the
	// installation admin.
	idp.userinfo = map[string]any{
		"sub":            "victim-admin",
		"email":          "ops-admin@corp.example",
		"email_verified": true,
	}

	st := h.store
	admin := seedAccount(t, st, "ops-admin", "ops-admin@corp.example", models.RoleAdmin)

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if outcome != nil {
		t.Fatal("the disagreement produced a session: nvrWay picked which identity to believe")
	}
	iderr := identityError(t, err)
	if iderr.Reason != ReasonClaimsDisagree {
		t.Fatalf("reason = %q, want %q", iderr.Reason, ReasonClaimsDisagree)
	}
	if iderr.Subject != "victim-admin" || iderr.Email != "ops-admin@corp.example" {
		t.Fatalf("refusal names %q / %q, want the assertion the attacker aimed at the admin account",
			iderr.Subject, iderr.Email)
	}

	// And the account is untouched: no provider stamped on it, roles intact.
	reloaded, err := st.Users.GetByID(admin.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ProviderID != "" {
		t.Fatal("the disagreement stamped an identity onto the admin account")
	}
	if !hasRole(t, st, reloaded.ID, models.RoleAdmin) {
		t.Fatal("the disagreement changed the admin's roles")
	}
}

// TestHandleCallbackRefusesAMissingIDToken: a provider that publishes a key set
// and answers the token request with none is a provider whose identity nobody
// verified. The flow refuses it rather than falling back to the userinfo call.
func TestHandleCallbackRefusesAMissingIDToken(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	idp.idToken = ""
	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops@corp.example", "email_verified": true}
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if outcome != nil {
		t.Fatal("an unverified identity produced a session")
	}
	if reason := reasonOf(t, err); reason != ReasonMissingIDToken {
		t.Fatalf("reason = %q, want %q", reason, ReasonMissingIDToken)
	}
}

// TestHandleCallbackRefusesANonceFromAnEarlierLogin: the nonce is what keeps a
// token minted for one login from satisfying the next.
func TestHandleCallbackRefusesANonceFromAnEarlierLogin(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "ops@corp.example", "nonce-FROM-THE-PREVIOUS-LOGIN", true, nil))
	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops@corp.example", "email_verified": true}

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if outcome != nil {
		t.Fatal("a replayed login produced a session")
	}
	if reason := reasonOf(t, err); reason != ReasonNonceMismatch {
		t.Fatalf("reason = %q, want %q", reason, ReasonNonceMismatch)
	}
}

// TestHandleCallbackRefusesATokenSignedByAnUnpublishedKey: the key set is fetched,
// not configured, and a token signed by a key the provider does not publish stays
// refused.
func TestHandleCallbackRefusesATokenSignedByAnUnpublishedKey(t *testing.T) {
	idp := newFakeIDP(t)
	published := newIDPKeys(t, "k1")
	ghost := newIDPKeys(t, "k2")
	provider := ssoProvider(idp, published)
	provider.Issuer = published.server.URL + "/realnvrway"
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	// jwks_test.go's fixture mints against a fixed issuer; sign against the
	// provider's, so the only thing wrong with the token is the key.
	idp.idToken = mint(t, ghost.signing["k2"], "k2", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "ops@corp.example", "nonce-1", true, nil))
	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops@corp.example", "email_verified": true}

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if outcome != nil {
		t.Fatal("a token signed by an unpublished key produced a session")
	}
	if reason := reasonOf(t, err); reason != ReasonIDTokenVerification {
		t.Fatalf("reason = %q, want %q", reason, ReasonIDTokenVerification)
	}
}

// TestHandleCallbackRefusesAnAssertionWithNoSubject: `sub` is the only stable
// handle an IdP offers. Without it an assertion cannot be provisioned, linked, or
// audited, and it is exactly what an IdP misconfigured an hour ago sends.
func TestHandleCallbackRefusesAnAssertionWithNoSubject(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.Issuer = ""
	provider.JwksURI = "" // no issuer: no key set, so identity rests on userinfo
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	idp.userinfo = map[string]any{"email": "ops@corp.example", "email_verified": true}

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "", "verifier-1")
	if outcome != nil {
		t.Fatal("an assertion with no subject produced a session")
	}
	if reason := reasonOf(t, err); reason != ReasonSubjectMissing {
		t.Fatalf("reason = %q, want %q", reason, ReasonSubjectMissing)
	}
}

// TestFindEmptySubjectDoesNotResolveALocalAccount is the store's second line: the
// JIT lookup needs both halves, so a local row (provider_id = "") never answers an
// assertion with no subject.
func TestFindEmptySubjectDoesNotResolveALocalAccount(t *testing.T) {
	st := newTestSSOStore(t)
	local := seedAccount(t, st, "ops-admin", "ops-admin@corp.example", models.RoleAdmin)

	user, err := st.Users.FindByProvider("corp", "")
	if err == nil && user != nil {
		t.Fatalf("an empty subject resolved account %q (%s)", user.ID, user.Username)
	}
	if user != nil && user.ID == local.ID {
		t.Fatal("the JIT door opened for an assertion with no subject")
	}
}

// TestHandleCallbackRefusesAnUnverifiedEmailWhenTheProviderRequiresOne: the
// contract's `email_verified` rule, honored rather than parsed and ignored.
func TestHandleCallbackRefusesAnUnverifiedEmailWhenTheProviderRequiresOne(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "ops@corp.example", "nonce-1", false, nil))
	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops@corp.example", "email_verified": true}

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if outcome != nil {
		t.Fatal("an unverified email passed a provider that requires a verified one")
	}
	if reason := reasonOf(t, err); reason != ReasonEmailUnverified {
		t.Fatalf("reason = %q, want %q", reason, ReasonEmailUnverified)
	}
	// The signed token is the source the product believes, and it says unverified.
	if iderr := identityError(t, err); iderr.Email != "ops@corp.example" {
		t.Fatalf("refusal names %q", iderr.Email)
	}
}

// TestHandleCallbackRefusesAnEmailOutsideTheAllowList: a list of domains is a wall.
func TestHandleCallbackRefusesAnEmailOutsideTheAllowList(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.AllowedDomains = "corp.example"
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "ida@other.example", "nonce-1", true, nil))
	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ida@other.example", "email_verified": true}

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if outcome != nil {
		t.Fatal("an identity outside the allow-list produced a session")
	}
	if reason := reasonOf(t, err); reason != ReasonDomainNotAllowed {
		t.Fatalf("reason = %q, want %q", reason, ReasonDomainNotAllowed)
	}
}

// TestHandleCallbackProvisionsAtTheViewerFloor is JIT provisioning: a new identity
// gets an account with the viewer role recorded on it, no password, and the
// assertion as a claim summary — not the raw id_token.
func TestHandleCallbackProvisionsAtTheViewerFloor(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	jwtAuth := auth.NewJWTAuth("sso-test-secret")
	h := NewOAuthHandler(newTestSSOStore(t), jwtAuth)

	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "ops@corp.example", "nonce-1", true, nil))
	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops@corp.example", "email_verified": true, "name": "Ada Ops"}

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if outcome.User == nil {
		t.Fatal("no account")
	}
	if !outcome.CreatedJIT {
		t.Fatal("the account was not recorded as JIT-provisioned")
	}
	if !outcome.VerifiedByIDToken {
		t.Fatal("the login does not record that the identity came from a signed token")
	}
	if !outcome.EmailVerified {
		t.Fatal("the verified-email fact did not reach the outcome")
	}
	if outcome.User.PasswordHash != "" {
		t.Fatal("a JIT account was given a password")
	}
	if outcome.User.Provider != provider.Name || outcome.User.ProviderID != "subject-1" {
		t.Fatalf("account carries %q/%q, want the provider and the asserted subject",
			outcome.User.Provider, outcome.User.ProviderID)
	}
	if !hasRole(t, h.store, outcome.User.ID, models.RoleViewer) {
		t.Fatal("the JIT account is not at the viewer floor")
	}
	if hasRole(t, h.store, outcome.User.ID, models.RoleAdmin) {
		t.Fatal("JIT provisioning granted admin")
	}

	// The session tokens are nvrWay's, carrying the account and its roles.
	claims, err := jwtAuth.Verify(outcome.AccessToken)
	if err != nil {
		t.Fatalf("verify access token: %v", err)
	}
	if claims.UserID != outcome.User.ID || !claims.HasRole(models.RoleViewer) {
		t.Fatalf("access token claims %+v", claims)
	}
	if refresh, err := jwtAuth.VerifyRefresh(outcome.RefreshToken); err != nil || refresh.UserID != outcome.User.ID {
		t.Fatalf("refresh token: %+v err %v", refresh, err)
	}

	// The claim summary is JSON, and the raw id_token is not stored: it is an
	// access credential to another system, and a dump of `users` must not carry it.
	var summary map[string]any
	if err := json.Unmarshal([]byte(outcome.User.SSOAttributes), &summary); err != nil {
		t.Fatalf("sso_attributes is not JSON: %v (%q)", err, outcome.User.SSOAttributes)
	}
	if strings.Contains(outcome.User.SSOAttributes, "eyJ") {
		t.Fatal("the raw id_token was stored in the account record")
	}
}

// TestHandleCallbackReusesTheAccountForTheSameIdentity: the same person signing in
// twice must land on one account.
func TestHandleCallbackReusesTheAccountForTheSameIdentity(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	login := func() *LoginOutcome {
		t.Helper()
		nonce, err := RandomNonce()
		if err != nil {
			t.Fatalf("nonce: %v", err)
		}
		idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
			provider.Issuer, provider.ClientID, "subject-1", "ops@corp.example", nonce, true, nil))
		idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops@corp.example", "email_verified": true}
		outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, nonce, "verifier-1")
		if err != nil {
			t.Fatalf("callback: %v", err)
		}
		return outcome
	}

	first, second := login(), login()
	if first.User.ID != second.User.ID {
		t.Fatalf("one identity produced two accounts: %q then %q", first.User.ID, second.User.ID)
	}
	if second.CreatedJIT {
		t.Fatal("the second login provisioned a second account")
	}
	count, err := h.store.Users.Count()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("%d accounts, want the one JIT account", count)
	}
}

// TestUniqueUsernameCollision: `users.username` is a unique index, and an assertion
// whose name is taken is exactly the case the contract is about — the account gets a
// distinct name, and nothing is overwritten.
func TestUniqueUsernameCollision(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.RequireVerifiedEmail = false
	st := newTestSSOStore(t)
	h := NewOAuthHandler(st, auth.NewJWTAuth("sso-test-secret"))

	taken := seedAccount(t, st, "ops", "ops@local.example")

	// The username claim is the name the product wants; two identities asserting
	// the same one, on different emails, must not collide with each other or with
	// the account that got there first.
	idp.userinfo = map[string]any{
		"sub": "subject-1", "email": "first@corp.example", "email_verified": true,
		"preferred_username": "ops",
	}
	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "first@corp.example", "nonce-1", true, nil))

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if outcome.User.Username == "ops" {
		t.Fatal("the JIT account took a username an account already holds")
	}
	if outcome.User.ID == taken.ID {
		t.Fatal("the JIT login arrived as the existing account")
	}

	// And the next identity with the same asserted name gets a third name, not a
	// collision error.
	provider.Name = "other"
	idp.userinfo = map[string]any{
		"sub": "subject-2", "email": "second@corp.example", "email_verified": true,
		"preferred_username": "ops",
	}
	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-2", "second@corp.example", "nonce-2", true, nil))
	second, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-2", "verifier-1")
	if err != nil {
		t.Fatalf("second callback: %v", err)
	}
	if second.User.Username == "ops" || second.User.Username == outcome.User.Username {
		t.Fatalf("second identity got %q, colliding with %q", second.User.Username, outcome.User.Username)
	}
	if second.User.Username != "ops-3" {
		t.Fatalf("second identity got %q, want the third name in the sequence", second.User.Username)
	}
}

// TestHandleCallbackParksAPendingIdentityForAnExistingEmail is the card's headline:
// a permissive IdP that asserts the email of the installation admin produces a
// ledger row and a pending identity. It does not produce the admin's session, and
// it does not touch the admin's roles.
func TestHandleCallbackParksAPendingIdentityForAnExistingEmail(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys) // link_policy=pending, the default
	st := newTestSSOStore(t)
	h := NewOAuthHandler(st, auth.NewJWTAuth("sso-test-secret"))

	admin := seedAccount(t, st, "ops-admin", "ops-admin@corp.example", models.RoleAdmin)

	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "ops-admin@corp.example", "nonce-1", true, nil))
	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops-admin@corp.example", "email_verified": true}

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if outcome != nil {
		t.Fatal("asserting an existing account's email produced a session")
	}
	iderr := identityError(t, err)
	if iderr.Reason != ReasonIdentityPending {
		t.Fatalf("reason = %q, want %q", iderr.Reason, ReasonIdentityPending)
	}
	if iderr.TargetUserID != admin.ID {
		t.Fatalf("target account = %q, want the account the assertion pointed at", iderr.TargetUserID)
	}
	if iderr.PendingUserID == "" {
		t.Fatal("the refusal does not name the pending identity an admin has to decide")
	}

	pending, err := st.PendingIdentities.GetByID(iderr.PendingUserID)
	if err != nil {
		t.Fatalf("pending identity: %v", err)
	}
	if pending.Provider != provider.Name || pending.ProviderSubject != "subject-1" {
		t.Fatalf("pending row names %q/%q", pending.Provider, pending.ProviderSubject)
	}
	if pending.Email != "ops-admin@corp.example" || pending.MatchedUserID != admin.ID {
		t.Fatalf("pending row points at %q / %q", pending.Email, pending.MatchedUserID)
	}
	if !pending.IsPending() {
		t.Fatalf("pending row status = %q", pending.Status)
	}

	// Nothing that exists was touched.
	reloaded, err := st.Users.GetByID(admin.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Provider == provider.Name {
		t.Fatal("the pending path stamped the provider onto the admin account")
	}
	if !hasRole(t, st, admin.ID, models.RoleAdmin) {
		t.Fatal("the pending path changed the admin's roles")
	}
	count, err := st.Users.Count()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("%d accounts, want the one the operator created", count)
	}
}

// TestLinkPolicyLinkIsTheOperatorsChoice: match-by-email is available per provider,
// it is audited (the outcome says so), and it does not touch roles — linking says
// who the identity is, not what it may do.
func TestLinkPolicyLinkIsTheOperatorsChoice(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.LinkPolicy = models.SSOLinkPolicyLink
	st := newTestSSOStore(t)
	h := NewOAuthHandler(st, auth.NewJWTAuth("sso-test-secret"))

	admin := seedAccount(t, st, "ops-admin", "ops-admin@corp.example", models.RoleAdmin)

	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "ops-admin@corp.example", "nonce-1", true, nil))
	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops-admin@corp.example", "email_verified": true}

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if outcome.User.ID != admin.ID {
		t.Fatalf("login arrived as %q, want the account the operator chose to link", outcome.User.ID)
	}
	if !outcome.LinkedExisting {
		t.Fatal("a link_policy=link login does not record that it linked an existing account")
	}
	if hasRole(t, st, outcome.User.ID, models.RoleViewer) {
		t.Fatal("linking granted a role the operator never mapped")
	}
	if !hasRole(t, st, outcome.User.ID, models.RoleAdmin) {
		t.Fatal("linking changed the account's roles")
	}
	// The identity is stamped, so the next login resolves through case 1.
	reloaded, err := st.Users.GetByID(admin.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Provider != provider.Name || reloaded.ProviderID != "subject-1" {
		t.Fatalf("account carries %q/%q", reloaded.Provider, reloaded.ProviderID)
	}
}

// TestRoleMappingOwnsRoleAssignment: a configured mapping owns role assignment on
// every login, which is what "owns" has to mean — the technician who leaves the
// group stops being admin.
func TestRoleMappingOwnsRoleAssignment(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.RoleMapping = `{"rmm-admins":"admin"}`
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	login := func(groups []string) *LoginOutcome {
		t.Helper()
		idp.userinfo = map[string]any{
			"sub": "subject-1", "email": "ops@corp.example", "email_verified": true, "groups": groups,
		}
		idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
			provider.Issuer, provider.ClientID, "subject-1", "ops@corp.example", "nonce-1", true, groups))
		outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
		if err != nil {
			t.Fatalf("callback: %v", err)
		}
		return outcome
	}

	first := login([]string{"rmm-admins"})
	if !hasRole(t, h.store, first.User.ID, models.RoleAdmin) {
		t.Fatal("the mapping asserted the group and the account is not admin")
	}
	if !hasRole(t, h.store, first.User.ID, models.RoleViewer) {
		t.Fatal("the mapping left the account below the viewer floor")
	}
	if !contains(first.MappedRoles, models.RoleAdmin) {
		t.Fatalf("mapped roles = %v, want admin recorded on the outcome", first.MappedRoles)
	}

	second := login(nil)
	if second.User.ID != first.User.ID {
		t.Fatal("the same identity got a second account")
	}
	if hasRole(t, h.store, second.User.ID, models.RoleAdmin) {
		t.Fatal("the mapping owns role assignment, so leaving the group must end admin")
	}
	if !hasRole(t, h.store, second.User.ID, models.RoleViewer) {
		t.Fatal("a mapping that owns roles must not leave an account with nothing")
	}
}

// TestNoMappingIsNotEveryoneIsAdmin: no mapping is no mapping. The viewer floor
// stands, and a group claim grants nothing on its own.
func TestNoMappingIsNotEveryoneIsAdmin(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.RoleMapping = ""
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	idp.userinfo = map[string]any{
		"sub": "subject-1", "email": "ops@corp.example", "email_verified": true, "groups": []string{"rmm-admins"},
	}
	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "ops@corp.example", "nonce-1", true, []string{"rmm-admins"}))

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if hasRole(t, h.store, outcome.User.ID, models.RoleAdmin) {
		t.Fatal("an unconfigured mapping granted admin")
	}
	if !hasRole(t, h.store, outcome.User.ID, models.RoleViewer) {
		t.Fatal("the viewer floor is missing")
	}
	if len(outcome.MappedRoles) != 0 {
		t.Fatalf("mapped roles = %v, want none", outcome.MappedRoles)
	}
}

// TestMappingCannotQuietlyStripAnUnmappedRole: a mapping owns the roles it names,
// and nothing else.
func TestMappingCannotQuietlyStripAnUnmappedRole(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.RoleMapping = `{"viewers":"viewer"}`
	provider.LinkPolicy = models.SSOLinkPolicyLink
	st := newTestSSOStore(t)
	h := NewOAuthHandler(st, auth.NewJWTAuth("sso-test-secret"))

	operator := seedAccount(t, st, "ops", "ops@corp.example", models.RoleOperator, models.RoleViewer)

	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops@corp.example", "email_verified": true, "groups": []string{"viewers"}}
	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "ops@corp.example", "nonce-1", true, []string{"viewers"}))

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if outcome.User.ID != operator.ID {
		t.Fatalf("login arrived as %q", outcome.User.ID)
	}
	if !hasRole(t, st, operator.ID, models.RoleOperator) {
		t.Fatal("a mapping removed a role it never named")
	}
}

// TestIdentityFromClaimsWhenThereIsNoUserinfoEndpoint: the OIDC code flow allows an
// id_token-only provider, and the token is verified before it is trusted, which is
// what makes the fallback safe.
func TestIdentityFromClaimsWhenThereIsNoUserinfoEndpoint(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.UserInfoURL = ""
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	idp.idToken = mint(t, keys.signing["k1"], "k1", verifiedClaims(time.Now(),
		provider.Issuer, provider.ClientID, "subject-1", "ops@corp.example", "nonce-1", true, nil))

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if outcome.Subject != "subject-1" || outcome.Email != "ops@corp.example" {
		t.Fatalf("identity = %q/%q", outcome.Subject, outcome.Email)
	}
	if !outcome.VerifiedByIDToken {
		t.Fatal("an id_token-only login does not record where the identity came from")
	}
	if idp.userinfoRequests != 0 {
		t.Fatal("the handler called an endpoint the provider does not publish")
	}
}

// TestProviderWithNoKeySetRecordsAnUnverifiedIdentity: an operator who typed the
// endpoints has no key set to check against. The login may proceed, and the outcome
// says the identity rests on the userinfo call — it does not imply a verification
// that did not happen.
func TestProviderWithNoKeySetRecordsAnUnverifiedIdentity(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.Issuer = ""
	provider.JwksURI = ""
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops@corp.example", "email_verified": true}

	outcome, err := h.HandleCallback(context.Background(), provider, "code-1", provider.RedirectURI, "nonce-1", "verifier-1")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if outcome.VerifiedByIDToken {
		t.Fatal("the outcome claims a verification that no key set made possible")
	}
}

// TestUserInfoGroupClaimIsTheMappedClaim: the role mapping reads the claim the
// operator named, from the response the provider published.
func TestUserInfoGroupClaimIsTheMappedClaim(t *testing.T) {
	idp := newFakeIDP(t)
	keys := newIDPKeys(t, "k1")
	provider := ssoProvider(idp, keys)
	provider.ClaimsGroups = "roles"
	h := NewOAuthHandler(newTestSSOStore(t), auth.NewJWTAuth("sso-test-secret"))

	provider.UserInfoURL = idp.userInfoURL()
	idp.userinfo = map[string]any{"sub": "subject-1", "email": "ops@corp.example", "email_verified": true, "roles": []string{"netbox"}}

	info, err := h.GetUserInfo(provider, "provider-access-token")
	if err != nil {
		t.Fatalf("userinfo: %v", err)
	}
	if len(info.Groups) != 1 || info.Groups[0] != "netbox" {
		t.Fatalf("groups = %v, want the mapped claim read as published", info.Groups)
	}
}

// reasonOf / identityError pull the named state out of a refusal, so the tests
// assert the string the console renders rather than a message.
func reasonOf(t *testing.T, err error) string {
	t.Helper()
	return identityError(t, err).Reason
}

func identityError(t *testing.T, err error) *IdentityError {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got a session")
	}
	iderr, ok := err.(*IdentityError)
	if !ok {
		t.Fatalf("refusal is %T (%v), want an *IdentityError with a named reason", err, err)
	}
	return iderr
}

// hasRole reads the grants an account holds, which is the thing the contract keeps
// promising it will not touch.
func hasRole(t *testing.T, st *store.Store, userID, role string) bool {
	t.Helper()
	names, err := st.Users.RolesFor(userID)
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	return contains(names, role)
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
