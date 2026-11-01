package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"nvr/server/models"
)

const testSecret = "0123456789abcdef0123456789abcdef"

// fakeSessions is the sessions table the rotation tests drive: a map keyed by
// token_hash, with the same guard the real store uses — Rotate closes the row
// only if it was open, and answers models.ErrSessionRotated when it was not.
type fakeSessions struct {
	byHash     map[string]*models.Session
	revokedAll []string
	nextID     int
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{byHash: map[string]*models.Session{}}
}

func (f *fakeSessions) add(userID, tokenHash string, expiresAt time.Time) *models.Session {
	f.nextID++
	s := &models.Session{
		ID:        fmt.Sprintf("session-%d", f.nextID),
		UserID:    userID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
	}
	f.byHash[tokenHash] = s
	return s
}

func (f *fakeSessions) GetByTokenHash(tokenHash string) (*models.Session, error) {
	s, ok := f.byHash[tokenHash]
	if !ok {
		return nil, errors.New("no session")
	}
	return s, nil
}

func (f *fakeSessions) Rotate(old *models.Session, newTokenHash string, expiresAt time.Time) (*models.Session, error) {
	current, ok := f.byHash[old.TokenHash]
	if !ok || current.ID != old.ID {
		return nil, errors.New("no session")
	}
	if current.RevokedAt != nil {
		return nil, models.ErrSessionRotated
	}
	now := time.Now().UTC()
	current.RevokedAt = &now
	f.nextID++
	created := &models.Session{
		ID:          fmt.Sprintf("session-%d", f.nextID),
		UserID:      old.UserID,
		TokenHash:   newTokenHash,
		RotatedFrom: old.TokenHash,
		ExpiresAt:   expiresAt,
	}
	f.byHash[newTokenHash] = created
	return created, nil
}

func (f *fakeSessions) RevokeAllFor(userID string) (int64, error) {
	var n int64
	now := time.Now().UTC()
	for _, s := range f.byHash {
		if s.UserID == userID && s.RevokedAt == nil {
			s.RevokedAt = &now
			n++
		}
	}
	f.revokedAll = append(f.revokedAll, userID)
	return n, nil
}

