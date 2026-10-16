// Package sub renders subscriptions. Like the core adapter layer it is
// plugin-based: each client format implements Renderer.
package sub

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Node is one client-side proxy entry (one endpoint of one inbound).
type Node struct {
	Name     string
	Region   string // ISO country code when known (panel nodes); otherwise detected from Name
	Source   string // origin label (panel / source name), informational
	Server   string
	Port     int
	Protocol model.Protocol

	UUID     string
	Password string
	Username string // socks / http / naive
	Method   string // shadowsocks cipher
	Flow     string
	// Encryption is the VLESS Encryption client key ("" = none); clients
	// that cannot speak it skip the node.
	Encryption string
	// PQRealityOnly marks a REALITY server (Xray 26.9.8+) that rejects
	// Client Hellos without an X25519MLKEM768 key share; sing-box and
	// mihomo clients send none, so those formats skip the node.
	PQRealityOnly bool

	Transport model.TransportSpec
	TLS       model.ClientTLS

	UpMbps            int
	DownMbps          int
	Obfs              string
	ObfsPassword      string
	CongestionControl string

	ForceUDP bool // ask clients to relay UDP even where it is optional
}

// Info is the account summary shared by every format.
type Info struct {
	Title    string
	Username string
	Upload   int64
	Download int64
	Total    int64 // 0 = unlimited
	Expire   *time.Time
	URL      string // the subscription URL itself

	// Extras shown on the browser info page only.
	Status     string // active / limited / expired / disabled
	Announce   string // announcement text
	SupportURL string // support / contact link
	BotURL     string // Telegram deep link for one-tap binding
	ResetNote  string // e.g. "每月 1 日重置"
	// ExpireAfterDays: validity starts on first use (no Expire yet).
	ExpireAfterDays int
	// StatusLine prepends Shadowrocket's "STATUS=" line to share links; it
	// shows under the subscription name instead of the raw userinfo header.
	StatusLine bool       `json:"-"`
	Daily      []DayUsage // recent daily traffic, oldest first
	Links      []PageLink // per-node share links

	// Scheme is the rule scheme for client configs (nil = minimal default).
	Scheme *Scheme `json:"-"`
}

// DayUsage is one day of traffic on the info page.
type DayUsage struct {
	Day  time.Time
	Up   int64
	Down int64
}

// PageLink is one node shown on the info page.
type PageLink struct {
	Name     string
	Protocol string
	Link     string
}

// usage summarises the plan: used, total, left and percent.
func usage(i Info) (used, left int64, pct int) {
	used = i.Upload + i.Download
	if i.Total > 0 {
		left = max(i.Total-used, 0)
		pct = int(min(used*100/i.Total, 100))
	}
	return
}

// ExpiryLeft reads like "剩 30 天" / "已到期" / "长期有效".
func ExpiryLeft(i Info) string {
	switch {
	case i.Expire != nil:
		switch d := time.Until(*i.Expire); {
		case d >= 24*time.Hour:
			return fmt.Sprintf("剩 %d 天", int(d.Hours()/24))
		case d > 0:
			return fmt.Sprintf("剩 %d 小时", max(int(d.Hours()), 1))
		}
		return "已到期"
	case i.ExpireAfterDays > 0:
		return fmt.Sprintf("首次使用后 %d 天到期", i.ExpireAfterDays)
	}
	return "长期有效"
}

// ExpiryText reads like "2026-11-15 到期 · 剩 30 天" / "长期有效 · 无到期时间".
func ExpiryText(i Info) string {
	switch {
	case i.Expire != nil:
		return i.Expire.Local().Format("2006-01-02") + " 到期 · " + ExpiryLeft(i)
	case i.ExpireAfterDays > 0:
		return fmt.Sprintf("首次使用后 %d 天到期", i.ExpireAfterDays)
	}
	return "长期有效 · 无到期时间"
}

// StatusText is the one-line usage summary for Shadowrocket's STATUS= line:
// "已用 30 GB / 100 GB（30%）· 剩余 70 GB · 2026-10-26 到期（剩 9 天）".
func StatusText(i Info) string {
	used, left, pct := usage(i)
	parts := []string{"已用 " + HumanBytes(used) + " · 不限流量"}
	if i.Total > 0 {
		parts = []string{fmt.Sprintf("已用 %s / %s（%d%%）", HumanBytes(used), HumanBytes(i.Total), pct), "剩余 " + HumanBytes(left)}
	}
	if i.Expire != nil {
		parts = append(parts, i.Expire.Local().Format("2006-01-02")+" 到期（"+ExpiryLeft(i)+"）")
	} else {
		parts = append(parts, ExpiryLeft(i))
	}
	return strings.Join(parts, " · ")
}

