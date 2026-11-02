// Package provider is nvrWay's in-repo OpenID Provider for the test suite (2.4-1).
//
// It is an implementation of the protocol, not a mock of nvrWay's SSO layer: it
// publishes a discovery document, serves a JWKS, mints RS256-signed id_tokens, runs
// the authorization-code exchange with PKCE, and rotates its signing key on command.
// The tests that matter — "a provider configured by issuer alone logs people in",
// "a token signed by a key the IdP does not publish is refused", "a rotation
// mid-session keeps logins working" — cannot be asserted against a stub of the
// verification code, because a stub of the verifier agrees with whatever the
// verifier does.
//
// Two shapes, one implementation:
//
//   - `New()` starts a provider on a random localhost port for Go tests
//     (`nvr/server/sso/testidp/provider`);
//   - `Listen` serves the same provider on a fixed address with a named issuer,
//     with the consent screen switched on, which is what `../` (the `go run` CLI)
//     uses so a person — or a curl script — can walk authorize → consent →
//     callback → token → userinfo by hand.
//
// nvrWay is OIDC-only, so this fixture has no SAML half.
package provider

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Options configures a provider. Zero values are the conformant defaults.
type Options struct {
	// Issuer is what an operator types into the console, and what the provider
	// publishes as `iss`. Empty means "the URL this server is listening on",
	// which is what `New()` wants: a test cannot know the port in advance.
	Issuer string
	// ClientID/ClientSecret are the confidential-client credentials. When
	// ClientSecret is set the token endpoint refuses a request that presents a
	// different one — the check a real IdP runs, and the reason nvrWay's provider
	// row carries a secret at all.
	ClientID     string
	ClientSecret string
	// Consent draws the "who is signing in, and what is asserted" screen at
	// /authorize instead of redirecting straight back. Off for Go tests (a test
	// drives the flow by URL), on for the CLI (a human walks it).
	Consent bool
	// Claims is the single user this IdP has: the one-user provider an operator
	// signs in to, and the shape most tests want.
	Claims Claims
	// RequirePKCE makes the provider refuse a token request that presents no
	// verifier at all — a strict IdP, and the only way to prove the client sends
	// a verifier rather than proving it sent one that matched.
	RequirePKCE bool
}

// Claims is what the IdP asserts about a user for one login.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Username      string
	Groups        []string
	Nonce         string
	RedirectURI   string
	// Extra carries the claim shapes the mapping must handle: a group claim under
	// a non-standard name, an email claim under `upn`, a user with no email.
	Extra map[string]any
	// IDTokenSubject makes the signed token name a **different** person from the
	// userinfo call — the shape 2.4-2's agreement rule refuses.
	IDTokenSubject string
	// TokenSabotage makes the token endpoint mint an id_token the verifier must
	// refuse: "expired", "foreign_aud", "wrong_key", "none", "hs256",
	// "sub_mismatch", "nonce_swap", "missing".
	TokenSabotage string

	// challenge and nonce are captured from the /authorize request, the way a real
	// IdP captures them: what the client sent at the start of this login is what
	// the token endpoint must bind the code to.
	challenge string
	nonce     string
}

// IdP is a running provider fixture.
type IdP struct {
	// Server is set by New(); nil in Listen mode.
	Server *httptest.Server

	opts Options

	// http is the server in Listen mode (New() uses httptest.Server instead).
	http *http.Server

	mu     sync.Mutex
	key    *rsa.PrivateKey
	kid    string
	pub    map[string]*rsa.PublicKey // published JWKS, by kid
	codes  map[string]*Claims
	tokens map[string]*Claims
	// docBreak is the sabotage asked for by a discovery test.
	docBreak string
	// defaultClaims answers any /authorize the fixture has not seen a code for:
	// the console generates its CSRF state inside the authorize redirect, so a
	// test cannot know it in advance.
	defaultClaims *Claims
	// lastChallenge and verifierOK record what the token endpoint saw, so a test
	// asserts that the client presented a verifier instead of asserting that a
	// stub of itself did.
	lastChallenge string
	verifierOK    bool
	requirePKCE   bool
	// fetches counts JWKS requests: the number the refresh rule is about.
	fetches int
}

