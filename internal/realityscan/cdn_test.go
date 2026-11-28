package realityscan

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestCDNByName(t *testing.T) {
	for name, want := range map[string]string{
		"www.icann.org.cdn.cloudflare.net": "Cloudflare",
		"dualstack.python.map.fastly.net":  "Fastly",
		"eff.map.fastly.net":               "Fastly",
		"e337066.dscb.akamaiedge.net":      "Akamai",
		"www.ripe.net.edgekey.net":         "Akamai",
		"example.akamaized.net":            "Akamai",
		"dn0cqeoaah463.cloudfront.net":     "CloudFront",
		"mgnzsqc.x.incapdns.net":           "Imperva",
		// Direct origins and first-party infrastructure stay unflagged.
		"dyna.wikimedia.org":             "",
		"cms01.univie.ac.at":             "",
		"web.geo.freebsd.org":            "",
		"edge12.g.yimg.jp":               "",
		"www.wikipedia.org":              "",
		"www.debian.org":                 "",
		"notcloudflare.net.evil.example": "",
	} {
		if got := cdnByName(name); got != want {
			t.Errorf("cdnByName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCDNByIP(t *testing.T) {
	for ip, want := range map[string]string{
		"104.16.45.99":    "Cloudflare", // www.ietf.org: A record straight into Cloudflare space
		"172.65.90.24":    "Cloudflare", // inside 172.64.0.0/13
		"2606:4700::6810": "Cloudflare",
		"151.101.1.55":    "Fastly",     // www.kernel.org
		"3.166.96.46":     "CloudFront", // www.lovelive-anime.jp
		"52.84.158.94":    "CloudFront", // www.jma.go.jp (no CNAME visible)
		"54.192.100.49":   "CloudFront",
		// Origins: Wikimedia, Debian/OSUOSL, Arch, IIT Bombay, NTU.
		"208.80.153.224":  "",
		"140.211.166.200": "",
		"209.126.35.79":   "",
		"103.21.124.133":  "",
		"140.112.8.116":   "",
		"127.0.0.1":       "",
		"20.219.233.214":  "", // Azure IaaS hosting is an origin, not a CDN front
	} {
		addr := netip.MustParseAddr(ip)
		if got := cdnByIP(addr); got != want {
			t.Errorf("cdnByIP(%s) = %q, want %q", ip, got, want)
		}
	}
}

func TestCDNProviderFollowsCanonicalName(t *testing.T) {
	old := lookupCNAME
	defer func() { lookupCNAME = old }()
	lookupCNAME = func(_ context.Context, name string) (string, error) {
		if name == "www.example.org" {
			return "www.example.org.map.fastly.net", nil
		}
		return "", errors.New("no CNAME")
	}
	if got := CDNProvider(t.Context(), "www.example.org", netip.MustParseAddr("192.0.2.1")); got != "Fastly" {
		t.Fatalf("canonical-name CDN missed: %q", got)
	}
	// A lookup failure is not fatal: the address check still applies.
	if got := CDNProvider(t.Context(), "www.example.net", netip.MustParseAddr("104.16.0.1")); got != "Cloudflare" {
		t.Fatalf("IP fallback after lookup failure: %q", got)
	}
	if got := CDNProvider(t.Context(), "192.0.2.5", netip.MustParseAddr("192.0.2.5")); got != "" {
		t.Fatalf("plain IP origin flagged: %q", got)
	}
}

func TestProbeRejectsCDNFrontedTarget(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	roots = x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	defer func() { roots = nil }()
	old := cdnCheck
	defer func() { cdnCheck = old }()
	cdnCheck = func(context.Context, string, netip.Addr) string { return "Cloudflare" }
	r := Probe(t.Context(), strings.TrimPrefix(srv.URL, "https://"))
	if r.OK || r.CDN != "Cloudflare" || !strings.Contains(r.Problem, "Cloudflare") || len(r.Names) != 0 {
		t.Fatalf("CDN-fronted target accepted: %+v", r)
	}
	if r.TLS != "1.3" || r.ALPN != "h2" {
		t.Fatalf("TLS facts should still be reported: %+v", r)
	}
}
