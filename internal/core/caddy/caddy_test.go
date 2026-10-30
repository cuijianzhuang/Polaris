package caddy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

// TestLiveFront starts Caddy in front of two plain HTTP backends and checks
// path routing over TLS.
func TestLiveFront(t *testing.T) {
	bin := os.Getenv("POLARIS_CADDY_BIN")
	sec := coretest.TLS(t, "cdn.example.com")
	backend := func(name string) string {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, name+r.URL.Path) }))
		return l.Addr().String()
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	l, _ = net.Listen("tcp", "127.0.0.1:0")
	admin := l.Addr().String()
	l.Close()
	spec := &model.NodeSpec{Fronts: []model.FrontRule{
		{Domain: "cdn.example.com", Port: port, Path: "/ws", Upstream: backend("a"), CertPEM: sec.CertPEM, KeyPEM: sec.KeyPEM},
		{Domain: "cdn.example.com", Port: port, Path: "/ws-long", Upstream: backend("b"), CertPEM: sec.CertPEM, KeyPEM: sec.KeyPEM},
	}}
	a := New(core.Options{BinPath: bin, WorkDir: t.TempDir(), APIListen: admin})
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "@r0 path /ws-long /ws-long/*") {
		t.Fatalf("longest path must come first:\n%s", cfg)
	}
	if bin == "" {
		t.Skip("POLARIS_CADDY_BIN not set")
	}
	if err := a.Validate(cfg); err != nil {
		t.Fatalf("caddy rejected config: %v\n%s", err, cfg)
	}
	core.AtomicWrite(a.configPath(), cfg, 0o600)
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(ctx)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "cdn.example.com"},
		DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, fmt.Sprintf("127.0.0.1:%d", port))
		}}}
	get := func(p string) (int, string) {
		var resp *http.Response
		var err error
		for i := 0; i < 20; i++ {
			if resp, err = client.Get("https://cdn.example.com" + p); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s: %v\n%s", p, err, strings.Join(a.Logs(30), "\n"))
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/ws"); code != 200 || body != "a/ws" {
		t.Fatalf("/ws -> %d %q", code, body)
	}
	if code, body := get("/ws-long/x"); code != 200 || body != "b/ws-long/x" {
		t.Fatalf("/ws-long -> %d %q", code, body)
	}
	if code, _ := get("/other"); code != 404 {
		t.Fatalf("unknown path -> %d", code)
	}
	// Graceful reload through the admin API keeps the PID.
	pid := a.Health(ctx).PID
	spec.Fronts = spec.Fronts[:1]
	cfg, _ = a.Render(spec)
	core.AtomicWrite(a.configPath(), cfg, 0o600)
	if err := a.Reload(ctx); err != nil || a.Health(ctx).PID != pid {
		t.Fatalf("reload: %v", err)
	}
	if code, _ := get("/ws-long/x"); code == 200 {
		if _, body := get("/ws-long/x"); strings.HasPrefix(body, "b") {
			t.Fatal("removed route still served")
		}
	}
}

