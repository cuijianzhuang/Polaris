package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/plugin"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Plugins are outside services that extend the panel. A plugin serves a
// manifest; installing it gives the plugin its own API token and event
// webhook, and the panel talks to it with signed requests:
//
//	GET  {base}/polaris-plugin.json   manifest
//	POST {base}/polaris/install       credentials handshake (optional)
//	POST {base}/polaris/config        configuration push (optional)
//	GET  {base}/polaris/health        liveness (optional)
//	POST {base}/polaris/uninstall     goodbye (optional)
//
// The community edition may install a few (license.CommunityPlugins); a
// license lifts or sets the cap.

const (
	manifestPath      = "/polaris-plugin.json"
	defaultWebhookURL = "/polaris/events"
	pluginPageTTL     = 5 * time.Minute
	pluginPortalTTL   = 10 * time.Minute
)

var pluginClient = &http.Client{
	Timeout: 6 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || req.URL.Host != via[0].URL.Host {
			return http.ErrUseLastResponse // a plugin must not bounce the panel to another host
		}
		return nil
	},
}

// pluginBase normalises the address an admin typed: http(s), a host, an
// optional path prefix, no credentials, query or fragment.
func pluginBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("插件地址应为 http(s):// 开头的完整网址")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("插件地址不能带账号、查询参数或 #")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// fetchManifest reads and validates the manifest of the plugin at base.
func (s *Server) fetchManifest(ctx context.Context, base string) (*plugin.Manifest, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+manifestPath, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Polaris/"+Version)
	resp, err := pluginClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("连不上插件：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("插件没有提供描述文件（%s 返回 HTTP %d）", manifestPath, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, plugin.MaxManifestBytes+1))
	if err != nil {
		return nil, nil, err
	}
	m, err := plugin.Parse(data, allScopes, checkEvents)
	if err != nil {
		return nil, nil, err
	}
	if m.MinPanel != "" && !plugin.AtLeast(Version, m.MinPanel) {
		return nil, nil, fmt.Errorf("这个插件需要面板 %s 或更高版本（当前 %s）", m.MinPanel, Version)
	}
	return m, data, nil
}

// callPlugin sends a signed request to a plugin. body may be nil.
func (s *Server) callPlugin(ctx context.Context, p *store.Plugin, method, path string, body any) (int, []byte, error) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	secret, _ := s.st.Box.Open(p.Secret)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, method, p.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Polaris/"+Version)
	req.Header.Set("X-Polaris-Signature", "t="+ts+",v1="+signWebhook(secret, ts, raw))
	resp, err := pluginClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, out, nil
}

func pluginManifestOf(p *store.Plugin) *plugin.Manifest {
	var m plugin.Manifest
	if json.Unmarshal([]byte(p.Manifest), &m) != nil {
		return &plugin.Manifest{ID: p.ID, Name: p.Name, Version: p.Version}
	}
	return &m
}

// pluginSettings returns what an admin may see (secrets omitted), which
// secrets are set, and every value in the clear (for the plugin itself).
func (s *Server) pluginSettings(p *store.Plugin, m *plugin.Manifest) (shown map[string]string, set map[string]bool, plain map[string]string) {
	stored := map[string]string{}
	_ = json.Unmarshal([]byte(p.Settings), &stored)
	shown, set, plain = map[string]string{}, map[string]bool{}, map[string]string{}
	for _, f := range m.Settings {
		v, ok := stored[f.Key]
		if !ok {
			if f.Default != "" {
				shown[f.Key], plain[f.Key] = f.Default, f.Default
			}
			continue
		}
		if f.Type == "secret" {
			open, _ := s.st.Box.Open(v)
			if open != "" {
				set[f.Key], plain[f.Key] = true, open
			}
			continue
		}
		shown[f.Key], plain[f.Key] = v, v
	}
	return
}

type pluginView struct {
	store.Plugin
	Manifest  *plugin.Manifest  `json:"manifest"`
	Values    map[string]string `json:"values"`
	HasSecret map[string]bool   `json:"hasSecret"`
}

func (s *Server) pluginView(p *store.Plugin) pluginView {
	m := pluginManifestOf(p)
	shown, set, _ := s.pluginSettings(p, m)
	return pluginView{Plugin: *p, Manifest: m, Values: shown, HasSecret: set}
}

func (s *Server) pluginAPIBase() string {
	return s.panelBase() + strings.TrimRight(s.cfg.AdminPath, "/") + "/api/v1"
}

// --- API ---

