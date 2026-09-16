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
		t.Fatalf("permisos: got %o, want 600", perm)
	}
	// no queda fichero temporal
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp residual: %v", err)
	}

	// segunda carga: reutiliza el mismo secreto
	s2, err := LoadOrCreateSecret(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if string(s1) != string(s2) {
		t.Fatal("no reutiliza el secreto persistido")
	}
}

func TestLoadOrCreateSecretLongitudInvalida(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("corto"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSecret(path); err == nil {
		t.Fatal("esperaba error por longitud inválida")
	} else if !strings.Contains(err.Error(), "mínimo") {
		t.Fatalf("error inesperado: %v", err)
	}
}

// TestCipherVectorAESGCM: vector fijo construido con la construcción exacta
// del algoritmo (base64(nonce|AES-256-GCM(key, plaintext))). Garantiza que el
// formato no cambia respecto a los datos ya cifrados por ocnews.
func TestCipherVectorAESGCM(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef") // 32 bytes fijos
	nonce := []byte("nonce-fijo12")                   // 12 bytes (GCM estándar)
	plain := "user:app-token-secreto"

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
		t.Fatalf("Decrypt del vector fijo: %v", err)
	}
	if got != plain {
		t.Fatalf("Decrypt: got %q, want %q", got, plain)
	}

	// roundtrip con nonce aleatorio
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if enc == want {
		t.Fatal("Encrypt no usa nonce aleatorio")
	}
	dec, err := c.Decrypt(enc)
	if err != nil || dec != plain {
		t.Fatalf("roundtrip: %q %v", dec, err)
	}

	// cadena vacía pasa intacta; basura → ErrDecrypt
	if s, err := c.Encrypt(""); s != "" || err != nil {
		t.Fatalf("Encrypt(\"\"): %q %v", s, err)
	}
	if _, err := c.Decrypt("!!!"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Decrypt basura: %v", err)
	}
	if _, err := c.Decrypt(enc[:len(enc)-4] + "AAAA"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Decrypt manipulado: %v", err)
	}
}

func TestNewCipherRechazaClaveCorta(t *testing.T) {
	if _, err := NewCipher([]byte("corta")); err == nil {
		t.Fatal("esperaba error por clave corta")
	}
}
