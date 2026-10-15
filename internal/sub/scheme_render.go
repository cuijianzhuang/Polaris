package sub

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Per-format rendering of a Scheme into the template fields .ProxyGroups,
// .RuleProviders / .RemoteRules, .Rules (and sing-box's .RuleSets / .Final).

var (
	minimalOnce sync.Once
	minimal     *Scheme
)

// schemeOf returns the scheme to render: Info.Scheme, else the minimal default.
func schemeOf(info Info) *Scheme {
	if info.Scheme != nil {
		return info.Scheme
	}
	minimalOnce.Do(func() { minimal, _ = ParseScheme(MinimalScheme) })
	return minimal
}

var iniName = strings.NewReplacer(",", " ", "=", " ")

func surgeName(n Node) string { return iniName.Replace(n.Name) }
func quanxName(n Node) string { return strings.ReplaceAll(n.Name, ",", " ") }

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- Clash / mihomo ---

type clashProvider struct {
	Type     string `yaml:"type"`
	Behavior string `yaml:"behavior"`
	Format   string `yaml:"format"`
	URL      string `yaml:"url"`
	Path     string `yaml:"path"`
	Interval int    `yaml:"interval"`
}

func clashScheme(sc *Scheme, nodes []Node) (groups, providers, rules string) {
	// Written by hand with JSON-quoted strings (valid YAML): yaml.v3 would
	// escape emoji in group names as \U0001F680.
	var gl []string
	for _, g := range sc.Resolve(nodes, nil) {
		gl = append(gl, "  - name: "+jsonStr(g.Name), "    type: "+g.Type)
		if g.Type == "load-balance" {
			gl = append(gl, "    strategy: consistent-hashing")
		}
		if g.Type != "select" {
			gl = append(gl, "    url: "+jsonStr(g.URL), fmt.Sprintf("    interval: %d", g.Interval))
		}
		if g.Type == "url-test" && g.Tolerance > 0 {
			gl = append(gl, fmt.Sprintf("    tolerance: %d", g.Tolerance))
		}
		gl = append(gl, "    proxies: "+flowList(g.Proxies))
	}
	groups = strings.Join(gl, "\n")

	prov := map[string]clashProvider{}
	var rs []string
	final := ""
	for _, r := range sc.Rules {
		if r.URL != "" {
			name := providerName(r.URL)
			format, ext := "text", ".list"
			if r.Behavior != "classical" {
				format, ext = "yaml", ".yaml"
			}
			prov[name] = clashProvider{Type: "http", Behavior: r.Behavior, Format: format, URL: r.URL,
				Path: "./ruleset/" + name + ext, Interval: orInt(r.Interval, 86400)}
			rs = append(rs, "RULE-SET,"+name+","+r.Group)
			continue
		}
		t, v := inlineRule(r.Inline)
		switch t {
		case "FINAL":
			final = "MATCH," + r.Group
		case "GEOIP", "IP-CIDR", "IP-CIDR6", "IP-ASN":
			rs = append(rs, t+","+v+","+r.Group+",no-resolve")
		default:
			if v != "" {
				rs = append(rs, t+","+v+","+r.Group)
			}
		}
	}
	if final == "" {
		final = "MATCH," + sc.Groups[0].Name
	}
	var rl []string
	for _, r := range append(rs, final) {
		rl = append(rl, "  - "+jsonStr(r))
	}
	rules = strings.Join(rl, "\n")
	if len(prov) > 0 {
		b, _ := yaml.Marshal(map[string]any{"rule-providers": prov})
		providers = string(b)
	}
	return
}

func orInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// --- sing-box ---

const (
	sbGeosite = "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-%s.srs"
	sbGeoip   = "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-%s.srs"
)

