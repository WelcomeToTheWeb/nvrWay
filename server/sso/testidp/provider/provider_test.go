package provider_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"nvr/server/sso"
	"nvr/server/sso/testidp/provider"
)

// The fixture is only worth having if it behaves like a provider, so these tests
// drive it over HTTP and verify its tokens with the code under test (`sso`), not
// with a copy of the verification rules.

// The verifier and its S256 challenge. The challenge is computed, not remembered:
// the pair RFC 7636 prints is the one a fixture author remembers, and a fixture
// whose constants do not match its own transform fails the flow it exists to prove.
const (
	testVerifier  = "dBjJhvNaTfJmYEUPuDaFHjFDBUC2QnKe9LZDBcQKmvk"
	testChallenge = "QRy8Cz78gnbyjv7z_qjaOBueDRH805HXtQO9iDyW3aU"
)

func start(t *testing.T, opts provider.Options) *provider.IdP {
	t.Helper()
	if opts.ClientID == "" {
		opts.ClientID = "nvrdev"
	}
	idp := provider.New(opts)
	t.Cleanup(idp.Close)
	return idp
}

func get(t *testing.T, client *http.Client, url string) (int, []byte, *http.Response) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, body, resp
}

// TestDiscoveryDocumentIsConformant: the document is what discovery.go is written
// to read, so every member it validates has to be published.
func TestDiscoveryDocumentIsConformant(t *testing.T) {
	idp := start(t, provider.Options{})

	status, body, _ := get(t, idp.Server.Client(), idp.URL("/.well-known/openid-configuration"))
	if status != http.StatusOK {
		t.Fatalf("discovery answered %d", status)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("discovery document is not JSON: %v", err)
	}

	for _, key := range []string{
		"issuer", "authorization_endpoint", "token_endpoint",
		"userinfo_endpoint", "jwks_uri", "response_types_supported",
		"code_challenge_methods_supported", "id_token_signing_alg_values_supported",
	} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("document has no %s: %s", key, body)
		}
	}
	if doc["issuer"] != idp.Issuer() {
		t.Fatalf("issuer = %v, want %q", doc["issuer"], idp.Issuer())
	}
	for _, key := range []string{"authorization_endpoint", "token_endpoint", "userinfo_endpoint", "jwks_uri"} {
		if !strings.HasPrefix(doc[key].(string), idp.Issuer()) {
			t.Fatalf("%s = %v is not under the issuer", key, doc[key])
		}
	}

	// The document discovery.go accepts, read by discovery.go itself: this is the
	// "configure a provider by naming its issuer" claim, end to end.
	discovered, err := sso.Discover(idp.Server.Client(), idp.Issuer(), "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if discovered.JwksURI == "" || !discovered.PKCESupported() || !discovered.ScopeSupports("email") {
		t.Fatalf("discovery read %+v", discovered)
	}
	if !discovered.SupportsSigningAlg("RS256") {
		t.Fatal("the provider does not publish RS256, which is the only algorithm the verifier speaks")
	}
}

