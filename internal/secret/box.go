// Package secret encrypts sensitive columns (REALITY private keys, certificate
// keys, SS master keys, API tokens) with AES-GCM. The key comes from the
// environment and never touches the database.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

const prefix = "enc:v1:"

// Box seals and opens strings.
type Box struct{ aead cipher.AEAD }

// New derives a 256-bit key from the passphrase.
func New(passphrase string) (*Box, error) {
	if passphrase == "" {
		return nil, errors.New("secret key is empty")
	}
	k := sha256.Sum256([]byte(passphrase))
	blk, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext; empty strings stay empty.
func (b *Box) Seal(plain string) string {
	if plain == "" || strings.HasPrefix(plain, prefix) {
		return plain
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	ct := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(ct)
}

// Open decrypts a sealed string. Unsealed input is returned unchanged so that
// plaintext rows from before encryption was enabled keep working.
func (b *Box) Open(s string) (string, error) {
	if !strings.HasPrefix(s, prefix) {
		return s, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		return "", err
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("ciphertext too short")
	}
	pt, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// MustOpen decrypts or returns an empty string on failure.
func (b *Box) MustOpen(s string) string {
	v, err := b.Open(s)
	if err != nil {
		return ""
	}
	return v
}