// New starts a provider on a random localhost port, with the Options' defaults.
func New(opts Options) *IdP {
	p := newIdP(opts)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.handleDiscovery)
	mux.HandleFunc("/jwks", p.handleJWKS)
	mux.HandleFunc("/authorize", p.handleAuthorize)
	mux.HandleFunc("/consent", p.handleConsent)
	mux.HandleFunc("/token", p.handleToken)
	mux.HandleFunc("/userinfo", p.handleUserInfo)
	mux.HandleFunc("/callback", p.handleCallbackEcho)
	p.Server = httptest.NewServer(mux)
	if strings.TrimSpace(opts.Issuer) == "" {
		p.opts.Issuer = p.Server.URL
	}
	return p
}

// Listen serves the provider on `addr` (":9500") for as long as the caller wants,
// with the issuer it names. It returns the provider and the listener's address.
//
// This is the CLI's path: a fixed port, a fixed issuer, and the consent screen on,
// so the flow is walkable with a browser or with curl.
func Listen(addr string, opts Options) (*IdP, net.Addr, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	p := newIdP(opts)

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.handleDiscovery)
	mux.HandleFunc("/jwks", p.handleJWKS)
	mux.HandleFunc("/authorize", p.handleAuthorize)
	mux.HandleFunc("/consent", p.handleConsent)
	mux.HandleFunc("/token", p.handleToken)
	mux.HandleFunc("/userinfo", p.handleUserInfo)
	// The CLI's redirect sink: the walk needs a redirect_uri that answers, and a
	// test-IdP that hosts its own callback is the honest place for it.
	mux.HandleFunc("/callback", p.handleCallbackEcho)

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	p.http = server
	return p, listener.Addr(), nil
}

func newIdP(opts Options) *IdP {
	p := &IdP{
		opts:        opts,
		pub:         make(map[string]*rsa.PublicKey),
		codes:       make(map[string]*Claims),
		tokens:      make(map[string]*Claims),
		requirePKCE: opts.RequirePKCE,
	}
	if p.opts.Issuer != "" {
		p.opts.Issuer = strings.TrimRight(p.opts.Issuer, "/")
	}
	if p.opts.ClientID == "" {
		p.opts.ClientID = "nvrdev"
	}
	claims := opts.Claims
	if claims.Subject == "" {
		claims.Subject = "nvr-user-1"
	}
	if claims.Email == "" {
		claims.Email = "ops@nvrway.test"
	}
	if claims.Name == "" {
		claims.Name = "Ada Ops"
	}
	p.defaultClaims = &claims

	key, kid := mustKey()
	p.key, p.kid = key, kid
	p.pub[kid] = &key.PublicKey
	return p
}

// Close stops a provider started by New().
func (p *IdP) Close() {
	if p.Server != nil {
		p.Server.Close()
	}
	if p.http != nil {
		_ = p.http.Close()
	}
}

// Issuer is what an operator types into the console.
func (p *IdP) Issuer() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.opts.Issuer != "" {
		return p.opts.Issuer
	}
	if p.Server != nil {
		return p.Server.URL
	}
	return ""
}

// URL is a path on the provider (its authorize endpoint, for a scripted walk).
func (p *IdP) URL(path string) string { return p.Issuer() + path }

// SeedCode registers an authorization code the provider will hand out at
// /authorize for the state a client sends, carrying `c`. Returning the code the
// /authorize handler will emit lets a test drive the real redirect.
func (p *IdP) SeedCode(code string, c Claims) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	claims := c
	p.codes[code] = &claims
	return code
}

// SeedLogin makes the IdP answer any authorization request with this user, so a
// test can walk the flow without knowing the CSRF state the console generated.
func (p *IdP) SeedLogin(c Claims) {
	p.mu.Lock()
	defer p.mu.Unlock()
	claims := c
	p.defaultClaims = &claims
}

// Rotate mints a new signing key and publishes both keys — the state an IdP is in
// for the window after a rotation.
func (p *IdP) Rotate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	key, kid := mustKey()
	p.key, p.kid = key, kid
	p.pub[kid] = &key.PublicKey
}

