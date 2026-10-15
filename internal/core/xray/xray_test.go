package xray

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func spec(t *testing.T) *model.NodeSpec {
	x := model.CoreXray
	obs, routes := coretest.Chain()
	return &model.NodeSpec{NodeID: "n1", Rev: 1, Inbounds: []model.Inbound{
		coretest.Inbound(x, "vless-reality", 20443, model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"},
			model.TransportSpec{Type: model.TransTCP}, coretest.Reality(t)),
		coretest.Inbound(x, "vless-xhttp", 20444, model.ProtocolSpec{Type: model.ProtoVLESS},
			model.TransportSpec{Type: model.TransXHTTP, Path: "/x"}, coretest.TLS(t, "a.example.com")),
		coretest.Inbound(x, "vmess-ws", 20445, model.ProtocolSpec{Type: model.ProtoVMess},
			model.TransportSpec{Type: model.TransWS, Path: "/ws"}, model.SecuritySpec{}),
		coretest.Inbound(x, "trojan-grpc", 20446, model.ProtocolSpec{Type: model.ProtoTrojan},
			model.TransportSpec{Type: model.TransGRPC, ServiceName: "g"}, coretest.TLS(t, "b.example.com")),
		coretest.Inbound(x, "ss", 20447, model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "2022-blake3-aes-128-gcm",
			ServerKey: "Ar9iPjxjvKbx8PmqXbX5Sg=="}, model.TransportSpec{}, model.SecuritySpec{}),
		coretest.Inbound(x, "kcp", 20448, model.ProtocolSpec{Type: model.ProtoVMess},
			model.TransportSpec{Type: model.TransKCP, HeaderType: "wechat-video"}, model.SecuritySpec{}),
		coretest.Inbound(model.CoreSingbox, "not-mine", 20449, model.ProtocolSpec{Type: model.ProtoTUIC},
			model.TransportSpec{}, model.SecuritySpec{}),
	}, Outbounds: obs, Routes: routes}
}

func TestRender(t *testing.T) {
	a := New(core.Options{Version: "26.3.27", WorkDir: t.TempDir(), BinPath: os.Getenv("POLARIS_XRAY_BIN")})
	s := spec(t)
	for _, in := range s.InboundsFor(model.CoreXray) {
		if err := a.Capabilities("26.3.27").SupportsInbound(&in); err != nil {
			t.Fatalf("capability: %v", err)
		}
	}
	cfg, err := a.Render(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(cfg, &m); err != nil {
		t.Fatal(err)
	}
	if n := len(m["inbounds"].([]any)); n != 6 {
		t.Fatalf("want 6 xray inbounds, got %d", n)
	}
	if !strings.Contains(string(cfg), `"balancerTag": "auto"`) || !strings.Contains(string(cfg), `"dialerProxy": "resi"`) {
		t.Fatalf("missing balancer/detour:\n%s", cfg)
	}
	if os.Getenv("POLARIS_XRAY_BIN") != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("xray rejected config: %v\n%s", err, cfg)
		}
	}
}

func TestCapabilitiesVersionGate(t *testing.T) {
	a := New(core.Options{})
	if a.Capabilities("1.8.4").Supports(model.ProtoVLESS, model.TransXHTTP, model.SecTLS) {
		t.Fatal("xhttp must need >= 24.9.30")
	}
	if !a.Capabilities("25.1.1").Supports(model.ProtoVLESS, model.TransXHTTP, model.SecReality) {
		t.Fatal("xhttp+reality expected")
	}
	if a.Capabilities("25.1.1").SupportsProtocol(model.ProtoTUIC) {
		t.Fatal("xray has no tuic")
	}
}

func TestAccountEncoding(t *testing.T) {
	in := &model.Inbound{Protocol: model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "aes-256-gcm"}}
	if _, err := account(in, coretest.Users()[0]); err != core.ErrNotSupported {
		t.Fatalf("legacy ss should fall back to reload, got %v", err)
	}
	in.Protocol.Type = model.ProtoVLESS
	b, err := account(in, coretest.Users()[0])
	if err != nil || !strings.Contains(string(b), "xray.proxy.vless.Account") {
		t.Fatalf("bad vless account: %v %q", err, b)
	}
}
