package sub

import (
	"fmt"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() {
	Register(loonRenderer{})
	Register(quanxRenderer{})
}

// DefaultLoonTemplate is the default Loon profile.
const DefaultLoonTemplate = `[General]
skip-proxy = 192.168.0.0/16,10.0.0.0/8,172.16.0.0/12,localhost,*.local
dns-server = system,223.5.5.5

[Proxy]
{{ .Proxies }}

[Proxy Group]
节点选择 = select,自动选择,故障转移{{ range .Regions }},{{ .Name }}{{ end }},DIRECT{{ .NamesCSV }}
自动选择 = url-test{{ .NamesCSV }},url=http://www.gstatic.com/generate_204,interval=600
故障转移 = fallback{{ .NamesCSV }},url=http://www.gstatic.com/generate_204,interval=600
{{- range .Regions }}
{{ .Name }} = url-test,{{ join .Names "," }},url=http://www.gstatic.com/generate_204,interval=600
{{- end }}

[Rule]
GEOIP,CN,DIRECT
FINAL,节点选择
`

type loonRenderer struct{}

func (loonRenderer) Format() string      { return "loon" }
func (loonRenderer) ContentType() string { return "text/plain; charset=utf-8" }

func (loonRenderer) Render(nodes []Node, info Info, tpl string) ([]byte, error) {
	var lines []string
	var kept []Node
	for _, n := range nodes {
		if l := LoonLine(n); l != "" {
			lines = append(lines, l)
			kept = append(kept, n)
		}
	}
	return execTemplate(orDefault(tpl, DefaultLoonTemplate), templateData(kept, info, strings.Join(lines, "\n")))
}

func q(s string) string { return `"` + strings.ReplaceAll(s, `"`, ``) + `"` }

// LoonLine renders a Loon [Proxy] line, or "" when unsupported.
func LoonLine(n Node) string {
	name := strings.NewReplacer(",", " ", "=", " ").Replace(n.Name)
	host, port := n.Server, fmt.Sprint(n.Port)
	tlsOpts := func() []string {
		var o []string
		if n.TLS.Type == model.SecTLS || n.TLS.Type == model.SecReality {
			o = append(o, "over-tls=true")
		}
		if n.TLS.ServerName != "" {
			o = append(o, "sni="+n.TLS.ServerName)
		}
		o = append(o, fmt.Sprintf("skip-cert-verify=%v", n.TLS.Insecure))
		return o
	}
	transport := func() ([]string, bool) {
		switch n.Transport.Type {
		case "", model.TransTCP:
			return []string{"transport=tcp"}, true
		case model.TransWS:
			o := []string{"transport=ws", "path=" + orDefault(n.Transport.Path, "/")}
			if n.Transport.Host != "" {
				o = append(o, "host="+n.Transport.Host)
			}
			return o, true
		case model.TransHTTPUpgrade:
			return []string{"transport=http", "path=" + orDefault(n.Transport.Path, "/")}, true
		}
		return nil, false
	}
	var parts []string
	switch n.Protocol {
	case model.ProtoShadowsocks:
		parts = []string{"Shadowsocks", host, port, n.Method, q(n.Password), "udp=true"}
	case model.ProtoVMess:
		t, ok := transport()
		if !ok {
			return ""
		}
		parts = append([]string{"vmess", host, port, "auto", q(n.UUID)}, t...)
		parts = append(parts, tlsOpts()...)
	case model.ProtoVLESS:
		t, ok := transport()
		if !ok {
			return ""
		}
		parts = append([]string{"VLESS", host, port, q(n.UUID)}, t...)
		if n.Flow != "" {
			parts = append(parts, "flow="+n.Flow)
		}
		if n.TLS.Type == model.SecReality {
			parts = append(parts, "public-key="+q(n.TLS.PublicKey), "short-id="+n.TLS.ShortID)
		}
		parts = append(parts, tlsOpts()...)
		parts = append(parts, "udp=true")
	case model.ProtoTrojan:
		parts = []string{"trojan", host, port, q(n.Password)}
		if t, ok := transport(); ok && n.Transport.Type == model.TransWS {
			parts = append(parts, t...)
		}
		parts = append(parts, tlsOpts()...)
		parts = append(parts, "udp=true")
	case model.ProtoHysteria2:
		parts = []string{"Hysteria2", host, port, q(n.Password)}
		if n.TLS.ServerName != "" {
			parts = append(parts, "sni="+n.TLS.ServerName)
		}
		if n.ObfsPassword != "" {
			parts = append(parts, "salamander-password="+n.ObfsPassword)
		}
		parts = append(parts, fmt.Sprintf("skip-cert-verify=%v", n.TLS.Insecure), "udp=true")
	case model.ProtoSOCKS:
		parts = []string{"socks5", host, port, n.Username, q(n.Password), "udp=true"}
	case model.ProtoHTTP:
		parts = []string{"http", host, port, n.Username, q(n.Password)}
	default:
		return ""
	}
	return name + " = " + strings.Join(parts, ",")
}

// DefaultQuanxTemplate is the default Quantumult X profile.
const DefaultQuanxTemplate = `[general]
server_check_url=http://www.gstatic.com/generate_204

[dns]
server=223.5.5.5

[policy]
static=节点选择, 自动选择{{ range .Regions }}, {{ .Name }}{{ end }}, direct{{ .NamesCSV }}
url-latency-benchmark=自动选择{{ .NamesCSV }}, check-interval=600, tolerance=50
{{- range .Regions }}
url-latency-benchmark={{ .Name }}, {{ join .Names ", " }}, check-interval=600
{{- end }}

[server_local]
{{ .Proxies }}

[filter_local]
geoip, cn, direct
final, 节点选择
`

type quanxRenderer struct{}

func (quanxRenderer) Format() string      { return "quanx" }
func (quanxRenderer) ContentType() string { return "text/plain; charset=utf-8" }

func (quanxRenderer) Render(nodes []Node, info Info, tpl string) ([]byte, error) {
	var lines []string
	var kept []Node
	for _, n := range nodes {
		if l := QuanxLine(n); l != "" {
			lines = append(lines, l)
			kept = append(kept, n)
		}
	}
	return execTemplate(orDefault(tpl, DefaultQuanxTemplate), templateData(kept, info, strings.Join(lines, "\n")))
}

// QuanxLine renders a Quantumult X [server_local] line, or "" when unsupported.
func QuanxLine(n Node) string {
	tag := "tag=" + strings.ReplaceAll(n.Name, ",", " ")
	hp := fmt.Sprintf("%s:%d", n.Server, n.Port)
	obfs := func() ([]string, bool) {
		tls := n.TLS.Type == model.SecTLS || n.TLS.Type == model.SecReality
		host := orDefault(n.TLS.ServerName, n.Transport.Host)
		switch n.Transport.Type {
		case "", model.TransTCP:
			if tls {
				o := []string{"obfs=over-tls"}
				if host != "" {
					o = append(o, "obfs-host="+host)
				}
				return o, true
			}
			return nil, true
		case model.TransWS:
			mode := "ws"
			if tls {
				mode = "wss"
			}
			o := []string{"obfs=" + mode, "obfs-uri=" + orDefault(n.Transport.Path, "/")}
			if h := orDefault(n.Transport.Host, n.TLS.ServerName); h != "" {
				o = append(o, "obfs-host="+h)
			}
			return o, true
		}
		return nil, false
	}
	var parts []string
	switch n.Protocol {
	case model.ProtoShadowsocks:
		parts = []string{"shadowsocks=" + hp, "method=" + n.Method, "password=" + n.Password, "udp-relay=true"}
	case model.ProtoVMess:
		o, ok := obfs()
		if !ok {
			return ""
		}
		parts = append([]string{"vmess=" + hp, "method=chacha20-ietf-poly1305", "password=" + n.UUID, "aead=true"}, o...)
	case model.ProtoVLESS:
		o, ok := obfs()
		if !ok {
			return ""
		}
		parts = append([]string{"vless=" + hp, "method=none", "password=" + n.UUID}, o...)
		if n.Flow != "" {
			parts = append(parts, "vless-flow="+n.Flow)
		}
		if n.TLS.Type == model.SecReality {
			parts = append(parts, "reality-base64-pubkey="+n.TLS.PublicKey, "reality-hex-shortid="+n.TLS.ShortID)
		}
	case model.ProtoTrojan:
		parts = []string{"trojan=" + hp, "password=" + n.Password}
		if n.Transport.Type == model.TransWS {
			o, _ := obfs()
			parts = append(parts, o...)
		} else {
			parts = append(parts, "over-tls=true")
			if n.TLS.ServerName != "" {
				parts = append(parts, "tls-host="+n.TLS.ServerName)
			}
		}
		parts = append(parts, "udp-relay=true")
	case model.ProtoHTTP:
		parts = []string{"http=" + hp, "username=" + n.Username, "password=" + n.Password}
	case model.ProtoSOCKS:
		parts = []string{"socks5=" + hp, "username=" + n.Username, "password=" + n.Password, "udp-relay=true"}
	default:
		return ""
	}
	if n.TLS.Insecure {
		parts = append(parts, "tls-verification=false")
	}
	return strings.Join(append(parts, tag), ", ")
}
