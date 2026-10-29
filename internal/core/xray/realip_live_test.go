package xray

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestRenderTrustedForwardedFor(t *testing.T) {
	st, err := renderStream(model.TransportSpec{Type: model.TransWS, Path: "/ws", TrustedForwardedFor: []string{"CF-Connecting-IP"}}, model.SecuritySpec{}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	so, _ := st["sockopt"].(map[string]any)
	if fmt.Sprint(so["trustedXForwardedFor"]) != "[CF-Connecting-IP]" {
		t.Fatalf("sockopt = %v", st["sockopt"])
	}
	// Raw TCP has no headers to read; outbounds never get it.
	for _, c := range []struct {
		tr     model.TransportSpec
		server bool
	}{{model.TransportSpec{Type: model.TransTCP, TrustedForwardedFor: []string{"CF-Connecting-IP"}}, true},
		{model.TransportSpec{Type: model.TransWS, TrustedForwardedFor: []string{"CF-Connecting-IP"}}, false}} {
		st, _ := renderStream(c.tr, model.SecuritySpec{}, c.server, false)
		if _, ok := st["sockopt"]; ok {
			t.Fatalf("%+v server=%v: sockopt %v", c.tr, c.server, st["sockopt"])
		}
	}
}

// TestLiveTrustedForwardedFor runs a real Xray behind a pretend Cloudflare:
// with the trusted header the user shows up with the visitor's address, and
// a request without it keeps the connection's.
func TestLiveTrustedForwardedFor(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" {
		t.Skip("POLARIS_XRAY_BIN not set")
	}
	api := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	dir := t.TempDir()
	a := New(core.Options{Version: "26.3.27", BinPath: bin, WorkDir: dir, APIListen: api})
	port := freePort(t)
	in := coretest.Inbound(model.CoreXray, "ws", port, model.ProtocolSpec{Type: model.ProtoVLESS},
		model.TransportSpec{Type: model.TransWS, Path: "/ws", TrustedForwardedFor: []string{"CF-Connecting-IP"}}, model.SecuritySpec{})
	in.Listen = "127.0.0.1"
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}})
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

	web, _ := net.Listen("tcp", "127.0.0.1:0")
	defer web.Close()
	go func() { // holds every connection open, so Xray keeps the user online
		for {
			c, err := web.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	webPort := web.Addr().(*net.TCPAddr).Port

	dial := func(h http.Header) {
		t.Helper()
		var c *websocket.Conn
		var err error
		for i := 0; i < 50; i++ {
			dctx, cancel := context.WithTimeout(ctx, time.Second)
			c, _, err = websocket.Dial(dctx, fmt.Sprintf("ws://127.0.0.1:%d/ws", port), &websocket.DialOptions{HTTPHeader: h})
			cancel()
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		uuid, _ := hex.DecodeString(strings.ReplaceAll(coretest.Users()[0].UUID, "-", ""))
		req := append([]byte{0}, uuid...)
		req = append(req, 0, 1, byte(webPort>>8), byte(webPort), 1, 127, 0, 0, 1)
		if err := c.Write(ctx, websocket.MessageBinary, req); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.CloseNow() })
	}
	online := func() []string {
		got, err := a.OnlineIPs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got[coretest.Users()[0].ID]
	}
	dial(http.Header{"Cf-Connecting-Ip": {"198.51.100.7"}, "X-Forwarded-For": {"198.51.100.7, 172.69.33.48"}})
	var ips []string
	for i := 0; i < 30; i++ {
		if ips = online(); len(ips) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if fmt.Sprint(ips) != "[198.51.100.7]" {
		t.Fatalf("via CDN: online %v; logs:\n%s", ips, strings.Join(a.Logs(30), "\n"))
	}
	// A forged X-Forwarded-For without the CDN's header is ignored: the
	// connection's own address (loopback, which Xray never lists) counts.
	dial(http.Header{"X-Forwarded-For": {"203.0.113.9"}})
	time.Sleep(500 * time.Millisecond)
	if ips = online(); fmt.Sprint(ips) != "[198.51.100.7]" {
		t.Fatalf("forged header: online %v", ips)
	}
}
