package sub

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// The parsing half of the conversion engine: anything an upstream
// subscription may return (share links, base64 link lists, Clash/mihomo YAML,
// sing-box JSON) becomes []Node, which every renderer can output.

// ParseSubscription auto-detects the payload format.
func ParseSubscription(data []byte) ([]Node, error) {
	trim := bytes.TrimSpace(data)
	if len(trim) == 0 {
		return nil, nil
	}
	switch {
	case trim[0] == '{':
		return parseSingbox(trim)
	case bytes.Contains(trim, []byte("proxies:")):
		return parseClash(trim)
	}
	if !bytes.Contains(trim, []byte("://")) {
		if dec, ok := decodeB64(string(trim)); ok {
			return ParseSubscription([]byte(dec))
		}
	}
	return ParseLinks(string(trim)), nil
}

// ParseLinks parses one share link per line, skipping anything unrecognised.
func ParseLinks(text string) []Node {
	var out []Node
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if n, err := ParseLink(l); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func decodeB64(s string) (string, bool) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' {
			return -1
		}
		return r
	}, s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), true
		}
	}
	return "", false
}

// ParseLink parses a single share URI.
func ParseLink(link string) (Node, error) {
	scheme, _, ok := strings.Cut(link, "://")
	if !ok {
		return Node{}, fmt.Errorf("not a link")
	}
	switch strings.ToLower(scheme) {
	case "vmess":
		return parseVMess(link)
	case "ss":
		return parseSS(link)
	}
	u, err := url.Parse(link)
	if err != nil {
		return Node{}, err
	}
	n := Node{Name: fragment(u), Server: u.Hostname()}
	n.Port, _ = strconv.Atoi(u.Port())
	q := u.Query()
	pass, _ := u.User.Password()
	user := u.User.Username()
	switch strings.ToLower(u.Scheme) {
	case "vless":
		n.Protocol, n.UUID, n.Flow = model.ProtoVLESS, user, q.Get("flow")
		if n.Flow == "xtls-rprx-vision-udp443" {
			n.Flow, n.VisionUDP443 = "xtls-rprx-vision", true
		}
		if e := q.Get("encryption"); e != "" && e != "none" {
			n.Encryption = e
		}
		applyStreamQuery(&n, q)
	case "trojan":
		n.Protocol, n.Password = model.ProtoTrojan, user
		applyStreamQuery(&n, q)
		if n.TLS.Type == "" || n.TLS.Type == model.SecNone {
			n.TLS.Type = model.SecTLS
		}
	case "hysteria2", "hy2":
		n.Protocol, n.Password = model.ProtoHysteria2, user
		if pass != "" {
			n.Password = user + ":" + pass
		}
		n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: q.Get("sni"), Insecure: q.Get("insecure") == "1"}
		n.ObfsPassword, n.Obfs = q.Get("obfs-password"), q.Get("obfs")
		n.Transport.Type = model.TransQUIC
	case "tuic":
		n.Protocol, n.UUID, n.Password = model.ProtoTUIC, user, pass
		n.CongestionControl = q.Get("congestion_control")
		n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: q.Get("sni"), Insecure: q.Get("allow_insecure") == "1" || q.Get("insecure") == "1"}
		if a := q.Get("alpn"); a != "" {
			n.TLS.ALPN = strings.Split(a, ",")
		}
		n.Transport.Type = model.TransQUIC
	case "anytls":
		n.Protocol, n.Password = model.ProtoAnyTLS, user
		n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: q.Get("sni"), Insecure: q.Get("insecure") == "1"}
	case "socks", "socks5":
		n.Protocol = model.ProtoSOCKS
		if pass == "" && user != "" {
			if dec, ok := decodeB64(user); ok {
				user, pass, _ = strings.Cut(dec, ":")
			}
		}
		n.Username, n.Password = user, pass
	case "http", "https":
		n.Protocol, n.Username, n.Password = model.ProtoHTTP, user, pass
		if u.Scheme == "https" {
			n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: u.Hostname()}
		}
	case "naive+https", "naive+quic":
		n.Protocol, n.Username, n.Password = model.ProtoNaive, user, pass
		n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: u.Hostname()}
	default:
		return Node{}, fmt.Errorf("unsupported scheme %s", u.Scheme)
	}
	if n.Name == "" {
		n.Name = n.Server + ":" + strconv.Itoa(n.Port)
	}
	if n.Server == "" || n.Port == 0 {
		return Node{}, fmt.Errorf("missing server or port")
	}
	return n, nil
}

