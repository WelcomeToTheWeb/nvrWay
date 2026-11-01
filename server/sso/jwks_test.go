package sso

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// idpKeys is a provider's signing keys and the JWKS endpoint that publishes
// them. `fetches` counts the GETs the client paid for, which is the number the
// refresh rule is about.
type idpKeys struct {
	server    *httptest.Server
	signing   map[string]*rsa.PrivateKey
	published map[string]*rsa.PrivateKey
	fetches   *atomic.Int64
}

func newIDPKeys(t *testing.T, kids ...string) *idpKeys {
	t.Helper()
	set := &idpKeys{
		signing:   map[string]*rsa.PrivateKey{},
		published: map[string]*rsa.PrivateKey{},
		fetches:   &atomic.Int64{},
	}
	for _, kid := range kids {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		set.signing[kid] = key
		set.published[kid] = key
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		set.fetches.Add(1)
		var keys []map[string]any
		for kid, key := range set.published {
			keys = append(keys, map[string]any{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": kid,
				"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(server.Close)
	set.server = server
	return set
}

func (set *idpKeys) jwksURI() string { return set.server.URL + "/jwks" }

func (set *idpKeys) mint(t *testing.T, kid, issuer, clientID string, skew time.Duration) string {
	t.Helper()
	key, ok := set.signing[kid]
	if !ok {
		t.Fatalf("no signing key %q", kid)
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   issuer,
		"aud":   clientID,
		"sub":   "subject-9",
		"email": "ida@corp.example",
		"iat":   now.Add(-skew).Unix(),
		"exp":   now.Add(10*time.Minute - skew).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestVerifyIDTokenAcceptsATokenSignedByAPublishedKey(t *testing.T) {
	set := newIDPKeys(t, "k1")
	client := NewJWKSClient(DefaultHTTPClient())

	claims, err := client.VerifyIDToken(set.jwksURI(), set.server.URL, "nvrway-client", set.mint(t, "k1", set.server.URL, "nvrway-client", 0))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims["sub"] != "subject-9" {
		t.Fatalf("claims: %v", claims)
	}
	if claims["email"] != "ida@corp.example" {
		t.Fatalf("claims: %v", claims)
	}
}

func TestUnknownKidRefreshesTheKeySetExactlyOnce(t *testing.T) {
	// The rotation case, and the flood case, in one test: a key the client has
	// never seen is fetched once more and works; a key the provider does not
	// publish is fetched once more and stays refused, and asking again does not
	// pay for another fetch.
	set := newIDPKeys(t, "k1")
	client := NewJWKSClient(DefaultHTTPClient())

	known := set.mint(t, "k1", set.server.URL, "nvrway-client", 0)
	if _, err := client.VerifyIDToken(set.jwksURI(), set.server.URL, "nvrway-client", known); err != nil {
		t.Fatalf("verify with the published key: %v", err)
	}
	if got := set.fetches.Load(); got != 1 {
		t.Fatalf("a warm cache paid for %d fetches, want 1", got)
	}

	// The rotation: the IdP starts publishing k2, the client has k1 cached.
	key2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rotation key: %v", err)
	}
	set.published["k2"] = key2
	set.signing["k2"] = key2

	rotated := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": set.server.URL,
		"aud": "nvrway-client",
		"sub": "subject-9",
		"exp": time.Now().Add(10 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	})
	rotated.Header["kid"] = "k2"
	rotatedToken, err := rotated.SignedString(key2)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := client.VerifyIDToken(set.jwksURI(), set.server.URL, "nvrway-client", rotatedToken); err != nil {
		t.Fatalf("the rotated key was not picked up: %v", err)
	}
	if got := set.fetches.Load(); got != 2 {
		t.Fatalf("after a rotation: %d fetches, want 2", got)
	}

	// A key the provider does not publish at all.
	ghost, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate ghost key: %v", err)
	}
	set.signing["ghost"] = ghost
	bogus := set.mint(t, "ghost", set.server.URL, "nvrway-client", 0)

	_, err = client.VerifyIDToken(set.jwksURI(), set.server.URL, "nvrway-client", bogus)
	if !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("an unpublished kid answered %v, want ErrUnknownKeyID", err)
	}
	before := set.fetches.Load()
	if want := int64(3); before != want {
		t.Fatalf("the unknown kid paid for %d total fetches, want %d (one refresh)", before, want)
	}

	// Asking again with the same kid must not fetch: a login loop against an IdP
	// that does not publish the key would otherwise hammer it.
	for range 5 {
		if _, err := client.VerifyIDToken(set.jwksURI(), set.server.URL, "nvrway-client", bogus); !errors.Is(err, ErrUnknownKeyID) {
			t.Fatalf("repeat verify answered %v", err)
		}
	}
	if got := set.fetches.Load(); got != before {
		t.Fatalf("%d repeats fetched %d more times, want 0", 5, got-before)
	}
}

func TestVerifyIDTokenRefusesAnAlgorithmItDoesNotSpeak(t *testing.T) {
	set := newIDPKeys(t, "k1")
	client := NewJWKSClient(DefaultHTTPClient())

	// alg: none, addressed to the kid the provider publishes. The header decides,
	// before any key is consulted.
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1","typ":"JWT"}`)) +
		"." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"`+set.server.URL+`","aud":"nvrway-client","sub":"attacker","exp":9999999999}`)) + "."
	_, err := client.VerifyIDToken(set.jwksURI(), set.server.URL, "nvrway-client", unsigned)
	if !errors.Is(err, ErrTokenVerification) {
		t.Fatalf("alg none answered %v, want ErrTokenVerification", err)
	}
	if !strings.Contains(err.Error(), "RS256") {
		t.Fatalf("the refusal does not name the algorithm: %q", err.Error())
	}

	// The HS256/JWKS confusion: a token "signed" with a secret derived from
	// something the provider publishes. No MAC key is ever consulted.
	secret := set.published["k1"].PublicKey.N.Bytes()
	mac := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": set.server.URL,
		"aud": "nvrway-client",
		"sub": "attacker",
		"exp": time.Now().Add(10 * time.Minute).Unix(),
	})
	mac.Header["kid"] = "k1"
	forged, err := mac.SignedString(secret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := client.VerifyIDToken(set.jwksURI(), set.server.URL, "nvrway-client", forged); !errors.Is(err, ErrTokenVerification) {
		t.Fatalf("an HS256 token answered %v, want ErrTokenVerification", err)
	}

	// Not a JWS at all.
	if _, err := client.VerifyIDToken(set.jwksURI(), set.server.URL, "nvrway-client", "not-a-token"); !errors.Is(err, ErrTokenVerification) {
		t.Fatalf("garbage answered %v, want ErrTokenVerification", err)
	}
}

func TestVerifyIDTokenChecksTheClaimsThatMakeItThisProvidersToken(t *testing.T) {
	set := newIDPKeys(t, "k1")
	client := NewJWKSClient(DefaultHTTPClient())

	cases := []struct {
		name   string
		issuer string
		aud    string
		skew   time.Duration
	}{
		{"another issuer", "https://other-idp.example", "nvrway-client", 0},
		{"another client", set.server.URL, "some-other-client", 0},
		{"an expired token", set.server.URL, "nvrway-client", 20 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := set.mint(t, "k1", tc.issuer, tc.aud, tc.skew)
			if _, err := client.VerifyIDToken(set.jwksURI(), set.server.URL, "nvrway-client", token); !errors.Is(err, ErrTokenVerification) {
				t.Fatalf("%s was accepted: %v", tc.name, err)
			}
		})
	}

	// Clock skew of a few seconds does not invalidate a login: two machines, one
	// NTP server short of agreement.
	near := set.mint(t, "k1", set.server.URL, "nvrway-client", 30*time.Second)
	if _, err := client.VerifyIDToken(set.jwksURI(), set.server.URL, "nvrway-client", near); err != nil {
		t.Fatalf("a 30-second skew refused the login: %v", err)
	}
}

func TestProbeReportsAKeySetTheLoginCanUse(t *testing.T) {
	client := NewJWKSClient(DefaultHTTPClient())
	set := newIDPKeys(t, "k1")
	if err := client.Probe(set.jwksURI()); err != nil {
		t.Fatalf("probe: %v", err)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
	}))
	defer empty.Close()
	if err := client.Probe(empty.URL + "/jwks"); err == nil {
		t.Fatal("a key set with no RSA signing keys probed ok")
	}

	notJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>"))
	}))
	defer notJSON.Close()
	if err := client.Probe(notJSON.URL + "/jwks"); err == nil {
		t.Fatal("a key set that is not JSON probed ok")
	}

	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gone.Close()
	if err := client.Probe(gone.URL + "/jwks"); err == nil {
		t.Fatal("a key set that answers 404 probed ok")
	}
}

func TestRandomNonceIsUnguessableAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		nonce, err := RandomNonce()
		if err != nil {
			t.Fatalf("nonce: %v", err)
		}
		if nonce == "" {
			t.Fatal("an empty nonce")
		}
		if strings.ContainsAny(nonce, "+/=") {
			t.Fatalf("nonce %q is not base64url without padding", nonce)
		}
		raw, err := base64.RawURLEncoding.DecodeString(nonce)
		if err != nil {
			t.Fatalf("nonce %q does not decode: %v", nonce, err)
		}
		if len(raw) < 16 {
			t.Fatalf("nonce carries %d bytes, want at least 16", len(raw))
		}
		if seen[nonce] {
			t.Fatalf("nonce %q repeated", nonce)
		}
		seen[nonce] = true
	}
}
