// Package auth owns the two credentials nvrWay issues: the HS256 JWT pair
// (access + refresh) and the argon2id password hash behind them.
//
// The API shape is OurWay's (NVRWAY_V1_PLAN.md Step 2), not a new design:
// secret in, token out, claims out. What OurWay learned is kept — the `alg`
// header is never trusted, logout denies the access token by its JTI, and a
// password change bumps a per-user generation so every access token issued
// before it stops verifying.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"nvr/server/models"
)

// Token types. The `type` claim is what keeps a refresh token from being
// accepted as an access token: both are signed with the same secret, so only
// this claim tells them apart.
const (
	TokenTypeAccess  = "access"
	TokenTypeRefresh = "refresh"
)

// Issuer strings. They name the installation in a token a person pastes into a
// forum asking why it does not work.
const (
	IssuerAccess  = "nvrway"
	IssuerRefresh = "nvrway-refresh"
)

// The refusal reasons, distinct on purpose: a login handler answers 401 for all
// of them, but "your session expired, sign in again" and "this token was signed
// by somebody else" are not the same sentence, and the log line names which one
// happened.
var (
	// ErrTokenExpired is a token past its exp.
	ErrTokenExpired = errors.New("token expired")
	// ErrTokenSignature is a token whose signature does not verify. The usual
	// cause is a secret change — a JWT_SECRET rotation, or two replicas that do
	// not share one.
	ErrTokenSignature = errors.New("token signature is invalid")
	// ErrTokenMalformed is a string that is not a JWT at all.
	ErrTokenMalformed = errors.New("token is malformed")
	// ErrTokenSigningMethod enforces the HS256-only rule: a token presenting
	// another algorithm in its header — RS256 with a public key the server
	// happens to trust, or the classic `alg: none` — is refused on the
	// algorithm, before its signature is considered.
	ErrTokenSigningMethod = errors.New("token signing method is not HS256")
	// ErrNotAccessToken is a refresh token presented to an access-token gate.
	ErrNotAccessToken = errors.New("token is not an access token")
	// ErrNotRefreshToken is an access token presented to the refresh route.
	ErrNotRefreshToken = errors.New("token is not a refresh token")
	// ErrTokenRevoked is a logged-out access token (denylist hit).
	ErrTokenRevoked = errors.New("token has been revoked")
	// ErrTokenGenerationStale is an access token issued before the account's
	// last password change.
	ErrTokenGenerationStale = errors.New("token generation is stale")

	// ErrRefreshUnknown is a refresh token that verifies against the secret but
	// has no session row: it was issued by a server whose sessions are gone (a
	// restore, a rebuild, a secret rotation). There is nothing to rotate, so the
	// client has to sign in again.
	ErrRefreshUnknown = errors.New("refresh token is not a known session")
	// ErrRefreshReused is reuse detection: this token was already rotated, so
	// presenting it again is a replay. The honest client has the replacement;
	// whoever replays has a copy, so every live session of that account is
	// dropped and both of them have to sign in.
	ErrRefreshReused = errors.New("refresh token was already used")
	// ErrRefreshSubjectMismatch is a refresh token whose claims name a different
	// account than the session row its hash points at.
	ErrRefreshSubjectMismatch = errors.New("refresh token does not match its session")
)

// Claims is nvrWay's token body: the account, its roles at issue time, and
// which of the two token types this is.
type Claims struct {
	UserID   string   `json:"user_id"`
	Username string   `json:"username"`
	Roles    []string `json:"roles"`
	Type     string   `json:"type"`
	// Gen is the account's token generation at issue time. Bumping it (a
	// password change) invalidates every access token issued before it. 0 means
	// "issued before generations existed" and skips the check.
	Gen uint32 `json:"gen,omitempty"`
	jwt.RegisteredClaims
}

// JWTAuth issues and verifies tokens.
//
// denylist holds JTI → expiry for logged-out access tokens; userGen holds the
// per-account generation. Both are in-memory on purpose: access tokens live 30
// minutes, so a restart clearing them costs nothing an operator notices, and
// logging in needs no Redis (plan decision 8).
type JWTAuth struct {
	secret []byte

	mu       sync.RWMutex
	denylist map[string]time.Time
	userGen  map[string]uint32
}

// NewJWTAuth builds the service over the configured secret (`JWT_SECRET`).
// config.Load generates an ephemeral secret when the environment has none, so an
// empty string reaching here is a caller that bypassed it — sign() and parse()
// refuse, rather than signing with an empty key.
func NewJWTAuth(secret string) *JWTAuth {
	return &JWTAuth{
		secret:   []byte(secret),
		denylist: make(map[string]time.Time),
		userGen:  make(map[string]uint32),
	}
}