// TestBreakDocumentProducesEachNamedDiscoveryFailure is the operator-facing half:
// the console shows one state per failure shape, and the fixture has to be able to
// produce every one of them.
func TestBreakDocumentProducesEachNamedDiscoveryFailure(t *testing.T) {
	cases := map[string]string{
		"no_jwks":    sso.DiscoveryBadDocument,
		"issuer":     sso.DiscoveryIssuerMismatch,
		"no_code":    sso.DiscoveryNoCodeFlow,
		"garbage":    sso.DiscoveryBadDocument,
		"off":        sso.DiscoveryUnreachable,
		"es256_only": "", // conformant document, unusable provider: the preview's job
	}
	for kind, wantKind := range cases {
		idp := start(t, provider.Options{})
		idp.BreakDocument(kind)

		_, err := sso.Discover(idp.Server.Client(), idp.Issuer(), "")
		if wantKind == "" {
			if err != nil {
				t.Fatalf("%s: unexpected failure %v", kind, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: discovery accepted the sabotaged document", kind)
		}
		var discErr *sso.DiscoveryError
		if !asDiscovery(err, &discErr) {
			t.Fatalf("%s: refusal is %T, want *sso.DiscoveryError", kind, err)
		}
		if discErr.Kind != wantKind {
			t.Fatalf("%s: kind = %q, want %q", kind, discErr.Kind, wantKind)
		}
	}
}

func asDiscovery(err error, target **sso.DiscoveryError) bool {
	if de, ok := err.(*sso.DiscoveryError); ok {
		*target = de
		return true
	}
	return false
}

// TestJWKSPublishesRSAKeys: the key set is fetched, not configured, and it has to
// be the shape sso/jwks.go assembles.
func TestJWKSPublishesRSAKeys(t *testing.T) {
	idp := start(t, provider.Options{})

	status, body, _ := get(t, idp.Server.Client(), idp.URL("/jwks"))
	if status != http.StatusOK {
		t.Fatalf("jwks answered %d", status)
	}
	var doc struct {
		Keys []struct {
			Kid, Kty, Use, Alg, N, E string
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("jwks is not JSON: %v", err)
	}
	if len(doc.Keys) != 1 {
		t.Fatalf("%d keys published, want one", len(doc.Keys))
	}
	key := doc.Keys[0]
	if key.Kty != "RSA" || key.Use != "sig" || key.Alg != "RS256" || key.Kid == "" {
		t.Fatalf("published key %+v", key)
	}
	if _, err := base64.RawURLEncoding.DecodeString(key.N); err != nil {
		t.Fatalf("modulus is not base64url: %v", err)
	}

	// The probe an operator's test-connection runs.
	if err := sso.NewJWKSClient(idp.Server.Client()).Probe(idp.URL("/jwks")); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// TestConsentWalkCompletes is the card's exit criterion, run in-process:
// authorize -> consent -> callback -> token -> userinfo, with cookies, following
// redirects the way a browser does.
func TestConsentWalkCompletes(t *testing.T) {
	idp := start(t, provider.Options{Consent: true})
	client := redirectClient(t, idp)
	redirect := idp.URL("/callback")

	authorize := idp.URL("/authorize?client_id=nvrdev&response_type=code&redirect_uri=" +
		url.QueryEscape(redirect) + "&state=state-1&scope=openid%20email%20profile" +
		"&nonce=nonce-1&code_challenge_method=S256&code_challenge=" + url.QueryEscape(testChallenge))

	status, body, _ := get(t, client, authorize)
	if status != http.StatusOK {
		t.Fatalf("authorize answered %d", status)
	}
	if !strings.Contains(string(body), "consent") || !strings.Contains(string(body), "ops@nvrway.test") {
		t.Fatalf("authorize did not show the consent screen: %s", body)
	}

	status, body, _ = get(t, client, idp.URL("/consent?approve=state-1"))
	if status != http.StatusOK {
		t.Fatalf("consent answered %d", status)
	}
	if !strings.Contains(string(body), `"code":"code-state-1"`) {
		t.Fatalf("consent did not land on the callback with the code: %s", body)
	}

	tokens := postForm(t, client, idp.URL("/token"), url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-state-1"},
		"client_id":     {"nvrdev"},
		"client_secret": {"devsecret"},
		"redirect_uri":  {redirect},
		"code_verifier": {testVerifier},
	})
	if tokens["id_token"] == nil || tokens["access_token"] == nil {
		t.Fatalf("token response %+v", tokens)
	}

	status, body, _ = userInfo(t, client, idp, tokens["access_token"].(string))
	if status != http.StatusOK {
		t.Fatalf("userinfo answered %d: %s", status, body)
	}
	var info map[string]any
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("userinfo is not JSON: %v", err)
	}
	if info["sub"] != "nvr-user-1" || info["email"] != "ops@nvrway.test" {
		t.Fatalf("userinfo asserts %+v", info)
	}
	if !idp.VerifierAccepted() {
		t.Fatal("the provider did not accept the verifier that matches the challenge")
	}
	// The provider recorded the challenge this login sent, and it is the one the
	// verifier corresponds to: the fixture's PKCE check is real, not a formality.
	if got := idp.LastCodeChallenge(); got != testChallenge {
		t.Fatalf("the provider recorded challenge %q, want the one the client sent", got)
	}

	// And the id_token that walk minted verifies against the published keys, with
	// the nonce this login sent: the fixture is a provider the verifier accepts.
	claims, err := sso.NewJWKSClient(idp.Server.Client()).VerifyIDToken(
		idp.URL("/jwks"), idp.Issuer(), "nvrdev", tokens["id_token"].(string))
	if err != nil {
		t.Fatalf("verify id_token: %v", err)
	}
	if claims["sub"] != "nvr-user-1" || claims["nonce"] != "nonce-1" {
		t.Fatalf("signed claims %+v", claims)
	}
}

// TestTokenRequestWithoutVerifierIsRefused: a code issued with a challenge is only
// spendable by the client that can present the verifier it corresponds to.
func TestTokenRequestWithoutVerifierIsRefused(t *testing.T) {
	idp := start(t, provider.Options{})
	client := idp.Server.Client()

	_, _, _ = get(t, client, idp.URL("/authorize?client_id=nvrdev&response_type=code&redirect_uri="+
		url.QueryEscape(idp.URL("/callback"))+"&state=state-2&code_challenge="+url.QueryEscape(testChallenge)+
		"&code_challenge_method=S256"))

	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {"code-state-2"},
		"client_id":    {"nvrdev"},
		"redirect_uri": {idp.URL("/callback")},
	}
	// No code_verifier at all.
	status, body := postFormRaw(t, client, idp.URL("/token"), form)
	if status == http.StatusOK {
		t.Fatalf("a code minted with a challenge was spent without a verifier: %s", body)
	}
	if !strings.Contains(string(body), "invalid_grant") {
		t.Fatalf("token answered %d %s, want invalid_grant", status, body)
	}
	if idp.VerifierAccepted() {
		t.Fatal("the provider recorded a verifier it did not receive")
	}
}

// TestRequirePKCERefusesAChallengeFreeLogin is the strict-IdP setting, and the only
// way to prove a client sends a verifier at all.
func TestRequirePKCERefusesAChallengeFreeLogin(t *testing.T) {
	idp := start(t, provider.Options{RequirePKCE: true})
	client := idp.Server.Client()

	_, _, _ = get(t, client, idp.URL("/authorize?client_id=nvrdev&response_type=code&redirect_uri="+
		url.QueryEscape(idp.URL("/callback"))+"&state=state-3"))

	status, body := postFormRaw(t, client, idp.URL("/token"), url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {"code-state-3"},
		"client_id":    {"nvrdev"},
		"redirect_uri": {idp.URL("/callback")},
	})
	if status == http.StatusOK {
		t.Fatalf("a strict provider accepted a token request with no verifier: %s", body)
	}

	tokens := postForm(t, client, idp.URL("/token"), url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-state-3"},
		"client_id":     {"nvrdev"},
		"client_secret": {"devsecret"},
		"redirect_uri":  {idp.URL("/callback")},
		"code_verifier": {testVerifier},
	})
	if tokens["id_token"] == nil {
		t.Fatal("a strict provider refused the verifier that satisfies its rule")
	}
}