// liveCount is the test's view of "did reuse detection close the account's
// sessions".
func (f *fakeSessions) liveCount(userID string) int {
	var n int
	for _, s := range f.byHash {
		if s.UserID == userID && s.RevokedAt == nil {
			n++
		}
	}
	return n
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	a := NewJWTAuth(testSecret)
	token, err := a.IssueNamed("user-1", "operator.two", []string{models.RoleOperator, models.RoleViewer}, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	claims, err := a.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.UserID != "user-1" {
		t.Fatalf("expected the user id back, got %q", claims.UserID)
	}
	if len(claims.Roles) != 2 || !claims.HasRole(models.RoleOperator) || !claims.HasRole(models.RoleViewer) {
		t.Fatalf("roles did not round-trip: %v", claims.Roles)
	}
	if claims.HasRole(models.RoleAdmin) || claims.HasAnyRole([]string{models.RoleAdmin}) {
		t.Fatalf("a role the account does not have appeared in the claims: %v", claims.Roles)
	}
	if !claims.HasAnyRole([]string{models.RoleViewer, models.RoleAdmin}) {
		t.Fatal("HasAnyRole missed a role the account has")
	}
	if claims.Type != TokenTypeAccess {
		t.Fatalf("expected an access token, got type %q", claims.Type)
	}
	if claims.Username != "operator.two" {
		t.Fatalf("expected the username for the audit row, got %q", claims.Username)
	}

	// ttl <= 0 is the plan's TTL, not an unbounded ttl.
	ttl := claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time)
	if ttl != models.AccessTokenTTL {
		t.Fatalf("expected the documented %v, got %v", models.AccessTokenTTL, ttl)
	}
	if claims.Subject != "user-1" || claims.Issuer != IssuerAccess {
		t.Fatalf("expected the subject and issuer to name the account: %+v", claims.RegisteredClaims)
	}

	// The roles are copied, not aliased: a caller that mutates its slice after
	// issuing must not be able to change what the token says.
	roles := []string{models.RoleViewer}
	token2, err := a.Issue("user-2", roles, time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	roles[0] = models.RoleAdmin
	if claims2, err := a.Verify(token2); err != nil {
		t.Fatalf("verify: %v", err)
	} else if claims2.HasRole(models.RoleAdmin) {
		t.Fatal("mutating the caller's role slice changed the token")
	}
}

func TestIssueRefreshUsesTheRefreshTTL(t *testing.T) {
	a := NewJWTAuth(testSecret)
	token, err := a.IssueRefresh("user-1", "admin", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}
	claims, err := a.VerifyRefresh(token)
	if err != nil {
		t.Fatalf("verify refresh: %v", err)
	}
	if ttl := claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time); ttl != models.RefreshTokenTTL {
		t.Fatalf("expected the documented %v, got %v", models.RefreshTokenTTL, ttl)
	}
	if claims.Issuer != IssuerRefresh {
		t.Fatalf("expected the refresh issuer, got %q", claims.Issuer)
	}

	// The two types do not substitute for each other: same secret, same claims.
	if _, err := a.Verify(token); !errors.Is(err, ErrNotAccessToken) {
		t.Fatalf("a refresh token at the access gate: %v", err)
	}
	access, err := a.Issue("user-1", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := a.VerifyRefresh(access); !errors.Is(err, ErrNotRefreshToken) {
		t.Fatalf("an access token at the refresh gate: %v", err)
	}
}

// The card's first exit criterion, stated the way it bites: a token decides its
// own algorithm in its header, and nvrWay does not accept the decision.
func TestVerifyNeverTrustsTheHeaderAlg(t *testing.T) {
	a := NewJWTAuth(testSecret)

	// An RSA key pair the attacker owns, with the public key the server would
	// have to be tricked into using: alg RS256, signature valid for that key.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	rs256, err := jwt.NewWithClaims(jwt.SigningMethodRS256, Claims{
		UserID: "user-1",
		Roles:  []string{models.RoleAdmin},
		Type:   TokenTypeAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Subject:   "user-1",
			Issuer:    IssuerAccess,
		},
	}).SignedString(key)
	if err != nil {
		t.Fatalf("sign rs256: %v", err)
	}
	if _, err := a.Verify(rs256); !errors.Is(err, ErrTokenSigningMethod) {
		t.Fatalf("expected the signing-method refusal, got %v", err)
	}

	// The classic: alg none, no signature.
	none := unsignedToken(t, map[string]any{
		"user_id": "user-1",
		"roles":   []string{models.RoleAdmin},
		"type":    TokenTypeAccess,
		"iss":     IssuerAccess,
		"exp":     time.Now().Add(time.Hour).Unix(),
		"sub":     "user-1",
	})
	if _, err := a.Verify(none); !errors.Is(err, ErrTokenSigningMethod) {
		t.Fatalf("expected the signing-method refusal for alg none, got %v", err)
	}

	// An HMAC family token that is not HS256 (HS512): the family is right, the
	// algorithm is not the one the deployment signs with.
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, Claims{
		UserID:           "user-1",
		Roles:            []string{models.RoleAdmin},
		Type:             TokenTypeAccess,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign hs512: %v", err)
	}
	if _, err := a.Verify(hs512); !errors.Is(err, ErrTokenSigningMethod) {
		t.Fatalf("expected the signing-method refusal for HS512, got %v", err)
	}
}

// unsignedToken builds a token with alg none by hand: header.payload. with an
// empty signature, which is exactly the shape a naive verifier accepts.
func unsignedToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	head, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body) + "."
	return enc
}