// SecretIsEmpty reports the missing-signing-key condition, for the boot line.
func (j *JWTAuth) SecretIsEmpty() bool { return len(j.secret) == 0 }

// Issue mints an access token for an account carrying these roles.
//
// ttl <= 0 means "the plan's TTL" (models.AccessTokenTTL, 30 min), so there is
// one answer to "how long does an access token live".
//
// The roles are the roles at issue time: a role change takes effect at the next
// refresh, not in the middle of a request that is already authorised. That is
// why the middleware reads the claims and not the account.
func (j *JWTAuth) Issue(userID string, roles []string, ttl time.Duration) (string, error) {
	return j.IssueNamed(userID, "", roles, ttl)
}

// IssueNamed is Issue for a caller holding the username — the same token with
// one more claim. The audit ledger records usernames, and accounts get deleted,
// so a record that resolves to "user 9f3… vanished" is not a record.
func (j *JWTAuth) IssueNamed(userID, username string, roles []string, ttl time.Duration) (string, error) {
	if userID == "" {
		return "", errors.New("auth: user_id is required")
	}
	if ttl <= 0 {
		ttl = models.AccessTokenTTL
	}
	return j.sign(userID, username, roles, TokenTypeAccess, ttl)
}

// IssueRefresh mints a refresh token. ttl <= 0 means models.RefreshTokenTTL
// (14 days).
func (j *JWTAuth) IssueRefresh(userID, username string, roles []string, ttl time.Duration) (string, error) {
	if userID == "" {
		return "", errors.New("auth: user_id is required")
	}
	if ttl <= 0 {
		ttl = models.RefreshTokenTTL
	}
	return j.sign(userID, username, roles, TokenTypeRefresh, ttl)
}

// Verify validates an access token and returns its claims.
//
// The order is deliberate: shape and algorithm first (a token that lies about
// its own header never reaches a signature check), signature and expiry next,
// then the type claim, then the in-memory revocations.
func (j *JWTAuth) Verify(token string) (*Claims, error) {
	claims, err := j.parse(token)
	if err != nil {
		return nil, err
	}
	if claims.Type != TokenTypeAccess {
		return nil, ErrNotAccessToken
	}

	j.mu.RLock()
	defer j.mu.RUnlock()
	if exp, ok := j.denylist[claims.ID]; ok && time.Now().Before(exp) {
		return nil, ErrTokenRevoked
	}
	if claims.Gen > 0 {
		if cur, ok := j.userGen[claims.UserID]; ok && cur != claims.Gen {
			return nil, ErrTokenGenerationStale
		}
	}
	return claims, nil
}

// VerifyRefresh validates a refresh token. Rotation and reuse detection are not
// here: they need the sessions table, so they live in RotateRefresh.
func (j *JWTAuth) VerifyRefresh(token string) (*Claims, error) {
	claims, err := j.parse(token)
	if err != nil {
		return nil, err
	}
	if claims.Type != TokenTypeRefresh {
		return nil, ErrNotRefreshToken
	}
	return claims, nil
}

// parse verifies shape, algorithm, signature and expiry.
//
// The header is read first and refused unless it says HS256. The parse then
// repeats the same rule with WithValidMethods, so the refusal does not depend on
// the pre-check being the only guard.
func (j *JWTAuth) parse(token string) (*Claims, error) {
	if j.SecretIsEmpty() {
		return nil, errors.New("auth: signing secret is empty")
	}

	var head jwt.MapClaims
	parsed, _, err := jwt.NewParser().ParseUnverified(token, &head)
	if err != nil {
		// Not a header and a payload: garbage, not a bad signature.
		return nil, mapJWTError(err)
	}
	if alg, ok := parsed.Header["alg"].(string); !ok || alg != jwt.SigningMethodHS256.Alg() {
		return nil, ErrTokenSigningMethod
	}

	claims := &Claims{}
	token2, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			// The header said HS256 and the method is not HMAC: header and key
			// material disagree. Trust neither.
			return nil, ErrTokenSigningMethod
		}
		return j.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, mapJWTError(err)
	}
	if !token2.Valid {
		return nil, ErrTokenMalformed
	}
	return claims, nil
}

