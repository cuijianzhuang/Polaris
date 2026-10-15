package sub

import (
	"fmt"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() { Register(surgeRenderer{}) }

// DefaultSurgeTemplate is the default Surge profile. Protocols Surge cannot
// express (VLESS, REALITY, xhttp…) are skipped.
const DefaultSurgeTemplate = `#!MANAGED-CONFIG {{ .URL }} interval=43200 strict=false

[General]
loglevel = notify
skip-proxy = 127.0.0.1, 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12, localhost, *.local
dns-server = system, 223.5.5.5

[Proxy]
{{ .Proxies }}

[Proxy Group]
节点选择 = select, 自动选择, DIRECT{{ .NamesCSV }}
自动选择 = url-test{{ .NamesCSV }}, url=http://www.gstatic.com/generate_204, interval=600

[Rule]
GEOIP,CN,DIRECT
FINAL,节点选择,dns-failed
`

type surgeRenderer struct{}

func (surgeRenderer) Format() string      { return "surge" }
func (surgeRenderer) ContentType() string { return "text/plain; charset=utf-8" }

func (surgeRenderer) Render(nodes []Node, info Info, tpl string) ([]byte, error) {
	var lines, names []string
	for _, n := range nodes {
		if l := SurgeLine(n); l != "" {
			lines = append(lines, l)
			names = append(names, n.Name)
		}
	}
	csv := ""
	for _, n := range names {
		csv += ", " + n
	}
	return execTemplate(orDefault(tpl, DefaultSurgeTemplate), map[string]any{
		"URL":      info.URL,
		"Proxies":  strings.Join(lines, "\n"),
		"NamesCSV": csv,
	})
}

// SurgeLine renders one [Proxy] line, or "" if Surge cannot express the node.
func SurgeLine(n Node) string {
	name := strings.NewReplacer(",", " ", "=", " ").Replace(n.Name)
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
	return name + " = " + strings.Join(parts, ", ")
}
