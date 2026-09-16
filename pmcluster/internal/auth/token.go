// Package auth handles pmcluster bearer tokens: generation, argon2id
// hashing, and constant-time comparison.
//
// Token format (v2): pmc_<token_id>_<secret>
//
//	token_id — 8 hex chars (4 random bytes), the unhashed public index
//	           stored in users.token_id for fast indexed lookup.
//	secret   — 32 random bytes, base64url-encoded (~43 chars).
//	The secret is argon2id-hashed and stored in users.token_hash.
//
// Legacy tokens (pre-v2) are plain base64url without a prefix or id;
// they are split on "_" — if there's only one part it's treated as a
// legacy token.  Once all users are re-issued v2 tokens the fallback
// can be dropped.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	rawTokenBytes = 32
	rawIDBytes    = 4
)

// argon2id parameters per OWASP 2024 guidance for interactive auth.
const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 1
	argonSaltLen uint32 = 16
	argonKeyLen  uint32 = 32
)

// hashPrefix tags the format so future algorithm changes are distinguishable.
const hashPrefix = "argon2id$v1$"

// v2TokenPrefix tags the new token format on the wire.
const v2TokenPrefix = "pmc_"

// ErrInvalidToken: malformed plaintext (distinct from "valid format, wrong value").
var ErrInvalidToken = errors.New("invalid token format")

// ErrInvalidHash: stored hash blob is corrupt — DB tampering or version skew.
var ErrInvalidHash = errors.New("invalid stored hash")

// GenerateToken returns a v2-format token string:
//
//	pmc_<8 hex id>_<43-char base64url secret>
//
// The caller should pass the token_id to the store alongside the hash so
// UserByToken can do an indexed lookup.
func GenerateToken() (string, error) {
	idBytes := make([]byte, rawIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return "", fmt.Errorf("read random id bytes: %w", err)
	}
	secretBytes := make([]byte, rawTokenBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", fmt.Errorf("read random secret bytes: %w", err)
	}
	return v2TokenPrefix + hex.EncodeToString(idBytes) + "_" + base64.RawURLEncoding.EncodeToString(secretBytes), nil
}

// SplitToken decomposes a token into (tokenID, secret).
//
//   - v2 tokens:  pmc_<hexID>_<secret>       → (hexID, secret)
//   - legacy tokens: plain base64 with no prefix → ("", token)
//
// tokenID is returned as the hex string so callers can pass it directly to
// a WHERE token_id = ? query.
func SplitToken(token string) (tokenID, secret string) {
	if !strings.HasPrefix(token, v2TokenPrefix) {
		return "", token
	}
	rest := strings.TrimPrefix(token, v2TokenPrefix)
	idx := strings.IndexByte(rest, '_')
	if idx < 0 {
		return "", token
	}
	return rest[:idx], rest[idx+1:]
}

// FastHash derives a fast SHA-256 index from a legacy token so we can
// still do an indexed lookup without storing the plaintext.  This is only
// for the migration bridgedev — once all tokens are v2, FastHash is
// unused.
func FastHash(token string) string {
	d := sha256.Sum256([]byte(token))
	return hex.EncodeToString(d[:])
}

// HashToken hashes with argon2id; the returned string encodes the salt so
// VerifyToken needs no additional state.
func HashToken(token string) (string, error) {
	if token == "" {
		return "", ErrInvalidToken
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	digest := argon2.IDKey([]byte(token), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return hashPrefix + hex.EncodeToString(salt) + "$" + hex.EncodeToString(digest), nil
}

// VerifyToken returns true iff token hashes to stored. Constant-time.
func VerifyToken(token, stored string) (bool, error) {
	if !strings.HasPrefix(stored, hashPrefix) {
		return false, ErrInvalidHash
	}
	parts := strings.Split(strings.TrimPrefix(stored, hashPrefix), "$")
	if len(parts) != 2 {
		return false, ErrInvalidHash
	}
	salt, err := hex.DecodeString(parts[0])
	if err != nil || len(salt) != int(argonSaltLen) {
		return false, ErrInvalidHash
	}
	want, err := hex.DecodeString(parts[1])
	if err != nil || len(want) != int(argonKeyLen) {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey([]byte(token), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return subtle.ConstantTimeCompare(want, got) == 1, nil
}
