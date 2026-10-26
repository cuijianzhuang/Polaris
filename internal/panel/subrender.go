package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Rendering shared by every subscription: global operators, the converter
// backend (built-in renderers or a subconverter-compatible service), rule
// templates, and the safe HTTP client for outside fetches. Subscription
// sources and groups (subscription conversion) are a paid feature in
// polaris-pro (subconvert_pro.go).

var errPrivateTarget = errors.New("refusing to fetch a private / loopback address")

var (
	_, cgnat, _   = net.ParseCIDR("100.64.0.0/10") // carrier-grade NAT / Tailscale
	_, thisNet, _ = net.ParseCIDR("0.0.0.0/8")
)

func checkSourceIP(ip net.IP, allowPrivate bool) error {
	// An explicit LAN opt-in must not also enable metadata/link-local or
	// non-destination addresses.
	if ip == nil || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() || thisNet.Contains(ip) {
		return errPrivateTarget
	}
	if !allowPrivate && (ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)) {
		return errPrivateTarget
	}
	return nil
}

// A proxy resolves and dials the destination itself: the socket guard only
// sees the proxy. Check every destination (including redirects) before handing
// it to an environment proxy. The configured proxy remains a trusted resolver;
// this check does not pin the proxy's remote DNS result.
func sourceProxy(allowPrivate bool, proxy func(*http.Request) (*url.URL, error), lookup func(context.Context, string, string) ([]net.IP, error)) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		p, err := proxy(req)
		if err != nil || p == nil {
			return p, err
		}
		host := req.URL.Hostname()
		ips := []net.IP{net.ParseIP(host)}
		if ips[0] == nil {
			ips, err = lookup(req.Context(), "ip", host)
			if err != nil {
				return nil, err
			}
		}
		if len(ips) == 0 {
			return nil, errPrivateTarget
		}
		for _, ip := range ips {
			if err := checkSourceIP(ip, allowPrivate); err != nil {
				return nil, err
			}
		}
		return p, nil
	}
}

// safeClient refuses to connect to private ranges unless allowed (SSRF guard).
func (s *Server) safeClient(timeout time.Duration) *http.Client {
	allowPrivate := s.st.GetSetting("allow_private_sources", "false") == "true"
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		return checkSourceIP(net.ParseIP(host), allowPrivate)
	}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = dialer.DialContext
	tr.Proxy = sourceProxy(allowPrivate, http.ProxyFromEnvironment, net.DefaultResolver.LookupIP)
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}}
}

func (s *Server) globalOperators() []sub.Operator {
	var ops []sub.Operator
	_ = json.Unmarshal([]byte(s.st.GetSetting("sub_operators", "[]")), &ops)
	return ops
}

// converterConfig resolves builtin vs. external conversion.
type converterConfig struct {
	External bool
	Backend  string
	Config   string
	Params   string
}

func (s *Server) converterFor(p *store.SubProfile) converterConfig {
	mode := s.st.GetSetting("converter_mode", "builtin")
	cc := converterConfig{Backend: s.st.GetSetting("converter_backend", ""), Config: s.st.GetSetting("converter_config", ""),
		Params: s.st.GetSetting("converter_params", "emoji=true&udp=true&scv=true")}
	if p != nil {
		if p.Converter != "" {
			mode = p.Converter
		}
		if p.Backend != "" {
			cc.Backend = p.Backend
		}
		if p.BackendConfig != "" {
			cc.Config = p.BackendConfig
		}
	} else if s.st.GetSetting("converter_for_users", "false") != "true" {
		mode = "builtin"
	}
	cc.External = mode == "external" && cc.Backend != ""
	ro := s.renderOptsFor(p)
	if src := s.schemeSourceURL(ro.Scheme); src != "" && (p == nil || p.BackendConfig == "") {
		cc.Config = src
	}
	cc.Params = externalParams(cc.Params, ro)
	return cc
}

var externalTargets = map[string]string{
	"clash": "clash", "singbox": "singbox", "surge": "surge&ver=4", "loon": "loon", "quanx": "quanx", "links": "v2ray",
}

