// Package plugin describes third-party plugins: the manifest a plugin
// serves at /polaris-plugin.json and the checks the panel applies to it.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MaxManifestBytes caps a manifest the panel is willing to read.
const MaxManifestBytes = 64 << 10

// Manifest is what a plugin says about itself. Unknown fields are ignored so
// a newer plugin still installs on an older panel.
type Manifest struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description,omitempty"`
	Author      string    `json:"author,omitempty"`
	Homepage    string    `json:"homepage,omitempty"`
	MinPanel    string    `json:"minPanel,omitempty"`    // lowest panel version, e.g. "0.1.40"
	Scopes      []string  `json:"scopes,omitempty"`      // API scopes the plugin asks for
	Events      []string  `json:"events,omitempty"`      // events it wants; empty = none
	WebhookPath string    `json:"webhookPath,omitempty"` // where events are posted, default /polaris/events
	Pages       []Page    `json:"pages,omitempty"`       // admin pages shown inside the panel
	Portal      []Link    `json:"portal,omitempty"`      // links in the user center
	Settings    []Setting `json:"settings,omitempty"`    // fields the admin fills in
	// Subscription asks to serve extra subscription formats. Rendering one
	// sends the plugin the user's nodes, credentials included, so it is
	// approved on its own.
	Subscription *Subscription `json:"subscription,omitempty"`
}

// Subscription lists what a plugin adds to subscriptions.
type Subscription struct {
	Formats []SubFormat `json:"formats,omitempty"`
	// Transform: the plugin may filter, rename and reorder each user's nodes
	// at /polaris/transform before any format is rendered. It sees node
	// names and endpoints, never credentials.
	Transform bool `json:"transform,omitempty"`
}

// Transforms reports whether the manifest asks to rewrite node lists.
func (m *Manifest) Transforms() bool { return m.Subscription != nil && m.Subscription.Transform }

// SubFormat is a client format the plugin renders at /polaris/render.
type SubFormat struct {
	Name        string `json:"name"`        // ?format=<name>
	Title       string `json:"title"`       // shown to admins
	ContentType string `json:"contentType"` // the only type its responses may carry
}

// SubContentTypes are the response types a format may declare: text
// configurations only, never HTML or script on the subscription origin.
var SubContentTypes = []string{"text/plain", "text/yaml", "application/yaml", "application/x-yaml", "application/json", "application/xml", "text/xml"}

// FormatNames lists the subscription formats the manifest asks for.
func (m *Manifest) FormatNames() []string {
	if m.Subscription == nil {
		return nil
	}
	out := make([]string, 0, len(m.Subscription.Formats))
	for _, f := range m.Subscription.Formats {
		out = append(out, f.Name)
	}
	return out
}

// Page is an admin page the plugin serves; the panel embeds it.
type Page struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Icon  string `json:"icon,omitempty"`
	Path  string `json:"path"`
}

// Link is a link the plugin adds to the user center.
type Link struct {
	Title string `json:"title"`
	Path  string `json:"path"`
}

// Setting is one configuration field.
type Setting struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // string / text / secret / number / bool / select / url
	Required bool     `json:"required,omitempty"`
	Default  string   `json:"default,omitempty"`
	Hint     string   `json:"hint,omitempty"`
	Options  []string `json:"options,omitempty"` // for select
}

// SettingTypes are the field types a manifest may use.
var SettingTypes = []string{"string", "text", "secret", "number", "bool", "select", "url"}

