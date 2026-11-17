package singbox

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
	s := model.CoreSingbox
	obs, routes := coretest.Chain()
	for i := range obs {
		set, _ := obs[i].ParseSettings()
		set.Fallback = ""
		obs[i].Settings = coretest.Settings(set)
	}
	obs = append(obs, model.Outbound{Tag: "warp", Type: model.OutWireGuard, Settings: coretest.Settings(model.OutboundSettings{
		Server: "162.159.192.1", Port: 2408, PrivateKey: "YNXtAzepDqRv9H52osJVDQnznT5AM11eCK3ESpwSt04=",
		PeerPublicKey: "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=", LocalAddress: []string{"172.16.0.2/32"}})})
	routes = append(routes, model.RouteRule{Priority: 5, Match: model.RouteMatch{Geosite: []string{"netflix"}, GeoIP: []string{"private"}}, Outbound: "warp"})
	return &model.NodeSpec{NodeID: "n1", Rev: 1, Inbounds: []model.Inbound{
		coretest.Inbound(s, "vless-reality", 21443, model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"},
			model.TransportSpec{}, coretest.Reality(t)),
		coretest.Inbound(s, "hy2", 21444, model.ProtocolSpec{Type: model.ProtoHysteria2, Obfs: "salamander", ObfsPassword: "o"},
			model.TransportSpec{}, coretest.TLS(t, "h.example.com")),
		coretest.Inbound(s, "tuic", 21445, model.ProtocolSpec{Type: model.ProtoTUIC}, model.TransportSpec{}, coretest.TLS(t, "t.example.com")),
		coretest.Inbound(s, "anytls", 21446, model.ProtocolSpec{Type: model.ProtoAnyTLS}, model.TransportSpec{}, coretest.TLS(t, "a.example.com")),
		coretest.Inbound(s, "ss", 21447, model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "2022-blake3-aes-128-gcm",
			ServerKey: "Ar9iPjxjvKbx8PmqXbX5Sg=="}, model.TransportSpec{}, model.SecuritySpec{}),
		coretest.Inbound(s, "stls", 21448, model.ProtocolSpec{Type: model.ProtoShadowTLS, HandshakeServer: "www.example.com:443", Detour: "ss"},
			model.TransportSpec{}, model.SecuritySpec{}),
		coretest.Inbound(s, "vmess-ws", 21449, model.ProtocolSpec{Type: model.ProtoVMess}, model.TransportSpec{Type: model.TransWS, Path: "/ws"}, model.SecuritySpec{}),
		coretest.Inbound(s, "trojan-hu", 21450, model.ProtocolSpec{Type: model.ProtoTrojan}, model.TransportSpec{Type: model.TransHTTPUpgrade, Path: "/u"}, coretest.TLS(t, "u.example.com")),
		coretest.Inbound(s, "naive", 21451, model.ProtocolSpec{Type: model.ProtoNaive}, model.TransportSpec{}, coretest.TLS(t, "n.example.com")),
	}, Outbounds: obs, Routes: routes}
}

func TestRenderInboundProxyProtocol(t *testing.T) {
	s := model.CoreSingbox
	proxy := func(in model.Inbound) map[string]any {
		t.Helper()
		ib, err := renderInbound(in)
		if err != nil {
			t.Fatal(err)
		}
		return ib
	}
	// AnyTLS over TCP with PROXY protocol: the listener must parse the
	// PROXY header the relay forwarder prepends.
	anytls := coretest.Inbound(s, "anytls-proxy", 21460, model.ProtocolSpec{Type: model.ProtoAnyTLS},
		model.TransportSpec{Type: model.TransTCP, AcceptProxyProtocol: true}, coretest.TLS(t, "a.example.com"))
	if got := proxy(anytls)["proxy_protocol"]; got != true {
		t.Fatalf("AnyTLS TCP proxy_protocol=%v, want true", got)
	}
	// Same inbound without the flag: no proxy_protocol key.
	plain := coretest.Inbound(s, "anytls-plain", 21461, model.ProtocolSpec{Type: model.ProtoAnyTLS},
		model.TransportSpec{Type: model.TransTCP}, coretest.TLS(t, "a.example.com"))
	if _, ok := proxy(plain)["proxy_protocol"]; ok {
		t.Fatal("AnyTLS TCP without AcceptProxyProtocol emitted proxy_protocol")
	}
	// UDP/QUIC protocols must never get proxy_protocol, even if the flag
	// is set: a PROXY header is a TCP-stream concept.
	for _, p := range []model.Protocol{model.ProtoHysteria2, model.ProtoTUIC} {
		in := coretest.Inbound(s, "udp-"+string(p), 21462, model.ProtocolSpec{Type: p},
			model.TransportSpec{AcceptProxyProtocol: true}, coretest.TLS(t, "u.example.com"))
		if _, ok := proxy(in)["proxy_protocol"]; ok {
			t.Fatalf("%s emitted proxy_protocol", p)
		}
	}
	// Stream transports run over TCP, so the PROXY header arrives before
	// the application handshake: WS and gRPC listeners must parse it too.
	for _, tc := range []struct {
		name string
		typ  model.Transport
	}{
		{"ws", model.TransWS},
		{"grpc", model.TransGRPC},
	} {
		in := coretest.Inbound(s, "vmess-"+string(tc.typ)+"-proxy", 21463, model.ProtocolSpec{Type: model.ProtoVMess},
			model.TransportSpec{Type: tc.typ, Path: "/ws", AcceptProxyProtocol: true}, model.SecuritySpec{})
		if got := proxy(in)["proxy_protocol"]; got != true {
			t.Fatalf("VMess %s proxy_protocol=%v, want true", tc.name, got)
		}
	}
	// Shadowsocks listens on TCP and UDP; its TCP listener qualifies.
	ss := coretest.Inbound(s, "ss-proxy", 21464, model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "2022-blake3-aes-128-gcm", ServerKey: "MTIzNDU2Nzg5MDEyMzQ1Ng=="},
		model.TransportSpec{Type: model.TransTCP, AcceptProxyProtocol: true}, model.SecuritySpec{})
	if got := proxy(ss)["proxy_protocol"]; got != true {
		t.Fatalf("Shadowsocks proxy_protocol=%v, want true", got)
	}
}

