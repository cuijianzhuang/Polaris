package caddy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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
