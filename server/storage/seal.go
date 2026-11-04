// Package storage is the media tier: the destinations footage goes to, the
// drivers that speak to them, the key layout that makes a restore land back
// where it came from, and the queue that carries objects there.
//
// Two things are decided at the front of the package rather than discovered in
// a driver:
//
//   - **A database backup is readable.** Every credential column holds sealed
//     text (AES-256-GCM, `seal.go`), never plaintext, and every one of those
//     columns is `json:"-"`, so no endpoint echoes one either. Reading a backup
//     tells an operator which cameras point at which bucket; it does not tell
//     them the access key. The seal key never leaves the process.
//   - **A tier answers in names.** `Ping` failures map to the four errors the
//     API reports (`discovery_unreachable`, `bad credentials`, `sftp host key
//     changed`, `bucket missing`) rather than to a driver's own sentence, so
//     SettingsPage can say what to fix and `scripts/verify.sh` can grep for it.
package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// The sealed format, stated once because a value that cannot be read back is a
// credential an operator has to retype:
//
//	base64.StdEncoding( nonce[12] || AES-256-GCM(key, plaintext) )
//
// 12 is GCM's standard nonce length and the one `cipher.AEAD.NonceSize` returns
// for AES-256; the tag is the GCM tag, appended by `Seal` and verified by
// `Open`. There is no version prefix, because the plan's format is "base64
// stored" and a prefix would break the one question the reader can otherwise
// answer from the bytes: whether this column holds a sealed value at all.
const (
	sealNonceBytes = 12
	// sealOverhead is nonce + GCM tag, the bytes a sealed value carries on top
	// of the plaintext it hides. Anything shorter than this cannot be one.
	sealOverhead = sealNonceBytes + 16
)

// ErrNotSealed is a stored value that is not a value this server wrote: not
// base64, or too short to hold a nonce and a tag. It is a distinct answer from
// ErrSealOpen because the operator's fix is different — re-enter the credential
// here, versus find the key that sealed it somewhere else.
var ErrNotSealed = errors.New("storage: the stored secret is not a sealed value")

// ErrSealOpen is a well-formed sealed value that does not open. GCM cannot tell
// a wrong key from a tampered ciphertext, so both answers are this one, and the
// honest statement of it is "the seal key this process has is not the one that
// sealed that row".
var ErrSealOpen = errors.New("storage: the secret cannot be opened with this seal key")

// Seal encrypts a credential for storage in a database column.
//
// The empty string seals as the empty string rather than as a ciphertext of
// nothing, because "no credential" has to stay answerable without the key: the
// console's `secret_set` field, the per-kind validation and the sweeper all ask
// "is there a credential here" and none of them should need the seal key to
// answer it.
//
// The plan sketches `Seal(plain, key) string`. It returns an error too: the
// nonce comes from `crypto/rand`, and a sealing path that swallowed a random
// failure would write a credential sealed with a zero nonce — the one mistake
// that quietly reuses a nonce under the same key.
func Seal(plain string, key [32]byte) (string, error) {
	if plain == "" {
		return "", nil
	}
	aead, err := newSealer(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, sealNonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("storage: generating the sealing nonce: %w", err)
	}
	// A fresh slice each call: `Seal` appends to it, and reusing one across two
	// credentials in a row would let the second overwrite the first's tail.
	out := make([]byte, 0, len(nonce)+len(plain)+16)
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Unseal reverses Seal.
//
// A sealed value is opened with the key this process was configured with, and
// only ever for the request or the job that needs the credential — the plaintext
// lives in a local variable and is handed to a driver, never back to a handler
// that could serialise it.
func Unseal(sealed string, key [32]byte) (string, error) {
	if sealed == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("%w: it is not base64", ErrNotSealed)
	}
	if len(raw) <= sealOverhead {
		return "", fmt.Errorf("%w: %d bytes, need more than %d", ErrNotSealed, len(raw), sealOverhead)
	}
	aead, err := newSealer(key)
	if err != nil {
		return "", err
	}
	plain, err := aead.Open(nil, raw[:sealNonceBytes], raw[sealNonceBytes:], nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSealOpen, err)
	}
	return string(plain), nil
}

// newSealer builds the AES-256-GCM box for a key. The key is 32 bytes by
// construction — `config.SealKey` produces exactly that shape from
// NVR_SECRET_KEY, or from SHA-256(JWT_SECRET) when it is unset — so a wrong
// length here is a caller that built a key by hand.
func newSealer(key [32]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("storage: seal key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("storage: seal gcm: %w", err)
	}
	return aead, nil
}
