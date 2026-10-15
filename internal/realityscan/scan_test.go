package realityscan

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if !r.OK || !strings.HasPrefix(r.Target, "example.com:") {
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