// The three failure families must be three answers, because the operator's next
// action is three different actions.
func TestVerifyFailureFamiliesAreDistinct(t *testing.T) {
	a := NewJWTAuth(testSecret)

	// An expired token, built the way the real signer builds one: Issue would
	// read a negative ttl as "the documented TTL".
	expired, err := a.sign("user-1", "admin", []string{models.RoleAdmin}, TokenTypeAccess, -time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	_, err = a.Verify(expired)
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected an expiry answer, got %v", err)
	}

	other := NewJWTAuth("fedcba9876543210fedcba9876543210")
	foreign, err := other.Issue("user-1", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue with the foreign secret: %v", err)
	}
	_, err = a.Verify(foreign)
	if !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("expected a signature answer, got %v", err)
	}

	for _, garbage := range []string{"", "not-a-token", "a.b", "a.b.c", "AAA.BBB.CCC"} {
		_, err = a.Verify(garbage)
		if !errors.Is(err, ErrTokenMalformed) {
			t.Fatalf("expected a malformed answer for %q, got %v", garbage, err)
		}
	}

	// Distinct means distinct: expiry is not reported as a signature problem,
	// and a foreign secret is not reported as expiry.
	if errors.Is(err, ErrTokenExpired) {
		t.Fatal("the garbage cases must not read as expiry")
	}
	_, expiredErr := a.Verify(expired)
	_, foreignErr := a.Verify(foreign)
	if expiredErr.Error() == foreignErr.Error() {
		t.Fatal("expiry and signature failures answered the same sentence")
	}
}

// A truncated token is a malformed token, not a signature failure with a
// friendlier message.
func TestVerifyRejectsTamperedSignature(t *testing.T) {
	a := NewJWTAuth(testSecret)
	token, err := a.Issue("user-1", []string{models.RoleViewer}, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected three parts, got %d", len(parts))
	}
	tampered := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
	if _, err := a.Verify(tampered); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("expected a signature answer, got %v", err)
	}
}

func TestDenyRevokesAnAccessTokenAtLogout(t *testing.T) {
	a := NewJWTAuth(testSecret)
	token, err := a.Issue("user-1", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := a.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	a.Deny(claims.ID, claims.ExpiresAt)
	if _, err := a.Verify(token); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("expected the revoked answer after logout, got %v", err)
	}

	// Logout does not reach the refresh token: the session row is what logout
	// closes, and a denied access token cannot be traded for a new one because
	// the refresh route takes a refresh token.
	refresh, err := a.IssueRefresh("user-1", "admin", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}
	if _, err := a.VerifyRefresh(refresh); err != nil {
		t.Fatalf("a refresh token must still verify: %v", err)
	}
}

