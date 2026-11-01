package sso

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWKS (2.4-2): the keys an id_token is verified against.
//
// Three rules shape this file.
//
// **The key set is fetched, not configured.** An IdP rotates signing keys, and a
// deployment that pastes a public key into a config file breaks the day it
// rotates. `jwks_uri` came from discovery, and a lookup that misses a `kid`
// re-fetches once — that is what keeps logins working across a rotation, which
// the acceptance criteria name as its own test.
//
// **One fetch per unknown key, not per request.** A cached set is refreshed at
// most every `jwksRefreshInterval`, and an unknown `kid` triggers a single
// refresh; a second miss for the same `kid` is refused, so a token signed by a
// key the IdP does not publish stays refused instead of turning a login into a
// flood of requests to the IdP.
//
// **Only RSA signing keys are assembled here, by hand.** `jwks_uri` belongs to
// another machine: the body is size-capped, the key type is checked, and the
// algorithm is checked on the token before any key is consulted. Accepting
// whatever the token's own header names is how `alg: none` and the HS256/JWKS
// confusion get a login.

const (
	jwksRefreshInterval = 15 * time.Minute
	jwksMaxBodyBytes    = 1 << 20
	// IDTokenLeeway is the clock-skew allowance for `exp`/`iat`: two machines
	// whose clocks differ by seconds must not be able to invalidate a login.
	IDTokenLeeway = 60 * time.Second
)

var (
	// ErrUnknownKeyID: the token names a key the provider does not publish.
	ErrUnknownKeyID = errors.New("id_token names a key the provider does not publish")
	// ErrTokenVerification: signature, iss, aud, exp — any of them. The reason is
	// wrapped, and the reason is what the audit row records.
	ErrTokenVerification = errors.New("id_token verification failed")
)

// jwksSet is a provider's published signing keys.
type jwksSet struct {
	fetched time.Time
	keys    map[string]*rsa.PublicKey // by kid
}

// JWKSClient fetches and caches provider key sets, keyed by `jwks_uri`.
//
// `misses` remembers the key ids that were not in the published set. It is the
// difference between "a login loop against an IdP that does not publish the key
// the token names" costing one extra request per installation and costing one per
// attempt, and it is why a second miss for the same kid is refused on the spot.
type JWKSClient struct {
	client *http.Client
	mu     sync.Mutex
	sets   map[string]*jwksSet
	misses map[string]map[string]time.Time
}

