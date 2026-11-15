package cred

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateSecretCrea0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "secret")
	s1, err := LoadOrCreateSecret(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(s1) != SecretLen {
		t.Fatalf("len: %d", len(s1))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions: got %o, want 600", perm)
	}
	// no temporary file is left behind
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("residual tmp: %v", err)
	}

	// second load: it reuses the same secret
	s2, err := LoadOrCreateSecret(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if string(s1) != string(s2) {
		t.Fatal("it does not reuse the persisted secret")
	}
}

func TestLoadOrCreateSecretLongitudInvalida(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSecret(path); err == nil {
		t.Fatal("expected an error for invalid length")
	} else if !strings.Contains(err.Error(), "minimum") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCipherVectorAESGCM: fixed vector built with the exact construction of
// the algorithm (base64(nonce|AES-256-GCM(key, plaintext))). It guarantees
// that the format does not change with respect to data already encrypted by
// ocnews.
func TestCipherVectorAESGCM(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef") // 32 fixed bytes
	nonce := []byte("nonce-abcdef")                   // 12 bytes (standard GCM)
	plain := "user:app-token-secret"

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil))

	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Decrypt(want)
	if err != nil {
		t.Fatalf("Decrypt of the fixed vector: %v", err)
	}
	if got != plain {
		t.Fatalf("Decrypt: got %q, want %q", got, plain)
	}

	// roundtrip with a random nonce
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if enc == want {
		t.Fatal("Encrypt does not use a random nonce")
	}
	dec, err := c.Decrypt(enc)
	if err != nil || dec != plain {
		t.Fatalf("roundtrip: %q %v", dec, err)
	}

	// the empty string goes through untouched, garbage -> ErrDecrypt
	if s, err := c.Encrypt(""); s != "" || err != nil {
		t.Fatalf("Encrypt(\"\"): %q %v", s, err)
	}
	if _, err := c.Decrypt("!!!"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Decrypt garbage: %v", err)
	}
	if _, err := c.Decrypt(enc[:len(enc)-4] + "AAAA"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Decrypt tampered: %v", err)
	}
}

func TestNewCipherRechazaClaveCorta(t *testing.T) {
	if _, err := NewCipher([]byte("short")); err == nil {
		t.Fatal("expected an error for a short key")
	}
}
