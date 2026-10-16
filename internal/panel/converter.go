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

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Built-in subscription conversion: upstream subscriptions and manual nodes
// are parsed into sub.Node, merged with panel nodes into profiles, pushed
// through an operator chain and rendered by the built-in renderers — or
// handed to a third-party converter backend (subconverter-compatible).

const defaultSourceUA = "v2rayN/7.0"

var errPrivateTarget = errors.New("refusing to fetch a private / loopback address")

var (
	_, cgnat, _   = net.ParseCIDR("100.64.0.0/10") // carrier-grade NAT / Tailscale
	_, thisNet, _ = net.ParseCIDR("0.0.0.0/8")
)

// safeClient refuses to connect to private ranges unless allowed (SSRF guard).
func (s *Server) safeClient(timeout time.Duration) *http.Client {
	allowPrivate := s.st.GetSetting("allow_private_sources", "false") == "true"
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() ||
			ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip) || thisNet.Contains(ip) {
			return errPrivateTarget
		}
		return nil
	}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = dialer.DialContext
	tr.Proxy = http.ProxyFromEnvironment
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}}
}

// fetchSource downloads (or parses, for manual sources) and caches nodes.
func (s *Server) fetchSource(ctx context.Context, src *store.SubSource) ([]sub.Node, error) {
	var nodes []sub.Node
	var info store.SubTraffic
	var err error
	if src.Kind == "manual" {
		nodes, err = sub.ParseSubscription([]byte(src.Content))
	} else {
		nodes, info, err = s.downloadSubscription(ctx, src.URL, orDefault(src.UserAgent, defaultSourceUA))
	}
	now := time.Now()
	if err != nil {
		s.st.DB.Model(src).Updates(map[string]any{"last_error": truncate(err.Error(), 1900), "fetched_at": now})
		return nil, err
	}
	b, _ := json.Marshal(nodes)
	src.Cache, src.NodeCount, src.FetchedAt, src.LastError = string(b), len(nodes), &now, ""
	s.st.DB.Model(src).Updates(map[string]any{"cache": src.Cache, "node_count": len(nodes), "fetched_at": now,
		"last_error": "", "user_info": store.NewJSON(info)})
	src.UserInfo = store.NewJSON(info)
	return nodes, nil
}