// singboxScheme renders selector/urltest outbounds, route rules (each JSON
// object prefixed with ", "), the rule_set array and the final outbound.
// sing-box has no fallback / load-balance outbounds, so those become urltest;
// groups whose first choice is REJECT become reject actions.
func singboxScheme(sc *Scheme, nodes []Node) (groups, rules, ruleSets, final, main string) {
	resolved := sc.Resolve(nodes, nil)
	reject := map[string]bool{"REJECT": true}
	for _, g := range resolved {
		if len(g.Proxies) > 0 && g.Proxies[0] == "REJECT" {
			reject[g.Name] = true
		}
	}
	known := map[string]bool{}
	var outs []string
	for _, g := range resolved {
		if reject[g.Name] {
			continue
		}
		known[g.Name] = true
		if main == "" {
			main = g.Name
		}
		var members []string
		for _, p := range g.Proxies {
			switch {
			case reject[p]:
			case p == "DIRECT":
				members = append(members, "direct")
			default:
				members = append(members, p)
			}
		}
		if len(members) == 0 {
			members = []string{"direct"}
		}
		o := map[string]any{"tag": g.Name, "outbounds": members}
		if g.Type == "select" {
			o["type"] = "selector"
		} else {
			o["type"] = "urltest"
			o["url"] = g.URL
			o["interval"] = fmt.Sprintf("%ds", g.Interval)
			if g.Tolerance > 0 {
				o["tolerance"] = g.Tolerance
			}
		}
		b, _ := json.Marshal(o)
		outs = append(outs, string(b))
	}
	if main == "" {
		main = "direct"
	}
	groups = strings.Join(outs, ",\n    ")

	type ruleSet struct {
		Tag            string `json:"tag"`
		Type           string `json:"type"`
		Format         string `json:"format"`
		URL            string `json:"url"`
		DownloadDetour string `json:"download_detour"`
	}
	var sets []ruleSet
	haveSet := map[string]bool{}
	addSet := func(tag, format, url string) {
		if !haveSet[tag] {
			haveSet[tag] = true
			sets = append(sets, ruleSet{tag, "remote", format, url, main})
		}
	}
	addSet("geosite-cn", "binary", fmt.Sprintf(sbGeosite, "cn")) // used by the DNS rules

	target := func(group string) (map[string]any, bool) {
		switch {
		case reject[group]:
			return map[string]any{"action": "reject"}, true
		case group == "DIRECT":
			return map[string]any{"outbound": "direct"}, true
		case known[group]:
			return map[string]any{"outbound": group}, true
		}
		return nil, false
	}
	var out []map[string]any
	// Consecutive rule-set matches for the same target collapse into one rule.
	pushSet := func(tag, group string) {
		if n := len(out); n > 0 {
			if last := out[n-1]; last["_group"] == group && last["rule_set"] != nil {
				last["rule_set"] = append(last["rule_set"].([]string), tag)
				return
			}
		}
		if t, ok := target(group); ok {
			t["rule_set"] = []string{tag}
			t["_group"] = group
			out = append(out, t)
		}
	}
	finalGroup := ""
	for _, r := range sc.Rules {
		if r.URL != "" {
			if sc.RuleSetURL == nil || r.Behavior != "classical" {
				continue
			}
			tag := providerName(r.URL)
			addSet(tag, "source", sc.RuleSetURL(r.URL))
			pushSet(tag, r.Group)
			continue
		}
		t, v := inlineRule(r.Inline)
		lv := strings.ToLower(v)
		switch t {
		case "FINAL":
			finalGroup = r.Group
		case "GEOIP":
			if lv == "lan" || lv == "private" {
				if tg, ok := target(r.Group); ok {
					tg["ip_is_private"] = true
					out = append(out, tg)
				}
			} else if lv != "" {
				addSet("geoip-"+lv, "binary", fmt.Sprintf(sbGeoip, lv))
				pushSet("geoip-"+lv, r.Group)
			}
		case "GEOSITE":
			if lv != "" {
				addSet("geosite-"+lv, "binary", fmt.Sprintf(sbGeosite, lv))
				pushSet("geosite-"+lv, r.Group)
			}
		default:
			field := map[string]string{"DOMAIN": "domain", "DOMAIN-SUFFIX": "domain_suffix", "DOMAIN-KEYWORD": "domain_keyword",
				"DOMAIN-REGEX": "domain_regex", "IP-CIDR": "ip_cidr", "IP-CIDR6": "ip_cidr", "PROCESS-NAME": "process_name"}[t]
			if field == "" || v == "" {
				continue
			}
			if tg, ok := target(r.Group); ok {
				tg[field] = []string{v}
				out = append(out, tg)
			}
		}
	}
	var parts []string
	for _, r := range out {
		delete(r, "_group")
		b, _ := json.Marshal(r)
		parts = append(parts, ", "+string(b))
	}
	rules = strings.Join(parts, "\n      ")
	b, _ := json.Marshal(sets)
	ruleSets = string(b)
	switch {
	case finalGroup == "DIRECT":
		final = "direct"
	case known[finalGroup]:
		final = finalGroup
	default:
		final = main
	}
	return
}

// --- Surge / Loon ---

func iniGroupLine(g ResolvedGroup, sep string, loon bool) string {
	parts := append([]string{g.Type}, g.Proxies...)
	switch g.Type {
	case "url-test":
		parts = append(parts, "url="+g.URL, fmt.Sprintf("interval=%d", g.Interval))
		if g.Tolerance > 0 {
			parts = append(parts, fmt.Sprintf("tolerance=%d", g.Tolerance))
		}
	case "fallback":
		parts = append(parts, "url="+g.URL, fmt.Sprintf("interval=%d", g.Interval))
	case "load-balance":
		parts = append(parts, "url="+g.URL)
		if !loon {
			parts = append(parts, "persistent=true")
		}
	}
	return iniName.Replace(g.Name) + " = " + strings.Join(parts, sep)
}