// NewJWKSClient builds a client sharing the SSO HTTP timeout policy.
func NewJWKSClient(client *http.Client) *JWKSClient {
	if client == nil {
		client = DefaultHTTPClient()
	}
	return &JWKSClient{
		client: client,
		sets:   make(map[string]*jwksSet),
		misses: make(map[string]map[string]time.Time),
	}
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// Probe fetches a provider's published key set and reports whether it contains
// any usable signing key. It is the test-connection check: an operator who has
// typed an issuer deserves to be told the JWKS endpoint is unreachable *now*, not
// to learn it from a failed login.
func (c *JWKSClient) Probe(uri string) error {
	set, err := c.set(uri, false)
	if err != nil {
		return err
	}
	if len(set.keys) == 0 {
		return fmt.Errorf("jwks endpoint %s publishes no RSA signing keys", uri)
	}
	return nil
}

// key returns the provider's key for `kid`, refreshing the set once when the kid
// is unknown — the rotation case.
//
// Exactly once is the rule, and the miss log is what enforces it: a kid that has
// already been looked for and not found, within the refresh interval, is refused
// without a fetch.
func (c *JWKSClient) key(uri, kid string) (*rsa.PublicKey, error) {
	set, err := c.set(uri, false)
	if err != nil {
		return nil, err
	}
	if key, ok := set.keys[kid]; ok {
		return key, nil
	}

	if c.kidAlreadyMissed(uri, kid) {
		return nil, fmt.Errorf("%w: kid %q (already looked for, and the provider does not publish it)", ErrUnknownKeyID, kid)
	}

	refreshed, err := c.set(uri, true)
	if err != nil {
		return nil, err
	}
	if key, ok := refreshed.keys[kid]; ok {
		c.clearMissed(uri, kid)
		return key, nil
	}
	c.markMissed(uri, kid)
	return nil, fmt.Errorf("%w: kid %q", ErrUnknownKeyID, kid)
}

// kidAlreadyMissed reports a search for this kid that already cost a refresh.
func (c *JWKSClient) kidAlreadyMissed(uri, kid string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.misses[uri][kid]
	return ok && time.Since(at) < jwksRefreshInterval
}

func (c *JWKSClient) markMissed(uri, kid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.misses[uri] == nil {
		c.misses[uri] = make(map[string]time.Time)
	}
	c.misses[uri][kid] = time.Now().UTC()
}

func (c *JWKSClient) clearMissed(uri, kid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if set, ok := c.misses[uri]; ok {
		delete(set, kid)
	}
}

// set returns the cached set, fetching it when absent, stale, or forced.
func (c *JWKSClient) set(uri string, force bool) (*jwksSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if set, ok := c.sets[uri]; ok && !force && time.Since(set.fetched) < jwksRefreshInterval {
		return set, nil
	}

	resp, err := c.client.Get(uri)
	if err != nil {
		return nil, fmt.Errorf("jwks request failed for %s: %w", uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks endpoint %s answered %d", uri, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("reading jwks endpoint %s: %w", uri, err)
	}

	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("jwks endpoint %s is not JSON: %w", uri, err)
	}

	set := &jwksSet{fetched: time.Now().UTC(), keys: make(map[string]*rsa.PublicKey)}
	for _, k := range doc.Keys {
		if k.Kid == "" {
			continue
		}
		// RSA signing keys only. An `oct` entry would let a shared secret verify
		// a "signed" token, which is exactly the confusion this check exists to
		// close; one unparsable key in a published set is not a reason to stop
		// logging people in with the rest of it.
		if !strings.EqualFold(k.Kty, "RSA") {
			continue
		}
		if k.Use != "" && !strings.EqualFold(k.Use, "sig") {
			continue
		}
		pub, err := rsaPublicKeyFromJWK(k)
		if err != nil {
			continue
		}
		set.keys[k.Kid] = pub
	}
	c.sets[uri] = set
	return set, nil
}

func rsaPublicKeyFromJWK(k jwk) (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("jwk %s: modulus: %w", k.Kid, err)
	}
	exponent, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("jwk %s: exponent: %w", k.Kid, err)
	}
	if len(modulus) == 0 || len(exponent) == 0 {
		return nil, fmt.Errorf("jwk %s: empty key material", k.Kid)
	}
	e := new(big.Int).SetBytes(exponent)
	if !e.IsInt64() {
		return nil, fmt.Errorf("jwk %s: exponent out of range", k.Kid)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(e.Int64())}, nil
}

// VerifyIDToken verifies an id_token against the provider's published keys and
// the claims that make it *this* provider's token for *this* client: `iss` is the
// issuer the operator named, `aud` is the provider's client id (a token minted
// for another client is not ours, whatever it asserts), `exp` is required.
//
// Nothing in the returned claims is trusted before this function returns no
// error.
func (c *JWKSClient) VerifyIDToken(uri, issuer, clientID, tokenString string) (map[string]any, error) {
	var header struct {
		Kid string `json:"kid"`
		Alg string `json:"alg"`
	}
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a JWS", ErrTokenVerification)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrTokenVerification, err)
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrTokenVerification, err)
	}
	if !strings.EqualFold(header.Alg, "RS256") {
		return nil, fmt.Errorf("%w: alg %q is not RS256", ErrTokenVerification, header.Alg)
	}

	key, err := c.key(uri, header.Kid)
	if err != nil {
		return nil, err
	}

	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (any, error) {
		return key, nil
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(strings.TrimRight(issuer, "/")),
		jwt.WithAudience(clientID),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(IDTokenLeeway),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenVerification, err)
	}
	if !parsed.Valid {
		return nil, fmt.Errorf("%w: token not valid", ErrTokenVerification)
	}
	return claims, nil
}

// RandomNonce builds the `nonce` the code flow binds an id_token to: a token
// minted for an earlier login must not satisfy a later one.
func RandomNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