func (s *Server) downloadSubscription(ctx context.Context, rawURL, ua string) ([]sub.Node, store.SubTraffic, error) {
	var info store.SubTraffic
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, info, fmt.Errorf("invalid subscription URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, info, err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := s.safeClient(30 * time.Second).Do(req)
	if err != nil {
		return nil, info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, info, fmt.Errorf("upstream returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, info, err
	}
	info = parseUserInfo(resp.Header.Get("subscription-userinfo"))
	nodes, err := sub.ParseSubscription(body)
	if err == nil && len(nodes) == 0 {
		err = fmt.Errorf("no supported nodes in response")
	}
	return nodes, info, err
}

func parseUserInfo(h string) store.SubTraffic {
	var t store.SubTraffic
	for _, kv := range strings.Split(h, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		switch strings.TrimSpace(k) {
		case "upload":
			t.Upload = n
		case "download":
			t.Download = n
		case "total":
			t.Total = n
		case "expire":
			t.Expire = n
		}
	}
	return t
}

// sourceNodes returns cached nodes, fetching on first use.
func (s *Server) sourceNodes(ctx context.Context, src *store.SubSource) []sub.Node {
	var nodes []sub.Node
	if src.Cache != "" {
		_ = json.Unmarshal([]byte(src.Cache), &nodes)
	} else if src.Kind == "manual" || src.FetchedAt == nil {
		nodes, _ = s.fetchSource(ctx, src)
	}
	for i := range nodes {
		nodes[i].Source = src.Name
	}
	return nodes
}

func (s *Server) globalOperators() []sub.Operator {
	var ops []sub.Operator
	_ = json.Unmarshal([]byte(s.st.GetSetting("sub_operators", "[]")), &ops)
	return ops
}

// profileNodes assembles a profile's nodes and aggregated traffic info.
func (s *Server) profileNodes(ctx context.Context, p *store.SubProfile) ([]sub.Node, sub.Info, error) {
	info := sub.Info{Title: p.Name, URL: s.profileURL(p.Token)}
	var nodes []sub.Node
	if p.PanelUserID != "" {
		var u store.User
		if err := s.st.DB.First(&u, "id = ?", p.PanelUserID).Error; err == nil {
			if s.servedUser(&u) {
				pn, err := s.subNodes(&u)
				if err != nil {
					return nil, info, err
				}
				nodes = append(nodes, pn...)
			}
			info.Upload, info.Download, info.Total, info.Expire = u.UsedUp, u.UsedDown, u.TrafficLimit, u.ExpireAt
		}
	}
	if len(p.SourceIDs.V) > 0 {
		var srcs []store.SubSource
		s.st.DB.Where("id IN ? AND enabled = ?", p.SourceIDs.V, true).Order("sort_order, created_at").Find(&srcs)
		for i := range srcs {
			nodes = append(nodes, s.sourceNodes(ctx, &srcs[i])...)
			ui := srcs[i].UserInfo.V
			info.Upload += ui.Upload
			info.Download += ui.Download
			info.Total += ui.Total
			if ui.Expire > 0 {
				t := time.Unix(ui.Expire, 0)
				if info.Expire == nil || t.Before(*info.Expire) {
					info.Expire = &t
				}
			}
		}
	}
	ops := p.Operators.V
	if p.UseGlobalOps {
		ops = append(s.globalOperators(), ops...)
	}
	out, err := sub.ApplyChain(nodes, ops)
	return out, info, err
}

func (s *Server) profileURL(token string) string {
	return s.subBase() + "/sub/p/" + token
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
	nodes = applyNodeOpts(s.decorateNodes(nodes, info, raw || sub.ReadsProfileHeaders(c.GetHeader("User-Agent"))), ro)
	var body []byte
	ctype := ""
	if cc.External && !raw && format != "links" {
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

// profileSubscription serves /sub/p/{token}.
func (s *Server) profileSubscription(c *gin.Context) {
	if !s.subEnabled() {
		c.String(http.StatusNotFound, "404 page not found")
		return
	}
	if !s.subLimit.Allow(clientIP(c.Request)) {
		c.String(http.StatusTooManyRequests, "too many requests")
		return
	}
	var p store.SubProfile
	if err := s.st.DB.First(&p, "token = ? AND enabled = ?", c.Param("token"), true).Error; err != nil || len(c.Param("token")) < 16 {
		c.String(http.StatusNotFound, "404 page not found")
		return
	}
	nodes, info, err := s.profileNodes(c.Request.Context(), &p)
	if err != nil {
		serverError(c, err)
		return
	}
	if c.Query("raw") != "1" {
		s.st.DB.Model(&p).UpdateColumn("downloads", p.Downloads+1)
	}
	var pu *store.User
	if p.PanelUserID != "" {
		var u store.User
		if s.st.DB.First(&u, "id = ?", p.PanelUserID).Error == nil {
			pu = &u
		}
	}
	s.serveNodes(c, nodes, info, p.Templates.V, s.converterFor(&p), s.renderOptsFor(&p), "/sub/p/"+p.Token, pu)
}

// refreshDueSources runs every minute.
func (s *Server) refreshDueSources() {
	var srcs []store.SubSource
	s.st.DB.Where("enabled = ? AND kind = ? AND refresh_min > 0", true, "url").Find(&srcs)
	for i := range srcs {
		src := &srcs[i]
		if src.FetchedAt != nil && time.Since(*src.FetchedAt) < time.Duration(src.RefreshMin)*time.Minute {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctxBG(), 45*time.Second)
		if _, err := s.fetchSource(ctx, src); err != nil {
			s.log.Warn("refresh subscription source", "source", src.Name, "err", err)
		}
		cancel()
	}
}

// --- API: sources ---

type sourceReq struct {
	Name       string `json:"name" binding:"required"`
	Kind       string `json:"kind" binding:"required"`
	URL        string `json:"url"`
	Content    string `json:"content"`
	UserAgent  string `json:"userAgent"`
	Group      string `json:"group"`
	Enabled    *bool  `json:"enabled"`
	RefreshMin int    `json:"refreshMin"`
	SortOrder  int    `json:"sortOrder"`
	Remark     string `json:"remark"`
}

func (s *Server) listSources(c *gin.Context) {
	var ss []store.SubSource
	s.st.DB.Order("sort_order, created_at").Find(&ss)
	c.JSON(http.StatusOK, ss)
}

func (s *Server) saveSource(c *gin.Context) {
	var req sourceReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Kind != "url" && req.Kind != "manual" {
		badRequest(c, fmt.Errorf("kind must be url or manual"))
		return
	}
	src := store.SubSource{ID: store.NewID(), Enabled: true, UserInfo: store.NewJSON(store.SubTraffic{})}
	if id := c.Param("id"); id != "" {
		if err := s.st.DB.First(&src, "id = ?", id).Error; err != nil {
			dbError(c, err)
			return
		}
	}
	src.Name, src.Kind, src.URL, src.Content, src.UserAgent, src.Group = req.Name, req.Kind, strings.TrimSpace(req.URL), req.Content, req.UserAgent, req.Group
	src.RefreshMin, src.SortOrder, src.Remark = req.RefreshMin, req.SortOrder, req.Remark
	if req.Enabled != nil {
		src.Enabled = *req.Enabled
	}
	src.Cache = "" // re-parse with the new definition
	if err := s.st.DB.Save(&src).Error; err != nil {
		dbError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 40*time.Second)
	defer cancel()
	_, ferr := s.fetchSource(ctx, &src)
	s.auditCtx(c, "sub_source.save", src.Name, src.Kind)
	resp := gin.H{"source": src}
	if ferr != nil {
		resp["warning"] = ferr.Error()
	}
	c.JSON(http.StatusOK, resp)
}

func (s *Server) deleteSource(c *gin.Context) {
	s.st.DB.Delete(&store.SubSource{}, "id = ?", c.Param("id"))
	s.auditCtx(c, "sub_source.delete", c.Param("id"), "")
	c.Status(http.StatusNoContent)
}

func (s *Server) refreshSource(c *gin.Context) {
	var src store.SubSource
	if err := s.st.DB.First(&src, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 40*time.Second)
	defer cancel()
	nodes, err := s.fetchSource(ctx, &src)
	if err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"nodeCount": len(nodes), "source": src})
}

// nodeRow is the preview shape of a node.
type nodeRow struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Server   string `json:"server"`
	Port     int    `json:"port"`
	Region   string `json:"region"`
	Source   string `json:"source"`
	Network  string `json:"network"`
	Security string `json:"security"`
}

func rows(nodes []sub.Node) []nodeRow {
	out := make([]nodeRow, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeRow{Name: n.Name, Protocol: string(n.Protocol), Server: n.Server, Port: n.Port, Region: sub.NodeRegion(n),
			Source: n.Source, Network: string(n.Transport.Type), Security: string(n.TLS.Type)})
	}
	return out
}