// Certificates are referenced by path, so a renewal (same domain, new PEM)
// must still change the rendered config or the agent never reloads Caddy.
func TestRenderChangesWithCert(t *testing.T) {
	a := New(core.Options{WorkDir: t.TempDir()})
	render := func(sec model.SecuritySpec) string {
		cfg, err := a.Render(&model.NodeSpec{Fronts: []model.FrontRule{
			{Domain: "cdn.example.com", Port: 443, Path: "/ws", Upstream: "127.0.0.1:1", CertPEM: sec.CertPEM, KeyPEM: sec.KeyPEM}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(cfg)
	}
	first := render(coretest.TLS(t, "cdn.example.com"))
	if first == render(coretest.TLS(t, "cdn.example.com")) {
		t.Fatalf("config unchanged after a certificate change:\n%s", first)
	}
}

// Cloudflare's edges are trusted proxies, so the inbound behind sees the
// visitor in X-Forwarded-For instead of the edge address.
func TestRenderTrustsCloudflare(t *testing.T) {
	sec := coretest.TLS(t, "cdn.example.com")
	a := New(core.Options{BinPath: os.Getenv("POLARIS_CADDY_BIN"), WorkDir: t.TempDir(), APIListen: "127.0.0.1:2019"})
	cfg, err := a.Render(&model.NodeSpec{Fronts: []model.FrontRule{
		{Domain: "cdn.example.com", Port: 443, Path: "/ws", Upstream: "127.0.0.1:1", CertPEM: sec.CertPEM, KeyPEM: sec.KeyPEM}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "trusted_proxies static 173.245.48.0/20 ") || !strings.Contains(string(cfg), " 2c0f:f248::/32\n") {
		t.Fatalf("Cloudflare ranges not trusted:\n%s", cfg)
	}
	if a.opt.BinPath == "" {
		t.Skip("POLARIS_CADDY_BIN not set")
	}
	if err := a.Validate(cfg); err != nil {
		t.Fatalf("caddy rejected config: %v\n%s", err, cfg)
	}
}

// A forwarded header is trusted only after validating the TCP peer. The
// upstream receives a single sanitized IP even if the client supplied all
// three header names. Loopback represents a trusted proxy only in phase 2.
func TestLiveForwardedHeadersUseTrustedSocketPeer(t *testing.T) {
	bin := os.Getenv("POLARIS_CADDY_BIN")
	if bin == "" {
		t.Skip("POLARIS_CADDY_BIN not set")
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"xff": r.Header.Get("X-Forwarded-For"), "marker": r.Header.Get("X-Polaris-Client-IP"), "cf": r.Header.Get("CF-Connecting-IP")})
	}))
	defer backend.Close()
	address := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		return addr
	}
	listen, admin := address(), address()
	_, portText, _ := net.SplitHostPort(listen)
	port, _ := strconv.Atoi(portText)
	sec := coretest.TLS(t, "cdn.example.com")
	a := New(core.Options{BinPath: bin, WorkDir: t.TempDir(), APIListen: admin})
	cfg, err := a.Render(&model.NodeSpec{Fronts: []model.FrontRule{{Domain: "cdn.example.com", Port: port, Path: "/ws", Upstream: strings.TrimPrefix(backend.URL, "http://"), CertPEM: sec.CertPEM, KeyPEM: sec.KeyPEM}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if err = core.AtomicWrite(a.configPath(), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(ctx)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(sec.CertPEM)) {
		t.Fatal("test certificate invalid")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "cdn.example.com"}, DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, listen)
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: tr}
	request := func() map[string]string {
		t.Helper()
		var resp *http.Response
		var err error
		for i := 0; i < 30; i++ {
			req, _ := http.NewRequest("GET", "https://cdn.example.com/ws", nil)
			req.Header.Set("CF-Connecting-IP", "198.51.100.7")
			req.Header.Set("X-Forwarded-For", "203.0.113.9, 172.69.33.48")
			req.Header.Set("X-Polaris-Client-IP", "203.0.113.8")
			resp, err = client.Do(req)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err, strings.Join(a.Logs(30), "\n"))
		}
		defer resp.Body.Close()
		var got map[string]string
		if resp.StatusCode != 200 {
			t.Fatal(resp.Status)
		}
		if err = json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := request(); got["xff"] != "127.0.0.1" || got["marker"] != "127.0.0.1" || got["cf"] != "" {
		t.Fatal("untrusted peer forged upstream address", got)
	}
	// Test-only proxy allowlist: prove genuine proxy metadata is still accepted,
	// and its CF visitor header overrides the client-supplied XFF chain.
	trusted := strings.Replace(string(cfg), strings.Join(model.CloudflareRanges, " "), "127.0.0.1/32", 1)
	if err = core.AtomicWrite(a.configPath(), []byte(trusted), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got := request(); got["xff"] != "198.51.100.7" || got["marker"] != "198.51.100.7" || got["cf"] != "" {
		t.Fatal("trusted proxy visitor IP lost or XFF chain trusted", got)
	}
}