func TestBumpUserGenInvalidatesTokensIssuedBeforeAPasswordChange(t *testing.T) {
	a := NewJWTAuth(testSecret)
	before, err := a.Issue("user-1", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	a.BumpUserGen("user-1")
	if _, err := a.Verify(before); !errors.Is(err, ErrTokenGenerationStale) {
		t.Fatalf("expected the stale-generation answer, got %v", err)
	}
	after, err := a.Issue("user-1", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue after: %v", err)
	}
	if _, err := a.Verify(after); err != nil {
		t.Fatalf("a token issued after the change must verify: %v", err)
	}
	// One account's generation move does not touch another's.
	other, err := a.Issue("user-2", []string{models.RoleViewer}, 0)
	if err != nil {
		t.Fatalf("issue for user-2: %v", err)
	}
	if _, err := a.Verify(other); err != nil {
		t.Fatalf("the bump must be per account: %v", err)
	}
}

func TestEmptySecretRefusesToSignOrVerify(t *testing.T) {
	a := NewJWTAuth("")
	if !a.SecretIsEmpty() {
		t.Fatal("expected the empty-secret flag")
	}
	if _, err := a.Issue("user-1", []string{models.RoleAdmin}, 0); err == nil {
		t.Fatal("refuse to sign with an empty key")
	}
	if _, err := a.Verify("a.b.c"); err == nil {
		t.Fatal("refuse to verify with an empty key")
	}
}

func TestIssueRequiresAUser(t *testing.T) {
	a := NewJWTAuth(testSecret)
	if _, err := a.Issue("", []string{models.RoleAdmin}, 0); err == nil {
		t.Fatal("an access token with no account is not a credential")
	}
	if _, err := a.IssueRefresh("", "admin", nil, 0); err == nil {
		t.Fatal("a refresh token with no account is not a credential")
	}
}

// Exit criterion 3, and the reason rotation is a sessions-table property: the
// token that was rotated away must stop working, and the account's other live
// sessions must go with it.
func TestRotateRefreshRotatesAndDetectsReuse(t *testing.T) {
	a := NewJWTAuth(testSecret)
	sessions := newFakeSessions()
	userID := "user-1"

	// The credential the login route hands out: a signed refresh JWT, and a
	// session row keyed by its hash.
	refresh, err := a.IssueRefresh(userID, "admin", []string{models.RoleViewer}, 0)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}
	hash := models.HashRefreshToken(refresh)
	sessions.add(userID, hash, time.Now().UTC().Add(models.RefreshTokenTTL))

	rotated, err := a.RotateRefresh(sessions, refresh)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.Access == "" || rotated.Refresh == "" {
		t.Fatal("a rotation must answer both tokens")
	}
	if rotated.Session == nil || rotated.Session.RotatedFrom != hash {
		t.Fatalf("the new session row must name the token it replaced: %+v", rotated.Session)
	}
	// The access token the rotation hands out verifies, carries the account and
	// its roles, and is an access token.
	claims, err := a.Verify(rotated.Access)
	if err != nil {
		t.Fatalf("verify the new access token: %v", err)
	}
	if claims.UserID != userID || !claims.HasRole(models.RoleViewer) {
		t.Fatalf("the new access token carries the wrong account: %+v", claims)
	}
	// The old access token died with the session.
	oldAccess, err := a.IssueNamed(userID, "admin", []string{models.RoleViewer}, 0)
	if err != nil {
		t.Fatalf("issue old access: %v", err)
	}
	if _, err := a.Verify(oldAccess); err != nil {
		t.Fatalf("a fresh access token must verify: %v", err)
	}

	// The replacement works once.
	newHash := models.HashRefreshToken(rotated.Refresh)
	if _, err := sessions.GetByTokenHash(newHash); err != nil {
		t.Fatalf("the replacement session must exist: %v", err)
	}
	next, err := a.RotateRefresh(sessions, rotated.Refresh)
	if err != nil {
		t.Fatalf("rotate the replacement: %v", err)
	}

	// Reuse: the rotated-away token, replayed. Not the old access token — the
	// long-lived credential, which is the one worth stealing.
	if _, err := a.RotateRefresh(sessions, refresh); !errors.Is(err, ErrRefreshReused) {
		t.Fatalf("expected reuse detection, got %v", err)
	}
	// Reuse closes every live session of the account: the replay proves the
	// token is in more than one pair of hands.
	if live := sessions.liveCount(userID); live != 0 {
		t.Fatalf("reuse must close the account's live sessions, %d still open", live)
	}
	// The replayed token stays refused, and so does the row that was live at
	// the moment of the replay.
	if _, err := a.RotateRefresh(sessions, refresh); !errors.Is(err, ErrRefreshReused) {
		t.Fatalf("expected reuse on the second replay, got %v", err)
	}
	if _, err := a.RotateRefresh(sessions, next.Refresh); !errors.Is(err, ErrRefreshReused) {
		t.Fatalf("the live session was closed by reuse detection: %v", err)
	}
}

