package sso

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// discoveryDocument builds an IdP's `.well-known/openid-configuration` and
// returns a server that publishes it at the well-known path. The issuer is the
// server's own URL, which is the shape a conformant IdP has.
// discoveryDocument builds an IdP's `.well-known/openid-configuration` and
// returns a server that publishes it at the well-known path. The document names
// the server itself as its issuer, which is the shape a conformant IdP has and
// what makes the issuer check meaningful. `mutate` edits the document on every
// response, so a test can break one member and leave the rest conformant.
func discoveryDocument(t *testing.T, mutate func(map[string]any)) (*httptest.Server, string) {
	t.Helper()
	var issuer atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		base := "http://" + r.Host
		if v, ok := issuer.Load().(string); !ok || v == "" {
			issuer.Store(base)
		}
		doc := map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/authorize",
			"token_endpoint":                        base + "/token",
			"userinfo_endpoint":                     base + "/userinfo",
			"jwks_uri":                              base + "/jwks",
			"response_types_supported":              []string{"code", "id_token"},
			"scopes_supported":                      []string{"openid", "email", "profile", "offline_access"},
			"code_challenge_methods_supported":      []string{"S256"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if mutate != nil {
			mutate(doc)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(server.Close)
	return server, server.URL
}

func kindOf(t *testing.T, err error) string {
	t.Helper()
	var discoveryErr *DiscoveryError
	if err == nil {
		t.Fatal("expected a discovery failure")
	}
	if !asDiscovery(err, &discoveryErr) {
		t.Fatalf("failure is not a DiscoveryError: %T %v", err, err)
	}
	return discoveryErr.Kind
}

func asDiscovery(err error, target **DiscoveryError) bool {
	for err != nil {
		if de, ok := err.(*DiscoveryError); ok {
			*target = de
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func TestDiscoveryKindStringsAreExact(t *testing.T) {
	// The console maps each of these to a state, and the API answers them. They
	// are part of the contract, not internal detail.
	want := map[string]string{
		"DiscoveryOK":             DiscoveryOK,
		"DiscoveryUnreachable":    DiscoveryUnreachable,
		"DiscoveryBadDocument":    DiscoveryBadDocument,
		"DiscoveryIssuerMismatch": DiscoveryIssuerMismatch,
		"DiscoveryNoCodeFlow":     DiscoveryNoCodeFlow,
	}
	exact := map[string]string{
		"DiscoveryOK":             "ok",
		"DiscoveryUnreachable":    "discovery_unreachable",
		"DiscoveryBadDocument":    "discovery_document",
		"DiscoveryIssuerMismatch": "issuer_mismatch",
		"DiscoveryNoCodeFlow":     "no_code_flow",
	}
	for name, got := range want {
		if got != exact[name] {
			t.Fatalf("%s is %q, want %q", name, got, exact[name])
		}
	}
}

func TestDiscoverReadsAConformantProvider(t *testing.T) {
	_, issuer := discoveryDocument(t, nil)

	doc, err := Discover(DefaultHTTPClient(), issuer, "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if doc.AuthorizationEndpoint != issuer+"/authorize" || doc.TokenEndpoint != issuer+"/token" {
		t.Fatalf("endpoints: auth=%q token=%q, want the endpoints the document published under %s",
			doc.AuthorizationEndpoint, doc.TokenEndpoint, issuer)
	}
	if doc.JwksURI != issuer+"/jwks" {
		t.Fatalf("jwks_uri: %q", doc.JwksURI)
	}
	if doc.UserInfoEndpoint != issuer+"/userinfo" {
		t.Fatalf("userinfo_endpoint: %q", doc.UserInfoEndpoint)
	}
	if !doc.PKCESupported() {
		t.Fatal("PKCE was not reported as supported")
	}
	if !doc.ScopeSupports("email") || !doc.ScopeSupports("offline_access") {
		t.Fatalf("scope support: %v", doc.ScopesSupported)
	}
	if !doc.SupportsSigningAlg("RS256") {
		t.Fatal("RS256 was not reported")
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(doc.Raw), &raw); err != nil {
		t.Fatalf("Raw is not the published document: %v", err)
	}
	if raw["issuer"] != issuer {
		t.Fatalf("Raw issuer: %v", raw["issuer"])
	}
}

func TestDiscoverUnreachable(t *testing.T) {
	// A server that answers 503 at the well-known path.
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unreachable.Close()

	for _, tc := range []struct {
		name   string
		issuer string
	}{
		{"no listener", "http://127.0.0.1:1"},
		{"503 at the well-known path", unreachable.URL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Discover(DefaultHTTPClient(), tc.issuer, "")
			if got := kindOf(t, err); got != DiscoveryUnreachable {
				t.Fatalf("kind %q, want %q (%v)", got, DiscoveryUnreachable, err)
			}
		})
	}
}

func TestDiscoverRefusesAnIncompleteDocument(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"no authorization endpoint", func(doc map[string]any) {
			delete(doc, "authorization_endpoint")
		}, DiscoveryBadDocument},
		{"no token endpoint", func(doc map[string]any) {
			delete(doc, "token_endpoint")
		}, DiscoveryBadDocument},
		{"no jwks_uri", func(doc map[string]any) {
			delete(doc, "jwks_uri")
		}, DiscoveryBadDocument},
		{"no issuer", func(doc map[string]any) { delete(doc, "issuer") }, DiscoveryBadDocument},
		{"relative endpoint", func(doc map[string]any) {
			doc["authorization_endpoint"] = "/authorize"
		}, DiscoveryBadDocument},
		{"no code flow", func(doc map[string]any) {
			doc["response_types_supported"] = []string{"id_token", "token"}
		}, DiscoveryNoCodeFlow},
	} {
		_, issuer := discoveryDocument(t, tc.mutate)
		_, err := Discover(DefaultHTTPClient(), issuer, "")
		if got := kindOf(t, err); got != tc.want {
			t.Fatalf("%s: kind %q, want %q (%v)", tc.name, got, tc.want, err)
		}
	}
}

func TestDiscoverRefusesAnIssuerThatIsNotTheConfiguredOne(t *testing.T) {
	// The mis-typed issuer, and the proxy that answers with someone else's
	// document. Without this check both save, and every login afterwards fails
	// with a message that points at the IdP.
	server, _ := discoveryDocument(t, nil)
	const configured = "https://id.example/corp"

	_, err := Discover(DefaultHTTPClient(), configured, server.URL+"/.well-known/openid-configuration")
	if got := kindOf(t, err); got != DiscoveryIssuerMismatch {
		t.Fatalf("kind %q, want %q (%v)", got, DiscoveryIssuerMismatch, err)
	}

	// A trailing slash on the issuer an operator typed is the same issuer: the
	// document names itself with one, the operator typed none.
	slashServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"issuer":                           "http://" + r.Host + "/",
			"authorization_endpoint":           "http://" + r.Host + "/authorize",
			"token_endpoint":                   "http://" + r.Host + "/token",
			"jwks_uri":                         "http://" + r.Host + "/jwks",
			"response_types_supported":         []string{"code"},
			"code_challenge_methods_supported": []string{"S256"},
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer slashServer.Close()

	if _, err := Discover(DefaultHTTPClient(), slashServer.URL, ""); err != nil {
		t.Fatalf("a trailing slash on the document's issuer was refused: %v", err)
	}
}

func TestDiscoverTakesAnExplicitDiscoveryURL(t *testing.T) {
	// Some IdPs publish the document under a path that is not issuer +
	// well-known. The explicit URL wins.
	doc := map[string]any{
		"issuer":                   "https://idp.invalid",
		"authorization_endpoint":   "https://idp.invalid/authorize",
		"token_endpoint":           "https://idp.invalid/token",
		"jwks_uri":                 "https://idp.invalid/jwks",
		"response_types_supported": []string{"code"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realms/corp/.well-known/openid-configuration" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer server.Close()

	_, err := Discover(DefaultHTTPClient(), "https://idp.invalid",
		server.URL+"/realms/corp/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("explicit discovery URL: %v", err)
	}
	if _, err := Discover(DefaultHTTPClient(), "https://idp.invalid", server.URL+"/nope"); err == nil {
		t.Fatal("an explicit discovery URL that 404s was accepted")
	}
}

func TestDefaultHTTPClientHasATimeout(t *testing.T) {
	// An unreachable IdP must produce a named failure in seconds, not a login
	// that hangs.
	if client := DefaultHTTPClient(); client.Timeout <= 0 {
		t.Fatalf("the shared client has no timeout: %v", client.Timeout)
	}
}

func TestDiscoveryDocumentHelpersOnAnEmptyDocument(t *testing.T) {
	doc := &DiscoveryDocument{}
	if doc.PKCESupported() {
		t.Fatal("an empty document claims PKCE")
	}
	if doc.ScopeSupports("email") {
		t.Fatal("an empty document claims the email scope")
	}
	if doc.SupportsSigningAlg("RS256") {
		t.Fatal("an empty document claims RS256")
	}
	if !strings.EqualFold(DiscoveryOK, "ok") {
		t.Fatal("the success kind moved")
	}
}
