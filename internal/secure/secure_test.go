package secure

import (
	"strings"
	"testing"
)

func TestVirtualKeyFormatAndHash(t *testing.T) {
	k, err := GenerateVirtualKey()
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLikeVirtualKey(k) {
		t.Fatalf("generated key %q failed syntax check", k)
	}
	k2, _ := GenerateVirtualKey()
	if k == k2 {
		t.Fatal("keys must be unique")
	}
	h := HashKey(k)
	if len(h) != 64 || h == k || strings.Contains(h, k[3:]) {
		t.Fatalf("bad hash %q", h)
	}
	if HashKey(k) != h {
		t.Fatal("hash must be deterministic")
	}
	if HashKey(k2) == h {
		t.Fatal("different keys must hash differently")
	}
	for _, bad := range []string{"", "sk-abc", "og-xyz", "og-" + strings.Repeat("z", 48)} {
		if LooksLikeVirtualKey(bad) {
			t.Fatalf("%q should not look like a key", bad)
		}
	}
	if DisplayPrefix(k) != k[:11] {
		t.Fatal("display prefix")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("abc", "abc") || ConstantTimeEqual("abc", "abd") || ConstantTimeEqual("abc", "abcd") {
		t.Fatal("ConstantTimeEqual wrong")
	}
}

func TestCipherRoundTripAndTamper(t *testing.T) {
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParseMasterKey(mk)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("sk-live-very-secret")
	ct, err := c.Encrypt(secret, []byte("pk_1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, string(secret)) || !strings.HasPrefix(ct, "v1:") {
		t.Fatalf("ciphertext leaks or wrong format: %s", ct)
	}
	ct2, _ := c.Encrypt(secret, []byte("pk_1"))
	if ct == ct2 {
		t.Fatal("nonces must be random")
	}
	pt, err := c.Decrypt(ct, []byte("pk_1"))
	if err != nil || string(pt) != string(secret) {
		t.Fatalf("round trip failed: %v %q", err, pt)
	}
	if _, err := c.Decrypt(ct, []byte("pk_2")); err == nil {
		t.Fatal("decrypt with wrong AAD must fail")
	}
	raw := []byte(ct)
	raw[len(raw)-3] ^= 1
	if _, err := c.Decrypt(string(raw), []byte("pk_1")); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
	otherKey, _ := GenerateMasterKey()
	ok, _ := ParseMasterKey(otherKey)
	c2, _ := NewCipher(ok)
	if _, err := c2.Decrypt(ct, []byte("pk_1")); err == nil {
		t.Fatal("wrong master key must fail")
	}
}

func TestParseMasterKey(t *testing.T) {
	hexKey := strings.Repeat("ab", 32)
	if b, err := ParseMasterKey(hexKey); err != nil || len(b) != 32 {
		t.Fatalf("hex key: %v", err)
	}
	for _, bad := range []string{"", "short", "c2hvcnQ="} {
		if _, err := ParseMasterKey(bad); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}
