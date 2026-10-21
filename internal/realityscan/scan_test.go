package realityscan

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbe(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	roots = x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	defer func() { roots = nil }()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	// httptest's certificate names example.com: probing the IP finds it.
	r := Probe(t.Context(), "127.0.0.1:"+port)
	if r.TLS != "1.3" || r.ALPN != "h2" || !r.PQ || r.KeyShare != "X25519MLKEM768" {
		t.Fatalf("probe = %+v", r)
	}
	if !r.OK || r.Target != "127.0.0.1:"+port || r.ServerName != "example.com" || len(r.Names) != 1 || r.Names[0] != "example.com" || r.LatencyMs == nil || *r.LatencyMs < 0 {
		t.Fatalf("IP probe should discover example.com from the certificate: %+v", r)
	}
	rs, err := Scan(t.Context(), "127.0.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range rs {
		if x.TLS == "" {
			t.Fatalf("range scan kept a non-answer: %+v", x)
		}
	}
}

func TestScanTargetList(t *testing.T) {
	// Unreachable names fail fast; the point is parsing and dedupe.
	rs, err := Scan(context.Background(), "a.invalid, b.invalid:8443\na.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("want 2 deduplicated results, got %+v", rs)
	}
	if _, err := Scan(context.Background(), "a.invalid,10.0.0.0/33"); err == nil {
		t.Fatal("bad CIDR must fail")
	}
}

func TestIPDiscoveryRechecksSNI(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if hello.ServerName != "" {
			// The default virtual host looks suitable, but the named virtual
			// host behaves differently. Certificate discovery is not enough.
			return &tls.Config{Certificates: srv.TLS.Certificates, MaxVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}, nil
		}
		return nil, nil
	}}
	srv.StartTLS()
	defer srv.Close()
	roots = x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	defer func() { roots = nil }()
	address := strings.TrimPrefix(srv.URL, "https://")
	r := Probe(t.Context(), address)
	if r.OK || r.Target != address || r.ServerName != "example.com" || r.TLS != "1.2" || len(r.Names) != 0 {
		t.Fatalf("default-host certificate incorrectly passed live SNI validation: %+v", r)
	}
}

func TestScanCandidateEndpointIdentity(t *testing.T) {
	var servers []*httptest.Server
	for range 2 {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.EnableHTTP2 = true
		srv.StartTLS()
		t.Cleanup(srv.Close)
		servers = append(servers, srv)
	}
	roots = x509.NewCertPool()
	for _, srv := range servers {
		roots.AddCert(srv.Certificate())
	}
	defer func() { roots = nil }()
	first := Candidate{Address: strings.TrimPrefix(servers[0].URL, "https://"), ServerName: "example.com", ASN: "AS64500", Org: "Fixture", Country: "JP"}
	second := Candidate{Address: strings.TrimPrefix(servers[1].URL, "https://"), ServerName: "example.com"}
	rs, err := ScanCandidates(t.Context(), []Candidate{first, second, first, {Address: first.Address}})
	if err != nil || len(rs) != 2 {
		t.Fatalf("distinct dial endpoints collapsed: %+v %v", rs, err)
	}
	for _, r := range rs {
		if !r.OK || r.Target != r.Address || r.ServerName != "example.com" || len(r.Names) != 1 {
			t.Fatalf("endpoint and verified SNI diverged: %+v", r)
		}
		if r.Target == first.Address && (r.ASN != first.ASN || r.Org != first.Org || r.Country != first.Country) {
			t.Fatal("discovery metadata lost", r)
		}
	}
	bad := ProbeCandidate(t.Context(), Candidate{Address: first.Address, ServerName: "other.invalid"})
	if bad.OK || len(bad.Names) != 0 || bad.Problem != "证书对该域名无效" {
		t.Fatal("certificate accepted for an unverified SNI", bad)
	}
}

func TestScanCancellationAndLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ScanCandidates(ctx, []Candidate{{Address: "example.invalid"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan: %v", err)
	}
	var targets []Candidate
	for i := range maxTotal + 1 {
		targets = append(targets, Candidate{Address: fmt.Sprintf("host%d.invalid", i)})
	}
	start := time.Now()
	if _, err := ScanCandidates(t.Context(), targets); err == nil || time.Since(start) > time.Second {
		t.Fatalf("oversized input must fail before dialing: %v", err)
	}
	if _, err := Scan(t.Context(), "203.0.113.0/24,198.51.100.0/24,192.0.2.0/24"); err == nil {
		t.Fatal("CIDRs silently truncated beyond scan limit")
	}
}