// TestClientSecretIsEnforced: nvrWay's provider row carries a client secret, so the
// fixture has to refuse a request that presents a different one.
func TestClientSecretIsEnforced(t *testing.T) {
	idp := start(t, provider.Options{ClientSecret: "devsecret"})
	client := idp.Server.Client()

	_, _, _ = get(t, client, idp.URL("/authorize?client_id=nvrdev&response_type=code&redirect_uri="+
		url.QueryEscape(idp.URL("/callback"))+"&state=state-4&code_challenge="+url.QueryEscape(testChallenge)))

	status, body := postFormRaw(t, client, idp.URL("/token"), url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-state-4"},
		"client_id":     {"nvrdev"},
		"client_secret": {"the-wrong-secret"},
		"redirect_uri":  {idp.URL("/callback")},
		"code_verifier": {testVerifier},
	})
	if status == http.StatusOK {
		t.Fatalf("the provider accepted a wrong client secret: %s", body)
	}
	if !strings.Contains(string(body), "invalid_client") {
		t.Fatalf("token answered %d %s, want invalid_client", status, body)
	}
}

// TestCodeIsSingleUse: an intercepted code must not be spendable twice.
func TestCodeIsSingleUse(t *testing.T) {
	idp := start(t, provider.Options{})
	client := idp.Server.Client()

	_, _, _ = get(t, client, idp.URL("/authorize?client_id=nvrdev&response_type=code&redirect_uri="+
		url.QueryEscape(idp.URL("/callback"))+"&state=state-5&code_challenge="+url.QueryEscape(testChallenge)))

	sp := func() map[string]any {
		return postForm(t, client, idp.URL("/token"), url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {"code-state-5"},
			"client_id":     {"nvrdev"},
			"client_secret": {"devsecret"},
			"redirect_uri":  {idp.URL("/callback")},
			"code_verifier": {testVerifier},
		})
	}
	first := sp()
	if first["access_token"] == nil {
		t.Fatalf("first spend failed: %+v", first)
	}
	second := sp()
	if second["access_token"] != nil {
		t.Fatal("the same code was spendable twice")
	}
}

