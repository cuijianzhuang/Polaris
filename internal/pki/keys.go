// Package pki generates credentials and certificates: REALITY key pairs, SS
// 2022 PSKs, user secrets, the panel's internal CA and node client certs.
package pki

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/google/uuid"
)

// RandomBytes returns n cryptographically random bytes.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// Token returns a URL-safe random token of n bytes (subscription tokens use 32).
func Token(n int) string { return base64.RawURLEncoding.EncodeToString(RandomBytes(n)) }

// Hex returns n random bytes hex-encoded.
func Hex(n int) string { return hex.EncodeToString(RandomBytes(n)) }

// UUID returns a random v4 UUID.
func UUID() string { return uuid.NewString() }

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Password returns a random alphanumeric string of length n.
func Password(n int) string {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alnum)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		out[i] = alnum[v.Int64()]
	}
	return string(out)
}

// RealityKeyPair returns an X25519 private/public key pair encoded the way
// Xray and sing-box expect (base64 raw URL).
func RealityKeyPair() (priv, pub string, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(k.Bytes()), enc.EncodeToString(k.PublicKey().Bytes()), nil
}

// RealityPublicKey derives the public key from a private key.
func RealityPublicKey(priv string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(priv)
	if err != nil {
		return "", err
	}
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// ShortID returns a random REALITY short id of 1-8 bytes (hex).
func ShortID(n int) string {
	if n < 1 || n > 8 {
		n = 8
	}
	return Hex(n)
}

// SS2022Key returns a base64 PSK of the right length for the method.
func SS2022Key(method string) (string, error) {
	var n int
	switch method {
	case "2022-blake3-aes-128-gcm":
		n = 16
	case "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
		n = 32
	default:
		return "", fmt.Errorf("%s is not a 2022 cipher", method)
	}
	return base64.StdEncoding.EncodeToString(RandomBytes(n)), nil
}

// RandomPath returns a random URL path like "/a1b2c3d4e5f6".
func RandomPath() string { return "/" + Hex(6) }