// PublishOnlyCurrentKey drops the previous key from the published set: the state
// after the rotation window has closed.
func (p *IdP) PublishOnlyCurrentKey() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pub = map[string]*rsa.PublicKey{p.kid: &p.key.PublicKey}
}

// BreakDocument sabotages the discovery document for a named test case: "no_jwks"
// (drops jwks_uri), "issuer" (names a different issuer), "no_code" (offers no
// authorization-code flow), "garbage" (not JSON), "off" (no answer), "es256_only"
// (publishes an id_token algorithm the RS256-only verifier does not accept).
func (p *IdP) BreakDocument(kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.docBreak = kind
}

// JWKSFetches counts the GETs this provider has answered, which is the number the
// refresh-on-unknown-kid rule is about.
func (p *IdP) JWKSFetches() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetches
}

func (p *IdP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	kind := p.docBreak
	issuer := p.issuerLocked()
	p.mu.Unlock()

	if kind == "off" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if kind == "garbage" {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>login</html>"))
		return
	}

	doc := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"userinfo_endpoint":                     issuer + "/userinfo",
		"jwks_uri":                              issuer + "/jwks",
		"response_types_supported":              []string{"code", "id_token"},
		"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
		"code_challenge_methods_supported":      []string{"S256"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	switch kind {
	case "no_jwks":
		delete(doc, "jwks_uri")
	case "issuer":
		doc["issuer"] = "https://some-other-idp.example"
	case "no_code":
		doc["response_types_supported"] = []string{"id_token"}
	case "es256_only":
		// A provider that publishes an algorithm the verifier does not accept
		// (2.4-2 is RS256-only): conformant, and unusable, and the preview is
		// where the operator learns which of the two it is.
		doc["id_token_signing_alg_values_supported"] = []string{"ES256"}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (p *IdP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	p.fetches++
	keys := make([]map[string]any, 0, len(p.pub))
	for kid, pub := range p.pub {
		keys = append(keys, map[string]any{
			"kid": kid,
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		})
	}
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

// handleAuthorize is the browser's redirect. It validates client_id,
// response_type and the redirect URI the seeded login named, captures the login's
// PKCE challenge and nonce, and then — depending on `Consent` — either sends the
// user straight back with a code, or shows them who is about to be asserted.
func (p *IdP) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state") // the fixture uses state as the code selector
	redirect := q.Get("redirect_uri")

	claims, err := p.prepareLogin(state, q)
	if err != nil {
		redirectTo(w, r, redirect, state, "error", err.Error())
		return
	}
	if !p.opts.Consent {
		p.issueCode(w, r, state, redirect, claims)
		return
	}

	http.SetCookie(w, &http.Cookie{Name: "testidp_state", Value: state, Path: "/"})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentPage.Execute(w, consentData{
		Issuer:     p.Issuer(),
		State:      state,
		ClientID:   q.Get("client_id"),
		Redirect:   redirect,
		Subject:    claims.Subject,
		Email:      claims.Email,
		Name:       claims.Name,
		Verified:   claims.EmailVerified,
		Groups:     strings.Join(claims.Groups, ", "),
		ApproveURL: p.Issuer() + "/consent?approve=" + url.QueryEscape(state),
	})
}

// prepareLogin resolves the state to the claims this login will be asserted with,
// applying the checks a real IdP applies before it shows a consent screen.
func (p *IdP) prepareLogin(state string, q url.Values) (*Claims, error) {
	if q.Get("response_type") != "code" {
		return nil, fmt.Errorf("unsupported_response_type")
	}
	if clientID := q.Get("client_id"); p.opts.ClientID != "" && clientID != p.opts.ClientID {
		return nil, fmt.Errorf("unauthorized_client")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	claims, ok := p.codes[state]
	if !ok && p.defaultClaims != nil {
		// Same user, new login: mint a claims record for this state so the token
		// endpoint can serve it.
		minted := *p.defaultClaims
		p.codes[state] = &minted
		claims, ok = &minted, true
	}
	if !ok {
		return nil, fmt.Errorf("invalid_request")
	}
	if claims.RedirectURI != "" && claims.RedirectURI != q.Get("redirect_uri") {
		return nil, fmt.Errorf("invalid_redirect_uri")
	}
	if claims.RedirectURI == "" {
		// The login's redirect: where the code goes, and what the token endpoint
		// has to see again. A seeded login may pin one (the invalid_redirect_uri
		// case); the default user goes where the client asked to go.
		claims.RedirectURI = q.Get("redirect_uri")
	}
	// The fixture binds the login's PKCE challenge and nonce to the code, the way
	// a real IdP does: the client sent them at the start of this login, and the
	// token endpoint is where they have to come back (2.4-2).
	claims.challenge = q.Get("code_challenge")
	if n := q.Get("nonce"); n != "" {
		claims.nonce = n
	}
	return claims, nil
}

// issueCode sends the user back to the client with a code, bound to the login's
// challenge and nonce.
func (p *IdP) issueCode(w http.ResponseWriter, r *http.Request, state, redirect string, claims *Claims) {
	realCode := "code-" + state
	p.mu.Lock()
	p.tokens["at-"+realCode] = claims
	p.mu.Unlock()
	redirectTo(w, r, redirect, state, "code", realCode)
}

func redirectTo(w http.ResponseWriter, r *http.Request, redirect, state, key, value string) {
	target := redirect + "?" + key + "=" + url.QueryEscape(value) + "&state=" + url.QueryEscape(state)
	http.Redirect(w, r, target, http.StatusFound)
}

// handleConsent is the "yes, and here is what is asserted" step. `GET
// /consent?approve=<state>` is the scriptable shape of it: a curl walk needs one
// URL, not a form to parse.
func (p *IdP) handleConsent(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("approve")
	if state == "" {
		if cookie, err := r.Cookie("testidp_state"); err == nil {
			state = cookie.Value
		}
	}
	if posted := r.FormValue("state"); posted != "" {
		state = posted // a POSTed form wins over the cookie
	}
	if state == "" {
		state = r.URL.Query().Get("state")
	}
	if state == "" {
		http.Error(w, "no login is pending", http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	claims, ok := p.codes[state]
	p.mu.Unlock()
	if !ok {
		http.Error(w, "that login is gone (a code is single use)", http.StatusBadRequest)
		return
	}

	claims.EmailVerified = r.FormValue("email_verified") != "false"
	redirect := r.FormValue("redirect_uri")
	if redirect == "" {
		redirect = claims.RedirectURI
	}
	p.issueCode(w, r, state, redirect, claims)
}

// handleCallbackEcho is the CLI's redirect sink: it answers the redirect_uri the
// walk uses, so a scripted run can show the code arriving.
func (p *IdP) handleCallbackEcho(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":  r.URL.Query().Get("code"),
		"state": r.URL.Query().Get("state"),
		"error": r.URL.Query().Get("error"),
	})
}

func (p *IdP) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.Form.Get("grant_type") != "authorization_code" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"unsupported_grant_type"}`))
		return
	}

	// The confidential-client check: nvrWay's provider row carries a client
	// secret, so the fixture has to honor it, and a walk that posts the wrong one
	// has to fail the way a real IdP fails it.
	if p.opts.ClientSecret != "" && r.Form.Get("client_secret") != p.opts.ClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return
	}

	realCode := r.Form.Get("code")
	claims := p.peekCode(realCode)
	if claims == nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	clientID := r.Form.Get("client_id")

	// PKCE, enforced: a code issued with a challenge is only spendable by the
	// client that can present the verifier it corresponds to.
	challenge := claims.challenge
	p.mu.Lock()
	p.lastChallenge = challenge
	requireVerifier := challenge != "" || p.requirePKCE
	p.mu.Unlock()
	if requireVerifier {
		verifier := r.Form.Get("code_verifier")
		matched := verifier != "" && (challenge == "" || s256(verifier) == challenge)
		p.mu.Lock()
		p.verifierOK = matched
		p.mu.Unlock()
		if !matched {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"code_verifier does not match the challenge the code was issued with"}`))
			return
		}
	}

	// The code is spent only by a request that passed every check: a code an
	// attacker holds must not be burnable by the one attempt that failed PKCE.
	p.consumeCode(stateOf(realCode))

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"access_token": "at-" + realCode,
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if idToken := p.MintFor(*claims, clientID); idToken != "" {
		resp["id_token"] = idToken
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// MintFor mints the id_token this login's token response carries, honoring
// `TokenSabotage` — the fixture's way of producing the tokens 2.4-2's acceptance
// criteria name: expired, minted for another client, signed by an unpublished key,
// `alg: none`, HS256 against an RSA set, a subject that disagrees with userinfo, a
// nonce from an earlier login, and no id_token at all.
func (p *IdP) MintFor(c Claims, clientID string) string {
	minted := c
	if c.nonce != "" {
		minted.Nonce = c.nonce
	}
	switch c.TokenSabotage {
	case "missing":
		return ""
	case "nonce_swap":
		// A token minted for some earlier login: same subject, same audience, a
		// nonce that is not the one this authorize redirect sent.
		minted.Nonce = "a-nonce-from-another-login"
		return p.MintIDToken(minted, clientID, "RS256")
	case "expired":
		return p.MintExpiredIDToken(minted, clientID)
	case "foreign_aud":
		return p.MintIDToken(minted, "some-other-client", "RS256")
	case "wrong_key":
		return p.MintIDTokenWithKey(minted, clientID)
	case "none":
		return p.MintIDToken(minted, clientID, "none")
	case "hs256":
		return p.MintIDToken(minted, clientID, "HS256")
	case "sub_mismatch":
		sub := c.IDTokenSubject
		if sub == "" {
			sub = "signed-as-" + c.Subject
		}
		minted.Subject = sub
		return p.MintIDToken(minted, clientID, "RS256")
	default:
		return p.MintIDToken(minted, clientID, "RS256")
	}
}

// LastCodeChallenge is the PKCE challenge the last /authorize carried, empty when
// the client sent none.
func (p *IdP) LastCodeChallenge() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastChallenge
}

