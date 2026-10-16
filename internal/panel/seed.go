package panel

import (
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Default system settings.
var defaultSettings = map[string]string{
	"site_title":            "Polaris",
	"sub_name_template":     "{flag} {node} {protocol}",
	"sub_update_interval":   "1",
	"ip_ban_minutes":        "0",
	"acme_email":            "",
	"default_xray":          "latest",
	"default_singbox":       "latest",
	"converter_mode":        "builtin",
	"converter_backend":     "",
	"converter_config":      "",
	"converter_params":      "emoji=true&udp=true&scv=true",
	"converter_for_users":   "false",
	"sub_operators":         "[]",
	"sub_apply_ops_users":   "false",
	"allow_private_sources": "false",
	"sub_enabled":           "true",
	"sub_show_info":         "auto",
	"sub_routing_enabled":   "false",
	"sub_show_bot":          "true",
	"portal_enabled":        "false",
	"sub_show_rate":         "true",
	"traffic_spike_factor":  "5",
	"traffic_spike_min_gb":  "5",
	"sub_scheme":            "",
	"sub_skip_cert":         "false",
	"sub_force_udp":         "false",
}

func (s *Server) seed() error {
	for k, v := range defaultSettings {
		var n int64
		s.st.DB.Model(&store.Setting{}).Where("key = ?", k).Count(&n)
		if n == 0 {
			if err := s.st.SetSetting(k, v); err != nil {
				return err
			}
		}
	}
	var admins int64
	s.st.DB.Model(&store.Admin{}).Count(&admins)
	if admins == 0 {
		user, pass := s.cfg.InitialAdmin.Username, s.cfg.InitialAdmin.Password
		if user == "" {
			user = "admin"
		}
		generated := false
		if pass == "" {
			pass = pki.Password(16)
			generated = true
		}
		h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if err := s.st.DB.Create(&store.Admin{ID: store.NewID(), Username: user, PasswordHash: string(h)}).Error; err != nil {
			return err
		}
		if generated {
			s.log.Warn("created initial admin; change the password after first login", "username", user, "password", pass)
		} else {
			s.log.Info("created initial admin", "username", user)
		}
	}
	for _, f := range []struct{ format, content string }{
		{"clash", sub.DefaultClashTemplate}, {"singbox", sub.DefaultSingboxTemplate}, {"surge", sub.DefaultSurgeTemplate},
		{"loon", sub.DefaultLoonTemplate}, {"quanx", sub.DefaultQuanxTemplate},
	} {
		var n int64
		s.st.DB.Model(&store.SubTemplate{}).Where("format = ?", f.format).Count(&n)
		if n == 0 {
			s.st.DB.Create(&store.SubTemplate{ID: store.NewID(), Format: f.format, Name: "默认", Content: f.content, IsDefault: true, UpdatedAt: time.Now()})
		}
	}
	for _, t := range builtinTemplates() {
		var n int64
		s.st.DB.Model(&store.InboundTemplate{}).Where("name = ?", t.Name).Count(&n)
		if n == 0 {
			t.ID = store.NewID()
			if err := s.st.DB.Create(&t).Error; err != nil {
				return err
			}
		}
	}
	return s.ensureDefaultGroup()
}

func builtinTemplates() []store.InboundTemplate {
	reality := store.TemplateItem{
		Key: "reality", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"},
		Transport:  model.TransportSpec{Type: model.TransTCP},
		Security:   model.SecuritySpec{Type: model.SecReality, RealityDest: "www.microsoft.com:443", RealityServerNames: []string{"www.microsoft.com"}},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       store.PortPolicy{Mode: "fixed", Fixed: 443},
		TagPattern: "{node.name}-vless-reality", Cert: "reality",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	rnd := store.PortPolicy{Mode: "range", Min: 20000, Max: 30000}
	hy2 := store.TemplateItem{
		Key: "hy2", Core: model.CoreAuto,
		Protocol:  model.ProtocolSpec{Type: model.ProtoHysteria2},
		Transport: model.TransportSpec{Type: model.TransQUIC},
		Security:  model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}", ALPN: []string{"h3"}},
		Port:      rnd, TagPattern: "{node.name}-hy2", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	tuic := hy2
	tuic.Key, tuic.Protocol, tuic.TagPattern = "tuic", model.ProtocolSpec{Type: model.ProtoTUIC, CongestionControl: "bbr"}, "{node.name}-tuic"
	anytls := store.TemplateItem{
		Key: "anytls", Core: model.CoreAuto,
		Protocol:  model.ProtocolSpec{Type: model.ProtoAnyTLS},
		Transport: model.TransportSpec{Type: model.TransTCP},
		Security:  model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}"},
		Port:      rnd, TagPattern: "{node.name}-anytls", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	xhttp := store.TemplateItem{
		Key: "xhttp", Core: model.CoreAuto,
		Protocol:  model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport: model.TransportSpec{Type: model.TransXHTTP, Path: "/{rand.path}", Mode: "auto"},
		Security:  model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}", ALPN: []string{"h2", "http/1.1"}},
		Sniffing:  model.SniffSpec{Enabled: true},
		Port:      rnd, TagPattern: "{node.name}-vless-xhttp", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}, {Kind: "cdn"}},
	}
	cdnWS := store.TemplateItem{
		Key: "cdn-ws", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport:  model.TransportSpec{Type: model.TransWS, Path: "/{rand.path}"},
		Security:   model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}"},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       store.PortPolicy{Mode: "fixed", Fixed: 2053},
		TagPattern: "{node.name}-vless-ws", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "cdn"}},
	}
	mk := func(name, desc string, items ...store.TemplateItem) store.InboundTemplate {
		return store.InboundTemplate{Name: name, Builtin: true, Description: desc, Items: store.NewJSON(items),
			Groups: store.NewJSON([]string{}), BoundLabels: store.NewJSON([]string{})}
	}

	// From the official XTLS/Xray-examples.
	// ---------------------------------------------------------------- PART 1 ----

	// Xray-examples: VLESS-TCP-XTLS-Vision (fallbacks to a decoy site dropped).
	visionTLS := store.TemplateItem{
		Key: "vision-tls", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"},
		Transport:  model.TransportSpec{Type: model.TransTCP},
		Security:   model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}", ALPN: []string{"http/1.1"}},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-vless-vision", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	// Xray-examples: VLESS-TCP-TLS, VLESS-TCP-TLS (minimal/maximal by rprx),
	// VLESS-TCP-TLS-proxy protocol (all without their Nginx fallbacks).
	vlessTLS := store.TemplateItem{
		Key: "vless-tls", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport:  model.TransportSpec{Type: model.TransTCP},
		Security:   model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}", ALPN: []string{"h2", "http/1.1"}},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-vless-tls", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	// Xray-examples: Trojan-TCP-TLS (minimal).
	trojanTLS := store.TemplateItem{
		Key: "trojan-tls", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoTrojan},
		Transport:  model.TransportSpec{Type: model.TransTCP},
		Security:   model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}", ALPN: []string{"http/1.1"}},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-trojan-tls", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	// Xray-examples: Trojan-gRPC-Caddy2/Nginx. Polaris terminates TLS itself
	// (or via its own Caddy front for CDN entries); serviceName is left empty
	// so prepareInbound picks a random one per node.
	trojanGRPC := store.TemplateItem{
		Key: "trojan-grpc", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoTrojan},
		Transport:  model.TransportSpec{Type: model.TransGRPC},
		Security:   model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}", ALPN: []string{"h2"}},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       store.PortPolicy{Mode: "fixed", Fixed: 2083},
		TagPattern: "{node.name}-trojan-grpc", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}, {Kind: "cdn"}},
	}
	// Xray-examples: VLESS-GRPC (Caddy front replaced by Polaris TLS).
	vlessGRPC := store.TemplateItem{
		Key: "vless-grpc", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport:  model.TransportSpec{Type: model.TransGRPC},
		Security:   model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}", ALPN: []string{"h2"}},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       store.PortPolicy{Mode: "fixed", Fixed: 2087},
		TagPattern: "{node.name}-vless-grpc", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}, {Kind: "cdn"}},
	}
	// Xray-examples: VLESS-gRPC-REALITY.
	grpcReality := store.TemplateItem{
		Key: "grpc-reality", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport:  model.TransportSpec{Type: model.TransGRPC},
		Security:   model.SecuritySpec{Type: model.SecReality, RealityDest: "www.microsoft.com:443", RealityServerNames: []string{"www.microsoft.com"}},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-vless-grpc-reality", Cert: "reality",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	// Xray-examples: VLESS-XHTTP-Reality (minimal-steal_others). Xray / mihomo only.
	xhttpReality := store.TemplateItem{
		Key: "xhttp-reality", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport:  model.TransportSpec{Type: model.TransXHTTP, Path: "/{rand.path}", Mode: "auto"},
		Security:   model.SecuritySpec{Type: model.SecReality, RealityDest: "www.microsoft.com:443", RealityServerNames: []string{"www.microsoft.com"}},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-vless-xhttp-reality", Cert: "reality",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	// Xray-examples: VLESS-TCP-REALITY (without being stolen) — approximated.
	// The example's dokodemo-door + routing trick cannot be expressed; REALITY's
	// native limitFallbackUpload/Download throttles anyone "borrowing" the
	// camouflage target instead. Xray only (other cores ignore the limits).
	realityLimit := reality
	realityLimit.Key, realityLimit.Core, realityLimit.TagPattern = "reality-limit", model.CoreXray, "{node.name}-vless-reality-limit"
	realityLimit.Security = model.SecuritySpec{Type: model.SecReality,
		RealityDest: "speed.cloudflare.com:443", RealityServerNames: []string{"speed.cloudflare.com"},
		RealityLimitUpload:   &model.LimitFallback{AfterBytes: 4 << 20, BytesPerSec: 256 << 10, BurstBytesPerSec: 1 << 20},
		RealityLimitDownload: &model.LimitFallback{AfterBytes: 4 << 20, BytesPerSec: 256 << 10, BurstBytesPerSec: 1 << 20}}
	// Xray-examples: VLESS-TCP (no TLS) and the "decryption" used in
	// VLESS-H2C-Caddy: VLESS Encryption over raw TCP. The decryption value is a
	// placeholder (all-zero X25519 key); planNode sees the auth type via
	// pki.VLESSAuthOf and generates a fresh key pair per node. Xray >= 25.9.11.
	vlessEnc := store.TemplateItem{
		Key: "vless-enc", Core: model.CoreXray,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision", Decryption: "mlkem768x25519plus.native.600s.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		Transport:  model.TransportSpec{Type: model.TransTCP},
		Security:   model.SecuritySpec{Type: model.SecNone},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-vless-enc", Cert: "none",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	// Xray-examples: VMess-TCP.
	vmessTCP := store.TemplateItem{
		Key: "vmess-tcp", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVMess},
		Transport:  model.TransportSpec{Type: model.TransTCP},
		Security:   model.SecuritySpec{Type: model.SecNone},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-vmess-tcp", Cert: "none",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	// Xray-examples: VMess-HTTP (TCP + HTTP header camouflage). Only the
	// request header is rendered; the example's custom response headers are lost.
	vmessHTTP := vmessTCP
	vmessHTTP.Key, vmessHTTP.TagPattern = "vmess-http", "{node.name}-vmess-http"
	vmessHTTP.Transport = model.TransportSpec{Type: model.TransTCP, HeaderType: "http", Path: "/"}
	vmessHTTP.Core = model.CoreXray
	// Xray-examples: VMess-TCP-TLS.
	vmessTLS := store.TemplateItem{
		Key: "vmess-tls", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVMess},
		Transport:  model.TransportSpec{Type: model.TransTCP},
		Security:   model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}"},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-vmess-tls", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	// Xray-examples: VMess-Websocket.
	vmessWS := store.TemplateItem{
		Key: "vmess-ws", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVMess},
		Transport:  model.TransportSpec{Type: model.TransWS, Path: "/{rand.path}"},
		Security:   model.SecuritySpec{Type: model.SecNone},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-vmess-ws", Cert: "none",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	// Xray-examples: VMess-Websocket-TLS.
	vmessWSTLS := store.TemplateItem{
		Key: "vmess-ws-tls", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVMess},
		Transport:  model.TransportSpec{Type: model.TransWS, Path: "/{rand.path}"},
		Security:   model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}"},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       store.PortPolicy{Mode: "fixed", Fixed: 2096},
		TagPattern: "{node.name}-vmess-ws-tls", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}, {Kind: "cdn"}},
	}
	// Xray-examples: VLESS-mKCPSeed / VMess-mKCPSeed. Xray only. NOTE: Polaris
	// renders kcp seed/header only for Xray < 26 (legacyKCP); on 26+ (where the
	// example uses finalmask "mkcp-legacy") the seed is dropped, so no seed is
	// set here — it would also be shared by every node (Seed is not expanded).
	vlessKCP := store.TemplateItem{
		Key: "vless-kcp", Core: model.CoreXray,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport:  model.TransportSpec{Type: model.TransKCP},
		Security:   model.SecuritySpec{Type: model.SecNone},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-vless-kcp", Cert: "none",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	vmessKCP := vlessKCP
	vmessKCP.Key, vmessKCP.Protocol, vmessKCP.TagPattern = "vmess-kcp", model.ProtocolSpec{Type: model.ProtoVMess}, "{node.name}-vmess-kcp"
	// Xray-examples: Shadowsocks-2022.
	ss2022 := store.TemplateItem{
		Key: "ss2022", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "2022-blake3-aes-128-gcm"},
		Transport:  model.TransportSpec{Type: model.TransTCP},
		Security:   model.SecuritySpec{Type: model.SecNone},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       rnd,
		TagPattern: "{node.name}-ss2022", Cert: "none",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	// Xray-examples: Shadowsocks-AEAD / Shadowsocks-TCP.
	ssAEAD := ss2022
	ssAEAD.Key, ssAEAD.Protocol, ssAEAD.TagPattern = "ss-aead", model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "aes-128-gcm"}, "{node.name}-ss-aead"

	// OPTIONAL — Xray-examples: VLESS-TLS-SplitHTTP-H3 (XHTTP over QUIC/H3).
	// Do NOT seed until model.Inbound.Network() treats XHTTP whose TLS ALPN is
	// only ["h3"] as "udp"; today it reports "tcp", so port-conflict checks and
	// firewall/forwarding treat a UDP listener as TCP.
	// xhttpH3 := store.TemplateItem{
	// 	Key: "xhttp-h3", Core: model.CoreXray,
	// 	Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS},
	// 	Transport:  model.TransportSpec{Type: model.TransXHTTP, Path: "/{rand.path}", Mode: "auto"},
	// 	Security:   model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}", ALPN: []string{"h3"}},
	// 	Sniffing:   model.SniffSpec{Enabled: true},
	// 	Port:       rnd,
	// 	TagPattern: "{node.name}-vless-xhttp-h3", Cert: "acme",
	// 	Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	// }

	return []store.InboundTemplate{
		mk("REALITY 直连", "VLESS + Vision + REALITY，固定 443", reality),
		mk("Hy2 + TUIC", "Hysteria2 与 TUIC v5，随机高位端口", hy2, tuic),
		mk("CDN WS 备用", "VLESS + WebSocket + TLS，走 CDN 入口", cdnWS),
		mk("全协议", "REALITY、XHTTP、Hysteria2、TUIC、AnyTLS", reality, xhttp, hy2, tuic, anytls),
		mk("Vision TLS 直连", "VLESS + TCP + XTLS Vision + TLS，需要域名与证书（ACME），随机未占用端口", visionTLS),
		mk("VLESS TLS 直连", "VLESS + TCP + TLS（无 Vision），需要域名与证书，随机未占用端口", vlessTLS),
		mk("Trojan TLS 直连", "Trojan + TCP + TLS，需要域名与证书，随机未占用端口", trojanTLS),
		mk("gRPC TLS（可 CDN）", "VLESS / Trojan + gRPC + TLS，需要域名与证书，可走 CDN，端口 2087 / 2083", vlessGRPC, trojanGRPC),
		mk("REALITY gRPC / XHTTP", "VLESS + gRPC / XHTTP + REALITY，无需域名，随机高位端口；XHTTP 仅 Xray / mihomo", grpcReality, xhttpReality),
		mk("REALITY 防偷跑", "VLESS + Vision + REALITY，限速回落防止目标站被盗用，无需域名，固定 443，仅 Xray 生效", realityLimit),
		mk("VLESS Encryption 直连", "VLESS + Vision + VLESS Encryption（ML-KEM-768），裸 TCP 无需域名与证书，需 Xray 25.9.11+", vlessEnc),
		mk("VMess 全家桶", "VMess TCP、TCP+HTTP 伪装、TCP+TLS、WS、WS+TLS（可 CDN）；TLS 项需要域名与证书", vmessTCP, vmessHTTP, vmessTLS, vmessWS, vmessWSTLS),
		mk("mKCP（UDP）", "VLESS / VMess + mKCP，UDP 随机高位端口，无需域名，仅 Xray", vlessKCP, vmessKCP),
		mk("Shadowsocks", "Shadowsocks 2022（2022-blake3-aes-128-gcm）与 AEAD（aes-128-gcm），无需域名，随机高位端口", ss2022, ssAEAD),
	}
}
