package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cuijianzhuang/polaris/internal/plugin"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Plugin subscription formats: a plugin may add client formats it renders
// itself. ?format=<name> on a subscription then posts the user's nodes to
// {base}/polaris/render (signed like every plugin call) and returns the
// plugin's answer, within a time and size budget and only with the content
// type its manifest declared. Built-in formats can never be replaced; when
// the plugin is disabled or removed its formats go with it.

const (
	pluginRenderTimeout = 3 * time.Second
	pluginRenderMax     = 4 << 20
	pluginRenderTTL     = 60 * time.Second
	pluginRenderEntries = 2000
	pluginRenderBytes   = 32 << 20
)

var errPluginFormat = errors.New("插件订阅格式暂不可用")

// Bound response buffers as well as retained cache bodies. Waiting consumes
// the existing request deadline, so overloaded plugins cannot pile up work.
var pluginSubSlots = make(chan struct{}, 8)

func acquirePluginSub(ctx context.Context) (func(), error) {
	select {
	case pluginSubSlots <- struct{}{}:
		return func() { <-pluginSubSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// reservedFormats are names a plugin may not take.
func reservedFormats() []string {
	return append(sub.Formats(), "html", "auto", "raw")
}

// checkPluginFormats rejects formats that clash with built-ins or with
// another installed plugin.
func (s *Server) checkPluginFormats(m *plugin.Manifest, self string) error {
	names := m.FormatNames()
	if len(names) == 0 {
		return nil
	}
	reserved := reservedFormats()
	var others []store.Plugin
	s.st.DB.Where("id <> ?", self).Find(&others)
	for _, n := range names {
		if slices.Contains(reserved, n) {
			return fmt.Errorf("订阅格式 %q 是面板内置格式，插件不能占用", n)
		}
		for i := range others {
			if slices.Contains(pluginManifestOf(&others[i]).FormatNames(), n) {
				return fmt.Errorf("订阅格式 %q 已被插件「%s」使用", n, others[i].Name)
			}
		}
	}
	return nil
}

// pluginFormat finds the enabled plugin serving a format.
func (s *Server) pluginFormat(name string) (*store.Plugin, plugin.SubFormat, bool) {
	if slices.Contains(reservedFormats(), name) {
		return nil, plugin.SubFormat{}, false
	}
	var ps []store.Plugin
	s.st.DB.Where("enabled = ?", true).Find(&ps)
	for i := range ps {
		m := pluginManifestOf(&ps[i])
		if m.Subscription == nil {
			continue
		}
		for _, f := range m.Subscription.Formats {
			if f.Name == name {
				return &ps[i], f, true
			}
		}
	}
	return nil, plugin.SubFormat{}, false
}

// pluginFormatNames lists the formats enabled plugins serve.
func (s *Server) pluginFormatNames() []string {
	var out []string
	var ps []store.Plugin
	s.st.DB.Where("enabled = ?", true).Find(&ps)
	for i := range ps {
		out = append(out, pluginManifestOf(&ps[i]).FormatNames()...)
	}
	return out
}

// renderRequest is what /polaris/render receives.
type renderRequest struct {
	Format string     `json:"format"`
	User   renderUser `json:"user"`
	Nodes  []sub.Node `json:"nodes"`
}

type renderUser struct {
	Username    string     `json:"username"`
	Title       string     `json:"title"`
	Upload      int64      `json:"upload"`
	Download    int64      `json:"download"`
	Total       int64      `json:"total"` // 0 = unlimited
	Expire      *time.Time `json:"expire,omitempty"`
	UpdateHours int        `json:"updateHours,omitempty"`
}

// renderCache keeps plugin output briefly, per user and input.
type renderCache struct {
	mu    sync.Mutex
	m     map[string]renderEntry
	bytes int
}

type renderEntry struct {
	body  []byte
	ctype string
	until time.Time
	used  time.Time
}

func (c *renderCache) get(key string, now time.Time) (renderEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if ok && !now.Before(e.until) {
		c.remove(key)
		ok = false
	}
	if ok {
		e.used = now
		c.m[key] = e
	}
	return e, ok
}

func (c *renderCache) put(key string, e renderEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]renderEntry{}
	}
	c.expire(now)
	c.remove(key)
	if len(e.body) > pluginRenderBytes || !now.Before(e.until) {
		return
	}
	for len(c.m) >= pluginRenderEntries || c.bytes+len(e.body) > pluginRenderBytes {
		var oldest string
		var at time.Time
		for k, v := range c.m {
			if at.IsZero() || v.used.Before(at) {
				oldest, at = k, v.used
			}
		}
		c.remove(oldest)
	}
	e.used = now
	c.m[key] = e
	c.bytes += len(e.body)
}

// remove/expire require mu. Bodies disappear even when a key is never read again.
func (c *renderCache) remove(key string) {
	if e, ok := c.m[key]; ok {
		c.bytes -= len(e.body)
		delete(c.m, key)
	}
}

func (c *renderCache) expire(now time.Time) {
	for k, e := range c.m {
		if !now.Before(e.until) {
			c.remove(k)
		}
	}
}

func (c *renderCache) prune() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expire(time.Now())
}

