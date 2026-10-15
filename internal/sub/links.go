package sub

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() { Register(linksRenderer{}) }

// linksRenderer outputs base64 share links for v2rayN / v2rayNG / Shadowrocket.
type linksRenderer struct{}

func (linksRenderer) Format() string      { return "links" }
func (linksRenderer) ContentType() string { return "text/plain; charset=utf-8" }

func (linksRenderer) Render(nodes []Node, _ Info, _ string) ([]byte, error) {
	var lines []string
	for _, n := range nodes {
		if l := ShareLink(n); l != "" {
			lines = append(lines, l)
		}
	}
	return []byte(base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n")))), nil
}

func hostPort(n Node) string { return net.JoinHostPort(n.Server, strconv.Itoa(n.Port)) }

// ShareLink returns the standard URI for a node, or "" if none exists.
func ShareLink(n Node) string {
	frag := "#" + url.PathEscape(n.Name)
	switch n.Protocol {
	case model.ProtoVLESS:
		q := streamQuery(n)
		q.Set("encryption", "none")
		if n.Flow != "" {
			q.Set("flow", n.Flow)
		}
		return "vless://" + n.UUID + "@" + hostPort(n) + "?" + q.Encode() + frag
	case model.ProtoTrojan:
		return "trojan://" + userEsc(n.Password) + "@" + hostPort(n) + "?" + streamQuery(n).Encode() + frag
	case model.ProtoVMess:
		v := map[string]string{
			"v": "2", "ps": n.Name, "add": n.Server, "port": strconv.Itoa(n.Port), "id": n.UUID,
			"aid": "0", "scy": "auto", "net": netName(n.Transport.Type), "type": "none",
			"host": n.Transport.Host, "path": n.Transport.Path, "tls": "", "sni": n.TLS.ServerName,
			"fp": n.TLS.Fingerprint,
		}
		if n.Transport.Type == model.TransGRPC {
			v["path"] = n.Transport.ServiceName
		}
		if n.Transport.Type == model.TransKCP {
			v["type"] = orDefault(n.Transport.HeaderType, "none")
		}
		if n.TLS.Type == model.SecTLS {
			v["tls"] = "tls"
		}
		if len(n.TLS.ALPN) > 0 {
			v["alpn"] = strings.Join(n.TLS.ALPN, ",")
		}
		b, _ := json.Marshal(v)
		return "vmess://" + base64.StdEncoding.EncodeToString(b)
	case model.ProtoShadowsocks:
		var userinfo string
		if model.SS2022KeyLen(n.Method) > 0 {
			// SIP022: plain, percent-encoded userinfo.
			userinfo = userEsc(n.Method) + ":" + userEsc(n.Password)
		} else {
			userinfo = base64.RawURLEncoding.EncodeToString([]byte(n.Method + ":" + n.Password))
		}
		return "ss://" + userinfo + "@" + hostPort(n) + frag
	case model.ProtoHysteria2:
		q := url.Values{}
		setNE(q, "sni", n.TLS.ServerName)
		if n.TLS.Insecure {
			q.Set("insecure", "1")
		}
		if n.ObfsPassword != "" {
			q.Set("obfs", orDefault(n.Obfs, "salamander"))
			q.Set("obfs-password", n.ObfsPassword)
		}
		return "hysteria2://" + userEsc(n.Password) + "@" + hostPort(n) + "/?" + q.Encode() + frag
	case model.ProtoTUIC:
		q := url.Values{}
		q.Set("congestion_control", orDefault(n.CongestionControl, "bbr"))
		q.Set("alpn", "h3")
		setNE(q, "sni", n.TLS.ServerName)
		if n.TLS.Insecure {
			q.Set("allow_insecure", "1")
		}
		return "tuic://" + n.UUID + ":" + userEsc(n.Password) + "@" + hostPort(n) + "?" + q.Encode() + frag
	case model.ProtoAnyTLS:
		q := url.Values{}
		setNE(q, "sni", n.TLS.ServerName)
		if n.TLS.Insecure {
			q.Set("insecure", "1")
		}
		return "anytls://" + userEsc(n.Password) + "@" + hostPort(n) + "/?" + q.Encode() + frag
	case model.ProtoSOCKS:
		return "socks://" + base64.RawURLEncoding.EncodeToString([]byte(n.Username+":"+n.Password)) + "@" + hostPort(n) + frag
	case model.ProtoHTTP:
		return "http://" + userEsc(n.Username) + ":" + userEsc(n.Password) + "@" + hostPort(n) + frag
	case model.ProtoNaive:
		return "naive+https://" + userEsc(n.Username) + ":" + userEsc(n.Password) + "@" + hostPort(n) + frag
	}
	return ""
}

// userEsc escapes a URI userinfo part. Unlike PathEscape it also escapes
// ":" and "@", which would otherwise split the credentials.
func userEsc(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func netName(t model.Transport) string {
	if t == "" {
		return "tcp"
	}
	return string(t)
}

// streamQuery encodes transport and security for vless/trojan URIs.
func streamQuery(n Node) url.Values {
	q := url.Values{}
	q.Set("type", netName(n.Transport.Type))
	switch n.Transport.Type {
	case model.TransWS, model.TransHTTPUpgrade, model.TransXHTTP:
		setNE(q, "path", n.Transport.Path)
		setNE(q, "host", n.Transport.Host)
		if n.Transport.Type == model.TransXHTTP {
			setNE(q, "mode", n.Transport.Mode)
		}
	case model.TransGRPC:
		setNE(q, "serviceName", n.Transport.ServiceName)
	case model.TransKCP:
		setNE(q, "headerType", n.Transport.HeaderType)
		setNE(q, "seed", n.Transport.Seed)
	default:
		q.Set("headerType", "none")
	}
	switch n.TLS.Type {
	case model.SecTLS:
		q.Set("security", "tls")
		setNE(q, "sni", n.TLS.ServerName)
		setNE(q, "fp", n.TLS.Fingerprint)
		if len(n.TLS.ALPN) > 0 {
			q.Set("alpn", strings.Join(n.TLS.ALPN, ","))
		}
		if n.TLS.Insecure {
			q.Set("allowInsecure", "1")
		}
	case model.SecReality:
		q.Set("security", "reality")
		setNE(q, "sni", n.TLS.ServerName)
		q.Set("fp", orDefault(n.TLS.Fingerprint, "chrome"))
		setNE(q, "pbk", n.TLS.PublicKey)
		setNE(q, "sid", n.TLS.ShortID)
	default:
		q.Set("security", "none")
	}
	return q
}

func setNE(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