// InfoNodes returns pseudo nodes that carry the plan, remaining traffic and
// expiry in their names (for clients that do not read subscription-userinfo).
func InfoNodes(i Info) []Node {
	used, left, pct := usage(i)
	var names []string
	if i.Total > 0 {
		names = append(names, fmt.Sprintf("📦 套餐 %s · 已用 %s（%d%%）", HumanBytes(i.Total), HumanBytes(used), pct))
		r := "📊 剩余流量 " + HumanBytes(left)
		if i.ResetNote != "" {
			r += " · " + i.ResetNote
		}
		names = append(names, r)
	} else {
		names = append(names, "📦 套餐 不限流量 · 已用 "+HumanBytes(used))
	}
	names = append(names, "⏳ "+ExpiryText(i))
	if i.Announce != "" {
		a := []rune(strings.ReplaceAll(i.Announce, "\n", " "))
		if len(a) > 40 {
			a = append(a[:40], '…')
		}
		names = append(names, "📢 "+strings.Join(strings.Fields(string(a)), " "))
	}
	out := make([]Node, 0, len(names))
	for k, n := range names {
		out = append(out, Node{Name: n, Protocol: "shadowsocks", Server: "127.0.0.1", Port: 1 + k, Method: "aes-128-gcm", Password: "info", Source: "info"})
	}
	return out
}

// InfoSuffix is appended to node names when info nodes are disabled but
// usage should still be visible (3x-ui "show info" style): " | 剩 70 GB · 30 天".
func InfoSuffix(i Info) string {
	var parts []string
	if i.Total > 0 {
		_, left, _ := usage(i)
		parts = append(parts, "剩 "+HumanBytes(left))
	}
	if i.Expire != nil {
		parts = append(parts, fmt.Sprintf("%d 天", max(int(time.Until(*i.Expire).Hours()/24), 0)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " | " + strings.Join(parts, " · ")
}

// Renderer turns nodes into one client format.
type Renderer interface {
	Format() string
	ContentType() string
	// Render produces the body. tpl is the format's rule template ("" = default).
	Render(nodes []Node, info Info, tpl string) ([]byte, error)
}

var renderers = map[string]Renderer{}

// Register adds a renderer.
func Register(r Renderer) { renderers[r.Format()] = r }

// Get returns a renderer by format name.
func Get(format string) (Renderer, bool) {
	r, ok := renderers[format]
	return r, ok
}

// Formats lists registered formats.
func Formats() []string {
	var out []string
	for k := range renderers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DetectFormat picks a format from ?format= or the User-Agent. "html" means a
// browser and should get the info page.
func DetectFormat(query, ua string) string {
	if query != "" {
		return strings.ToLower(query)
	}
	u := strings.ToLower(ua)
	switch {
	case containsAny(u, "clash", "mihomo", "stash", "flclash", "verge"):
		return "clash"
	case containsAny(u, "sing-box", "singbox", "sfa", "sfi", "sfm", "sft", "hiddify"):
		return "singbox"
	case strings.Contains(u, "surge"):
		return "surge"
	case strings.Contains(u, "loon"):
		return "loon"
	case strings.Contains(u, "quantumult"):
		return "quanx"
	case containsAny(u, "v2ray", "shadowrocket", "nekobox", "nekoray", "streisand", "karing"):
		return "links"
	case strings.HasPrefix(u, "mozilla/"):
		return "html"
	}
	return "links"
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// UserInfoHeader builds the subscription-userinfo header value.
func UserInfoHeader(i Info) string {
	s := fmt.Sprintf("upload=%d; download=%d; total=%d", i.Upload, i.Download, i.Total)
	if i.Expire != nil {
		s += fmt.Sprintf("; expire=%d", i.Expire.Unix())
	}
	return s
}

// NameVars fills a node-name template such as "{flag} {node} {protocol}".
func NameVars(tpl string, vars map[string]string) string {
	out := tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return strings.Join(strings.Fields(out), " ")
}

// Flag converts an ISO 3166 alpha-2 code into its emoji flag.
func Flag(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return ""
	}
	return string(rune(0x1F1E6+int(code[0]-'A'))) + string(rune(0x1F1E6+int(code[1]-'A')))
}

// ProtocolLabel is the display name used in node names.
func ProtocolLabel(n Node) string {
	switch n.Protocol {
	case model.ProtoVLESS:
		if n.TLS.Type == model.SecReality {
			return "REALITY"
		}
		if n.Transport.Type != "" && n.Transport.Type != model.TransTCP {
			return "VLESS-" + strings.ToUpper(string(n.Transport.Type))
		}
		return "VLESS"
	case model.ProtoVMess:
		return "VMess"
	case model.ProtoTrojan:
		return "Trojan"
	case model.ProtoShadowsocks:
		return "SS"
	case model.ProtoHysteria2:
		return "Hy2"
	case model.ProtoTUIC:
		return "TUIC"
	case model.ProtoAnyTLS:
		return "AnyTLS"
	case model.ProtoNaive:
		return "Naive"
	}
	return strings.ToUpper(string(n.Protocol))
}

// Dedupe makes node names unique (clients key proxies by name).
func Dedupe(nodes []Node) []Node {
	used := map[string]bool{}
	for _, n := range nodes {
		used[n.Name] = true
	}
	taken := map[string]bool{} // names already given out
	count := map[string]int{}
	out := make([]Node, len(nodes))
	for i, n := range nodes {
		if taken[n.Name] {
			// Pick a suffix no other node has or will keep.
			base := n.Name
			for c := count[base] + 2; ; c++ {
				cand := fmt.Sprintf("%s %d", base, c)
				if !used[cand] && !taken[cand] {
					n.Name, count[base] = cand, c-1
					break
				}
			}
		}
		taken[n.Name] = true
		out[i] = n
	}
	return out
}
