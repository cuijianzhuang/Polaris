package pki

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
)

// VLESS Encryption keys, in the format `xray vlessenc` prints: the server's
// "decryption" carries the private part (an X25519 key, or an ML-KEM-768
// seed for post-quantum authentication), the client's "encryption" the
// public part. Both use the ephemeral ML-KEM-768 + X25519 key exchange.
const (
	vlessEncPrefix  = "mlkem768x25519plus.native."
	VLESSAuthX25519 = "x25519"
	VLESSAuthMLKEM  = "mlkem768"
)

// VLESSEncPair generates a decryption / encryption pair.
func VLESSEncPair(auth string) (decryption, encryption string, err error) {
	enc := base64.RawURLEncoding
	var priv, pub []byte
	switch auth {
	case "", VLESSAuthX25519:
		k, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return "", "", err
		}
		priv, pub = k.Bytes(), k.PublicKey().Bytes()
	case VLESSAuthMLKEM:
		seed := make([]byte, mlkem.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return "", "", err
		}
		k, err := mlkem.NewDecapsulationKey768(seed)
		if err != nil {
			return "", "", err
		}
		priv, pub = seed, k.EncapsulationKey().Bytes()
	default:
		return "", "", fmt.Errorf("unknown VLESS authentication %q", auth)
	}
	return vlessEncPrefix + "600s." + enc.EncodeToString(priv), vlessEncPrefix + "0rtt." + enc.EncodeToString(pub), nil
}

// VLESSAuthOf reports which authentication a decryption key uses ("" when
// it is not a key this panel understands).
func VLESSAuthOf(decryption string) string {
	if !strings.HasPrefix(decryption, vlessEncPrefix) {
		return ""
	}
	parts := strings.Split(decryption, ".")
	b, err := base64.RawURLEncoding.DecodeString(parts[len(parts)-1])
	switch {
	case err != nil:
		return ""
	case len(b) == 32:
		return VLESSAuthX25519
	case len(b) == mlkem.SeedSize:
		return VLESSAuthMLKEM
	}
	return ""
}

// MLDSA65Pair generates a REALITY ML-DSA-65 seed and its verify key.
func MLDSA65Pair() (seed, verify string, err error) {
	var s [32]byte
	if _, err := rand.Read(s[:]); err != nil {
		return "", "", err
	}
	v, err := mldsa65Verify(s)
	if err != nil {
		return "", "", err
	}
	return base64.RawURLEncoding.EncodeToString(s[:]), v, nil
}

// MLDSA65Verify derives the verify key from a seed.
func MLDSA65Verify(seed string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(seed)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("invalid ML-DSA-65 seed")
	}
	return mldsa65Verify([32]byte(b))
}

func mldsa65Verify(seed [32]byte) (string, error) {
	pk, _ := mldsa65.NewKeyFromSeed(&seed)
	b, err := pk.MarshalBinary()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
