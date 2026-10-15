package mihomo

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestRender(t *testing.T) {
	bin := os.Getenv("POLARIS_MIHOMO_BIN")
	a := New(core.Options{Version: "1.19.15", WorkDir: t.TempDir(), BinPath: bin})
	m := model.CoreMihomo
	obs, routes := coretest.Chain()
	spec := &model.NodeSpec{Inbounds: []model.Inbound{
		coretest.Inbound(m, "vless-reality", 22443, model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"}, model.TransportSpec{}, coretest.Reality(t)),
		coretest.Inbound(m, "vmess-ws", 22444, model.ProtocolSpec{Type: model.ProtoVMess}, model.TransportSpec{Type: model.TransWS, Path: "/ws"}, model.SecuritySpec{}),
		coretest.Inbound(m, "trojan-grpc", 22445, model.ProtocolSpec{Type: model.ProtoTrojan}, model.TransportSpec{Type: model.TransGRPC, ServiceName: "g"}, coretest.TLS(t, "t.example.com")),
		coretest.Inbound(m, "hy2", 22446, model.ProtocolSpec{Type: model.ProtoHysteria2, Obfs: "salamander", ObfsPassword: "o"}, model.TransportSpec{}, coretest.TLS(t, "h.example.com")),
		coretest.Inbound(m, "tuic", 22447, model.ProtocolSpec{Type: model.ProtoTUIC}, model.TransportSpec{}, coretest.TLS(t, "u.example.com")),
		coretest.Inbound(m, "anytls", 22448, model.ProtocolSpec{Type: model.ProtoAnyTLS}, model.TransportSpec{}, coretest.TLS(t, "a.example.com")),
		coretest.Inbound(m, "ss", 22449, model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "2022-blake3-aes-128-gcm", ServerKey: "Ar9iPjxjvKbx8PmqXbX5Sg=="}, model.TransportSpec{}, model.SecuritySpec{}),
		coretest.Inbound(m, "xhttp", 22450, model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{Type: model.TransXHTTP, Path: "/x"}, coretest.TLS(t, "x.example.com")),
	}, Outbounds: obs, Routes: append(routes, model.RouteRule{Priority: 1, Match: model.RouteMatch{Inbound: []string{"hy2"}, Users: []string{"u1", "u2"}}, Outbound: "resi"})}
	for _, in := range spec.Inbounds {
		if err := a.Capabilities("1.19.15").SupportsInbound(&in); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	for _, want := range []string{"reality-config:", "IN-NAME,hy2", "AND,((IN-NAME,hy2),(OR,((IN-USER,u1),(IN-USER,u2)))),resi", "dialer-proxy: resi",
		"MATCH,DIRECT", "xhttp-config:"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
	if bin != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("mihomo rejected config: %v\n%s", err, cfg)
		}
	}
}

func TestLive(t *testing.T) {
	bin := os.Getenv("POLARIS_MIHOMO_BIN")
	if bin == "" {
		t.Skip("POLARIS_MIHOMO_BIN not set")
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	api := l.Addr().String()
	l.Close()
	a := New(core.Options{Version: "1.19.15", WorkDir: t.TempDir(), BinPath: bin, APIListen: api})
	m := model.CoreMihomo
	spec := &model.NodeSpec{Inbounds: []model.Inbound{
		coretest.Inbound(m, "vless-reality", 23443, model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"}, model.TransportSpec{}, coretest.Reality(t)),
		coretest.Inbound(m, "trojan-ws", 23444, model.ProtocolSpec{Type: model.ProtoTrojan}, model.TransportSpec{Type: model.TransWS, Path: "/ws"}, coretest.TLS(t, "t.example.com")),
		coretest.Inbound(m, "anytls", 23445, model.ProtocolSpec{Type: model.ProtoAnyTLS}, model.TransportSpec{}, coretest.TLS(t, "a.example.com")),
		coretest.Inbound(m, "hy2", 23446, model.ProtocolSpec{Type: model.ProtoHysteria2}, model.TransportSpec{}, coretest.TLS(t, "h.example.com")),
	}}
	for i := range spec.Inbounds {
		spec.Inbounds[i].Listen = "127.0.0.1"
	}
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	core.AtomicWrite(a.configPath(), cfg, 0o600)
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(ctx)
	time.Sleep(time.Second)
	for _, p := range []int{23443, 23444, 23445} {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			t.Fatalf("listener %d not up: %v\n%s", p, err, strings.Join(a.Logs(50), "\n"))
		}
		c.Close()
	}
	for _, line := range a.Logs(200) {
		if strings.Contains(line, "level=error") || strings.Contains(line, "level=fatal") {
			t.Fatalf("mihomo error: %s", line)
		}
	}
	// Reload through the controller with a changed user set.
	spec.Inbounds[0].Users = spec.Inbounds[0].Users[:1]
	cfg, _ = a.Render(spec)
	core.AtomicWrite(a.configPath(), cfg, 0o600)
	pid := a.Health(ctx).PID
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Health(ctx).PID != pid {
		t.Fatal("reload should not restart the process")
	}
	if _, err := a.CollectTraffic(ctx); err != nil {
		t.Fatalf("connections api: %v", err)
	}
}
