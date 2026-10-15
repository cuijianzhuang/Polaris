package sub

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// A Scheme is an engine-independent rule plan: policy groups plus rule sets,
// written in the subconverter / ACL4SSR ini dialect so the same scheme drives
// the built-in renderer and third-party backends alike:
//
//	ruleset=🎯 全球直连,https://…/LocalAreaNetwork.list
//	ruleset=🎯 全球直连,[]GEOIP,CN
//	ruleset=🐟 漏网之鱼,[]FINAL
//	custom_proxy_group=🚀 节点选择`select`[]♻️ 自动选择`[]DIRECT`.*
//	custom_proxy_group=♻️ 自动选择`url-test`.*`http://www.gstatic.com/generate_204`300,,50
//
// Group members starting with [] name another group, DIRECT or REJECT; any
// other member is a regular expression over node names. Polaris adds one
// member of its own, []@regions, which expands to an auto-generated url-test
// group per region that actually has nodes.
type Scheme struct {
	Groups []SchemeGroup
	Rules  []SchemeRule
	// RuleSetURL maps a classic rule list URL to one serving the same rules as
	// a sing-box source rule set (the panel converts them). Nil = sing-box
	// output only keeps inline rules.
	RuleSetURL func(listURL string) string
}

// SchemeGroup is one custom_proxy_group line.
type SchemeGroup struct {
	Name      string
	Type      string // select / url-test / fallback / load-balance
	Members   []string
	URL       string
	Interval  int
	Tolerance int
}

// SchemeRule is one ruleset line: either a remote list or an inline rule.
type SchemeRule struct {
	Group    string
	URL      string // remote list
	Behavior string // classical (default) / domain / ipcidr, from clash-domain: style prefixes
	Interval int
	Inline   string // "GEOIP,CN", "FINAL", "DOMAIN-SUFFIX,example.com"…
}

const regionsMember = "[]@regions"

// ParseScheme reads the ini dialect. Unknown keys are ignored so complete
// subconverter configs can be used as-is.
func ParseScheme(text string) (*Scheme, error) {
	sc := &Scheme{}
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == ';' || line[0] == '#' || line[0] == '[' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "ruleset", "surge_ruleset":
			if r, ok := parseRuleset(v); ok {
				sc.Rules = append(sc.Rules, r)
			}
		case "custom_proxy_group":
			if g, ok := parseGroup(v); ok {
				sc.Groups = append(sc.Groups, g)
			}
		}
	}
	if len(sc.Groups) == 0 {
		return nil, errors.New("规则方案中没有 custom_proxy_group")
	}
	return sc, nil
}

func parseRuleset(v string) (SchemeRule, bool) {
	group, src, ok := strings.Cut(v, ",")
	if !ok || group == "" {
		return SchemeRule{}, false
	}
	r := SchemeRule{Group: strings.TrimSpace(group)}
	src = strings.TrimSpace(src)
	if strings.HasPrefix(src, "[]") {
		r.Inline = strings.TrimSpace(src[2:])
		if r.Inline == "MATCH" {
			r.Inline = "FINAL"
		}
		return r, r.Inline != ""
	}
	for prefix, b := range map[string]string{"clash-domain:": "domain", "clash-ipcidr:": "ipcidr", "clash-classic:": "classical"} {
		if strings.HasPrefix(src, prefix) {
			r.Behavior, src = b, src[len(prefix):]
		}
	}
	if u, iv, ok := strings.Cut(src, ","); ok {
		src = u
		r.Interval, _ = strconv.Atoi(strings.TrimSpace(iv))
	}
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		return SchemeRule{}, false // local files of a subconverter install
	}
	r.URL = src
	if r.Behavior == "" {
		r.Behavior = "classical"
	}
	return r, true
}

func parseGroup(v string) (SchemeGroup, bool) {
	parts := strings.Split(v, "`")
	if len(parts) < 2 || parts[0] == "" {
		return SchemeGroup{}, false
	}
	g := SchemeGroup{Name: strings.TrimSpace(parts[0]), Type: strings.TrimSpace(parts[1])}
	switch g.Type {
	case "select", "url-test", "fallback", "load-balance":
	default:
		return SchemeGroup{}, false // relay / ssid groups have no portable meaning
	}
	rest := parts[2:]
	if g.Type != "select" {
		// Trailing `url`interval,timeout,tolerance.
		for len(rest) > 0 {
			last := rest[len(rest)-1]
			if strings.HasPrefix(last, "http://") || strings.HasPrefix(last, "https://") {
				g.URL = last
			} else if last != "" && strings.Trim(last, "0123456789,") == "" {
				f := strings.Split(last, ",")
				g.Interval, _ = strconv.Atoi(f[0])
				if len(f) > 2 {
					g.Tolerance, _ = strconv.Atoi(f[2])
				}
			} else {
				break
			}
			rest = rest[:len(rest)-1]
		}
		if g.URL == "" {
			g.URL = "http://www.gstatic.com/generate_204"
		}
		if g.Interval == 0 {
			g.Interval = 300
		}
	}
	for _, m := range rest {
		if m = strings.TrimSpace(m); m != "" {
			g.Members = append(g.Members, m)
		}
	}
	return g, true
}

