package sub

import (
	"encoding/json"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() { Register(singboxRenderer{}) }

// DefaultSingboxTemplate targets sing-box >= 1.11 clients (SFA / SFI / SFM).
// {{ .Outbounds }} is a JSON array fragment of proxy outbounds and {{ .Tags }}
// a JSON array of their tags; {{ .ProxyGroups }}, {{ .Rules }}, {{ .RuleSets }},
// {{ .Final }} and {{ .Main }} (the first selector) come from the rule scheme.
const DefaultSingboxTemplate = `{
  "log": {"level": "warn"},
  "dns": {
    "servers": [
      {"tag": "remote", "type": "https", "server": "1.1.1.1", "detour": {{ json .Main }}},
      {"tag": "local", "type": "udp", "server": "223.5.5.5"}
    ],
    "rules": [{"rule_set": "geosite-cn", "server": "local"}],
    "final": "remote"
  },
  "inbounds": [
    {"type": "tun", "tag": "tun-in", "address": ["172.19.0.1/30", "fdfe:dcba:9876::1/126"], "auto_route": true, "strict_route": true},
    {"type": "mixed", "tag": "mixed-in", "listen": "127.0.0.1", "listen_port": 2080}
  ],
  "outbounds": [
    {{ .ProxyGroups }},
    {{ .Outbounds }}{{ if .HasOutbounds }},{{ end }}
    {"type": "direct", "tag": "direct"}
  ],
  "route": {
    "rules": [
      {"action": "sniff"},
      {"protocol": "dns", "action": "hijack-dns"}{{ .Rules }}
    ],
    "rule_set": {{ .RuleSets }},
    "final": {{ json .Final }},
    "auto_detect_interface": true,
    "default_domain_resolver": "local"
  }
}
`

type singboxRenderer struct{}

func (singboxRenderer) Format() string      { return "singbox" }
func (singboxRenderer) ContentType() string { return "application/json; charset=utf-8" }

func (singboxRenderer) Render(nodes []Node, info Info, tpl string) ([]byte, error) {
	var parts []string
	var tags []string
	var kept []Node
	for _, n := range nodes {
		o := SingboxOutbound(n)
		if o == nil {
			continue
		}
		b, err := json.Marshal(o)
		if err != nil {
			return nil, err
		}
		parts = append(parts, string(b))
		tags = append(tags, n.Name)
		kept = append(kept, n)
	}
	if len(tags) == 0 {
		tags = []string{"direct"}
	}
	outs := ""
	for i, p := range parts {
		if i > 0 {
			outs += ",\n    "
		}
		outs += p
	}
	tagsJSON, _ := json.Marshal(tags)
	data := templateData(kept, info, "")
	data["Outbounds"] = outs
	data["HasOutbounds"] = len(parts) > 0
	data["Tags"] = string(tagsJSON)
	data["TagsInline"] = string(tagsJSON[1 : len(tagsJSON)-1])
	data["ProxyGroups"], data["Rules"], data["RuleSets"], data["Final"], data["Main"] = singboxScheme(schemeOf(info), kept)
	body, err := execTemplate(orDefault(tpl, DefaultSingboxTemplate), data)
	if err != nil {
		return nil, err
	}
	// Re-indent and validate.
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	return json.MarshalIndent(v, "", "  ")
}

// SingboxOutbound converts a node into a sing-box outbound, or nil.
func SingboxOutbound(n Node) map[string]any {
	o := map[string]any{"tag": n.Name, "server": n.Server, "server_port": n.Port}
	switch n.Protocol {
	case model.ProtoVLESS:
		o["type"] = "vless"
		o["uuid"] = n.UUID
		if n.Flow != "" {
			o["flow"] = n.Flow
		}
	case model.ProtoVMess:
		o["type"] = "vmess"
		o["uuid"] = n.UUID
		o["security"] = "auto"
	case model.ProtoTrojan:
		o["type"] = "trojan"
		o["password"] = n.Password
	case model.ProtoShadowsocks:
		o["type"] = "shadowsocks"
		o["method"] = n.Method
		o["password"] = n.Password
	case model.ProtoHysteria2:
		o["type"] = "hysteria2"
		o["password"] = n.Password
		if n.ObfsPassword != "" {
			o["obfs"] = map[string]any{"type": orDefault(n.Obfs, "salamander"), "password": n.ObfsPassword}
		}
	case model.ProtoTUIC:
		o["type"] = "tuic"
		o["uuid"] = n.UUID
		o["password"] = n.Password
		o["congestion_control"] = orDefault(n.CongestionControl, "bbr")
	case model.ProtoAnyTLS:
		o["type"] = "anytls"
		o["password"] = n.Password
	case model.ProtoSOCKS:
		o["type"] = "socks"
		o["username"], o["password"] = n.Username, n.Password
	case model.ProtoHTTP:
		o["type"] = "http"
		o["username"], o["password"] = n.Username, n.Password
	case model.ProtoNaive:
		o["type"] = "naive"
		o["username"], o["password"] = n.Username, n.Password
	default:
		return nil
	}
	switch n.Transport.Type {
	case "", model.TransTCP, model.TransQUIC:
	case model.TransWS:
		t := map[string]any{"type": "ws", "path": orDefault(n.Transport.Path, "/")}
		if n.Transport.Host != "" {
			t["headers"] = map[string]any{"Host": n.Transport.Host}
		}
		o["transport"] = t
	case model.TransHTTPUpgrade:
		t := map[string]any{"type": "httpupgrade", "path": orDefault(n.Transport.Path, "/")}
		if n.Transport.Host != "" {
			t["host"] = n.Transport.Host
		}
		o["transport"] = t
	case model.TransGRPC:
		o["transport"] = map[string]any{"type": "grpc", "service_name": n.Transport.ServiceName}
	default:
		return nil // xhttp / kcp have no sing-box client
	}
	if n.TLS.Type == model.SecTLS || n.TLS.Type == model.SecReality {
		t := map[string]any{"enabled": true}
		if n.TLS.ServerName != "" {
			t["server_name"] = n.TLS.ServerName
		}
		if n.TLS.Insecure {
			t["insecure"] = true
		}
		alpn := n.TLS.ALPN
		if n.Protocol == model.ProtoTUIC && len(alpn) == 0 {
			alpn = []string{"h3"}
		}
		if len(alpn) > 0 {
			t["alpn"] = alpn
		}
		fp := n.TLS.Fingerprint
		if n.TLS.Type == model.SecReality && fp == "" {
			fp = "chrome"
		}
		if fp != "" {
			t["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
		}
		if n.TLS.Type == model.SecReality {
			t["reality"] = map[string]any{"enabled": true, "public_key": n.TLS.PublicKey, "short_id": n.TLS.ShortID}
		}
		o["tls"] = t
	}
	return o
}