// mapJWTError turns the library's error into nvrWay's, so the handler answers a
// reason and the log says the same reason, while the library's detail stays
// reachable through Unwrap.
func mapJWTError(err error) error {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return wrap(ErrTokenExpired, err)
	case errors.Is(err, jwt.ErrTokenSignatureInvalid),
		errors.Is(err, jwt.ErrInvalidKey),
		errors.Is(err, jwt.ErrInvalidKeyType),
		errors.Is(err, jwt.ErrTokenUnverifiable):
		// The library reports a disallowed signing method as a signature
		// failure. Name the cause an operator can act on: the token asked for
		// an algorithm this server does not speak.
		if strings.Contains(err.Error(), "signing method") {
			return wrap(ErrTokenSigningMethod, err)
		}
		return wrap(ErrTokenSignature, err)
	case errors.Is(err, jwt.ErrTokenMalformed):
		return wrap(ErrTokenMalformed, err)
	default:
		// Claim-shape failures (not valid yet, used before issued, bad issuer,
		// bad audience) are tokens that do not describe themselves honestly.
		return wrap(ErrTokenMalformed, err)
	}
}

// wrap keeps nvrWay's reason in front of the library's, so errors.Is finds both.
func wrap(target, cause error) error { return &wrapped{target: target, cause: cause} }

type wrapped struct {
	target error
	cause  error
}

func (w *wrapped) Error() string { return w.target.Error() + ": " + w.cause.Error() }
func (w *wrapped) Unwrap() error { return w.cause }
func (w *wrapped) Is(target error) bool {
	return target == w.target || errors.Is(w.cause, target)
}

