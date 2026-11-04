package storage

// S7-02 — the seal: what a credential column holds, and what it takes to read
// it back.
//
// The exit criteria are "round-trip + tamper detection asserted", "key
// derivation asserted (same JWT_SECRET -> same key)", "NVR_SECRET_KEY
// documented as required in production" and "no sealed value is ever returned
// by an endpoint". The first two are here; the third is in
// `docs/configuration.md` and `.env.example` (both already state it, and the
// round-trip test below is what makes that sentence load-bearing); the fourth
// starts here at the model — a sealed value cannot be echoed by an endpoint
// that never serialises the column — and is asserted again at the response body
// in S7-10 and S6-07.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nvr/server/config"
	"nvr/server/models"
)

// sealKey is the key every test here seals with, built the way the process
// builds its own.
func sealKey(t *testing.T) [32]byte {
	t.Helper()
	cfg := &config.Config{SecretKey: strings.Repeat("ab", 32)}
	key, err := cfg.SealKey()
	if err != nil {
		t.Fatalf("SealKey: %v", err)
	}
	return key
}

func TestSealRoundTrip(t *testing.T) {
	key := sealKey(t)

	cases := []struct {
		name  string
		plain string
	}{
		{"an s3 secret key", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		{"a password with symbols", "p@ss w0rd/with=+chars:and,commas"},
		{"a private key path", "/etc/nvr/keys/id_ed25519"},
		{"a single character", "x"},
		{"a long secret", strings.Repeat("long-", 500)},
		{"non-ascii", "ключ-пароль-🔐"},
		{"a value with newlines", "-----BEGIN KEY-----\nabc\ndef\n-----END KEY-----\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sealed, err := Seal(tc.plain, key)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if sealed == tc.plain {
				t.Fatalf("Seal returned the plaintext unchanged: %q", sealed)
			}
			if strings.Contains(sealed, tc.plain) && len(tc.plain) > 3 {
				t.Fatalf("the sealed value contains the plaintext: %q", sealed)
			}
			got, err := Unseal(sealed, key)
			if err != nil {
				t.Fatalf("Unseal: %v", err)
			}
			if got != tc.plain {
				t.Fatalf("round trip changed the value: got %q, want %q", got, tc.plain)
			}
		})
	}
}

func TestSealEmptyStaysEmpty(t *testing.T) {
	// "No credential" has to stay answerable without the key: `secret_set`, the
	// per-kind validation and the sweeper all ask whether a credential is set.
	key := sealKey(t)
	sealed, err := Seal("", key)
	if err != nil {
		t.Fatalf("Seal(\"\"): %v", err)
	}
	if sealed != "" {
		t.Fatalf("Seal(\"\") = %q, want the empty string", sealed)
	}
	plain, err := Unseal("", key)
	if err != nil {
		t.Fatalf("Unseal(\"\"): %v", err)
	}
	if plain != "" {
		t.Fatalf("Unseal(\"\") = %q, want the empty string", plain)
	}
}

func TestSealUsesAFreshNonceEveryTime(t *testing.T) {
	// Two seals of the same plaintext must not produce the same bytes: a sealed
	// value that repeats is a nonce that repeated under the same key, and that
	// is the one leak GCM gives up for free.
	key := sealKey(t)
	const plain = "the-same-secret-every-time"

	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		sealed, err := Seal(plain, key)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if seen[sealed] {
			t.Fatalf("Seal produced the same value twice: %q", sealed)
		}
		seen[sealed] = true

		got, err := Unseal(sealed, key)
		if err != nil || got != plain {
			t.Fatalf("Unseal(%d) = %q, %v", i, got, err)
		}
	}
}