// listPlugins is GET /plugins.
func (s *Server) listPlugins(c *gin.Context) {
	var ps []store.Plugin
	s.st.DB.Order("created_at").Find(&ps)
	out := make([]pluginView, len(ps))
	for i := range ps {
		out[i] = s.pluginView(&ps[i])
	}
	order := []string{}
	for _, p := range s.transformPlugins() {
		order = append(order, p.ID)
	}
	c.JSON(http.StatusOK, gin.H{"plugins": out, "limit": s.license().Limit(license.LimitPlugins), "used": len(ps),
		"scopes": allScopes, "events": EventTypes, "transformOrder": order})
}

// setTransformOrder is PUT /plugin-transform-order {ids}: the order in which
// rewriting plugins see each subscription.
func (s *Server) setTransformOrder(c *gin.Context) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	known := map[string]bool{}
	for _, p := range s.transformPlugins() {
		known[p.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range req.IDs {
		if !known[id] || seen[id] {
			badRequest(c, fmt.Errorf("%q 不是已启用的订阅改写插件，或重复", id))
			return
		}
		seen[id] = true
	}
	b, _ := json.Marshal(req.IDs)
	if err := s.st.SetSetting(transformOrderSetting, string(b)); err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "plugin.transform_order", "", strings.Join(req.IDs, " → "))
	c.JSON(http.StatusOK, gin.H{"transformOrder": req.IDs})
}

// previewPlugin is POST /plugins/preview {baseUrl}: what installing would do.
func (s *Server) previewPlugin(c *gin.Context) {
	var req struct {
		BaseURL string `json:"baseUrl" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	base, err := pluginBase(req.BaseURL)
	if err != nil {
		badRequest(c, err)
		return
	}
	m, _, err := s.fetchManifest(c.Request.Context(), base)
	if err != nil {
		badRequest(c, err)
		return
	}
	var n int64
	s.st.DB.Model(&store.Plugin{}).Where("id = ?", m.ID).Count(&n)
	var used int64
	s.st.DB.Model(&store.Plugin{}).Count(&used)
	c.JSON(http.StatusOK, gin.H{"baseUrl": base, "manifest": m, "installed": n > 0,
		"limit": s.license().Limit(license.LimitPlugins), "used": used})
}

// installPlugin is POST /plugins {baseUrl, installKey}.
func (s *Server) installPlugin(c *gin.Context) {
	var req struct {
		BaseURL    string `json:"baseUrl" binding:"required"`
		InstallKey string `json:"installKey"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	base, err := pluginBase(req.BaseURL)
	if err != nil {
		badRequest(c, err)
		return
	}
	if !s.limitOK(c, license.LimitPlugins, 1) {
		return
	}
	m, raw, err := s.fetchManifest(c.Request.Context(), base)
	if err != nil {
		badRequest(c, err)
		return
	}
	var dup int64
	s.st.DB.Model(&store.Plugin{}).Where("id = ?", m.ID).Count(&dup)
	if dup > 0 {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "这个插件已经安装"})
		return
	}
	if err := s.checkPluginFormats(m, m.ID); err != nil {
		badRequest(c, err)
		return
	}

	by := c.GetString("admin")
	tok, tokSecret, err := s.issueAPIToken("插件 "+m.Name, m.ID, m.Scopes, 0, by)
	if err != nil {
		badRequest(c, err)
		return
	}
	rollback := func() {
		s.st.DB.Delete(&store.APIToken{}, "id = ?", tok.ID)
	}
	p := &store.Plugin{ID: m.ID, Name: m.Name, Version: m.Version, BaseURL: base, Manifest: string(raw), Enabled: true,
		Status: "manual", TokenID: tok.ID}
	pluginSecret := "plsec_" + pki.Token(32)
	p.Secret = s.st.Box.Seal(pluginSecret)

	hookSecret := ""
	if len(m.Events) > 0 {
		path := m.WebhookPath
		if path == "" {
			path = defaultWebhookURL
		}
		h, sec := s.newWebhook(webhookReq{Name: m.Name, URL: base + path, Format: "json", Events: m.Events}, m.ID)
		if err := s.st.DB.Create(h).Error; err != nil {
			rollback()
			dbError(c, err)
			return
		}
		p.WebhookID, hookSecret = h.ID, sec
	}
	if err := s.st.DB.Create(p).Error; err != nil {
		rollback()
		s.st.DB.Delete(&store.Webhook{}, "id = ?", p.WebhookID)
		dbError(c, err)
		return
	}

	creds := gin.H{"apiBase": s.pluginAPIBase(), "panelUrl": s.panelBase(), "token": tokSecret, "pluginSecret": pluginSecret,
		"webhookSecret": hookSecret, "panelVersion": Version}
	_, _, plain := s.pluginSettings(p, m)
	payload := gin.H{"panel_url": s.panelBase(), "api_base": creds["apiBase"], "token": tokSecret, "plugin_secret": pluginSecret,
		"webhook_secret": hookSecret, "panel_version": Version, "settings": plain}
	handshake, detail := s.handshake(c.Request.Context(), p, req.InstallKey, payload)
	s.st.DB.Model(p).Updates(map[string]any{"status": p.Status, "last_error": p.LastError, "last_seen_at": p.LastSeenAt})
	s.auditCtx(c, "plugin.install", m.ID, m.Name+" "+m.Version+"; "+strings.Join(m.Scopes, ","))
	out := gin.H{"plugin": s.pluginView(p), "handshake": handshake}
	if handshake != "ok" {
		out["credentials"], out["detail"] = creds, detail // shown once, for pasting into the plugin
	}
	c.JSON(http.StatusCreated, out)
}