func fragment(u *url.URL) string {
	if u.Fragment != "" {
		return u.Fragment
	}
	if f, err := url.PathUnescape(u.RawFragment); err == nil {
		return f
	}
	return ""
}

func applyStreamQuery(n *Node, q url.Values) {
	n.Transport = model.TransportSpec{Type: model.Transport(orDefault(q.Get("type"), "tcp")), Path: q.Get("path"), Host: q.Get("host"),
		ServiceName: q.Get("serviceName"), Mode: q.Get("mode"), HeaderType: q.Get("headerType"), Seed: q.Get("seed")}
	if n.Transport.Type == "raw" {
		n.Transport.Type = model.TransTCP
	}
	if n.Transport.Type == "splithttp" {
		n.Transport.Type = model.TransXHTTP
	}
	if n.Transport.HeaderType == "none" {
		n.Transport.HeaderType = ""
	}
	switch q.Get("security") {
	case "tls", "xtls":
		n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: q.Get("sni"), Fingerprint: q.Get("fp"),
			Insecure: q.Get("allowInsecure") == "1" || q.Get("insecure") == "1"}
		if a := q.Get("alpn"); a != "" {
			n.TLS.ALPN = strings.Split(a, ",")
		}
	case "reality":
		n.TLS = model.ClientTLS{Type: model.SecReality, ServerName: q.Get("sni"), Fingerprint: q.Get("fp"), PublicKey: q.Get("pbk"), ShortID: q.Get("sid"),
			SpiderX: q.Get("spx"), Mldsa65Verify: q.Get("pqv")}
	default:
		n.TLS = model.ClientTLS{Type: model.SecNone}
	}
}

func parseVMess(link string) (Node, error) {
	raw, ok := decodeB64(strings.TrimPrefix(link, "vmess://"))
	if !ok {
		return Node{}, fmt.Errorf("vmess: bad base64")
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return Node{}, fmt.Errorf("vmess: %w", err)
	}
	str := func(k string) string {
		switch x := v[k].(type) {
		case string:
			return x
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return ""
	}
	port, _ := strconv.Atoi(str("port"))
	n := Node{Name: str("ps"), Server: str("add"), Port: port, Protocol: model.ProtoVMess, UUID: str("id"),
		Transport: model.TransportSpec{Type: model.Transport(orDefault(str("net"), "tcp")), Host: str("host"), Path: str("path")}}
	if n.Transport.Type == model.TransGRPC {
		n.Transport.ServiceName, n.Transport.Path = n.Transport.Path, ""
	}
	if t := str("type"); t != "" && t != "none" && (n.Transport.Type == model.TransTCP || n.Transport.Type == model.TransKCP) {
		n.Transport.HeaderType = t
	}
	if str("tls") == "tls" {
		n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: str("sni"), Fingerprint: str("fp")}
		if a := str("alpn"); a != "" {
			n.TLS.ALPN = strings.Split(a, ",")
		}
	}
	if n.Server == "" || n.Port == 0 {
		return Node{}, fmt.Errorf("vmess: missing server")
	}
	return n, nil
}

// parseSS handles SIP002 (base64 or plain userinfo) and the legacy
// ss://base64(method:pass@host:port) form.
func parseSS(link string) (Node, error) {
	body := strings.TrimPrefix(link, "ss://")
	name := ""
	if i := strings.Index(body, "#"); i >= 0 {
		name, _ = url.PathUnescape(body[i+1:])
		body = body[:i]
	}
	if i := strings.Index(body, "?"); i >= 0 {
		body = body[:i] // plugin parameters are not supported
	}
	body = strings.TrimSuffix(body, "/")
	if !strings.Contains(body, "@") {
		dec, ok := decodeB64(body)
		if !ok {
			return Node{}, fmt.Errorf("ss: bad legacy link")
		}
		body = dec
	}
	at := strings.LastIndex(body, "@")
	if at < 0 {
		return Node{}, fmt.Errorf("ss: missing @")
	}
	userinfo, hostport := body[:at], body[at+1:]
	if dec, ok := decodeB64(userinfo); ok && strings.Contains(dec, ":") {
		userinfo = dec
	} else if un, err := url.PathUnescape(userinfo); err == nil {
		userinfo = un
	}
	method, pass, ok := strings.Cut(userinfo, ":")
	if !ok {
		return Node{}, fmt.Errorf("ss: bad userinfo")
	}
	host, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return Node{}, fmt.Errorf("ss: %w", err)
	}
	port, _ := strconv.Atoi(p)
	if name == "" {
		name = hostport
	}
	return Node{Name: name, Server: host, Port: port, Protocol: model.ProtoShadowsocks, Method: method, Password: pass}, nil
}