func TestSealTamperIsDetected(t *testing.T) {
	key := sealKey(t)
	sealed, err := Seal("hunter2", key)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	t.Run("a flipped ciphertext byte", func(t *testing.T) {
		for i := sealNonceBytes + 1; i < len(sealed); i++ {
			// Only worth checking a handful of positions; the base64 alphabet
			// makes an exact byte flip awkward, so swap two characters instead.
			if i > sealNonceBytes+8 {
				break
			}
			tampered := []byte(sealed)
			tampered[i] = nextBase64Char(tampered[i])
			if _, err := Unseal(string(tampered), key); !errors.Is(err, ErrSealOpen) {
				t.Fatalf("Unseal with byte %d changed: got %v, want ErrSealOpen", i, err)
			}
		}
	})

	t.Run("a flipped nonce byte", func(t *testing.T) {
		// A changed nonce is not a changed message, but GCM fails on it too —
		// and the point of asserting it is that nothing about the *shape* of the
		// damage can slip the check.
		tampered := []byte(sealed)
		tampered[3] = nextBase64Char(tampered[3])
		if _, err := Unseal(string(tampered), key); !errors.Is(err, ErrSealOpen) {
			t.Fatalf("Unseal with the nonce changed: got %v, want ErrSealOpen", err)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		if _, err := Unseal(sealed[:len(sealed)-4], key); !errors.Is(err, ErrSealOpen) {
			t.Fatalf("Unseal truncated: got %v, want ErrSealOpen", err)
		}
	})

	t.Run("the original still opens", func(t *testing.T) {
		got, err := Unseal(sealed, key)
		if err != nil || got != "hunter2" {
			t.Fatalf("Unseal after the tamper tests: %q, %v", got, err)
		}
	})
}

func TestSealWrongKeyDoesNotOpen(t *testing.T) {
	sealed, err := Seal("hunter2", sealKey(t))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	other := [32]byte{}
	other[0] = 0x01

	_, err = Unseal(sealed, other)
	if !errors.Is(err, ErrSealOpen) {
		t.Fatalf("Unseal with another key: got %v, want ErrSealOpen", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the failure names the plaintext, which is a leak: %v", err)
	}
}

func TestNotSealedValuesAreNamedAsTheirOwnFailure(t *testing.T) {
	key := sealKey(t)
	for _, raw := range []string{"not base64 !!", "a", "abcd", "", strings.Repeat("A", sealOverhead)} {
		_, err := Unseal(raw, key)
		if raw == "" {
			if err != nil {
				t.Fatalf("Unseal(\"\") = %v, want no error", err)
			}
			continue
		}
		if !errors.Is(err, ErrNotSealed) && !errors.Is(err, ErrSealOpen) {
			t.Fatalf("Unseal(%q) = %v, want ErrNotSealed or ErrSealOpen", raw, err)
		}
		if strings.Contains(err.Error(), "nil") {
			t.Fatalf("Unseal(%q) = %v", raw, err)
		}
	}

	// A value that is not base64 at all is the case an operator hits when a row
	// was written by hand, and it must not read as "wrong key".
	if _, err := Unseal("plainly-not-base64-$$$", key); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("Unseal of a hand-written value: got %v, want ErrNotSealed", err)
	}
}

func TestSealKeyComesFromTheConfiguration(t *testing.T) {
	// The criterion is "same JWT_SECRET -> same key". The seal key is
	// `config.SealKey`'s output, so what is really being asserted is that two
	// processes configured the same way open each other's rows, and two
	// configured differently do not — which is the difference between a backup
	// that restores and a backup that needs every credential retyped.
	t.Run("the same JWT_SECRET gives the same key", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "one-shared-deployment-secret")
		t.Setenv("NVR_SECRET_KEY", "")

		first, err := config.Load().SealKey()
		if err != nil {
			t.Fatalf("SealKey: %v", err)
		}
		second, err := config.Load().SealKey()
		if err != nil {
			t.Fatalf("SealKey: %v", err)
		}
		if first != second {
			t.Fatal("two loads of the same configuration produced different seal keys")
		}

		sealed, err := Seal("s3-secret", first)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		got, err := Unseal(sealed, second)
		if err != nil || got != "s3-secret" {
			t.Fatalf("Unseal with the other process's key: %q, %v", got, err)
		}
	})

	t.Run("a different JWT_SECRET gives a different key", func(t *testing.T) {
		t.Setenv("NVR_SECRET_KEY", "")

		t.Setenv("JWT_SECRET", "first-secret")
		before, err := config.Load().SealKey()
		if err != nil {
			t.Fatalf("SealKey: %v", err)
		}
		t.Setenv("JWT_SECRET", "second-secret")
		after, err := config.Load().SealKey()
		if err != nil {
			t.Fatalf("SealKey: %v", err)
		}
		if before == after {
			t.Fatal("rotating JWT_SECRET did not change the derived seal key")
		}

		sealed, err := Seal("s3-secret", before)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if _, err := Unseal(sealed, after); !errors.Is(err, ErrSealOpen) {
			t.Fatalf("a row sealed before a JWT rotation opens after it: %v", err)
		}
	})

	t.Run("NVR_SECRET_KEY wins and survives a JWT rotation", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "first-secret")
		t.Setenv("NVR_SECRET_KEY", strings.Repeat("cd", 32))

		before, err := config.Load().SealKey()
		if err != nil {
			t.Fatalf("SealKey: %v", err)
		}
		if hex(before) != strings.Repeat("cd", 32) {
			t.Fatalf("SealKey ignored NVR_SECRET_KEY: %s", hex(before))
		}

		t.Setenv("JWT_SECRET", "rotated-secret")
		after, err := config.Load().SealKey()
		if err != nil {
			t.Fatalf("SealKey: %v", err)
		}
		if before != after {
			t.Fatal("NVR_SECRET_KEY is supposed to be the stable one; a JWT rotation moved it")
		}
	})

	t.Run("an unparseable NVR_SECRET_KEY is refused, not defaulted", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "first-secret")
		t.Setenv("NVR_SECRET_KEY", "not-hex")
		if _, err := config.Load().SealKey(); err == nil {
			t.Fatal("a 64-char value that is not hex must not silently become the derived key")
		}
	})
}

