package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/api/gen/polaris/v1/polarisv1connect"
	"github.com/cuijianzhuang/polaris/internal/pki"
)

// Identity is the agent's enrolled credentials.
type Identity struct {
	NodeID string
	Cert   tls.Certificate
	CA     *x509.CertPool
	CAPEM  []byte
}

func (a *Agent) idDir() string { return filepath.Join(a.cfg.DataDir, "agent") }

// LoadIdentity reads the saved certificate, key and CA.
func LoadIdentity(dir string) (*Identity, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "node.crt"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "node.key"))
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid CA file")
	}
	return &Identity{NodeID: leaf.Subject.CommonName, Cert: pair, CA: pool, CAPEM: caPEM}, nil
}

// Enroll exchanges a one-time token for a client certificate. The panel's CA
// is pinned by the SHA-256 fingerprint printed in the install command.
func Enroll(ctx context.Context, panelURL, token, caSHA256, dataDir, version string) (*Identity, error) {
	caSHA256 = strings.ToLower(strings.ReplaceAll(caSHA256, ":", ""))
	if caSHA256 == "" {
		return nil, errors.New("--ca-sha256 is required to pin the panel CA")
	}
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, // verified below against the pinned CA
		MinVersion:         tls.VersionTLS12,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			var ca *x509.Certificate
			for _, der := range raw {
				if pki.Fingerprint(der) == caSHA256 {
					ca, _ = x509.ParseCertificate(der)
				}
			}
			if ca == nil {
				return errors.New("panel CA fingerprint mismatch")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			pool := x509.NewCertPool()
			pool.AddCert(ca)
			_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return err
		},
	}
	client := polarisv1connect.NewAgentServiceClient(httpClient(tlsCfg), panelURL, connect.WithGRPC())
	hostname, _ := os.Hostname()
	keyPEM, csrPEM, err := pki.NewCSR(hostname)
	if err != nil {
		return nil, err
	}
	resp, err := client.Enroll(ctx, connect.NewRequest(&polarisv1.EnrollRequest{
		Token: token, CsrPem: csrPEM, Hostname: hostname, AgentVersion: version}))
	if err != nil {
		return nil, fmt.Errorf("enroll: %w", err)
	}
	dir := filepath.Join(dataDir, "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for name, data := range map[string][]byte{"node.crt": resp.Msg.CertPem, "node.key": keyPEM, "ca.crt": resp.Msg.CaPem} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	return LoadIdentity(dir)
}

// httpClient builds an HTTP/2 client (required for gRPC bidi streams).
func httpClient(tlsCfg *tls.Config) *http.Client {
	return &http.Client{Transport: &http2.Transport{
		TLSClientConfig: tlsCfg,
		ReadIdleTimeout: 30 * time.Second,
		PingTimeout:     10 * time.Second,
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}, Config: cfg}
			return d.DialContext(ctx, network, addr)
		},
	}}
}

func (id *Identity) tlsConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{id.Cert}, RootCAs: id.CA, MinVersion: tls.VersionTLS12}
}
