package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// CA is the panel's internal certificate authority. It signs the agent-facing
// server certificate and one client certificate per node (mTLS).
type CA struct {
	Cert    *x509.Certificate
	Key     crypto.Signer
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA creates a fresh self-signed CA.
func NewCA(name string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name, Organization: []string{"Polaris"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return LoadCA(pemEncode("CERTIFICATE", der), pemEncode("EC PRIVATE KEY", keyDER))
}

// LoadCA parses a PEM CA certificate and key.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("ca key is not a signer")
	}
	return &CA{Cert: cert, Key: signer, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// Pool returns a cert pool containing the CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// Fingerprint is the SHA-256 of the CA certificate, pinned by agents on enrol.
func (ca *CA) Fingerprint() string { return Fingerprint(ca.Cert.Raw) }

// Fingerprint hex-encodes the SHA-256 of DER bytes.
func Fingerprint(der []byte) string {
	s := sha256.Sum256(der)
	return hex.EncodeToString(s[:])
}

// IssueServer issues a server certificate for the given hosts (DNS names or IPs).
func (ca *CA) IssueServer(hosts []string, validity time.Duration) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "polaris-panel"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else if h != "" {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: key}, nil
}

// SignCSR signs a node's CSR, binding the node id into the CommonName.
func (ca *CA) SignCSR(csrPEM []byte, nodeID string, validity time.Duration) (certPEM []byte, serialHex string, err error) {
	blk, _ := pem.Decode(csrPEM)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		return nil, "", errors.New("invalid CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("csr signature: %w", err)
	}
	sn := serial()
	tpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: nodeID, Organization: []string{"Polaris Node"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, "", err
	}
	return pemEncode("CERTIFICATE", der), sn.Text(16), nil
}

// NewCSR creates a key and CSR for an agent.
func NewCSR(name string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pemEncode("EC PRIVATE KEY", keyDER), pemEncode("CERTIFICATE REQUEST", der), nil
}

// SelfSigned creates a self-signed leaf certificate (for TLS inbounds without ACME).
func SelfSigned(hosts []string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	cn := "polaris"
	if len(hosts) > 0 {
		cn = hosts[0]
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pemEncode("CERTIFICATE", der), pemEncode("EC PRIVATE KEY", keyDER), nil
}

// ParseCertExpiry returns the NotAfter of the first certificate in PEM data.
func ParseCertExpiry(certPEM []byte) (time.Time, error) {
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		return time.Time{}, errors.New("invalid certificate PEM")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func pemEncode(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}
