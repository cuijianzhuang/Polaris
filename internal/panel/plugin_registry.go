package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/plugin"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// The plugin market reads a registry: a JSON list of plugins with their
// description, the scopes they ask for and how to deploy them. It is only a
// catalogue. The panel never downloads or runs anything from it; installing
// still reads the plugin's own manifest and the admin approves its scopes.
// Format: polaris-plugins/schema/registry.schema.json.

const (
	defaultPluginRegistry = "https://raw.githubusercontent.com/cuijianzhuang/polaris-plugins/main/registry/index.json"
	registryMaxBytes      = 1 << 20
	registryMaxEntries    = 500
	registryTTL           = 10 * time.Minute
)

var (
	registryIDRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,40}$`)
	registryEventRe = regexp.MustCompile(`^[a-z]+\.[a-z_]+$`)
	registryVerRe   = regexp.MustCompile(`^\d+\.\d+\.\d+([-+][0-9A-Za-z.+-]+)?$`)
	registryTypes   = []string{"plugin", "tool"}
	registryKinds   = []string{"official", "example", "community"}
	registryCats    = []string{"notify", "billing", "subscription", "monitor", "cdn", "users", "other"}
)

type registryDeploy struct {
	Command string `json:"command,omitempty"`
	Docker  string `json:"docker,omitempty"`
	Port    int    `json:"port,omitempty"`
}

type registryEntry struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Author      string          `json:"author"`
	Repo        string          `json:"repo"`
	Homepage    string          `json:"homepage,omitempty"`
	Language    string          `json:"language,omitempty"`
	Type        string          `json:"type"`
	Kind        string          `json:"kind"`
	Category    string          `json:"category"`
	Scopes      []string        `json:"scopes"`
	Events      []string        `json:"events"`
	MinPanel    string          `json:"minPanel,omitempty"`
	Deploy      *registryDeploy `json:"deploy,omitempty"`
	// Set by the panel, not read from the registry.
	UnknownScopes []string `json:"unknownScopes,omitempty"`
}

type registryFetch struct {
	key     string
	at      time.Time
	entries []registryEntry
	skipped int
	err     error
}

var registryCache struct {
	sync.Mutex
	last registryFetch
}

// registryURL is the configured registry, or the default one through the
// GitHub mirror when one is set.
func (s *Server) registryURL() (string, bool) {
	if u := strings.TrimSpace(s.st.GetSetting("plugin_registry_url", "")); u != "" {
		return u, true
	}
	if m := strings.TrimRight(s.st.GetSetting("github_mirror", ""), "/"); m != "" {
		return m + "/" + defaultPluginRegistry, false
	}
	return defaultPluginRegistry, false
}

func validRegistryURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("目录地址应为 https:// 开头的完整网址")
	}
	return nil
}

func httpsURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(raw) > 500 {
		return ""
	}
	return raw
}

func clipText(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// parseRegistry keeps the entries that are well formed and normalises the
// rest of each entry; a remote list is never trusted as-is. Only the
// default registry may call a plugin "official".
func parseRegistry(data []byte, official bool) ([]registryEntry, int, error) {
	var doc struct {
		Version int               `json:"version"`
		Plugins []json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, 0, fmt.Errorf("目录不是有效的 JSON：%v", err)
	}
	if doc.Version != 1 {
		return nil, 0, fmt.Errorf("不支持的目录版本 %d", doc.Version)
	}
	known := map[string]bool{}
	for _, s := range allScopes {
		known[s] = true
	}
	var out []registryEntry
	skipped := 0
	seen := map[string]bool{}
	for _, raw := range doc.Plugins {
		if len(out) >= registryMaxEntries {
			skipped++
			continue
		}
		var e registryEntry
		if json.Unmarshal(raw, &e) != nil || !registryIDRe.MatchString(e.ID) || seen[e.ID] || strings.TrimSpace(e.Name) == "" || httpsURL(e.Repo) == "" || !slices.Contains(registryTypes, e.Type) {
			skipped++
			continue
		}
		seen[e.ID] = true
		e.Name, e.Description, e.Author, e.Language = clipText(e.Name, 60), clipText(e.Description, 300), clipText(e.Author, 60), clipText(e.Language, 30)
		e.Repo, e.Homepage = httpsURL(e.Repo), httpsURL(e.Homepage)
		if !slices.Contains(registryKinds, e.Kind) || (e.Kind == "official" && !official) {
			e.Kind = "community"
		}
		if !slices.Contains(registryCats, e.Category) {
			e.Category = "other"
		}
		scopes := []string{}
		e.UnknownScopes = nil
		for _, sc := range e.Scopes {
			switch {
			case known[sc] && !slices.Contains(scopes, sc):
				scopes = append(scopes, sc)
			case !known[sc] && len(sc) <= 40 && len(e.UnknownScopes) < 10:
				e.UnknownScopes = append(e.UnknownScopes, sc)
			}
		}
		e.Scopes = scopes
		events := []string{}
		for _, ev := range e.Events {
			if registryEventRe.MatchString(ev) && len(events) < 50 && !slices.Contains(events, ev) {
				events = append(events, ev)
			}
		}
		e.Events = events
		if !registryVerRe.MatchString(e.MinPanel) {
			e.MinPanel = ""
		}
		if d := e.Deploy; d != nil {
			d.Command, d.Docker = clipText(d.Command, 600), clipText(d.Docker, 200)
			if d.Port < 1 || d.Port > 65535 {
				d.Port = 0
			}
			if d.Command == "" && d.Docker == "" && d.Port == 0 {
				e.Deploy = nil
			}
		}
		out = append(out, e)
	}
	return out, skipped, nil
}

func (s *Server) fetchRegistry(ctx context.Context, src string, official, refresh bool) registryFetch {
	registryCache.Lock()
	defer registryCache.Unlock()
	last := registryCache.last
	if !refresh && last.key == src && time.Since(last.at) < registryTTL {
		return last
	}
	f := registryFetch{key: src, at: time.Now()}
	f.entries, f.skipped, f.err = func() ([]registryEntry, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Polaris/"+Version)
		resp, err := s.safeClient(15 * time.Second).Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("读取插件目录失败：%v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, 0, fmt.Errorf("读取插件目录失败：HTTP %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, registryMaxBytes+1))
		if err != nil {
			return nil, 0, fmt.Errorf("读取插件目录失败：%v", err)
		}
		if len(data) > registryMaxBytes {
			return nil, 0, errors.New("插件目录超过 1 MiB")
		}
		return parseRegistry(data, official)
	}()
	// Keep showing the last good list when a refresh fails.
	if f.err != nil && last.key == src && last.err == nil {
		last.err = f.err
		return last
	}
	registryCache.last = f
	return f
}

// getPluginRegistry is GET /plugin-registry[?refresh=1].
func (s *Server) getPluginRegistry(c *gin.Context) {
	src, custom := s.registryURL()
	f := s.fetchRegistry(c.Request.Context(), src, !custom, c.Query("refresh") == "1")
	var ids []string
	s.st.DB.Model(&store.Plugin{}).Pluck("id", &ids)
	type view struct {
		registryEntry
		Installed  bool `json:"installed"`
		Compatible bool `json:"compatible"`
	}
	list := make([]view, 0, len(f.entries))
	for _, e := range f.entries {
		list = append(list, view{e, e.Type == "plugin" && slices.Contains(ids, e.ID), e.MinPanel == "" || plugin.AtLeast(Version, e.MinPanel)})
	}
	out := gin.H{"url": src, "custom": custom, "defaultUrl": defaultPluginRegistry, "fetchedAt": f.at, "plugins": list, "skipped": f.skipped, "version": Version}
	if f.err != nil {
		out["error"] = f.err.Error()
	}
	c.JSON(http.StatusOK, out)
}

// setPluginRegistry is PUT /plugin-registry {url}; "" restores the default.
func (s *Server) setPluginRegistry(c *gin.Context) {
	var req struct {
		URL string `json:"url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	u := strings.TrimSpace(req.URL)
	if u == defaultPluginRegistry {
		u = ""
	}
	if u != "" {
		if err := validRegistryURL(u); err != nil {
			badRequest(c, err)
			return
		}
	}
	if err := s.st.SetSetting("plugin_registry_url", u); err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
