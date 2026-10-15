package pki

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
	"time"
)

func TestRealityKeys(t *testing.T) {
	priv, pub, err := RealityKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := RealityPublicKey(priv); got != pub {
		t.Fatal("derived public key mismatch")
	}
	if b, _ := base64.RawURLEncoding.DecodeString(pub); len(b) != 32 {
		t.Fatal("bad key length")
	}
}

func TestSS2022Key(t *testing.T) {
	k, _ := SS2022Key("2022-blake3-aes-128-gcm")
	if b, _ := base64.StdEncoding.DecodeString(k); len(b) != 16 {
		t.Fatal("aes-128 needs 16 bytes")
	}
	if _, err := SS2022Key("aes-128-gcm"); err == nil {
		t.Fatal("legacy cipher must be rejected")
	}
}

func TestCASignsNodeCert(t *testing.T) {
	ca, err := NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	_, csr, _ := NewCSR("host")
	certPEM, serial, err := ca.SignCSR(csr, "node-1", time.Hour)
	if err != nil || serial == "" {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	c, _ := x509.ParseCertificate(blk.Bytes)
	if c.Subject.CommonName != "node-1" {
		t.Fatal("node id must be bound in CN")
	}
	if _, err := c.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadCA(ca.CertPEM, ca.KeyPEM); err != nil || loaded.Fingerprint() != ca.Fingerprint() {
		t.Fatal("reload CA")
	}
}
