package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const ciphertextVersion = "v1:"

// Cipher encrypts provider API keys at rest with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// GenerateMasterKey returns a fresh base64-encoded 32-byte master key.
func GenerateMasterKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// ParseMasterKey accepts a 32-byte key encoded as 64 hex chars or base64
// (standard or URL alphabet, padded or not).
func ParseMasterKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("master key is empty")
	}
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != 32 {
				return nil, fmt.Errorf("master key must decode to 32 bytes, got %d", len(b))
			}
			return b, nil
		}
	}
	return nil, errors.New("master key must be 32 bytes encoded as base64 or 64 hex characters (generate one with `opengate keygen`)")
}

// NewCipher builds an AES-256-GCM cipher from a 32-byte key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext with a random nonce. aad binds the ciphertext to its
// context (e.g. the database row id) so ciphertexts cannot be swapped between rows.
func (c *Cipher) Encrypt(plaintext, aad []byte) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, plaintext, aad)
	return ciphertextVersion + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a value produced by Encrypt.
func (c *Cipher) Decrypt(s string, aad []byte) ([]byte, error) {
	if !strings.HasPrefix(s, ciphertextVersion) {
		return nil, errors.New("unknown ciphertext version")
	}
	raw, err := base64.StdEncoding.DecodeString(s[len(ciphertextVersion):])
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns+c.aead.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	pt, err := c.aead.Open(nil, raw[:ns], raw[ns:], aad)
	if err != nil {
		return nil, errors.New("decrypt failed (wrong master key or tampered data)")
	}
	return pt, nil
}