func TestSealedValuesAreNeverSerialised(t *testing.T) {
	// The criterion is "no sealed value is ever returned by an endpoint". The
	// endpoints serialise these models, and the secret columns carry `json:"-"`,
	// so the invariant is checked at the model: a sealed value put in one of
	// them cannot reach a response body, and `secret_set` is the field that
	// says a credential is there instead.
	key := sealKey(t)

	password, err := Seal("webdav-password", key)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	access, err := Seal("AKIAIOSFODNN7EXAMPLE", key)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	secret, err := Seal("wJalrXUtnFEMI/K7MDENG", key)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	path, err := Seal("/etc/nvr/keys/id_ed25519", key)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	backend := &models.StorageBackend{
		ID: "b1", Name: "minio", Kind: models.StorageKindS3,
		Password: password, AccessKey: access, SecretKey: secret, PrivateKeyPath: path,
	}
	assertSealedAreAbsent(t, backend, "storage backend", password, access, secret, path)
	if !backend.SecretSet() {
		t.Fatal("SecretSet is false for a backend that has credentials")
	}

	hook := &models.Webhook{ID: "w1", Name: "ops", URL: "https://ops.example/hook", Secret: secret}
	assertSealedAreAbsent(t, hook, "webhook", secret)
	if !hook.SecretSet() {
		t.Fatal("SecretSet is false for a webhook that has a signing secret")
	}

	// What the console is given instead is `secret_set`, and it is a *method* on
	// the row rather than a field — encoding/json does not serialise methods,
	// so the API's view has to name it. That half of the criterion is asserted
	// on the response body in S7-10 and S6-07; what is pinned here is the half
	// that makes it true: the row itself cannot leak the value, and the method
	// the view calls answers correctly.
	raw, err := json.Marshal(backend)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "secret_set") {
		t.Fatalf("the model serialises secret_set itself (%s), so S7-10's view would set it twice", raw)
	}
}

// assertSealedAreAbsent marshals a model and fails if any sealed value appears
// in the bytes, or if the plaintext appears either.
func assertSealedAreAbsent(t *testing.T, row any, what string, sealed ...string) {
	t.Helper()
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("Marshal %s: %v", what, err)
	}
	for _, value := range sealed {
		if value == "" {
			continue
		}
		if strings.Contains(string(raw), value) {
			t.Fatalf("the %s response contains a sealed value: %s", what, raw)
		}
	}
	// `json:"-"` is the whole mechanism, so a rename that drops it must fail
	// something. The column names are the canary: none of them may appear.
	for _, column := range []string{"password", "access_key", "secret_key", "private_key_path", "secret"} {
		if what == "storage backend" && column == "secret" {
			// `secret_set` legitimately names itself.
			continue
		}
		if strings.Contains(string(raw), `"`+column+`"`) {
			t.Fatalf("the %s response serialises the %q column: %s", what, column, raw)
		}
	}
}

func hex(b [32]byte) string {
	const digits = "0123456789abcdef"
	var out strings.Builder
	for _, c := range b {
		out.WriteByte(digits[c>>4])
		out.WriteByte(digits[c&0x0f])
	}
	return out.String()
}

// nextBase64Char moves one character along the standard alphabet, so a tampered
// value stays decodable base64 and still reaches the authentication check —
// which is the tamper that matters. A flip to a non-alphabet byte fails at the
// base64 decode and proves nothing about GCM.
func nextBase64Char(c byte) byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	i := strings.IndexByte(alphabet, c)
	if i < 0 {
		return 'A'
	}
	return alphabet[(i+1)%len(alphabet)]
}

// TestSealedFormatIsThePlansFormat pins the bytes on the wire: base64 of
// nonce||ciphertext||tag, with no prefix. A format change is a migration, and
// this test is what makes it a deliberate one.
func TestSealedFormatIsThePlansFormat(t *testing.T) {
	key := sealKey(t)
	sealed, err := Seal("a-secret-of-medium-length", key)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.ContainsAny(sealed, ":|") {
		t.Fatalf("the sealed value carries a prefix, which the plan's format does not: %q", sealed)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("the sealed value is not base64: %v", err)
	}
	const plain = "a-secret-of-medium-length"
	if len(raw) != sealNonceBytes+len(plain)+16 {
		t.Fatalf("sealed length = %d, want %d (nonce %d + plaintext %d + tag 16)",
			len(raw), sealNonceBytes+len(plain)+16, sealNonceBytes, len(plain))
	}
}