// TestSabotageShapesProduceTheTokensTheVerifierMustRefuse: every shape 2.4-2's
// acceptance criteria name, checked by handing it to the code under test.
func TestSabotageShapesProduceTheTokensTheVerifierMustRefuse(t *testing.T) {
	cases := []struct {
		sabotage string
		contains string
	}{
		{"expired", "expired"},
		{"foreign_aud", "aud"},
		{"wrong_key", "does not publish"},
		{"none", "RS256"},
		{"hs256", "RS256"},
	}

	for _, tc := range cases {
		idp := start(t, provider.Options{})
		client := idp.Server.Client()
		seedAndSpend(t, idp, client, "state-"+tc.sabotage, tc.sabotage)

		tokens := postForm(t, client, idp.URL("/token"), url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {"code-state-" + tc.sabotage},
			"client_id":     {"nvrdev"},
			"client_secret": {"devsecret"},
			"redirect_uri":  {idp.URL("/callback")},
			"code_verifier": {testVerifier},
		})
		idToken, _ := tokens["id_token"].(string)
		if idToken == "" {
			t.Fatalf("%s: no id_token in the response", tc.sabotage)
		}
		_, err := sso.NewJWKSClient(client).VerifyIDToken(
			idp.URL("/jwks"), idp.Issuer(), "nvrdev", idToken)
		if err == nil {
			t.Fatalf("%s: the verifier accepted the sabotaged token", tc.sabotage)
		}
		if !strings.Contains(err.Error(), tc.contains) {
			t.Fatalf("%s: refusal is %q, want it to name %q", tc.sabotage, err.Error(), tc.contains)
		}
	}

	// The shape the flow refuses before it verifies anything: no id_token at all.
	idp := start(t, provider.Options{})
	client := idp.Server.Client()
	seedAndSpend(t, idp, client, "state-missing", "missing")
	tokens := postForm(t, client, idp.URL("/token"), url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-state-missing"},
		"client_id":     {"nvrdev"},
		"client_secret": {"devsecret"},
		"redirect_uri":  {idp.URL("/callback")},
		"code_verifier": {testVerifier},
	})
	if tokens["id_token"] != nil {
		t.Fatal("the missing sabotage minted a token")
	}
}

// TestNonceSwapSabotageMintsATokenFromAnEarlierLogin: a nonce-swapped token is a
// **valid** token — signature, iss, aud, exp all fine — and only the flow's nonce
// check can refuse it. That is the shape the fixture has to produce, and the reason
// `nonce_mismatch` exists as a named state.
func TestNonceSwapSabotageMintsATokenFromAnEarlierLogin(t *testing.T) {
	idp := start(t, provider.Options{})
	client := idp.Server.Client()

	_, _, _ = get(t, client, idp.URL("/authorize?client_id=nvrdev&response_type=code&redirect_uri="+
		url.QueryEscape(idp.URL("/callback"))+"&state=state-nonce&nonce=nonce-THIS-LOGIN&code_challenge="+url.QueryEscape(testChallenge)))
	tokens := postForm(t, client, idp.URL("/token"), url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-state-nonce"},
		"client_id":     {"nvrdev"},
		"client_secret": {"devsecret"},
		"redirect_uri":  {idp.URL("/callback")},
		"code_verifier": {testVerifier},
	})
	if tokens["id_token"] == nil {
		t.Fatal("the login's own token is missing: the nonce check would have nothing to compare")
	}

	swapped := mintFor(t, idp, provider.Claims{
		Subject: "subject-nonce", Email: "ops@nvrway.test", EmailVerified: true, TokenSabotage: "nonce_swap",
	})
	claims, err := sso.NewJWKSClient(client).VerifyIDToken(
		idp.URL("/jwks"), idp.Issuer(), "nvrdev", swapped)
	if err != nil {
		t.Fatalf("a swapped-nonce token must verify as a token: %v", err)
	}
	if claims["nonce"] != "a-nonce-from-another-login" {
		t.Fatalf("claims nonce = %v, want the earlier login's nonce", claims["nonce"])
	}
}