func TestCapabilitiesProxyProtocol(t *testing.T) {
	caps, ok := core.Capabilities(model.CoreSingbox, "1.13.5")
	if !ok {
		t.Fatal("no sing-box capabilities")
	}
	found := false
	for _, f := range caps.Features {
		if f == model.FeatureProxyProtocol {
			found = true
		}
	}
	if !found {
		t.Fatalf("sing-box features %v lack %s", caps.Features, model.FeatureProxyProtocol)
	}
	// An AnyTLS TCP inbound asking for PROXY protocol must be supported.
	in := coretest.Inbound(model.CoreSingbox, "anytls-proxy", 21460, model.ProtocolSpec{Type: model.ProtoAnyTLS},
		model.TransportSpec{Type: model.TransTCP, AcceptProxyProtocol: true}, coretest.TLS(t, "a.example.com"))
	if err := caps.SupportsInbound(&in); err != nil {
		t.Fatalf("sing-box rejects PROXY inbound: %v", err)
	}
}

func TestRenderOutboundTUIC(t *testing.T) {
	// The TUIC outbound must be listed as a capability: a chain hop to
	// a TUIC landing resolves to it, and chain creation validates the
	// entry core against this list.
	caps, ok := core.Capabilities(model.CoreSingbox, "1.13.5")
	if !ok {
		t.Fatal("no sing-box capabilities")
	}
	if !caps.SupportsOutbound(model.OutTUIC) {
		t.Fatalf("sing-box capabilities %v lack %s", caps.Outbounds, model.OutTUIC)
	}
	// The rendered outbound mirrors the sing-box client config
	// (internal/sub/singbox.go): type/uuid/password/congestion_control
	// plus server/port, with TLS supplied by the shared block.
	render := func(set model.OutboundSettings) map[string]any {
		t.Helper()
		outs, _, err := renderOutbounds([]model.Outbound{{Tag: "tuic-hop", Type: model.OutTUIC, Settings: coretest.Settings(set)}})
		if err != nil {
			t.Fatal(err)
		}
		if len(outs) != 2 { // direct + the TUIC hop
			t.Fatalf("outs = %d, want 2", len(outs))
		}
		ob, ok := outs[1].(map[string]any)
		if !ok {
			t.Fatalf("outbound is %T", outs[1])
		}
		return ob
	}
	base := model.OutboundSettings{Server: "land.example.com", Port: 443,
		UUID: "11111111-1111-4111-8111-111111111111", Password: "tuic-password", CongestionControl: "bbr",
		Security: &model.ClientTLS{Type: model.SecTLS, ServerName: "land.example.com", ALPN: []string{"h3"}}}
	ob := render(base)
	for k, want := range map[string]any{"type": "tuic", "server": "land.example.com", "server_port": 443,
		"uuid": "11111111-1111-4111-8111-111111111111", "password": "tuic-password", "congestion_control": "bbr"} {
		if ob[k] != want {
			t.Fatalf("tuic outbound %s = %v, want %v (full: %v)", k, ob[k], want, ob)
		}
	}
	tls, ok := ob["tls"].(map[string]any)
	if !ok || tls["server_name"] != "land.example.com" {
		t.Fatalf("tuic outbound tls = %v", ob["tls"])
	}
	// Empty congestion control defaults to bbr, like the TUIC inbound
	// and the sing-box client config.
	base.CongestionControl = ""
	if got := render(base)["congestion_control"]; got != "bbr" {
		t.Fatalf("empty congestion_control rendered %v, want bbr", got)
	}
}

func TestRender(t *testing.T) {
	bin := os.Getenv("POLARIS_SINGBOX_BIN")
	a := New(core.Options{Version: "1.13.5", WorkDir: t.TempDir(), BinPath: bin})
	s := spec(t)
	for _, in := range s.InboundsFor(model.CoreSingbox) {
		if err := a.Capabilities("1.13.5").SupportsInbound(&in); err != nil {
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
	for _, want := range []string{`"action": "reject"`, `"detour": "resi"`, `"type": "urltest"`, `geosite-netflix`, `"ip_is_private": true`, `"endpoints"`} {
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("missing %s in:\n%s", want, cfg)
		}
	}
	if bin != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("sing-box rejected config: %v\n%s", err, cfg)
		}
	}
}

func TestHotUsersUnsupported(t *testing.T) {
	a := New(core.Options{})
	if err := a.AddUsers(t.Context(), "x", nil); err != core.ErrNotSupported {
		t.Fatal("sing-box must fall back to reload")
	}
}

func TestBalancerFallbackRejected(t *testing.T) {
	a := New(core.Options{})
	_, err := a.Render(&model.NodeSpec{Outbounds: []model.Outbound{{Tag: "primary", Type: model.OutBalancer, Settings: coretest.Settings(model.OutboundSettings{Members: []string{"direct"}, Fallback: "backup"})}}})
	if err == nil || !strings.Contains(err.Error(), "sing-box does not support group fallback") {
		t.Fatalf("fallback must not be silently ignored: %v", err)
	}
}