// --- Clash / mihomo YAML ---

func parseClash(data []byte) ([]Node, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("clash yaml: %w", err)
	}
	var out []Node
	for _, p := range doc.Proxies {
		if n, ok := fromClash(p); ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func mstr(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func mbool(m map[string]any, k string) bool { b, _ := m[k].(bool); return b }

func mmap(m map[string]any, k string) map[string]any {
	x, _ := m[k].(map[string]any)
	return x
}

func mlist(m map[string]any, k string) []string {
	var out []string
	if l, ok := m[k].([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func fromClash(p map[string]any) (Node, bool) {
	port, _ := strconv.Atoi(mstr(p, "port"))
	n := Node{Name: mstr(p, "name"), Server: mstr(p, "server"), Port: port}
	sni := orDefault(mstr(p, "servername"), mstr(p, "sni"))
	tls := func() {
		n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: sni, Insecure: mbool(p, "skip-cert-verify"),
			Fingerprint: mstr(p, "client-fingerprint"), ALPN: mlist(p, "alpn")}
	}
	switch mstr(p, "type") {
	case "vless", "vmess", "trojan":
		switch mstr(p, "type") {
		case "vless":
			n.Protocol, n.UUID, n.Flow = model.ProtoVLESS, mstr(p, "uuid"), mstr(p, "flow")
			if e := mstr(p, "encryption"); e != "" && e != "none" {
				n.Encryption = e
			}
		case "vmess":
			n.Protocol, n.UUID = model.ProtoVMess, mstr(p, "uuid")
		default:
			n.Protocol, n.Password = model.ProtoTrojan, mstr(p, "password")
		}
		if mbool(p, "tls") || n.Protocol == model.ProtoTrojan {
			tls()
		}
		if r := mmap(p, "reality-opts"); r != nil {
			n.TLS.Type, n.TLS.PublicKey, n.TLS.ShortID = model.SecReality, mstr(r, "public-key"), mstr(r, "short-id")
		}
		n.Transport.Type = model.TransTCP
		switch mstr(p, "network") {
		case "http":
			n.Transport.Type, n.Transport.HeaderType = model.TransTCP, "http"
			o := mmap(p, "http-opts")
			if ps, ok := o["path"].([]any); ok && len(ps) > 0 {
				n.Transport.Path, _ = ps[0].(string)
			}
			if h := mmap(o, "headers"); h != nil {
				if hs, ok := h["Host"].([]any); ok && len(hs) > 0 {
					n.Transport.Host, _ = hs[0].(string)
				}
			}
		case "ws":
			n.Transport.Type = model.TransWS
			o := mmap(p, "ws-opts")
			n.Transport.Path = mstr(o, "path")
			if h := mmap(o, "headers"); h != nil {
				n.Transport.Host = mstr(h, "Host")
			}
			if mbool(o, "v2ray-http-upgrade") {
				n.Transport.Type = model.TransHTTPUpgrade
			}
		case "grpc":
			n.Transport.Type = model.TransGRPC
			n.Transport.ServiceName = mstr(mmap(p, "grpc-opts"), "grpc-service-name")
		case "xhttp":
			o := mmap(p, "xhttp-opts")
			n.Transport = model.TransportSpec{Type: model.TransXHTTP, Path: mstr(o, "path"), Host: mstr(o, "host"), Mode: mstr(o, "mode")}
		case "", "tcp":
		default:
			return n, false
		}
	case "ss":
		n.Protocol, n.Method, n.Password = model.ProtoShadowsocks, mstr(p, "cipher"), mstr(p, "password")
		if mstr(p, "plugin") != "" {
			return n, false
		}
	case "hysteria2":
		n.Protocol, n.Password = model.ProtoHysteria2, mstr(p, "password")
		n.ObfsPassword = mstr(p, "obfs-password")
		tls()
	case "tuic":
		n.Protocol, n.UUID, n.Password = model.ProtoTUIC, mstr(p, "uuid"), mstr(p, "password")
		n.CongestionControl = mstr(p, "congestion-controller")
		tls()
	case "anytls":
		n.Protocol, n.Password = model.ProtoAnyTLS, mstr(p, "password")
		tls()
	case "socks5":
		n.Protocol, n.Username, n.Password = model.ProtoSOCKS, mstr(p, "username"), mstr(p, "password")
	case "http":
		n.Protocol, n.Username, n.Password = model.ProtoHTTP, mstr(p, "username"), mstr(p, "password")
	default:
		return n, false
	}
	return n, n.Server != "" && n.Port > 0
}

// --- sing-box JSON ---

func parseSingbox(data []byte) ([]Node, error) {
	var doc struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("sing-box json: %w", err)
	}
	var out []Node
	for _, o := range doc.Outbounds {
		port, _ := strconv.Atoi(mstr(o, "server_port"))
		n := Node{Name: mstr(o, "tag"), Server: mstr(o, "server"), Port: port}
		switch mstr(o, "type") {
		case "vless":
			n.Protocol, n.UUID, n.Flow = model.ProtoVLESS, mstr(o, "uuid"), mstr(o, "flow")
		case "vmess":
			n.Protocol, n.UUID = model.ProtoVMess, mstr(o, "uuid")
		case "trojan":
			n.Protocol, n.Password = model.ProtoTrojan, mstr(o, "password")
		case "shadowsocks":
			n.Protocol, n.Method, n.Password = model.ProtoShadowsocks, mstr(o, "method"), mstr(o, "password")
		case "hysteria2":
			n.Protocol, n.Password = model.ProtoHysteria2, mstr(o, "password")
			if ob := mmap(o, "obfs"); ob != nil {
				n.Obfs, n.ObfsPassword = mstr(ob, "type"), mstr(ob, "password")
			}
		case "tuic":
			n.Protocol, n.UUID, n.Password, n.CongestionControl = model.ProtoTUIC, mstr(o, "uuid"), mstr(o, "password"), mstr(o, "congestion_control")
		case "anytls":
			n.Protocol, n.Password = model.ProtoAnyTLS, mstr(o, "password")
		case "socks":
			n.Protocol, n.Username, n.Password = model.ProtoSOCKS, mstr(o, "username"), mstr(o, "password")
		case "http":
			n.Protocol, n.Username, n.Password = model.ProtoHTTP, mstr(o, "username"), mstr(o, "password")
		default:
			continue
		}
		if t := mmap(o, "transport"); t != nil {
			switch mstr(t, "type") {
			case "ws":
				n.Transport = model.TransportSpec{Type: model.TransWS, Path: mstr(t, "path")}
				if h := mmap(t, "headers"); h != nil {
					n.Transport.Host = mstr(h, "Host")
				}
			case "httpupgrade":
				n.Transport = model.TransportSpec{Type: model.TransHTTPUpgrade, Path: mstr(t, "path"), Host: mstr(t, "host")}
			case "grpc":
				n.Transport = model.TransportSpec{Type: model.TransGRPC, ServiceName: mstr(t, "service_name")}
			}
		}
		if t := mmap(o, "tls"); t != nil && mbool(t, "enabled") {
			n.TLS = model.ClientTLS{Type: model.SecTLS, ServerName: mstr(t, "server_name"), Insecure: mbool(t, "insecure"), ALPN: mlist(t, "alpn")}
			if u := mmap(t, "utls"); u != nil {
				n.TLS.Fingerprint = mstr(u, "fingerprint")
			}
			if r := mmap(t, "reality"); r != nil && mbool(r, "enabled") {
				n.TLS.Type, n.TLS.PublicKey, n.TLS.ShortID = model.SecReality, mstr(r, "public_key"), mstr(r, "short_id")
			}
		}
		if n.Server != "" && n.Port > 0 {
			out = append(out, n)
		}
	}
	return out, nil
}
