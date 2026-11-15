// Package cred: persistent secrets and AES-256-GCM symmetric encryption
// (SPEC §2.1). It unifies the secret pattern of the ocnews/ocnotes imgproxy,
// the ocnews feedsecret and the ocphotos mediasecret (quick win Q3: fail loud
// if there is no entropy, with no hardcoded fallback).
//
// Anyone with read access to the data dir can decrypt: the defense is file
// isolation (0600, dir 0700), not the shared secret.
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

// MinSecretLen is the minimum accepted length of a persistent secret.
const MinSecretLen = 32

// SecretLen is the length of the generated secrets (32 bytes = 256 bits).
const SecretLen = 32

// LoadOrCreateSecret reads the secret from path, if it does not exist it
// generates 32 random bytes and persists them (0600, atomic write
// tmp+rename). If the file exists but is shorter than 32 bytes it is an ERROR
// (weak or corrupt secret: it is never regenerated silently, so that it does
// not invalidate existing signatures/encrypted data without the operator
// noticing). If rand fails, error - any predictable fallback is forbidden
// (SPEC Q3).
func LoadOrCreateSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) < MinSecretLen {
			return nil, fmt.Errorf("invalid secret %s: %d bytes (minimum %d)", path, len(data), MinSecretLen)
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read secret %s: %w", path, err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create secret dir: %w", err)
		}
	}
	key := make([]byte, SecretLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate secret %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, key, 0o600); err != nil {
		return nil, fmt.Errorf("persist secret %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("persist secret %s: %w", path, err)
	}
	return key, nil
}

// Cipher encrypts/decrypts credentials at rest with AES-256-GCM.
type Cipher struct {
	key []byte
}

// NewCipher builds a Cipher with a 32 byte key (e.g. from LoadOrCreateSecret).
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != SecretLen {
		return nil, fmt.Errorf("invalid key: %d bytes (%d expected)", len(key), SecretLen)
	}
	k := make([]byte, len(key))
	copy(k, key)
	return &Cipher{key: k}, nil
}

// Encrypt encrypts and returns base64(nonce | ciphertext). "" stays "".
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

// ErrDecrypt: the data does not decrypt with this key (rotated key or corruption).
var ErrDecrypt = errors.New("cannot decrypt the credential")

// Decrypt reverses Encrypt, "" stays "".
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