// VerifierAccepted reports whether the last token request presented a verifier
// that matched the challenge.
func (p *IdP) VerifierAccepted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verifierOK
}

// RequirePKCE makes the provider refuse a token request that presents no verifier
// at all — a strict IdP, and the only way to prove the client sends one.
func (p *IdP) RequirePKCE() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requirePKCE = true
}

// s256 is the PKCE S256 transform, computed here so the fixture can check a
// verifier without importing the code under test.
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// peekCode reads a code's claims without spending it: the checks that can refuse a
// token request (PKCE) run before the code is consumed.
func (p *IdP) peekCode(realCode string) *Claims {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.codes[strings.TrimPrefix(realCode, "code-")]
}

// consumeCode spends a code.
func (p *IdP) consumeCode(state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.codes, state)
}

func stateOf(realCode string) string { return strings.TrimPrefix(realCode, "code-") }

// DefaultClaims returns the user this IdP asserts, as it normalised it at boot.
func (p *IdP) DefaultClaims() Claims {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.defaultClaims == nil {
		return Claims{}
	}
	return *p.defaultClaims
}

func (p *IdP) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	at := strings.TrimPrefix(authz, "Bearer ")
	p.mu.Lock()
	claims, ok := p.tokens[at]
	p.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	out := map[string]any{
		"sub":            claims.Subject,
		"name":           claims.Name,
		"email":          claims.Email,
		"email_verified": claims.EmailVerified,
	}
	if claims.Username != "" {
		out["preferred_username"] = claims.Username
	}
	if len(claims.Groups) > 0 {
		out["groups"] = claims.Groups
	}
	for k, v := range claims.Extra {
		out[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// MintIDToken signs an id_token for these claims with the current key. `alg` lets
// a test produce a token the verifier must refuse ("none", "HS256").
//
// `clientID` becomes `aud`: a token minted for another client is the fixture's way
// of producing the foreign-audience case.
func (p *IdP) MintIDToken(c Claims, clientID, alg string) string {
	p.mu.Lock()
	key, kid, issuer := p.key, p.kid, p.issuerLocked()
	p.mu.Unlock()

	now := time.Now()
	claims := jwt.MapClaims{
		"iss":            issuer,
		"sub":            c.Subject,
		"aud":            clientID,
		"exp":            now.Add(time.Hour).Unix(),
		"iat":            now.Add(-time.Second).Unix(),
		"auth_time":      now.Unix(),
		"name":           c.Name,
		"email":          c.Email,
		"email_verified": c.EmailVerified,
	}
	if c.Username != "" {
		claims["preferred_username"] = c.Username
	}
	if len(c.Groups) > 0 {
		claims["groups"] = c.Groups
	}
	if c.Nonce != "" {
		claims["nonce"] = c.Nonce
	}
	for k, v := range c.Extra {
		claims[k] = v
	}

	switch alg {
	case "none":
		// The unsigned-token case: header alg=none, empty signature.
		return unsignedJWS(kid, claims)
	case "HS256":
		// The algorithm-confusion case: a token "signed" with a shared secret,
		// whose header names an algorithm the verifier must not honour.
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		token.Header["kid"] = kid
		signed, _ := token.SignedString([]byte("not-the-provider-key"))
		return signed
	default:
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = kid
		signed, err := token.SignedString(key)
		if err != nil {
			panic(fmt.Sprintf("sign: %v", err))
		}
		return signed
	}
}

// MintExpiredIDToken mints a token whose `exp` is in the past.
func (p *IdP) MintExpiredIDToken(c Claims, clientID string) string {
	p.mu.Lock()
	key, kid, issuer := p.key, p.kid, p.issuerLocked()
	p.mu.Unlock()

	now := time.Now().Add(-2 * time.Hour)
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": issuer, "sub": c.Subject, "aud": clientID,
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
		"email": c.Email, "email_verified": c.EmailVerified,
	})
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		panic(fmt.Sprintf("sign: %v", err))
	}
	return signed
}