// sign builds and signs one token. HS256, always: no code path here accepts an
// algorithm from a caller.
func (j *JWTAuth) sign(userID, username string, roles []string, typ string, ttl time.Duration) (string, error) {
	if j.SecretIsEmpty() {
		return "", errors.New("auth: signing secret is empty")
	}
	now := time.Now()

	issuer := IssuerAccess
	gen := uint32(0)
	if typ == TokenTypeAccess {
		gen = j.genFor(userID)
	} else {
		issuer = IssuerRefresh
	}

	claims := Claims{
		UserID:   userID,
		Username: username,
		Roles:    append([]string(nil), roles...),
		Type:     typ,
		Gen:      gen,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        newJTI(),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Subject:   userID,
			Issuer:    issuer,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(j.secret)
}

// genFor reads, and for a first issue starts, the account's generation.
func (j *JWTAuth) genFor(userID string) uint32 {
	j.mu.Lock()
	defer j.mu.Unlock()
	gen, ok := j.userGen[userID]
	if !ok {
		gen = 1
		j.userGen[userID] = gen
	}
	return gen
}

// Deny revokes an access token by its JTI until its expiry — logout. Unknown
// JTIs are recorded harmlessly: the point is that this token stops verifying.
func (j *JWTAuth) Deny(jti string, exp *jwt.NumericDate) {
	until := time.Now().Add(models.AccessTokenTTL)
	if exp != nil {
		until = exp.Time
	}
	now := time.Now()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.denylist[jti] = until
	// Opportunistic prune, so the map is bounded by live sessions.
	for k, v := range j.denylist {
		if v.Before(now) {
			delete(j.denylist, k)
		}
	}
}

// BumpUserGen invalidates every access token issued before this call for one
// account: the password changed, and the tokens issued under the old one must
// stop working for as long as they would have lived.
func (j *JWTAuth) BumpUserGen(userID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.userGen[userID]++
}

// HasRole reports whether the claims carry a role.
func (c *Claims) HasRole(role string) bool {
	if c == nil {
		return false
	}
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// HasAnyRole reports whether the claims carry any of these roles.
func (c *Claims) HasAnyRole(roles []string) bool {
	if c == nil {
		return false
	}
	for _, r := range c.Roles {
		for _, needed := range roles {
			if r == needed {
				return true
			}
		}
	}
	return false
}

// Grants reports whether these claims grant a permission. `permsByRole` is the
// role table read at gate time, so a permission a role does not have cannot be
// granted by a token that names that role — the claim carries a role name, the
// role row carries the authority.
func (c *Claims) Grants(perm string, permsByRole map[string][]string) bool {
	if c == nil {
		return false
	}
	for _, role := range c.Roles {
		if models.PermissionsGrant(permsByRole[role], perm) {
			return true
		}
	}
	return false
}

// SessionWriter is the sessions table's half of refresh rotation:
// `sessions.token_hash` and `sessions.rotated_from` are what make rotation
// observable, and reuse detection is a property of those rows, not of the
// token. `store.SessionStore` satisfies it; auth takes the interface, so it
// links no store and a replay is testable without one.
type SessionWriter interface {
	// GetByTokenHash returns the session a refresh token names, revoked rows
	// included: "expired" and "already used" are different answers.
	GetByTokenHash(tokenHash string) (*models.Session, error)
	// Rotate closes the presented session and opens its replacement, naming the
	// closed row in rotated_from. It answers models.ErrSessionRotated when the
	// row was already closed — two clients rotating the same token, one wins.
	Rotate(old *models.Session, newTokenHash string, expiresAt time.Time) (*models.Session, error)
	// RevokeAllFor closes every live session of an account.
	RevokeAllFor(userID string) (int64, error)
}

// Rotation is the outcome of a refresh: the new access token, the new refresh
// token, the claims the access token carries, and the session row the new
// refresh token is bound to. `RevokedAll` is how many of the account's live
// sessions the reuse detection closed — it is a number an operator reads.
type Rotation struct {
	Access     string
	Refresh    string
	Claims     *Claims
	Session    *models.Session
	RevokedAll int64
}

// RotateRefresh is refresh rotation with reuse detection, in the order that
// makes a replay visible. The refresh token is the signed JWT the login route
// issued (type=refresh, issuer nvrway-refresh, 14 days); the session row stores
// its SHA-256, so the token never lands in the database. The replacement is a
// new JWT, and its hash is the new row:
//
//  1. verify the token (HS256, signature, expiry, type claim)
//  2. find the session row its hash names; no row → the session is gone
//  3. a revoked row is a replay → drop every live session of that account
//  4. an expired row is the end of a session, nothing more
//  5. claims and row must name the same account
//  6. rotate in one guarded transaction (that guard is what makes a concurrent
//     replay lose), and issue a fresh access token
//
// The old access token is denied at rotation: a refresh is exactly the moment an
// eavesdropped access token stops being useful.
func (j *JWTAuth) RotateRefresh(sessions SessionWriter, refreshToken string) (*Rotation, error) {
	if sessions == nil {
		return nil, errors.New("auth: rotation needs the sessions store")
	}
	claims, err := j.VerifyRefresh(refreshToken)
	if err != nil {
		return nil, err
	}

	old, err := sessions.GetByTokenHash(models.HashRefreshToken(refreshToken))
	if err != nil || old == nil {
		return nil, ErrRefreshUnknown
	}
	if old.RevokedAt != nil {
		// Reuse. The honest client rotated this token and holds the replacement;
		// whoever presents it again has a copy. Every live session of the account
		// goes, so the thief's session and the copied-token owner's session both
		// end, and the one that has to sign in is the one that lost its token.
		n, _ := sessions.RevokeAllFor(old.UserID)
		return &Rotation{Claims: claims, RevokedAll: n}, ErrRefreshReused
	}
	if !time.Now().UTC().Before(old.ExpiresAt) {
		return nil, ErrTokenExpired
	}
	if claims.UserID != old.UserID {
		return nil, ErrRefreshSubjectMismatch
	}

	// The replacement refresh token is the same credential shape as the one it
	// replaces: a signed JWT, whose hash is the new session row. `models.NewRefreshToken`
	// mints an opaque string, and two shapes for one credential is how a
	// refresh route ends up accepting one and rejecting the other.
	newRefresh, err := j.IssueRefresh(old.UserID, claims.Username, claims.Roles, models.RefreshTokenTTL)
	if err != nil {
		return nil, err
	}
	newHash := models.HashRefreshToken(newRefresh)
	rotated, err := sessions.Rotate(old, newHash, time.Now().UTC().Add(models.RefreshTokenTTL))
	if err != nil {
		if errors.Is(err, models.ErrSessionRotated) {
			n, _ := sessions.RevokeAllFor(old.UserID)
			return &Rotation{Claims: claims, RevokedAll: n}, ErrRefreshReused
		}
		return nil, err
	}

	access, err := j.IssueNamed(old.UserID, claims.Username, claims.Roles, models.AccessTokenTTL)
	if err != nil {
		return nil, err
	}
	j.Deny(claims.ID, claims.ExpiresAt)

	newClaims, err := j.Verify(access)
	if err != nil {
		return nil, err
	}
	return &Rotation{
		Access:  access,
		Refresh: newRefresh,
		Claims:  newClaims,
		Session: rotated,
	}, nil
}

// newJTI is the token id logout denies by.
func newJTI() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}