// renderExternal asks a subconverter-compatible backend to convert our own
// base64 link list (callback) into the client format.
func (s *Server) renderExternal(ctx context.Context, cc converterConfig, format, callback string) ([]byte, string, error) {
	target, ok := externalTargets[format]
	if !ok {
		return nil, "", fmt.Errorf("external converter has no target for %s", format)
	}
	q := "target=" + target + "&url=" + url.QueryEscape(callback)
	if cc.Config != "" {
		q += "&config=" + url.QueryEscape(cc.Config)
	}
	if cc.Params != "" {
		q += "&" + strings.TrimPrefix(cc.Params, "&")
	}
	backend := strings.TrimRight(cc.Backend, "/")
	if !strings.HasSuffix(backend, "/sub") {
		backend += "/sub"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, backend+"?"+q, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := (&http.Client{Timeout: 40 * time.Second}).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("converter backend: %s %s", resp.Status, truncate(string(body), 200))
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// templateContent picks a format's template: explicit id, else the default.
// With a rule scheme active, a template that does not place the scheme
// ({{ .ProxyGroups }}) — e.g. one saved before schemes existed — is replaced
// by the built-in template so the chosen scheme always takes effect.
func (s *Server) templateContent(format string, ids map[string]string, scheme bool) string {
	var t store.SubTemplate
	content := ""
	if id := ids[format]; id != "" && s.st.DB.First(&t, "id = ? AND format = ?", id, format).Error == nil {
		content = t.Content
	} else if s.st.DB.Where("format = ? AND is_default = ?", format, true).First(&t).Error == nil {
		content = t.Content
	}
	if scheme && !strings.Contains(content, ".ProxyGroups") {
		return ""
	}
	return content
}

// serveNodes writes a subscription response in the requested format.
func (s *Server) serveNodes(c *gin.Context, nodes []sub.Node, info sub.Info, templates map[string]string, cc converterConfig, ro renderOpts, callback string, u *store.User) {
	info.Title = s.subTitle(info.Title)
	format := sub.DetectFormat(c.Query("format"), c.GetHeader("User-Agent"))
	raw := c.Query("raw") == "1" // callback from the external converter: always built-in links
	if raw {
		format = "links"
	}
	if format == "html" {
		s.pageExtras(&info, u, nodes)
		b, err := sub.RenderPage(info)
		if err != nil {
			serverError(c, err)
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", b)
		return
	}
	info.Announce = strings.TrimSpace(s.st.GetSetting("sub_announce", ""))
	planInfo(&info, u)
	info.StatusLine = format == "links" && !raw && strings.Contains(strings.ToLower(c.GetHeader("User-Agent")), "shadowrocket")
	if h, err := strconv.Atoi(s.st.GetSetting("sub_update_interval", "1")); err == nil {
		info.UpdateHours = h
	}
	// The external converter's callback (raw) is not the client: leave the
	// auto mode's decision to the real request.
	nodes = s.applyTransforms(c.Request.Context(), nodes, format, u)
	nodes = applyNodeOpts(s.decorateNodes(nodes, info, raw || sub.ReadsProfileHeaders(c.GetHeader("User-Agent"))), ro)
	if _, builtin := sub.Get(format); !builtin && !raw {
		if p, f, ok := s.pluginFormat(format); ok {
			body, ctype, err := s.renderPluginFormat(c.Request.Context(), p, f, nodes, info, u)
			if err != nil {
				s.log.Warn("plugin subscription format failed", "plugin", p.ID, "format", format, "err", err)
				c.String(http.StatusServiceUnavailable, errPluginFormat.Error())
				return
			}
			s.subHeaders(c, info)
			c.Header("content-disposition", "attachment; filename=\""+asciiName(info.Title)+"\"; filename*=UTF-8''"+url.PathEscape(info.Title))
			c.Header("X-Content-Type-Options", "nosniff")
			c.Data(http.StatusOK, ctype, body)
			return
		}
	}
	var body []byte
	ctype := ""
	hasSnell := false
	for _, n := range nodes {
		if n.Protocol == "snell" {
			hasSnell = true
			break
		}
	}
	if format == "surge" {
		nodes = sub.FilterSurgeSnell(nodes, c.GetHeader("User-Agent"))
	}
	if format == "singbox" {
		nodes = sub.FilterSnellClient(nodes, c.GetHeader("User-Agent"))
	}
	if cc.External && !raw && format != "links" && !(hasSnell && (format == "singbox" || format == "surge")) {
		var err error
		body, ctype, err = s.renderExternal(c.Request.Context(), cc, format, s.callbackURL(callback))
		if err != nil {
			s.log.Warn("external converter failed, using built-in", "err", err)
			body = nil
		}
	}
	if body == nil {
		r, ok := sub.Get(format)
		if !ok {
			c.String(http.StatusBadRequest, "unknown format")
			return
		}
		var err error
		if format != "links" {
			if info.Scheme, err = s.loadScheme(c.Request.Context(), ro.Scheme); err != nil {
				s.log.Warn("rule scheme unavailable, using the template default", "scheme", ro.Scheme, "err", err)
			}
		}
		if body, err = r.Render(nodes, info, s.templateContent(format, templates, info.Scheme != nil)); err != nil {
			serverError(c, err)
			return
		}
		ctype = r.ContentType()
	}
	s.subHeaders(c, info)
	// filename for clients that ignore the RFC 5987 form (older Clash builds).
	c.Header("content-disposition", "attachment; filename=\""+asciiName(info.Title)+"\"; filename*=UTF-8''"+url.PathEscape(info.Title))
	c.Data(http.StatusOK, ctype, body)
}

// subMeta lists formats, regions and operator types for the UI.
func (s *Server) subMeta(c *gin.Context) {
	type region struct{ Code, Name string }
	var rs []region
	for _, r := range sub.Regions() {
		rs = append(rs, region{r.Code, sub.Flag(r.Code) + " " + r.Zh})
	}
	c.JSON(http.StatusOK, gin.H{"formats": sub.Formats(), "pluginFormats": s.pluginFormatNames(), "regions": rs, "schemes": sub.Presets()})
}

// asciiName keeps the ASCII letters, digits and -_. of a title ("polaris"
// when nothing is left).
func asciiName(t string) string {
	var b strings.Builder
	for _, r := range t {
		if r < 128 && (r == '-' || r == '_' || r == '.' || r == ' ' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		return s
	}
	return "polaris"
}