// iniInline renders an inline rule for Surge / Loon ("" = unsupported).
func iniInline(r SchemeRule) string {
	t, v := inlineRule(r.Inline)
	switch {
	case t == "GEOSITE", t == "GEOIP" && strings.EqualFold(v, "lan"), v == "":
		return ""
	case t == "GEOIP", t == "IP-CIDR", t == "IP-CIDR6":
		return t + "," + v + "," + r.Group + ",no-resolve"
	}
	return t + "," + v + "," + r.Group
}

func surgeScheme(sc *Scheme, nodes []Node) (groups, rules string) {
	var gl []string
	for _, g := range sc.Resolve(nodes, surgeName) {
		gl = append(gl, iniGroupLine(g, ", ", false))
	}
	var rl []string
	final := sc.Groups[0].Name
	for _, r := range sc.Rules {
		switch {
		case r.URL != "" && r.Behavior == "classical":
			rl = append(rl, "RULE-SET,"+r.URL+","+r.Group)
		case r.Inline == "FINAL":
			final = r.Group
		case r.Inline != "":
			if l := iniInline(r); l != "" {
				rl = append(rl, l)
			}
		}
	}
	rl = append(rl, "FINAL,"+final+",dns-failed")
	return strings.Join(gl, "\n"), strings.Join(rl, "\n")
}

func loonScheme(sc *Scheme, nodes []Node) (groups, remote, rules string) {
	var gl, rm, rl []string
	for _, g := range sc.Resolve(nodes, surgeName) {
		gl = append(gl, iniGroupLine(g, ",", true))
	}
	final := sc.Groups[0].Name
	for _, r := range sc.Rules {
		switch {
		case r.URL != "" && r.Behavior == "classical":
			rm = append(rm, fmt.Sprintf("%s, policy=%s, tag=%s, enabled=true", r.URL, r.Group, providerName(r.URL)))
		case r.Inline == "FINAL":
			final = r.Group
		case r.Inline != "":
			if l := iniInline(r); l != "" {
				rl = append(rl, l)
			}
		}
	}
	rl = append(rl, "FINAL,"+final)
	return strings.Join(gl, "\n"), strings.Join(rm, "\n"), strings.Join(rl, "\n")
}

// --- Quantumult X ---

func quanxPolicy(name string) string {
	switch name {
	case "DIRECT":
		return "direct"
	case "REJECT":
		return "reject"
	}
	return strings.ReplaceAll(name, ",", " ")
}

func quanxScheme(sc *Scheme, nodes []Node) (groups, remote, rules string) {
	var gl, rm, rl []string
	for _, g := range sc.Resolve(nodes, quanxName) {
		members := make([]string, 0, len(g.Proxies))
		for _, p := range g.Proxies {
			members = append(members, quanxPolicy(p))
		}
		head := map[string]string{"select": "static", "url-test": "url-latency-benchmark", "fallback": "available", "load-balance": "round-robin"}[g.Type]
		line := head + "=" + quanxPolicy(g.Name) + ", " + strings.Join(members, ", ")
		if g.Type == "url-test" {
			line += fmt.Sprintf(", check-interval=%d", g.Interval)
			if g.Tolerance > 0 {
				line += fmt.Sprintf(", tolerance=%d", g.Tolerance)
			}
		}
		gl = append(gl, line)
	}
	qxType := map[string]string{"DOMAIN": "host", "DOMAIN-SUFFIX": "host-suffix", "DOMAIN-KEYWORD": "host-keyword",
		"IP-CIDR": "ip-cidr", "IP-CIDR6": "ip6-cidr", "GEOIP": "geoip", "USER-AGENT": "user-agent"}
	final := sc.Groups[0].Name
	for _, r := range sc.Rules {
		switch {
		case r.URL != "" && r.Behavior == "classical":
			rm = append(rm, fmt.Sprintf("%s, tag=%s, force-policy=%s, update-interval=%d, opt-parser=true, enabled=true",
				r.URL, providerName(r.URL), quanxPolicy(r.Group), orInt(r.Interval, 86400)))
		case r.Inline == "FINAL":
			final = r.Group
		case r.Inline != "":
			t, v := inlineRule(r.Inline)
			if qt := qxType[t]; qt != "" && v != "" && !(t == "GEOIP" && strings.EqualFold(v, "lan")) {
				rl = append(rl, qt+", "+strings.ToLower(v)+", "+quanxPolicy(r.Group))
			}
		}
	}
	rl = append(rl, "final, "+quanxPolicy(final))
	return strings.Join(gl, "\n"), strings.Join(rm, "\n"), strings.Join(rl, "\n")
}