func (s *Server) sourceNodesAPI(c *gin.Context) {
	var src store.SubSource
	if err := s.st.DB.First(&src, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(http.StatusOK, rows(s.sourceNodes(c.Request.Context(), &src)))
}

// --- API: profiles ---

type profileReq struct {
	Name          string            `json:"name" binding:"required"`
	Enabled       *bool             `json:"enabled"`
	SourceIDs     []string          `json:"sourceIds"`
	PanelUserID   string            `json:"panelUserId"`
	Operators     []sub.Operator    `json:"operators"`
	UseGlobalOps  bool              `json:"useGlobalOps"`
	Templates     map[string]string `json:"templates"`
	Converter     string            `json:"converter"`
	Backend       string            `json:"backend"`
	BackendConfig string            `json:"backendConfig"`
	Scheme        string            `json:"scheme"`
	SkipCert      *bool             `json:"skipCert"`
	ForceUDP      *bool             `json:"forceUdp"`
	Remark        string            `json:"remark"`
}

func (r *profileReq) apply(p *store.SubProfile) {
	p.Name, p.SourceIDs, p.PanelUserID = r.Name, store.NewJSON(nonNilStrs(r.SourceIDs)), r.PanelUserID
	if r.Operators == nil {
		r.Operators = []sub.Operator{}
	}
	if r.Templates == nil {
		r.Templates = map[string]string{}
	}
	p.Operators, p.UseGlobalOps, p.Templates = store.NewJSON(r.Operators), r.UseGlobalOps, store.NewJSON(r.Templates)
	p.Converter, p.Backend, p.BackendConfig, p.Remark = r.Converter, r.Backend, r.BackendConfig, r.Remark
	p.Scheme, p.SkipCert, p.ForceUDP = strings.TrimSpace(r.Scheme), r.SkipCert, r.ForceUDP
	if r.Enabled != nil {
		p.Enabled = *r.Enabled
	}
}

func (s *Server) listProfiles(c *gin.Context) {
	var ps []store.SubProfile
	s.st.DB.Order("name").Find(&ps)
	type view struct {
		store.SubProfile
		URL string `json:"url"`
	}
	out := make([]view, 0, len(ps))
	for _, p := range ps {
		out = append(out, view{SubProfile: p, URL: s.profileURL(p.Token)})
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) saveProfile(c *gin.Context) {
	var req profileReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := sub.ValidateChain(req.Operators); err != nil {
		badRequest(c, err)
		return
	}
	p := store.SubProfile{ID: store.NewID(), Token: pki.Token(24), Enabled: true}
	if id := c.Param("id"); id != "" {
		if err := s.st.DB.First(&p, "id = ?", id).Error; err != nil {
			dbError(c, err)
			return
		}
	}
	req.apply(&p)
	if err := s.st.DB.Save(&p).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "sub_profile.save", p.Name, "")
	c.JSON(http.StatusOK, p)
}

func (s *Server) deleteProfile(c *gin.Context) {
	s.st.DB.Delete(&store.SubProfile{}, "id = ?", c.Param("id"))
	s.auditCtx(c, "sub_profile.delete", c.Param("id"), "")
	c.Status(http.StatusNoContent)
}

func (s *Server) resetProfileToken(c *gin.Context) {
	tok := pki.Token(24)
	if res := s.st.DB.Model(&store.SubProfile{}).Where("id = ?", c.Param("id")).Update("token", tok); res.RowsAffected == 0 {
		notFound(c)
		return
	}
	s.auditCtx(c, "sub_profile.reset_token", c.Param("id"), "")
	c.JSON(http.StatusOK, gin.H{"token": tok, "url": s.profileURL(tok)})
}

// previewProfile runs a draft profile and returns the processed node list.
func (s *Server) previewProfile(c *gin.Context) {
	var req profileReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var p store.SubProfile
	req.apply(&p)
	nodes, info, err := s.profileNodes(c.Request.Context(), &p)
	if err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"nodes": rows(nodes), "info": info})
}

// subMeta lists formats, regions and operator types for the UI.
func (s *Server) subMeta(c *gin.Context) {
	type region struct{ Code, Name string }
	var rs []region
	for _, r := range sub.Regions() {
		rs = append(rs, region{r.Code, sub.Flag(r.Code) + " " + r.Zh})
	}
	c.JSON(http.StatusOK, gin.H{"formats": sub.Formats(), "regions": rs, "schemes": sub.Presets()})
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
