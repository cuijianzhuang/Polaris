package sub

import (
	"fmt"
	"net"
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
{{ .ProxyGroups }}

[Remote Rule]
{{ .RemoteRules }}

[Rule]
{{ .Rules }}
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
	data := templateData(kept, info, strings.Join(lines, "\n"))
	data["ProxyGroups"], data["RemoteRules"], data["Rules"] = loonScheme(schemeOf(info), kept)
	return execTemplate(orDefault(tpl, DefaultLoonTemplate), data)
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
		if n.ForceUDP {
			parts = append(parts, "udp=true")
		}
	case model.ProtoVLESS:
		if n.Encryption != "" {
			return "" // no VLESS Encryption
		}
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
		switch n.Transport.Type {
		case model.TransWS:
			t, _ := transport()
			parts = append(parts, t...)
		case "", model.TransTCP:
		default:
			return "" // gRPC / HTTPUpgrade cannot be expressed
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
{{ .ProxyGroups }}

[server_local]
{{ .Proxies }}

[filter_remote]
{{ .RemoteRules }}

[filter_local]
{{ .Rules }}
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
	data := templateData(kept, info, strings.Join(lines, "\n"))
	data["ProxyGroups"], data["RemoteRules"], data["Rules"] = quanxScheme(schemeOf(info), kept)
	return execTemplate(orDefault(tpl, DefaultQuanxTemplate), data)
}

// QuanxLine renders a Quantumult X [server_local] line, or "" when unsupported.
func QuanxLine(n Node) string {
	tag := "tag=" + strings.ReplaceAll(n.Name, ",", " ")
	hp := net.JoinHostPort(n.Server, fmt.Sprint(n.Port)) // brackets IPv6
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
		if n.Encryption != "" {
			return "" // no VLESS Encryption
		}
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
		if t := n.Transport.Type; t != "" && t != model.TransTCP && t != model.TransWS {
			return "" // gRPC / HTTPUpgrade cannot be expressed
		}
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
	if n.ForceUDP && (n.Protocol == model.ProtoVMess || n.Protocol == model.ProtoVLESS) {
		parts = append(parts, "udp-relay=true")
	}
	if n.TLS.Insecure {
		parts = append(parts, "tls-verification=false")
	}
	return strings.Join(append(parts, tag), ", ")
}