// renderPluginFormat asks the plugin for the subscription body. The cache key
// covers the plugin's installation and version, the format, the user and the
// exact nodes sent, so any change to access or nodes is a miss; usage figures
// may lag by up to a minute.
func (s *Server) renderPluginFormat(ctx context.Context, p *store.Plugin, f plugin.SubFormat, nodes []sub.Node, info sub.Info, u *store.User) ([]byte, string, error) {
	req := renderRequest{Format: f.Name, Nodes: nodes, User: renderUser{Username: info.Username, Title: info.Title,
		Upload: info.Upload, Download: info.Download, Total: info.Total, Expire: info.Expire, UpdateHours: info.UpdateHours}}
	nodeJSON, _ := json.Marshal(nodes)
	h := sha256.New()
	userID := ""
	if u != nil {
		userID = u.ID + "\x00" + u.SubToken
	}
	fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s\x00%s\x00", p.ID, p.Version, p.UpdatedAt.UnixNano(), f.Name, userID)
	h.Write(nodeJSON)
	key := hex.EncodeToString(h.Sum(nil))
	now := time.Now()
	if e, ok := s.renderCache.get(key, now); ok {
		return e.body, e.ctype, nil
	}

	ctx, cancel := context.WithTimeout(ctx, pluginRenderTimeout)
	defer cancel()
	release, err := acquirePluginSub(ctx)
	if err != nil {
		return nil, "", err
	}
	defer release()
	raw, _ := json.Marshal(req)
	if len(raw) > 8<<20 {
		return nil, "", errors.New("plugin input exceeds 8 MiB")
	}
	secret, _ := s.st.Box.Open(p.Secret)
	ts := strconv.FormatInt(now.Unix(), 10)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/polaris/render", bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("User-Agent", "Polaris/"+Version)
	hr.Header.Set("X-Polaris-Signature", "t="+ts+",v1="+signWebhook(secret, ts, raw))
	resp, err := pluginClient.Do(hr)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("plugin answered %s", resp.Status)
	}
	mt, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mt != f.ContentType {
		return nil, "", fmt.Errorf("plugin answered %q, not the declared %s", resp.Header.Get("Content-Type"), f.ContentType)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, pluginRenderMax+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > pluginRenderMax {
		return nil, "", errors.New("plugin output exceeds 4 MiB")
	}
	ctype := mt
	if cs := params["charset"]; cs != "" {
		ctype = mime.FormatMediaType(mt, map[string]string{"charset": cs})
	}
	s.renderCache.put(key, renderEntry{body: body, ctype: ctype, until: now.Add(pluginRenderTTL)}, now)
	return body, ctype, nil
}

// --- node list rewriting ---

const (
	pluginTransformTimeout = 2 * time.Second
	pluginTransformBudget  = 4 * time.Second
	transformOrderSetting  = "plugin_transform_order"
)

// transformNode is what a rewriting plugin sees: no credentials.
type transformNode struct {
	ID       string `json:"id"` // position in the list the panel sent
	Name     string `json:"name"`
	Region   string `json:"region,omitempty"`
	Source   string `json:"source,omitempty"`
	Protocol string `json:"protocol"`
	Server   string `json:"server"`
	Port     int    `json:"port"`
}

type transformRequest struct {
	Format string          `json:"format"`
	User   string          `json:"user"`
	Nodes  []transformNode `json:"nodes"`
}

