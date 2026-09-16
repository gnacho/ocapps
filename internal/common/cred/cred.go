// Package cred: secretos persistentes y cifrado simétrico AES-256-GCM
// (SPEC §2.1). Unifica el patrón secret de los imgproxy de ocnews/ocnotes,
// el feedsecret de ocnews y el mediasecret de ocphotos (quick-win Q3:
// fail-loud si no hay entropía, sin fallback hardcodeado).
//
// Quien tenga acceso de lectura al data dir puede descifrar: la defensa es
// el aislamiento de ficheros (0600, dir 0700), no el secreto compartido.
package cred

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MinSecretLen es la longitud mínima aceptada de un secreto persistente.
const MinSecretLen = 32

// SecretLen es la longitud de los secretos generados (32 bytes = 256 bits).
const SecretLen = 32

// LoadOrCreateSecret lee el secreto de path; si no existe, genera 32 bytes
// aleatorios y los persiste (0600, escritura atómica tmp+rename). Si el
// fichero existe pero es más corto de 32 bytes es un ERROR (secreto débil o
// corrupto: nunca se regenera en silencio, para no invalidar firmas/datos
// cifrados existentes sin que el operador lo note). Si rand falla, error —
// prohibido cualquier fallback predecible (SPEC Q3).
func LoadOrCreateSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) < MinSecretLen {
			return nil, fmt.Errorf("secreto %s inválido: %d bytes (mínimo %d)", path, len(data), MinSecretLen)
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("leer secreto %s: %w", path, err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("crear dir de secreto: %w", err)
		}
	}
	key := make([]byte, SecretLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generar secreto %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, key, 0o600); err != nil {
		return nil, fmt.Errorf("persistir secreto %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("persistir secreto %s: %w", path, err)
	}
	return key, nil
}

// Cipher cifra/descifra credenciales en reposo con AES-256-GCM.
type Cipher struct {
	key []byte
}

// NewCipher construye un Cipher con una clave de 32 bytes (p. ej. de
// LoadOrCreateSecret).
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != SecretLen {
		return nil, fmt.Errorf("clave inválida: %d bytes (esperados %d)", len(key), SecretLen)
	}
	k := make([]byte, len(key))
	copy(k, key)
	return &Cipher{key: k}, nil
}

// Encrypt cifra y devuelve base64(nonce | ciphertext). "" permanece "".
func (c *Cipher) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	gcm, err := c.gcm()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// ErrDecrypt: el dato no descifra con esta clave (clave rotada o corrupción).
var ErrDecrypt = errors.New("no se puede descifrar la credencial")

// Decrypt invierte Encrypt; "" permanece "".
func (c *Cipher) Decrypt(b64 string) (string, error) {
	if b64 == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", ErrDecrypt
	}
	gcm, err := c.gcm()
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", ErrDecrypt
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}

func (c *Cipher) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
