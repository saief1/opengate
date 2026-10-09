// Package secure contains the small amount of cryptography OpenGate relies on:
// virtual key generation/hashing and AES-256-GCM encryption of provider keys.
package secure

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// KeyPrefix is the prefix of every OpenGate virtual key.
const KeyPrefix = "og-"

// GenerateVirtualKey returns a new random virtual key ("og-" + 48 hex chars,
// 192 bits of entropy). The plaintext is shown to the operator exactly once;
// only its hash is stored.
func GenerateVirtualKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return KeyPrefix + hex.EncodeToString(b), nil
}

// HashKey returns the hex SHA-256 digest of a virtual key. Virtual keys carry
// 192 bits of entropy, so a fast hash is appropriate (no password-style KDF is
// needed to resist brute force), and it keeps per-request auth cheap.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// LooksLikeVirtualKey performs a cheap syntactic check before hitting storage.
func LooksLikeVirtualKey(key string) bool {
	if !strings.HasPrefix(key, KeyPrefix) || len(key) != len(KeyPrefix)+48 {
		return false
	}
	_, err := hex.DecodeString(key[len(KeyPrefix):])
	return err == nil
}

// DisplayPrefix returns a short, non-secret identifier for UIs and logs.
func DisplayPrefix(key string) string {
	if len(key) < 11 {
		return key
	}
	return key[:11]
}

// ConstantTimeEqual compares two secrets without leaking timing information.
func ConstantTimeEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// RandomID returns prefix + 16 random hex chars, used for row identifiers.
func RandomID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// RandomToken returns n random bytes, base64url-encoded (session and invite tokens).
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