// MintIDTokenWithKey signs with a key the provider does **not** publish, so the
// verifier sees a kid it cannot resolve: the "signed by the wrong key" case.
func (p *IdP) MintIDTokenWithKey(c Claims, clientID string) string {
	rogue, kid := mustKey()
	p.mu.Lock()
	issuer := p.issuerLocked()
	p.mu.Unlock()

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": issuer, "sub": c.Subject, "aud": clientID,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Second).Unix(),
		"email": c.Email, "email_verified": c.EmailVerified,
	})
	token.Header["kid"] = kid
	signed, err := token.SignedString(rogue)
	if err != nil {
		panic(fmt.Sprintf("sign: %v", err))
	}
	return signed
}

// issuerLocked reads the issuer with the lock already held (the mint helpers take
// the lock themselves for the key).
func (p *IdP) issuerLocked() string {
	if p.opts.Issuer != "" {
		return p.opts.Issuer
	}
	if p.Server != nil {
		return p.Server.URL
	}
	return ""
}

func unsignedJWS(kid string, claims jwt.MapClaims) string {
	headerJSON, _ := json.Marshal(map[string]string{"alg": "none", "kid": kid})
	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func mustKey() (*rsa.PrivateKey, string) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key, fmt.Sprintf("kid-%d", time.Now().UnixNano())
}

