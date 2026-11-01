package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// The parameters are a decision the card records, so they are asserted, not
// merely documented: a silent change to a cost parameter is a change to every
// account's resistance, made by whoever edited the constant.
func TestPasswordParametersAreTheRecordedOnes(t *testing.T) {
	if Argon2Time != 3 {
		t.Fatalf("time cost moved to %d; card S2-02 records 3", Argon2Time)
	}
	if Argon2MemoryKiB != 65536 {
		t.Fatalf("memory cost moved to %d KiB; card S2-02 records 65536", Argon2MemoryKiB)
	}
	if Argon2Threads != 4 {
		t.Fatalf("parallelism moved to %d; card S2-02 records 4", Argon2Threads)
	}
	if Argon2KeyLen != 32 || Argon2SaltLen != 16 {
		t.Fatalf("key/salt lengths moved to %d/%d; card S2-02 records 32/16", Argon2KeyLen, Argon2SaltLen)
	}
}

func TestHashPasswordEncodesParamsAndSalt(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	want := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$", argon2.Version, Argon2MemoryKiB, Argon2Time, Argon2Threads)
	if !strings.HasPrefix(encoded, want) {
		t.Fatalf("expected the stored string to begin %s, got %s", want, encoded)
	}

	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		t.Fatalf("expected $argon2id$v$params$salt$hash, got %q", encoded)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatalf("salt is not base64: %v", err)
	}
	if len(salt) != Argon2SaltLen {
		t.Fatalf("expected a %d-byte salt, got %d", Argon2SaltLen, len(salt))
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		t.Fatalf("hash is not base64: %v", err)
	}
	if len(key) != Argon2KeyLen {
		t.Fatalf("expected a %d-byte derived key, got %d", Argon2KeyLen, len(key))
	}

	// The salt is per hash: two accounts with the same password must not have
	// the same stored string, and the derived key must differ with it.
	again, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if again == encoded {
		t.Fatal("the same password hashed twice produced the same string: the salt is not random")
	}

	// Both verify, independently.
	for _, hash := range []string{encoded, again} {
		ok, err := VerifyPassword("correct horse battery staple", hash)
		if err != nil || !ok {
			t.Fatalf("expected the password to verify: %v (%v)", ok, err)
		}
	}
}

func TestVerifyPasswordIsFalseOnMismatch(t *testing.T) {
	encoded, err := HashPassword("the-real-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	for _, wrong := range []string{
		"",
		"the-real-password ",
		"The-real-password",
		"correct horse battery staple",
		strings.Repeat("x", PasswordMaxBytes),
	} {
		ok, err := VerifyPassword(wrong, encoded)
		if ok {
			t.Fatalf("%q verified against a hash it does not match", truncate(wrong))
		}
		// A wrong password is an answer, not an error: the handler's 401 is the
		// same for every wrong password.
		if err != nil {
			t.Fatalf("a wrong password must answer false without an error, got %v", err)
		}
	}

	// The oversize bound is an error, because it is a request that was never a
	// login attempt.
	if _, err := VerifyPassword(strings.Repeat("x", PasswordMaxBytes+1), encoded); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("expected the length bound, got %v", err)
	}
	if _, err := HashPassword(""); err == nil {
		t.Fatal("an empty password is not a password")
	}
	if _, err := HashPassword(strings.Repeat("x", PasswordMaxBytes+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("expected the length bound, got %v", err)
	}
}

// A stored string that does not parse is a false and an error, never a panic:
// the password column of a row written by hand, or an account seeded by an
// older build, must not take the login route down.
func TestVerifyPasswordRefusesMalformedStoredHashes(t *testing.T) {
	for _, bad := range []string{
		"",
		"bcrypt-looking-string",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=18$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=notamemory,t=3,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=4$c2FsdA",
		"$argon2id$v=19$m=65536,t=3,p=4$$",
		"$argon2id$v=19$m=4294967295,t=3,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=0,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=0$c2FsdA$aGFzaA",
	} {
		ok, err := VerifyPassword("correct horse battery staple", bad)
		if ok {
			t.Fatalf("%q verified", truncate(bad))
		}
		if err == nil {
			t.Fatalf("%q must answer an error", truncate(bad))
		}
	}
}

// The parameter set can move; accounts do not have to.
func TestNeedsRehashReportsAParameterChange(t *testing.T) {
	// A hash written under the OWASP minimum point, by hand.
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("legacy password"), salt, 2, 19456, 4, 32)
	legacy := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, 19456, 2, 4,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)

	if !NeedsRehash(legacy) {
		t.Fatal("a hash under other parameters must ask for a rehash")
	}
	// It still verifies: a parameter change cannot lock anyone out.
	ok, err := VerifyPassword("legacy password", legacy)
	if err != nil || !ok {
		t.Fatalf("the legacy hash must still verify: %v (%v)", ok, err)
	}
	if ok, _ := VerifyPassword("wrong", legacy); ok {
		t.Fatal("the legacy hash must refuse a wrong password")
	}

	current, err := HashPassword("current password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if NeedsRehash(current) {
		t.Fatal("a hash under the current parameters must not ask for a rehash")
	}
	// A malformed string never asks for a rehash: there is nothing to compare.
	if NeedsRehash("$argon2id$v=19$m=65536,t=3,p=4$$") {
		t.Fatal("a malformed hash must not ask for a rehash")
	}
}

// The login path's whole shape, in the two calls it makes: hash at account
// creation, verify at sign-in. `store.UserStore.SetPassword` takes the hash, so
// this is the only place a password becomes a stored string.
func TestHashAndVerifyAreTheLoginPath(t *testing.T) {
	hash, err := HashPassword("break-glass password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	// The stored string never contains the password.
	if strings.Contains(hash, "break-glass password") {
		t.Fatal("the stored string must not contain the password")
	}
	ok, err := VerifyPassword("break-glass password", hash)
	if err != nil || !ok {
		t.Fatalf("the password must verify: %v (%v)", ok, err)
	}

	// A hash is not a password: the shape the store keeps is the encoded string.
	if ok, _ := VerifyPassword(hash, hash); ok {
		t.Fatal("a hash must not verify as its own password")
	}
}

// The hash is the argon2id variant, not argon2i or argon2e: the format's name is
// part of the stored string, and the login path reads it.
func TestHashIsArgon2id(t *testing.T) {
	encoded, err := HashPassword("a password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("expected argon2id, got %s", truncate(encoded))
	}
	// Recomputed by hand, with the same parameters, the same key comes out — the
	// hash is argon2id over the salt in the string, with no application-side
	// peppering that a stolen database would let an attacker compute offline.
	parts := strings.Split(encoded, "$")
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatalf("salt: %v", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	got := argon2.IDKey([]byte("a password"), salt, Argon2Time, Argon2MemoryKiB, Argon2Threads, Argon2KeyLen)
	if string(got) != string(want) {
		t.Fatal("the stored key is not argon2id(password, salt) at the recorded parameters")
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