type transformReply struct {
	Nodes *[]struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"nodes"`
}

// transformPlugins are the enabled rewriting plugins in the admin's order;
// ones not yet placed come last, by id.
func (s *Server) transformPlugins() []store.Plugin {
	var ps []store.Plugin
	s.st.DB.Where("enabled = ?", true).Order("id").Find(&ps)
	var list []store.Plugin
	for _, p := range ps {
		if pluginManifestOf(&p).Transforms() {
			list = append(list, p)
		}
	}
	var order []string
	_ = json.Unmarshal([]byte(s.st.GetSetting(transformOrderSetting, "[]")), &order)
	rank := func(id string) int {
		if i := slices.Index(order, id); i >= 0 {
			return i
		}
		return len(order)
	}
	slices.SortStableFunc(list, func(a, b store.Plugin) int { return rank(a.ID) - rank(b.ID) })
	return list
}

// applyTransforms runs the rewriting plugins in order. Each may only drop,
// rename and reorder the nodes it was given; a failure, a timeout or an
// answer that names an unknown node leaves the list as it was.
func (s *Server) applyTransforms(ctx context.Context, nodes []sub.Node, format string, u *store.User) []sub.Node {
	ps := s.transformPlugins()
	if len(ps) == 0 || len(nodes) == 0 {
		return nodes
	}
	ctx, cancel := context.WithTimeout(ctx, pluginTransformBudget)
	defer cancel()
	user := ""
	if u != nil {
		user = u.Username
	}
	for i := range ps {
		if ctx.Err() != nil {
			break
		}
		out, err := s.transformOnce(ctx, &ps[i], nodes, format, user)
		if err != nil {
			s.log.Warn("plugin node rewrite skipped", "plugin", ps[i].ID, "err", err)
			continue
		}
		nodes = out
	}
	return nodes
}

func (s *Server) transformOnce(ctx context.Context, p *store.Plugin, nodes []sub.Node, format, user string) ([]sub.Node, error) {
	req := transformRequest{Format: format, User: user}
	for i, n := range nodes {
		req.Nodes = append(req.Nodes, transformNode{ID: strconv.Itoa(i), Name: n.Name, Region: n.Region, Source: n.Source,
			Protocol: string(n.Protocol), Server: n.Server, Port: n.Port})
	}
	raw, _ := json.Marshal(req)
	if len(raw) > 8<<20 {
		return nil, errors.New("plugin input exceeds 8 MiB")
	}
	h := sha256.New()
	fmt.Fprintf(h, "t\x00%s\x00%s\x00%d\x00", p.ID, p.Version, p.UpdatedAt.UnixNano())
	h.Write(raw)
	key := hex.EncodeToString(h.Sum(nil))
	now := time.Now()
	var body []byte
	if e, ok := s.renderCache.get(key, now); ok {
		body = e.body
	} else {
		ctx, cancel := context.WithTimeout(ctx, pluginTransformTimeout)
		defer cancel()
		release, err := acquirePluginSub(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		secret, _ := s.st.Box.Open(p.Secret)
		ts := strconv.FormatInt(now.Unix(), 10)
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/polaris/transform", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		hr.Header.Set("Content-Type", "application/json")
		hr.Header.Set("User-Agent", "Polaris/"+Version)
		hr.Header.Set("X-Polaris-Signature", "t="+ts+",v1="+signWebhook(secret, ts, raw))
		resp, err := pluginClient.Do(hr)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("plugin answered %s", resp.Status)
		}
		if body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20)); err != nil {
			return nil, err
		}
	}
	var rep transformReply
	if err := json.Unmarshal(body, &rep); err != nil || rep.Nodes == nil {
		return nil, errors.New("plugin answer is not {\"nodes\": [...]}")
	}
	seen := map[int]bool{}
	out := make([]sub.Node, 0, len(*rep.Nodes))
	for _, r := range *rep.Nodes {
		i, err := strconv.Atoi(r.ID)
		if err != nil || i < 0 || i >= len(nodes) || seen[i] || strconv.Itoa(i) != r.ID {
			return nil, fmt.Errorf("plugin returned unknown or repeated node %q", r.ID)
		}
		seen[i] = true
		n := nodes[i] // credentials and endpoint come from the panel, only the name may change
		if name := strings.TrimSpace(r.Name); name != "" {
			if utf8.RuneCountInString(name) > 100 || strings.ContainsFunc(name, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
				return nil, fmt.Errorf("plugin returned an invalid name for node %q", r.ID)
			}
			n.Name = name
		}
		out = append(out, n)
	}
	s.renderCache.put(key, renderEntry{body: body, until: now.Add(pluginRenderTTL)}, now)
	return out, nil
}
