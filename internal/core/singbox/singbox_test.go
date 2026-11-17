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
	// WS is TCP-based but not a raw TCP listener: no proxy_protocol.
	ws := coretest.Inbound(s, "vmess-ws-proxy", 21463, model.ProtocolSpec{Type: model.ProtoVMess},
		model.TransportSpec{Type: model.TransWS, Path: "/ws", AcceptProxyProtocol: true}, model.SecuritySpec{})
	if _, ok := proxy(ws)["proxy_protocol"]; ok {
		t.Fatal("VMess WS emitted proxy_protocol")
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