// TestSubMismatchSabotageNamesADifferentPerson: the fixture can produce the
// disagreement the contract refuses — signed token one subject, userinfo another.
func TestSubMismatchSabotageNamesADifferentPerson(t *testing.T) {
	idp := start(t, provider.Options{Claims: provider.Claims{
		Subject: "userinfo-subject", Email: "ops@nvrway.test", EmailVerified: true,
		IDTokenSubject: "signed-subject", TokenSabotage: "sub_mismatch",
	}})
	client := idp.Server.Client()

	_, _, _ = get(t, client, idp.URL("/authorize?client_id=nvrdev&response_type=code&redirect_uri="+
		url.QueryEscape(idp.URL("/callback"))+"&state=state-mismatch&code_challenge="+url.QueryEscape(testChallenge)))
	tokens := postForm(t, client, idp.URL("/token"), url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-state-mismatch"},
		"client_id":     {"nvrdev"},
		"client_secret": {"devsecret"},
		"redirect_uri":  {idp.URL("/callback")},
		"code_verifier": {testVerifier},
	})

	tok, _, err := new(jwt.Parser).ParseUnverified(tokens["id_token"].(string), jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claims, _ := tok.Claims.(jwt.MapClaims)
	if got := claims["sub"]; got != "signed-subject" {
		t.Fatalf("signed token names %v, want the disagreement subject", got)
	}

	_, body, _ := userInfo(t, client, idp, tokens["access_token"].(string))
	var info map[string]any
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("userinfo: %v", err)
	}
	if info["sub"] != "userinfo-subject" {
		t.Fatalf("userinfo names %v, want the person the assertion pointed at", info["sub"])
	}
}

// TestRotationKeepsLoginsVerifiable: the state a real IdP is in when it rotates —
// the new key signs, and both keys are published for the window.
func TestRotationKeepsLoginsVerifiable(t *testing.T) {
	idp := start(t, provider.Options{})
	client := idp.Server.Client()
	jwks := sso.NewJWKSClient(client)

	before := mintFor(t, idp, provider.Claims{Subject: "before-rotate", Email: "ops@nvrway.test", EmailVerified: true})
	if _, err := jwks.VerifyIDToken(idp.URL("/jwks"), idp.Issuer(), "nvrdev", before); err != nil {
		t.Fatalf("verify before rotation: %v", err)
	}

	idp.Rotate()
	after := mintFor(t, idp, provider.Claims{Subject: "after-rotate", Email: "ops@nvrway.test", EmailVerified: true})
	if _, err := jwks.VerifyIDToken(idp.URL("/jwks"), idp.Issuer(), "nvrdev", after); err != nil {
		t.Fatalf("verify after rotation: %v", err)
	}

	idp.PublishOnlyCurrentKey()
	// A **cold** verifier: the key set a running login cached is exactly what keeps
	// logins working across a rotation, so the refused case is the provider whose
	// keys no longer include the kid at all, read by a client that has never seen
	// the old key.
	cold := sso.NewJWKSClient(client)
	if _, err := cold.VerifyIDToken(idp.URL("/jwks"), idp.Issuer(), "nvrdev", before); err == nil {
		t.Fatal("a token signed by a key the provider stopped publishing still verifies")
	}
	if _, err := cold.VerifyIDToken(idp.URL("/jwks"), idp.Issuer(), "nvrdev", after); err != nil {
		t.Fatalf("the current key stopped verifying: %v", err)
	}
}

