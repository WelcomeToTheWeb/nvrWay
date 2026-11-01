package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The password hash nvrWay stores: argon2id, in the PHC string format the
// ecosystem reads (`$argon2id$v=19$m=…,t=…,p=…$salt$hash`), so the parameters
// travel with the hash and a parameter change never invalidates an account.
//
// The parameters are a decision, recorded on card S2-02 and in the commit that
// introduced them, not a library default:
//
//	time        3 passes
//	memory      65536 KiB (64 MiB) per hash
//	threads     4
//	key length  32 bytes
//	salt length 16 bytes, from the system CSPRNG
//
// Why these numbers. 64 MiB with 3 passes is the OWASP argon2id shape (its
// "recommended" point is t=2, m=19 MiB, p=4; its "preferred" point trades time
// for memory in the same order of magnitude), and nvrWay is an application
// server, not a password vault: the hashes it computes are a login at a rate
// limiter's pace (RATE_LIMIT_PER_MINUTE, 120/min by default), a password change,
// and the break-glass admin seed. 64 MiB x 4 threads is roughly a tenth of a
// second of CPU per hash on commodity hardware, which is the point of the
// exercise — a login is slow for the person typing it and ruinously slow for
// someone with a stolen password list. A machine room with a stolen list is the
// adversary this choice is against.
//
// argon2id, not argon2i: the hybrid is the mode that resists both the
// time-attack and the GPU-cracking family, and it is what the format's name
// says.
const (
	// Argon2Time is the number of passes over the memory region.
	Argon2Time = 3
	// Argon2MemoryKiB is the memory cost per hash, in KiB.
	Argon2MemoryKiB = 65536
	// Argon2Threads is the parallelism parameter.
	Argon2Threads = 4
	// Argon2KeyLen is the derived key length in bytes.
	Argon2KeyLen = 32
	// Argon2SaltLen is the salt length in bytes.
	Argon2SaltLen = 16

	// PasswordMaxBytes bounds the hashing cost an attacker can ask for: argon2's
	// cost is independent of password length, but memory is copied, and an
	// unbounded password is an unbounded allocation. 1024 bytes is longer than
	// any password a human types and shorter than anything a script sends.
	PasswordMaxBytes = 1024
)

// ErrPasswordTooLong is the bound above, answered before any hashing happens.
var ErrPasswordTooLong = errors.New("auth: password is too long")

// HashPassword turns a plaintext password into the stored string.
//
// The salt comes from the system CSPRNG and is random per hash, so two accounts
// with the same password have different stored strings.
func HashPassword(password string) (string, error) {
	if len(password) == 0 {
		return "", errors.New("auth: password is empty")
	}
	if len(password) > PasswordMaxBytes {
		return "", ErrPasswordTooLong
	}

	salt := make([]byte, Argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generating password salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, Argon2Time, Argon2MemoryKiB, Argon2Threads, Argon2KeyLen)
	return encodeHash(salt, key), nil
}

// VerifyPassword reports whether a plaintext password matches a stored hash.
//
// It parses the parameters out of the stored string and recomputes with them,
// so a hash written under older parameters still verifies after a parameter
// change (and NeedsRehash reports that it should be rewritten).
//
// The comparison is constant-time: `subtle.ConstantTimeCompare` over the
// derived keys, so the answer cannot be read off the time taken, and a stored
// string that does not parse is a false, not a panic — the login handler gets
// "no" and an error, and answers 401.
func VerifyPassword(password, encoded string) (bool, error) {
	if len(password) > PasswordMaxBytes {
		return false, ErrPasswordTooLong
	}
	salt, key, params, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	candidate := argon2.IDKey([]byte(password), salt, params.time, params.memoryKiB, params.threads, uint32(len(key)))
	return subtle.ConstantTimeCompare(key, candidate) == 1, nil
}

// NeedsRehash reports whether a stored hash was written under parameters other
// than the current ones. The login path uses it to rewrite the hash on a
// successful sign-in, so the parameter set moves forward without a mass
// migration and without locking anyone out.
func NeedsRehash(encoded string) bool {
	_, _, params, err := decodeHash(encoded)
	if err != nil {
		return false
	}
	return params.time != Argon2Time ||
		params.memoryKiB != Argon2MemoryKiB ||
		params.threads != Argon2Threads ||
		params.keyLen != Argon2KeyLen
}

// params is the parameter tuple as it appears in the stored string.
type params struct {
	version   uint32
	time      uint32
	memoryKiB uint32
	threads   uint8
	keyLen    uint32
}

// encodeHash writes the PHC string. Base64 without padding, as the format
// specifies: a stored hash is copied into logs, config files and the occasional
// forum post, and `=` is where padding starts arguments.
func encodeHash(salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		Argon2MemoryKiB, Argon2Time, Argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// decodeHash reads a PHC string back.
func decodeHash(encoded string) (salt, key []byte, p params, err error) {
	parts := strings.Split(encoded, "$")
	// "$argon2id$v=19$m=…,t=…,p=…$salt$hash" splits into 6 pieces, the first
	// one empty because the string starts with "$".
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, params{}, errors.New("auth: password hash is not an argon2id hash")
	}

	var version uint64
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, nil, params{}, fmt.Errorf("auth: password hash version: %w", err)
	}
	if uint32(version) != argon2.Version {
		return nil, nil, params{}, fmt.Errorf("auth: password hash version %d is not supported", version)
	}

	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memoryKiB, &p.time, &p.threads); err != nil {
		return nil, nil, params{}, fmt.Errorf("auth: password hash parameters: %w", err)
	}

	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, params{}, fmt.Errorf("auth: password hash salt: %w", err)
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, params{}, fmt.Errorf("auth: password hash: %w", err)
	}
	if len(key) == 0 || len(salt) == 0 {
		return nil, nil, params{}, errors.New("auth: password hash is truncated")
	}
	p.keyLen = uint32(len(key))
	p.version = uint32(version)

	// The parameters a stored string asks for are bounded: a hash string is
	// data, and "m=4294967295" in a database row is an invitation to allocate
	// the machine.
	if p.memoryKiB < 1024 || p.memoryKiB > 1024*1024 {
		return nil, nil, params{}, fmt.Errorf("auth: password hash memory cost %d KiB is out of range", p.memoryKiB)
	}
	if p.time == 0 || p.time > 100 {
		return nil, nil, params{}, fmt.Errorf("auth: password hash time cost %d is out of range", p.time)
	}
	if p.threads == 0 || p.threads > 64 {
		return nil, nil, params{}, fmt.Errorf("auth: password hash parallelism %d is out of range", p.threads)
	}
	if p.keyLen < 16 || p.keyLen > 1024 {
		return nil, nil, params{}, fmt.Errorf("auth: password hash key length %d is out of range", p.keyLen)
	}
	return salt, key, p, nil
}
