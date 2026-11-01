package store

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"nvr/server/models"
)

func issueSession(t *testing.T, st *Store, userID string, ttl time.Duration) (string, *models.Session) {
	t.Helper()
	token, hash, err := models.NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	session := &models.Session{
		UserID:    userID,
		TokenHash: hash,
		ExpiresAt: time.Now().UTC().Add(ttl),
		SourceIP:  "10.0.0.5",
		UserAgent: "nvr-web/0.1",
	}
	if err := st.Sessions.Create(session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return token, session
}

func TestSessionStoresTheHashAndNotTheToken(t *testing.T) {
	st := newTestStore(t)
	user := newUser("admin")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	token, session := issueSession(t, st, user.ID, models.RefreshTokenTTL)

	loaded, err := st.Sessions.GetByTokenHash(session.TokenHash)
	if err != nil {
		t.Fatalf("get by token hash: %v", err)
	}
	if loaded.ID != session.ID {
		t.Fatal("expected the issued session back")
	}
	// A database read cannot hand out a live session: the row holds a hash, and
	// the token lives only on the console.
	if loaded.TokenHash == token {
		t.Fatal("the row must hold the hash, never the token")
	}

	// The lookup is by hash, so a raw token is not a key.
	if _, err := st.Sessions.GetByTokenHash(token); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found for a raw token, got %v", err)
	}
	if _, err := st.Sessions.GetByTokenHash("nope"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
	if err := st.Sessions.Create(&models.Session{UserID: user.ID}); err == nil {
		t.Fatal("expected a session with no token hash to be refused")
	}
}

func TestSessionRotationNamesTheRowItReplaced(t *testing.T) {
	st := newTestStore(t)
	user := newUser("admin")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, old := issueSession(t, st, user.ID, models.RefreshTokenTTL)

	_, hash, err := models.NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	rotated, err := st.Sessions.Rotate(old, hash, time.Now().UTC().Add(models.RefreshTokenTTL))
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.RotatedFrom != old.TokenHash {
		t.Fatalf("the new row must name the token it replaced, got %q", rotated.RotatedFrom)
	}
	if rotated.UserID != user.ID {
		t.Fatal("rotation stays on the same account")
	}

	// The old token is closed.
	reloaded, err := st.Sessions.GetByTokenHash(old.TokenHash)
	if err != nil {
		t.Fatalf("reload old: %v", err)
	}
	if reloaded.RevokedAt == nil {
		t.Fatal("the rotated token must be recorded as revoked")
	}
	if reloaded.IsLive(time.Now().UTC()) {
		t.Fatal("a revoked token is not live")
	}
	if !rotated.IsLive(time.Now().UTC()) {
		t.Fatal("the replacement is live")
	}

	// An expired session is not live either, revoked or not.
	if rotated.IsLive(time.Now().UTC().Add(models.RefreshTokenTTL + time.Hour)) {
		t.Fatal("an expired session must not verify")
	}
}

func TestSessionReuseIsDetectable(t *testing.T) {
	st := newTestStore(t)
	user := newUser("admin")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, old := issueSession(t, st, user.ID, models.RefreshTokenTTL)

	_, hashOne, err := models.NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	if _, err := st.Sessions.Rotate(old, hashOne, time.Now().UTC().Add(models.RefreshTokenTTL)); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// The same token presented again is a replay. Two workers rotating it must
	// not both succeed, and the answer the auth path needs is the named error:
	// that is the moment the account's live sessions get dropped.
	_, hashTwo, err := models.NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	if _, err := st.Sessions.Rotate(old, hashTwo, time.Now().UTC().Add(models.RefreshTokenTTL)); !errors.Is(err, ErrSessionRotated) {
		t.Fatalf("expected ErrSessionRotated on reuse, got %v", err)
	}

	// The second token was never issued: the transaction rolled back.
	if _, err := st.Sessions.GetByTokenHash(hashTwo); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("a refused rotation must not create a session, got %v", err)
	}
}

func TestSessionRevokeAllFor(t *testing.T) {
	st := newTestStore(t)
	user := newUser("admin")
	other := newUser("operator-two")
	for _, u := range []*models.User{user, other} {
		if err := st.Users.Create(u); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}
	issueSession(t, st, user.ID, models.RefreshTokenTTL)
	issueSession(t, st, user.ID, models.RefreshTokenTTL)
	_, foreign := issueSession(t, st, other.ID, models.RefreshTokenTTL)

	live, err := st.Sessions.ListLiveFor(user.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	if len(live) != 2 {
		t.Fatalf("expected 2 live sessions, got %d", len(live))
	}

	dropped, err := st.Sessions.RevokeAllFor(user.ID)
	if err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if dropped != 2 {
		t.Fatalf("expected 2 sessions closed, got %d", dropped)
	}
	if live, err := st.Sessions.ListLiveFor(user.ID, time.Now().UTC()); err != nil || len(live) != 0 {
		t.Fatalf("expected no live sessions, got %d (%v)", len(live), err)
	}

	// A password change on one account does not sign anyone else out.
	if live, err := st.Sessions.ListLiveFor(other.ID, time.Now().UTC()); err != nil || len(live) != 1 {
		t.Fatalf("the other account keeps its session, got %d (%v)", len(live), err)
	}
	if _, err := st.Sessions.GetByID(foreign.ID); err != nil {
		t.Fatalf("reload foreign: %v", err)
	}

	if err := st.Sessions.Revoke("missing"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not-found revoking an unknown session, got %v", err)
	}
}

func TestSessionPruneExpired(t *testing.T) {
	st := newTestStore(t)
	user := newUser("admin")
	if err := st.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	issueSession(t, st, user.ID, models.RefreshTokenTTL)
	issueSession(t, st, user.ID, -time.Hour) // already expired

	pruned, err := st.Sessions.PruneExpired(time.Now().UTC())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected exactly the expired row pruned, got %d", pruned)
	}
}

func TestSessionTTLsAreThePlansOnes(t *testing.T) {
	if models.AccessTokenTTL != 30*time.Minute {
		t.Fatalf("access TTL is %v, the plan says 30 min", models.AccessTokenTTL)
	}
	if models.RefreshTokenTTL != 14*24*time.Hour {
		t.Fatalf("refresh TTL is %v, the plan says 14 days", models.RefreshTokenTTL)
	}
}

func TestRefreshTokenHashIsStableAnd64Hex(t *testing.T) {
	// The hash is the lookup key, so it must be deterministic; 64 hex chars is
	// what a SHA-256 in hex is, and the column is sized for it.
	const token = "00112233"
	first := models.HashRefreshToken(token)
	second := models.HashRefreshToken(token)
	if first != second {
		t.Fatal("the hash must be stable — it is the unique index the lookup walks")
	}
	if len(first) != 64 {
		t.Fatalf("expected a 64-char hex digest, got %d chars", len(first))
	}

	tokenA, hashA, err := models.NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	if tokenA == "" || hashA == "" {
		t.Fatal("expected a token and its hash")
	}
	if models.HashRefreshToken(tokenA) != hashA {
		t.Fatal("the returned hash must be the hash of the returned token")
	}
	tokenB, _, err := models.NewRefreshToken()
	if err != nil {
		t.Fatalf("new refresh token: %v", err)
	}
	if tokenA == tokenB {
		t.Fatal("two issued tokens must not collide")
	}
}
