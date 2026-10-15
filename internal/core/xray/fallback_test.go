package xray

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestFallbacksRender(t *testing.T) {
	master := coretest.Inbound(model.CoreXray, "vless-tls", 443, model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision",
		Fallbacks: []model.Fallback{{Path: "/ws", Dest: "127.0.0.1:2001"}, {Dest: "127.0.0.1:2002"}, {Path: "/x"}}},
		model.TransportSpec{Type: model.TransTCP}, coretest.TLS(t, "a.example.com"))
	ws := coretest.Inbound(model.CoreXray, "vless-ws", 2001, model.ProtocolSpec{Type: model.ProtoVLESS},
		model.TransportSpec{Type: model.TransWS, Path: "/ws"}, model.SecuritySpec{})
	ws.Listen = "127.0.0.1"
	trojan := coretest.Inbound(model.CoreXray, "trojan", 2002, model.ProtocolSpec{Type: model.ProtoTrojan},
		model.TransportSpec{Type: model.TransTCP}, model.SecuritySpec{})
	trojan.Listen = "127.0.0.1"
	a := New(core.Options{Version: "26.3.27", WorkDir: t.TempDir(), BinPath: os.Getenv("POLARIS_XRAY_BIN")})
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{master, ws, trojan}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	if !strings.Contains(s, `"path": "/ws"`) || !strings.Contains(s, `"dest": "127.0.0.1:2002"`) || strings.Count(s, `"dest"`) != 2 {
		t.Fatalf("fallbacks not rendered (unresolved entries must be dropped):\n%s", s)
	}
	if os.Getenv("POLARIS_XRAY_BIN") != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("xray rejected config: %v\n%s", err, cfg)
		}
	}
}

// TestLiveFallbacks starts a real Xray: a plain HTTPS request to the VLESS
// master is not a VLESS handshake, so Xray must route it by path to the
// matching fallback target, and everything else to the default one.
func TestLiveFallbacks(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" {
		t.Skip("POLARIS_XRAY_BIN not set")
	}
	target := func(name string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, name) }))
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}
	port := freePort(t)
	master := coretest.Inbound(model.CoreXray, "vless-tls", port, model.ProtocolSpec{Type: model.ProtoVLESS,
		Fallbacks: []model.Fallback{{Path: "/ws", Dest: target("ws-child")}, {Dest: target("default-child")}}},
		model.TransportSpec{Type: model.TransTCP}, coretest.TLS(t, "a.example.com"))
	master.Listen = "127.0.0.1"
	master.Security.ALPN = []string{"http/1.1"}
	a := New(core.Options{Version: "26.3.27", BinPath: bin, WorkDir: t.TempDir(), APIListen: fmt.Sprintf("127.0.0.1:%d", freePort(t))})
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{master}})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.AtomicWrite(a.configPath(), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(ctx)
	// Xray picks the fallback per connection (path of the first request), so
	// every request needs its own connection.
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "a.example.com"}, // self-signed test cert
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		},
	}}
	get := func(path string) string {
		var last error
		for i := 0; i < 50; i++ {
			resp, err := client.Get("https://a.example.com" + path)
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return string(b)
			}
			last = err
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("GET %s: %v", path, last)
		return ""
	}
	if got := get("/ws"); got != "ws-child" {
		t.Fatalf("/ws went to %q", got)
	}
	if got := get("/anything"); got != "default-child" {
		t.Fatalf("/anything went to %q", got)
	}
}