// ResolvedGroup is a group with members expanded to node names, other group
// names, DIRECT and REJECT.
type ResolvedGroup struct {
	SchemeGroup
	Proxies []string
}

// Resolve expands the scheme's groups for a node list. name maps a node to
// the name the target format uses for it (some formats sanitise names).
// Groups that end up empty get DIRECT, as subconverter does.
func (sc *Scheme) Resolve(nodes []Node, name func(Node) string) []ResolvedGroup {
	if name == nil {
		name = func(n Node) string { return n.Name }
	}
	known := map[string]bool{}
	usesRegions := false
	for _, g := range sc.Groups {
		known[g.Name] = true
		for _, m := range g.Members {
			usesRegions = usesRegions || m == regionsMember
		}
	}
	var regionGroups []ResolvedGroup
	var regionNames []string
	if usesRegions {
		byName := map[string]Node{}
		for _, n := range nodes {
			byName[n.Name] = n
		}
		for _, rg := range GroupByRegion(nodes) {
			if known[rg.Name] {
				continue
			}
			var members []string
			for _, nn := range rg.Names {
				members = append(members, name(byName[nn]))
			}
			regionNames = append(regionNames, rg.Name)
			regionGroups = append(regionGroups, ResolvedGroup{
				SchemeGroup: SchemeGroup{Name: rg.Name, Type: "url-test", URL: "http://www.gstatic.com/generate_204", Interval: 300, Tolerance: 50},
				Proxies:     members,
			})
		}
	}
	out := make([]ResolvedGroup, 0, len(sc.Groups)+len(regionGroups))
	for _, g := range sc.Groups {
		seen := map[string]bool{}
		var ps []string
		add := func(s string) {
			if !seen[s] && s != g.Name {
				seen[s] = true
				ps = append(ps, s)
			}
		}
		for _, m := range g.Members {
			switch {
			case m == regionsMember:
				for _, r := range regionNames {
					add(r)
				}
			case strings.HasPrefix(m, "[]"):
				ref := m[2:]
				if ref == "DIRECT" || ref == "REJECT" || known[ref] {
					add(ref)
				}
			default:
				re := compileFilter(m)
				if re == nil {
					continue
				}
				for _, n := range nodes {
					if re.MatchString(n.Name) {
						add(name(n))
					}
				}
			}
		}
		if len(ps) == 0 {
			ps = []string{"DIRECT"}
		}
		out = append(out, ResolvedGroup{SchemeGroup: g, Proxies: ps})
	}
	return append(out, regionGroups...)
}

var (
	filterMu    sync.Mutex
	filterCache = map[string]*regexp.Regexp{}
)

// compileFilter compiles a node-name filter. subconverter uses PCRE; the few
// constructs RE2 lacks (look-arounds, !!GROUP= selectors) match nothing
// rather than failing the whole subscription.
func compileFilter(expr string) *regexp.Regexp {
	filterMu.Lock()
	defer filterMu.Unlock()
	if re, ok := filterCache[expr]; ok {
		return re
	}
	re, err := regexp.Compile(expr)
	if err != nil || strings.HasPrefix(expr, "!!") {
		re = nil
	}
	if len(filterCache) < 1024 {
		filterCache[expr] = re
	}
	return re
}

// FinalGroup returns the FINAL rule's target, else the first group.
func (sc *Scheme) FinalGroup() string {
	for _, r := range sc.Rules {
		if r.Inline == "FINAL" {
			return r.Group
		}
	}
	return sc.Groups[0].Name
}

// providerName is a stable, readable identifier for a remote list.
func providerName(u string) string {
	base := strings.TrimSuffix(path.Base(u), path.Ext(u))
	base = regexp.MustCompile(`[^A-Za-z0-9_-]+`).ReplaceAllString(base, "")
	if base == "" {
		base = "rules"
	}
	h := sha1.Sum([]byte(u))
	return fmt.Sprintf("%s-%s", base, hex.EncodeToString(h[:3]))
}

// inlineRule splits "GEOIP,CN" into type and value (value may be empty).
func inlineRule(s string) (string, string) {
	t, v, _ := strings.Cut(s, ",")
	return strings.ToUpper(strings.TrimSpace(t)), strings.TrimSpace(v)
}