var (
	idRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,40}$`)
	slugRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	keyRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+([-+][0-9A-Za-z.+-]+)?$`)
	formatRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{1,23}$`)
)

// Parse decodes and validates a manifest. scopes lists the API scopes the
// panel knows; checkEvents rejects unknown event names.
func Parse(data []byte, scopes []string, checkEvents func([]string) error) (*Manifest, error) {
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("描述文件超过 %d KB", MaxManifestBytes>>10)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("描述文件不是有效的 JSON：%v", err)
	}
	if err := m.Validate(scopes, checkEvents); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest field by field and names the first problem.
func (m *Manifest) Validate(scopes []string, checkEvents func([]string) error) error {
	if !idRe.MatchString(m.ID) {
		return errors.New("id 只能用小写字母、数字和 -，2–41 个字符")
	}
	if n := strings.TrimSpace(m.Name); n == "" || len(m.Name) > 64 {
		return errors.New("name 不能为空，最长 64 个字符")
	}
	if !versionRe.MatchString(m.Version) {
		return errors.New("version 应为 1.2.3 这样的版本号")
	}
	if len(m.Description) > 500 || len(m.Author) > 100 {
		return errors.New("description 最长 500 个字符，author 最长 100 个字符")
	}
	if m.Homepage != "" && !isHTTPURL(m.Homepage) {
		return errors.New("homepage 应为 http(s) 网址")
	}
	if m.MinPanel != "" && !versionRe.MatchString(m.MinPanel) {
		return errors.New("minPanel 应为 1.2.3 这样的版本号")
	}
	seen := map[string]bool{}
	for _, sc := range m.Scopes {
		if !slices.Contains(scopes, sc) {
			return fmt.Errorf("未知的权限范围 %q", sc)
		}
		if seen[sc] {
			return fmt.Errorf("权限范围 %q 重复", sc)
		}
		seen[sc] = true
	}
	if len(m.Events) > 0 && checkEvents != nil {
		if err := checkEvents(m.Events); err != nil {
			return err
		}
	}
	if m.WebhookPath != "" {
		if err := checkPath(m.WebhookPath); err != nil {
			return fmt.Errorf("webhookPath：%w", err)
		}
	}
	if len(m.Pages) > 10 {
		return errors.New("pages 最多 10 个")
	}
	ids := map[string]bool{}
	for _, p := range m.Pages {
		if !slugRe.MatchString(p.ID) || ids[p.ID] {
			return fmt.Errorf("页面 id %q 不合法或重复", p.ID)
		}
		ids[p.ID] = true
		if t := strings.TrimSpace(p.Title); t == "" || len(p.Title) > 40 {
			return fmt.Errorf("页面 %q 的 title 不能为空，最长 40 个字符", p.ID)
		}
		if err := checkPath(p.Path); err != nil {
			return fmt.Errorf("页面 %q 的 path：%w", p.ID, err)
		}
	}
	if len(m.Portal) > 5 {
		return errors.New("portal 最多 5 个")
	}
	for _, l := range m.Portal {
		if t := strings.TrimSpace(l.Title); t == "" || len(l.Title) > 40 {
			return errors.New("portal 的 title 不能为空，最长 40 个字符")
		}
		if err := checkPath(l.Path); err != nil {
			return fmt.Errorf("portal %q 的 path：%w", l.Title, err)
		}
	}
	if sub := m.Subscription; sub != nil {
		if len(sub.Formats) > 5 {
			return errors.New("subscription.formats 最多 5 个")
		}
		names := map[string]bool{}
		for _, f := range sub.Formats {
			if !formatRe.MatchString(f.Name) || names[f.Name] {
				return fmt.Errorf("订阅格式名 %q 不合法或重复（小写字母开头，2–24 个字符）", f.Name)
			}
			names[f.Name] = true
			if t := strings.TrimSpace(f.Title); t == "" || len(f.Title) > 40 {
				return fmt.Errorf("订阅格式 %q 的 title 不能为空，最长 40 个字符", f.Name)
			}
			if !slices.Contains(SubContentTypes, f.ContentType) {
				return fmt.Errorf("订阅格式 %q 的 contentType 只能是 %s", f.Name, strings.Join(SubContentTypes, "、"))
			}
		}
	}
	if len(m.Settings) > 40 {
		return errors.New("settings 最多 40 项")
	}
	keys := map[string]bool{}
	for _, s := range m.Settings {
		if !keyRe.MatchString(s.Key) || keys[s.Key] {
			return fmt.Errorf("设置项 key %q 不合法或重复", s.Key)
		}
		keys[s.Key] = true
		if strings.TrimSpace(s.Label) == "" || len(s.Label) > 60 || len(s.Hint) > 300 {
			return fmt.Errorf("设置项 %q 的 label 不能为空（最长 60 个字符），hint 最长 300 个字符", s.Key)
		}
		if !slices.Contains(SettingTypes, s.Type) {
			return fmt.Errorf("设置项 %q 的 type %q 不支持", s.Key, s.Type)
		}
		if s.Type == "select" && len(s.Options) == 0 {
			return fmt.Errorf("设置项 %q 是 select，需要 options", s.Key)
		}
		if s.Type == "secret" && s.Default != "" {
			return fmt.Errorf("设置项 %q 是 secret，不能带默认值", s.Key)
		}
	}
	return nil
}

// Setting looks a field up by key.
func (m *Manifest) Setting(key string) (Setting, bool) {
	for _, s := range m.Settings {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

// CheckValues validates admin input against the manifest. Values are the
// whole new configuration; a missing secret counts as "unchanged" and is the
// caller's business, so required is only enforced on non-secret fields and on
// secrets the caller says are not set yet (have).
func (m *Manifest) CheckValues(values map[string]string, have map[string]bool) error {
	for k := range values {
		if _, ok := m.Setting(k); !ok {
			return fmt.Errorf("未知的设置项 %q", k)
		}
	}
	for _, s := range m.Settings {
		v, given := values[s.Key]
		v = strings.TrimSpace(v)
		if s.Type == "secret" && (!given || v == "") && have[s.Key] {
			continue // keep the stored secret
		}
		if v == "" {
			if s.Required {
				return fmt.Errorf("「%s」必填", s.Label)
			}
			continue
		}
		if len(v) > 4000 {
			return fmt.Errorf("「%s」太长", s.Label)
		}
		switch s.Type {
		case "number":
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				return fmt.Errorf("「%s」应为数字", s.Label)
			}
		case "bool":
			if v != "true" && v != "false" {
				return fmt.Errorf("「%s」应为 true 或 false", s.Label)
			}
		case "select":
			if !slices.Contains(s.Options, v) {
				return fmt.Errorf("「%s」不在可选项中", s.Label)
			}
		case "url":
			if !isHTTPURL(v) {
				return fmt.Errorf("「%s」应为 http(s) 网址", s.Label)
			}
		}
	}
	return nil
}

// AtLeast reports whether panel version have is at least want. Versions that
// do not parse (a development build) count as new enough.
func AtLeast(have, want string) bool {
	h, ok1 := triple(have)
	w, ok2 := triple(want)
	if !ok1 || !ok2 {
		return true
	}
	for i := range h {
		if h[i] != w[i] {
			return h[i] > w[i]
		}
	}
	return true
}

func triple(v string) (t [3]int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return t, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return t, false
		}
		t[i] = n
	}
	return t, true
}

// checkPath accepts a path on the plugin's own site: absolute, no query,
// fragment, dot segments or scheme-relative form.
func checkPath(p string) error {
	switch {
	case !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//"):
		return errors.New("应以单个 / 开头")
	case len(p) > 200 || strings.ContainsAny(p, "?#\\ \t\r\n"):
		return errors.New("不能包含空白、? 或 #，最长 200 个字符")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return errors.New("不能包含 . 或 ..")
		}
	}
	return nil
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}