// handshake gives a fresh plugin its credentials. It reports "ok" when the
// plugin accepted them, else "manual" (the admin pastes them).
func (s *Server) handshake(ctx context.Context, p *store.Plugin, installKey string, payload gin.H) (string, string) {
	raw, _ := json.Marshal(payload)
	secret, _ := s.st.Box.Open(p.Secret)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/polaris/install", bytes.NewReader(raw))
	if err != nil {
		return "manual", err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Polaris-Signature", "t="+ts+",v1="+signWebhook(secret, ts, raw))
	if installKey != "" {
		req.Header.Set("X-Polaris-Install-Key", installKey)
	}
	resp, err := pluginClient.Do(req)
	if err != nil {
		p.LastError = truncate(err.Error(), 500)
		return "manual", "插件没有应答安装请求：" + err.Error()
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		p.LastError = truncate(fmt.Sprintf("安装请求返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(msg))), 500)
		return "manual", p.LastError
	}
	now := time.Now()
	p.Status, p.LastError, p.LastSeenAt = "online", "", &now
	return "ok", ""
}

func (s *Server) loadPlugin(c *gin.Context) *store.Plugin {
	var p store.Plugin
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return nil
	}
	return &p
}

func (s *Server) getPlugin(c *gin.Context) {
	if p := s.loadPlugin(c); p != nil {
		c.JSON(http.StatusOK, s.pluginView(p))
	}
}

// updatePlugin is PUT /plugins/:id {enabled}.
func (s *Server) updatePlugin(c *gin.Context) {
	p := s.loadPlugin(c)
	if p == nil {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.st.DB.Model(p).Update("enabled", req.Enabled)
	if p.WebhookID != "" {
		s.st.DB.Model(&store.Webhook{}).Where("id = ?", p.WebhookID).Update("enabled", req.Enabled)
	}
	p.Enabled = req.Enabled
	s.auditCtx(c, map[bool]string{true: "plugin.enable", false: "plugin.disable"}[req.Enabled], p.ID, p.Name)
	c.JSON(http.StatusOK, s.pluginView(p))
}

// updatePluginSettings is PUT /plugins/:id/settings {values}: validated
// against the manifest, secrets sealed, then pushed to the plugin.
func (s *Server) updatePluginSettings(c *gin.Context) {
	p := s.loadPlugin(c)
	if p == nil {
		return
	}
	var req struct {
		Values map[string]string `json:"values"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	m := pluginManifestOf(p)
	stored := map[string]string{}
	_ = json.Unmarshal([]byte(p.Settings), &stored)
	have := map[string]bool{}
	for _, f := range m.Settings {
		if f.Type == "secret" && stored[f.Key] != "" {
			have[f.Key] = true
		}
	}
	if err := m.CheckValues(req.Values, have); err != nil {
		badRequest(c, err)
		return
	}
	next := map[string]string{}
	for _, f := range m.Settings {
		v, given := req.Values[f.Key]
		v = strings.TrimSpace(v)
		switch {
		case f.Type == "secret" && (!given || v == ""):
			if old := stored[f.Key]; old != "" {
				next[f.Key] = old
			}
		case v != "":
			if f.Type == "secret" {
				v = s.st.Box.Seal(v)
			}
			next[f.Key] = v
		}
	}
	b, _ := json.Marshal(next)
	s.st.DB.Model(p).Update("settings", string(b))
	p.Settings = string(b)
	s.auditCtx(c, "plugin.settings", p.ID, "")
	_, _, plain := s.pluginSettings(p, m)
	pushed, pushErr := true, ""
	if p.Enabled {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 6*time.Second)
		defer cancel()
		if code, body, err := s.callPlugin(ctx, p, http.MethodPost, "/polaris/config", gin.H{"settings": plain}); err != nil {
			pushed, pushErr = false, err.Error()
		} else if code < 200 || code > 299 {
			pushed, pushErr = false, fmt.Sprintf("插件返回 HTTP %d：%s", code, strings.TrimSpace(string(body)))
		}
	}
	c.JSON(http.StatusOK, gin.H{"plugin": s.pluginView(p), "pushed": pushed, "pushError": pushErr})
}

// refreshPlugin is POST /plugins/:id/refresh {approve}: reads the manifest
// again after the plugin was upgraded. Asking for more scopes or events needs
// the admin's approval.
func (s *Server) refreshPlugin(c *gin.Context) {
	p := s.loadPlugin(c)
	if p == nil {
		return
	}
	var req struct {
		Approve bool `json:"approve"`
	}
	_ = c.ShouldBindJSON(&req)
	m, raw, err := s.fetchManifest(c.Request.Context(), p.BaseURL)
	if err != nil {
		badRequest(c, err)
		return
	}
	if m.ID != p.ID {
		badRequest(c, fmt.Errorf("描述文件的 id 变成了 %q，应为 %q", m.ID, p.ID))
		return
	}
	old := pluginManifestOf(p)
	var addScopes, addEvents []string
	for _, sc := range m.Scopes {
		if !slices.Contains(old.Scopes, sc) {
			addScopes = append(addScopes, sc)
		}
	}
	for _, e := range m.Events {
		if !slices.Contains(old.Events, e) {
			addEvents = append(addEvents, e)
		}
	}
	if err := s.checkPluginFormats(m, p.ID); err != nil {
		badRequest(c, err)
		return
	}
	var addFormats []string
	for _, f := range m.FormatNames() {
		if !slices.Contains(old.FormatNames(), f) {
			addFormats = append(addFormats, f)
		}
	}
	addTransform := m.Transforms() && !old.Transforms()
	if (len(addScopes) > 0 || len(addEvents) > 0 || len(addFormats) > 0 || addTransform) && !req.Approve {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "新版本需要更多权限，请确认后再更新", "needApproval": true,
			"addScopes": addScopes, "addEvents": addEvents, "addFormats": addFormats, "addTransform": addTransform, "manifest": m})
		return
	}
	s.st.DB.Model(&store.APIToken{}).Where("id = ?", p.TokenID).Update("scopes", store.JSON[[]string]{V: m.Scopes})
	if p.WebhookID != "" {
		s.st.DB.Model(&store.Webhook{}).Where("id = ?", p.WebhookID).Update("events", store.JSON[[]string]{V: m.Events})
	}
	s.st.DB.Model(p).Updates(map[string]any{"name": m.Name, "version": m.Version, "manifest": string(raw)})
	p.Name, p.Version, p.Manifest = m.Name, m.Version, string(raw)
	s.auditCtx(c, "plugin.refresh", p.ID, old.Version+" → "+m.Version)
	c.JSON(http.StatusOK, s.pluginView(p))
}

// deletePlugin is DELETE /plugins/:id: the token and webhook go with it.
func (s *Server) deletePlugin(c *gin.Context) {
	p := s.loadPlugin(c)
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 4*time.Second)
	defer cancel()
	_, _, _ = s.callPlugin(ctx, p, http.MethodPost, "/polaris/uninstall", gin.H{"id": p.ID}) // best effort
	s.st.DB.Delete(&store.APIToken{}, "id = ?", p.TokenID)
	if p.WebhookID != "" {
		s.st.DB.Where("webhook_id = ?", p.WebhookID).Delete(&store.WebhookDelivery{})
		s.st.DB.Delete(&store.Webhook{}, "id = ?", p.WebhookID)
	}
	s.st.DB.Delete(p)
	s.auditCtx(c, "plugin.uninstall", p.ID, p.Name)
	c.Status(http.StatusNoContent)
}

// pluginOwnConfig is GET /plugin/config for a plugin's own token: its
// settings in the clear, so it can read them after a restart.
func (s *Server) pluginOwnConfig(c *gin.Context) {
	t := apiTokenOf(c)
	if t == nil || t.PluginID == "" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "只有插件自己的令牌可以读取插件配置"})
		return
	}
	var p store.Plugin
	if s.st.DB.First(&p, "id = ?", t.PluginID).Error != nil {
		notFound(c)
		return
	}
	_, _, plain := s.pluginSettings(&p, pluginManifestOf(&p))
	c.JSON(http.StatusOK, gin.H{"id": p.ID, "version": p.Version, "settings": plain})
}

// --- identity tokens for embedded pages and user-center links ---

// pluginToken signs a short-lived identity for a plugin to verify with the
// secret it received at install: scope "admin" for embedded admin pages,
// "user" for user-center links.
func (s *Server) pluginToken(p *store.Plugin, scope, subject, name string, ttl time.Duration) (string, error) {
	secret, err := s.st.Box.Open(p.Secret)
	if err != nil || secret == "" {
		return "", errors.New("插件密钥不可用")
	}
	now := time.Now()
	claims := jwt.MapClaims{"iss": "polaris", "aud": p.ID, "sub": subject, "name": name, "scope": scope,
		"jti": store.NewID(), "iat": now.Unix(), "exp": now.Add(ttl).Unix()}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

// pluginPageURL is GET /plugins/:id/pages/:page: the address the panel
// embeds, with a five-minute identity token.
func (s *Server) pluginPageURL(c *gin.Context) {
	p := s.loadPlugin(c)
	if p == nil {
		return
	}
	if !p.Enabled {
		badRequest(c, errors.New("插件已停用"))
		return
	}
	var page *plugin.Page
	for _, pg := range pluginManifestOf(p).Pages {
		if pg.ID == c.Param("page") {
			pg := pg
			page = &pg
		}
	}
	if page == nil {
		notFound(c)
		return
	}
	tok, err := s.pluginToken(p, "admin", c.GetString("admin"), c.GetString("admin"), pluginPageTTL)
	if err != nil {
		serverError(c, err)
		return
	}
	q := url.Values{"polaris_token": {tok}, "panel": {s.pluginAPIBase()}, "lang": {c.Query("lang")}}
	c.JSON(http.StatusOK, gin.H{"url": p.BaseURL + page.Path + "?" + q.Encode(), "title": page.Title})
}

// pluginPortalLinks returns the enabled plugins' user-center links for u,
// each carrying a ten-minute identity token.
func (s *Server) pluginPortalLinks(u *store.User) []portalLink {
	var ps []store.Plugin
	s.st.DB.Where("enabled = ?", true).Order("created_at").Find(&ps)
	var out []portalLink
	for i := range ps {
		for _, l := range pluginManifestOf(&ps[i]).Portal {
			tok, err := s.pluginToken(&ps[i], "user", u.ID, u.Username, pluginPortalTTL)
			if err != nil {
				continue
			}
			q := url.Values{"polaris_token": {tok}}
			out = append(out, portalLink{Title: l.Title, URL: ps[i].BaseURL + l.Path + "?" + q.Encode()})
		}
	}
	return out
}

type portalLink struct{ Title, URL string }

// --- health ---

// checkPlugins asks every enabled plugin whether it is alive and reports
// changes.
func (s *Server) checkPlugins() {
	var ps []store.Plugin
	s.st.DB.Where("enabled = ?", true).Find(&ps)
	var wg sync.WaitGroup
	for i := range ps {
		wg.Add(1)
		go func(p *store.Plugin) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			code, _, err := s.callPlugin(ctx, p, http.MethodGet, "/polaris/health", nil)
			ok := err == nil && code >= 200 && code < 300
			now := time.Now()
			upd := map[string]any{}
			switch {
			case ok:
				upd["status"], upd["last_error"], upd["last_seen_at"] = "online", "", now
				if p.Status == "offline" {
					s.notify(Event{Kind: "plugin_online", ResourceID: p.ID, Text: "✅ 插件 " + p.Name + " 已恢复", Data: map[string]any{"plugin": p.ID}})
				}
			default:
				msg := "HTTP " + strconv.Itoa(code)
				if err != nil {
					msg = err.Error()
				}
				if p.Status == "manual" && p.LastSeenAt == nil {
					// Never seen alive: stay "manual" until the plugin answers.
					upd["last_error"] = truncate(msg, 500)
					break
				}
				upd["status"], upd["last_error"] = "offline", truncate(msg, 500)
				if p.Status == "online" {
					s.notify(Event{Kind: "plugin_offline", ResourceID: p.ID, Text: "⚠️ 插件 " + p.Name + " 无法访问：" + truncate(msg, 200), Data: map[string]any{"plugin": p.ID}})
				}
			}
			s.st.DB.Model(&store.Plugin{}).Where("id = ?", p.ID).Updates(upd)
		}(&ps[i])
	}
	wg.Wait()
}
