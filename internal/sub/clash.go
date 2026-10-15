package sub

import (
	"bytes"
	"encoding/json"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() { Register(clashRenderer{}) }

// DefaultClashTemplate is the default mihomo rule template. {{ .Proxies }}
// expands to the proxies list and {{ .Names }} to a flow list of proxy names.
const DefaultClashTemplate = `mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
unified-delay: true
proxies:
{{ .Proxies }}
proxy-groups:
  - name: 节点选择
    type: select
    proxies: [自动选择, DIRECT, {{ .NamesInline }}]
  - name: 自动选择
    type: url-test
    url: https://www.gstatic.com/generate_204
    interval: 300
    tolerance: 50
    proxies: {{ .Names }}
rules:
  - GEOIP,LAN,DIRECT,no-resolve
  - GEOSITE,CN,DIRECT
  - GEOIP,CN,DIRECT,no-resolve
  - MATCH,节点选择
`

// clashRenderer outputs Clash / mihomo / Stash YAML.
type clashRenderer struct{}

func (clashRenderer) Format() string      { return "clash" }
func (clashRenderer) ContentType() string { return "text/yaml; charset=utf-8" }

func (clashRenderer) Render(nodes []Node, _ Info, tpl string) ([]byte, error) {
	var proxies []map[string]any
	var names []string
	for _, n := range nodes {
		p := ClashProxy(n)
		if p == nil {
			continue
		}
		proxies = append(proxies, p)
		names = append(names, n.Name)
	}
	if len(names) == 0 {
		names = []string{"DIRECT"}
	}
	var buf bytes.Buffer
	if len(proxies) > 0 {
		b, err := yaml.Marshal(proxies)
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			buf.WriteString("  " + l + "\n")
		}
	} else {
		buf.WriteString("  []\n")
	}
	return execTemplate(orDefault(tpl, DefaultClashTemplate), map[string]any{
		"Proxies":     strings.TrimRight(buf.String(), "\n"),
		"Names":       flowList(names),
		"NamesInline": strings.Trim(flowList(names), "[]"),
		"NameList":    names,
	})
}

func flowList(ss []string) string {
	b, _ := json.Marshal(ss) // JSON string arrays are valid YAML flow sequences
	return string(b)
}

func execTemplate(tpl string, data any) ([]byte, error) {
	t, err := template.New("sub").Parse(tpl)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ClashProxy converts a node into a mihomo proxy map, or nil if unsupported.
func ClashProxy(n Node) map[string]any {
	p := map[string]any{"name": n.Name, "server": n.Server, "port": n.Port}
	tlsOn := n.TLS.Type == model.SecTLS || n.TLS.Type == model.SecReality
	applyTLS := func(sniKey string) {
		if n.TLS.ServerName != "" {
			p[sniKey] = n.TLS.ServerName
		}
		if n.TLS.Insecure {
			p["skip-cert-verify"] = true
		}
		if len(n.TLS.ALPN) > 0 {
			p["alpn"] = n.TLS.ALPN
		}
	}
	switch n.Protocol {
	case model.ProtoVLESS, model.ProtoVMess, model.ProtoTrojan:
		switch n.Protocol {
		case model.ProtoVLESS:
			p["type"] = "vless"
			p["uuid"] = n.UUID
			if n.Flow != "" {
				p["flow"] = n.Flow
			}
		case model.ProtoVMess:
			p["type"] = "vmess"
			p["uuid"] = n.UUID
			p["alterId"] = 0
			p["cipher"] = "auto"
		default:
			p["type"] = "trojan"
			p["password"] = n.Password
		}
		p["udp"] = true
		if n.Protocol != model.ProtoTrojan {
			p["tls"] = tlsOn
		}
		if tlsOn {
			if n.Protocol == model.ProtoTrojan {
				applyTLS("sni")
			} else {
				applyTLS("servername")
			}
			if n.TLS.Fingerprint != "" || n.TLS.Type == model.SecReality {
				p["client-fingerprint"] = orDefault(n.TLS.Fingerprint, "chrome")
			}
		}
		if n.TLS.Type == model.SecReality {
			p["reality-opts"] = map[string]any{"public-key": n.TLS.PublicKey, "short-id": n.TLS.ShortID}
		}
		switch n.Transport.Type {
		case "", model.TransTCP:
		case model.TransWS, model.TransHTTPUpgrade:
			p["network"] = "ws"
			o := map[string]any{"path": orDefault(n.Transport.Path, "/")}
			if n.Transport.Host != "" {
				o["headers"] = map[string]any{"Host": n.Transport.Host}
			}
			if n.Transport.Type == model.TransHTTPUpgrade {
				o["v2ray-http-upgrade"] = true
			}
			p["ws-opts"] = o
		case model.TransGRPC:
			p["network"] = "grpc"
			p["grpc-opts"] = map[string]any{"grpc-service-name": n.Transport.ServiceName}
		case model.TransXHTTP:
			p["network"] = "xhttp"
			o := map[string]any{"path": orDefault(n.Transport.Path, "/")}
			if n.Transport.Host != "" {
				o["host"] = n.Transport.Host
			}
			if n.Transport.Mode != "" {
				o["mode"] = n.Transport.Mode
			}
			p["xhttp-opts"] = o
		default:
			return nil
		}
	case model.ProtoShadowsocks:
		p["type"] = "ss"
		p["cipher"] = n.Method
		p["password"] = n.Password
		p["udp"] = true
	case model.ProtoHysteria2:
		p["type"] = "hysteria2"
		p["password"] = n.Password
		applyTLS("sni")
		if n.ObfsPassword != "" {
			p["obfs"] = orDefault(n.Obfs, "salamander")
			p["obfs-password"] = n.ObfsPassword
		}
	case model.ProtoTUIC:
		p["type"] = "tuic"
		p["uuid"] = n.UUID
		p["password"] = n.Password
		p["congestion-controller"] = orDefault(n.CongestionControl, "bbr")
		applyTLS("sni")
		if _, ok := p["alpn"]; !ok {
			p["alpn"] = []string{"h3"}
		}
		p["udp-relay-mode"] = "native"
	case model.ProtoAnyTLS:
		p["type"] = "anytls"
		p["password"] = n.Password
		applyTLS("sni")
		p["udp"] = true
	case model.ProtoSOCKS:
		p["type"] = "socks5"
		p["username"], p["password"] = n.Username, n.Password
		p["udp"] = true
	case model.ProtoHTTP:
		p["type"] = "http"
		p["username"], p["password"] = n.Username, n.Password
	default:
		return nil
	}
	return p
}