// The consent screen. It is not decoration: the fixture's whole value for a
// scripted walk is that a person sees *what the IdP is about to assert*, because
// the SSO contract's headline test is an IdP asserting someone's email.
type consentData struct {
	Issuer     string
	State      string
	ClientID   string
	Redirect   string
	Subject    string
	Email      string
	Name       string
	Verified   bool
	Groups     string
	ApproveURL string
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>testidp consent</title></head>
<body>
<h1>Signing in to {{.ClientID}}</h1>
<p>The provider <code>{{.Issuer}}</code> is about to assert:</p>
<ul>
  <li>subject <code>{{.Subject}}</code></li>
  <li>email <code>{{.Email}}</code> (email_verified: {{.Verified}})</li>
  <li>name <code>{{.Name}}</code></li>
  <li>groups <code>{{.Groups}}</code></li>
</ul>
<form method="post" action="/consent">
  <input type="hidden" name="state" value="{{.State}}">
  <input type="hidden" name="redirect_uri" value="{{.Redirect}}">
  <button type="submit" name="email_verified" value="true">Approve</button>
  <button type="submit" name="email_verified" value="false">Approve as unverified</button>
</form>
<p>Scripted: <a href="{{.ApproveURL}}">{{.ApproveURL}}</a></p>
</body>
</html>
`))