// TestExtraClaimsArePublishedAsGiven: the claim shapes the mapping must handle —
// a group claim under a non-standard name, an email claim under `upn`, a user with
// no email — arrive in both id_token and userinfo as configured.
func TestExtraClaimsArePublishedAsGiven(t *testing.T) {
	idp := start(t, provider.Options{Claims: provider.Claims{
		Subject:       "subject-extra",
		EmailVerified: true,
		Extra:         map[string]any{"upn": "ops@entra.example", "roles": []string{"netbox-admins"}},
	}})
	client := idp.Server.Client()

	tokens := login(t, idp, client, "state-extra")
	_, body, _ := userInfo(t, client, idp, tokens["access_token"].(string))
	if !strings.Contains(string(body), "netbox-admins") {
		t.Fatalf("userinfo does not carry the mapped group claim: %s", body)
	}

	minted := mintFor(t, idp, idp.DefaultClaims())
	parsed, _, err := new(jwt.Parser).ParseUnverified(minted, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claims, _ := parsed.Claims.(jwt.MapClaims)
	if claims["upn"] != "ops@entra.example" {
		t.Fatalf("id_token claims %+v", claims)
	}
}

// TestUnsignedAndHS256TokensAreShapedAsTheAttackerLeavesThem: the fixture produces
// the header shapes the verifier refuses on the algorithm, before any signature.
func TestUnsignedAndHS256TokensAreShapedAsTheAttackerLeavesThem(t *testing.T) {
	for _, sabotage := range []string{"none", "hs256"} {
		idp := start(t, provider.Options{})
		token := mintFor(t, idp, provider.Claims{
			Subject: "subject-x", Email: "ops@nvrway.test", EmailVerified: true, TokenSabotage: sabotage,
		})
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Fatalf("%s: token is %d parts", sabotage, len(parts))
		}
		raw, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			t.Fatalf("%s: header: %v", sabotage, err)
		}
		var header struct {
			Alg string `json:"alg"`
			Kid string `json:"kid"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("%s: header JSON: %v", sabotage, err)
		}
		if sabotage == "none" && (header.Alg != "none" || parts[2] != "") {
			t.Fatalf("none-shaped token is %+v with signature %q", header, parts[2])
		}
		if sabotage == "hs256" && header.Alg != "HS256" {
			t.Fatalf("hs256-shaped token names %q", header.Alg)
		}
		if header.Kid == "" {
			t.Fatalf("%s: no kid in the header, so the key lookup never happens", sabotage)
		}
	}
}

// The helpers.

func seedAndSpend(t *testing.T, idp *provider.IdP, client *http.Client, state, sabotage string) {
	t.Helper()
	idp.SeedLogin(provider.Claims{
		Subject: "subject-" + sabotage, Email: "ops@nvrway.test", EmailVerified: true,
		TokenSabotage: sabotage,
	})
	_, _, _ = get(t, client, idp.URL("/authorize?client_id=nvrdev&response_type=code&redirect_uri="+
		url.QueryEscape(idp.URL("/callback"))+"&state="+state+"&code_challenge="+url.QueryEscape(testChallenge)))
}

// login walks authorize -> token (consent off) and returns the token endpoint's
// answer, so a test can reach userinfo with a bearer token the provider minted.
func login(t *testing.T, idp *provider.IdP, client *http.Client, state string) map[string]any {
	t.Helper()
	_, _, _ = get(t, client, idp.URL("/authorize?client_id=nvrdev&response_type=code&redirect_uri="+
		url.QueryEscape(idp.URL("/callback"))+"&state="+state+"&code_challenge="+url.QueryEscape(testChallenge)))
	return postForm(t, client, idp.URL("/token"), url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-" + state},
		"client_id":     {"nvrdev"},
		"client_secret": {"devsecret"},
		"redirect_uri":  {idp.URL("/callback")},
		"code_verifier": {testVerifier},
	})
}

func mintFor(t *testing.T, idp *provider.IdP, c provider.Claims) string {
	t.Helper()
	return idp.MintFor(c, "nvrdev")
}

func postForm(t *testing.T, client *http.Client, endpoint string, form url.Values) map[string]any {
	t.Helper()
	status, body := postFormRaw(t, client, endpoint, form)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("token endpoint answered %d %s: %v", status, body, err)
	}
	return out
}

func postFormRaw(t *testing.T, client *http.Client, endpoint string, form url.Values) (int, []byte) {
	t.Helper()
	resp, err := client.PostForm(endpoint, form)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, body
}

func userInfo(t *testing.T, client *http.Client, idp *provider.IdP, accessToken string) (int, []byte, *http.Response) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, idp.URL("/userinfo"), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("userinfo: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read userinfo: %v", err)
	}
	return resp.StatusCode, body, resp
}

// redirectClient is a client that follows redirects with a cookie jar — the shape
// `curl -b jar -L` has, which is what the card's walk is.
func redirectClient(t *testing.T, idp *provider.IdP) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := idp.Server.Client()
	client.Jar = jar
	client.Timeout = 10 * time.Second
	return client
}