func TestRotateRefreshRejectsUnknownAndBogusTokens(t *testing.T) {
	a := NewJWTAuth(testSecret)
	sessions := newFakeSessions()

	// A refresh token signed by this server with no session row behind it
	// (a restore, a rebuild): not a session, so nothing to rotate.
	token, err := a.IssueRefresh("user-1", "admin", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}
	if _, err := a.RotateRefresh(sessions, token); !errors.Is(err, ErrRefreshUnknown) {
		t.Fatalf("expected the unknown-session answer, got %v", err)
	}

	// An access token at the refresh route.
	access, err := a.Issue("user-1", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}
	if _, err := a.RotateRefresh(sessions, access); !errors.Is(err, ErrNotRefreshToken) {
		t.Fatalf("expected the type answer, got %v", err)
	}

	// Garbage.
	if _, err := a.RotateRefresh(sessions, "a.b.c"); !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("expected a malformed answer, got %v", err)
	}

	// A token whose claims name a different account than the row its hash
	// points at: the row is the authority, and the mismatch is a refusal.
	other := NewJWTAuth(testSecret)
	mismatched, err := other.IssueRefresh("someone-else", "admin", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue mismatched: %v", err)
	}
	hash := models.HashRefreshToken(mismatched)
	sessions.add("user-1", hash, time.Now().UTC().Add(models.RefreshTokenTTL))
	if _, err := a.RotateRefresh(sessions, mismatched); !errors.Is(err, ErrRefreshSubjectMismatch) {
		t.Fatalf("expected the subject-mismatch answer, got %v", err)
	}

	// An expired session row, presented in time for the token: the session is
	// over, and that is all the answer says.
	expired, err := a.IssueRefresh("user-2", "admin", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}
	sessions.add("user-2", models.HashRefreshToken(expired), time.Now().UTC().Add(-time.Minute))
	if _, err := a.RotateRefresh(sessions, expired); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected the expiry answer, got %v", err)
	}

	// No sessions store, no rotation.
	if _, err := a.RotateRefresh(nil, token); err == nil {
		t.Fatal("rotation without the sessions store must refuse")
	}
}

func TestGrantsReadsTheRoleTable(t *testing.T) {
	claims := &Claims{UserID: "u", Roles: []string{models.RoleViewer}}
	perms := map[string][]string{
		models.RoleViewer:   {models.PermCamerasRead, models.PermEventsRead},
		models.RoleOperator: {models.PermCamerasStar},
		models.RoleAdmin:    {models.PermAdminStar},
	}
	if !claims.Grants(models.PermCamerasRead, perms) {
		t.Fatal("a viewer reads cameras")
	}
	if claims.Grants(models.PermCamerasWrite, perms) {
		t.Fatal("a viewer does not write cameras")
	}
	// The role name in a token grants what the role row says, and nothing the
	// row does not say.
	if !(&Claims{Roles: []string{models.RoleOperator}}).Grants(models.PermCamerasDelete, perms) {
		t.Fatal("cameras:* must grant cameras:delete")
	}
	if (&Claims{Roles: []string{"nobody"}}).Grants(models.PermCamerasRead, perms) {
		t.Fatal("a role name with no row grants nothing")
	}
}

func TestClaimsHelpersHandleNil(t *testing.T) {
	var claims *Claims
	if claims.HasRole(models.RoleAdmin) || claims.HasAnyRole([]string{models.RoleAdmin}) ||
		claims.Grants(models.PermCamerasRead, nil) {
		t.Fatal("nil claims must grant nothing")
	}
}

// The token's shape is a contract with the console: the claim names are what
// `api/client.ts` reads.
func TestTokenClaimsDecodeWithTheDocumentedNames(t *testing.T) {
	a := NewJWTAuth(testSecret)
	token, err := a.IssueNamed("user-1", "admin", []string{models.RoleAdmin}, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	parts := strings.Split(token, ".")
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	for _, key := range []string{"user_id", "username", "roles", "type", "exp", "iat", "iss", "sub", "jti"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("claim %q is missing from the token: %s", key, string(body))
		}
	}
	if raw["type"] != TokenTypeAccess {
		t.Fatalf("expected type access, got %v", raw["type"])
	}

	// The header says HS256, which is what Verify demands and nothing else.
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header map[string]any
	if err := json.Unmarshal(head, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header["alg"] != "HS256" {
		t.Fatalf("expected HS256, got %v", header["alg"])
	}
}

// A refresh token's hash is a SHA-256 hex of the token: the string the sessions
// table stores, never the token.
func TestRefreshTokenHashIsTheStoredForm(t *testing.T) {
	token, hash, err := models.NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	if len(hash) != 64 {
		t.Fatalf("expected a 64-hex-character SHA-256, got %d characters", len(hash))
	}
	sum := sha256.Sum256([]byte(token))
	if want := hex.EncodeToString(sum[:]); hash != want {
		t.Fatalf("hash is not the hex SHA-256 of the token: %s vs %s", hash, want)
	}
	if hash == token {
		t.Fatal("the stored form must not be the token")
	}
}
