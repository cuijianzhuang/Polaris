package sub

import (
	"fmt"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() { Register(surgeRenderer{}) }

// DefaultSurgeTemplate is the default Surge profile. Protocols Surge cannot
// express (VLESS, REALITY, xhttp…) are skipped.
const DefaultSurgeTemplate = `#!MANAGED-CONFIG {{ .URL }} interval={{ .UpdateSeconds }} strict=false

[General]
loglevel = notify
skip-proxy = 127.0.0.1, 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12, localhost, *.local
dns-server = system, 223.5.5.5

[Proxy]
{{ .Proxies }}

[Proxy Group]
{{ .ProxyGroups }}

[Rule]
{{ .Rules }}
`

type surgeRenderer struct{}

func (surgeRenderer) Format() string      { return "surge" }
func (surgeRenderer) ContentType() string { return "text/plain; charset=utf-8" }

func (surgeRenderer) Render(nodes []Node, info Info, tpl string) ([]byte, error) {
	var lines []string
	var kept []Node
	for _, n := range nodes {
		if l := SurgeLine(n); l != "" {
			lines = append(lines, l)
			kept = append(kept, n)
		}
	}
	data := templateData(kept, info, strings.Join(lines, "\n"))
	data["ProxyGroups"], data["Rules"] = surgeScheme(schemeOf(info), kept)
	return execTemplate(orDefault(tpl, DefaultSurgeTemplate), data)
}

// SurgeLine renders one [Proxy] line, or "" if Surge cannot express the node.
func SurgeLine(n Node) string {
	name := strings.NewReplacer(",", " ", "=", " ", "\r", " ", "\n", " ").Replace(n.Name)
	var parts []string
	tls := func() {
		if n.TLS.ServerName != "" {
			parts = append(parts, "sni="+n.TLS.ServerName)
		}
		if n.TLS.Insecure {
			parts = append(parts, "skip-cert-verify=true")
		}
	}
	if n.TLS.Type == model.SecReality {
		return ""
	}
	switch n.Protocol {
	case model.ProtoSnell:
		if !n.SnellSinglePSK || n.SnellPSK == "" || (n.SnellVersion != 4 && n.SnellVersion != 6) {
			return ""
		}
		if (n.Transport.Type != "" && n.Transport.Type != model.TransTCP) || (n.TLS.Type != "" && n.TLS.Type != model.SecNone) {
			return ""
		}
		// Dedicated panel keys are URL-safe. Never let imported source fields
		// inject a second Surge policy parameter or line.
		for _, v := range []string{n.Server, n.SnellPSK, n.SnellMode, n.SnellObfs} {
			if strings.ContainsAny(v, ",\r\n") {
				return ""
			}
		}
		parts = []string{"snell", n.Server, fmt.Sprint(n.Port), "psk=" + n.SnellPSK, fmt.Sprintf("version=%d", n.SnellVersion)}
		if n.SnellVersion == 6 {
			switch n.SnellMode {
			case "", "default", "unshaped", "unsafe-raw":
			default:
				return ""
			}
			parts = append(parts, "mode="+orDefault(n.SnellMode, "default"))
		} else if n.SnellObfs == "http" {
			parts = append(parts, "obfs=http")
		} else if n.SnellObfs != "" && n.SnellObfs != "none" {
			return ""
		}
	case model.ProtoVMess, model.ProtoTrojan:
		if n.Protocol == model.ProtoVMess {
			parts = []string{"vmess", n.Server, fmt.Sprint(n.Port), "username=" + n.UUID, "vmess-aead=true"}
		} else {
			parts = []string{"trojan", n.Server, fmt.Sprint(n.Port), "password=" + n.Password}
		}
		switch n.Transport.Type {
		case "", model.TransTCP:
		case model.TransWS:
			parts = append(parts, "ws=true", "ws-path="+orDefault(n.Transport.Path, "/"))
			if n.Transport.Host != "" {
				parts = append(parts, "ws-headers=Host:"+n.Transport.Host)
			}
		default:
			return ""
		}
		if n.TLS.Type == model.SecTLS {
			if n.Protocol == model.ProtoVMess {
				parts = append(parts, "tls=true")
			}
			tls()
		}
	case model.ProtoShadowsocks:
		parts = []string{"ss", n.Server, fmt.Sprint(n.Port), "encrypt-method=" + n.Method, "password=" + n.Password, "udp-relay=true"}
	case model.ProtoHysteria2:
		parts = []string{"hysteria2", n.Server, fmt.Sprint(n.Port), "password=" + n.Password}
		if n.ObfsPassword != "" {
			return "" // Surge has no salamander obfs
		}
		tls()
	case model.ProtoTUIC:
		parts = []string{"tuic-v5", n.Server, fmt.Sprint(n.Port), "password=" + n.Password, "uuid=" + n.UUID, "alpn=h3"}
		tls()
	case model.ProtoAnyTLS:
		parts = []string{"anytls", n.Server, fmt.Sprint(n.Port), "password=" + n.Password}
		tls()
	case model.ProtoSOCKS:
		parts = []string{"socks5", n.Server, fmt.Sprint(n.Port), n.Username, n.Password}
	case model.ProtoHTTP:
		parts = []string{"http", n.Server, fmt.Sprint(n.Port), n.Username, n.Password}
	default:
		return ""
	}
	if n.ForceUDP && (n.Protocol == model.ProtoVMess || n.Protocol == model.ProtoTrojan || n.Protocol == model.ProtoSOCKS) {
		parts = append(parts, "udp-relay=true")
	}
	return name + " = " + strings.Join(parts, ", ")
}
