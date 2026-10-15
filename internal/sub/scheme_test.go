package sub

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const cmSample = `[custom]
;comment
ruleset=🎯 全球直连,https://raw.githubusercontent.com/ACL4SSR/ACL4SSR/master/Clash/LocalAreaNetwork.list
ruleset=🛑 全球拦截,https://raw.githubusercontent.com/ACL4SSR/ACL4SSR/master/Clash/BanAD.list,86400
ruleset=🎯 全球直连,[]GEOIP,LAN
ruleset=🎯 全球直连,[]GEOIP,CN
ruleset=🐟 漏网之鱼,[]FINAL
custom_proxy_group=🚀 节点选择` + "`select`[]♻️ 自动选择`[]🇭🇰 香港节点`[]DIRECT" + `
custom_proxy_group=♻️ 自动选择` + "`url-test`.*`http://www.gstatic.com/generate_204`300,,50" + `
custom_proxy_group=🎯 全球直连` + "`select`[]DIRECT`[]🚀 节点选择" + `
custom_proxy_group=🛑 全球拦截` + "`select`[]REJECT`[]DIRECT" + `
custom_proxy_group=🐟 漏网之鱼` + "`select`[]🚀 节点选择`[]🎯 全球直连`.*" + `
custom_proxy_group=🇭🇰 香港节点` + "`load-balance`(港|HK|Hong Kong)`http://www.gstatic.com/generate_204`300,,50" + `
custom_proxy_group=🇯🇵 日本节点` + "`url-test`(日本|JP)`http://www.gstatic.com/generate_204`300,,50" + `
enable_rule_generator=true
`

func TestParseScheme(t *testing.T) {
	sc, err := ParseScheme(cmSample)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Groups) != 7 || len(sc.Rules) != 5 {
		t.Fatalf("groups %d rules %d", len(sc.Groups), len(sc.Rules))
	}
	if g := sc.Groups[1]; g.Type != "url-test" || g.Interval != 300 || g.Tolerance != 50 || g.Members[0] != ".*" {
		t.Fatalf("auto group: %+v", g)
	}
	if r := sc.Rules[1]; r.Interval != 86400 || r.Behavior != "classical" {
		t.Fatalf("ruleset interval: %+v", r)
	}
	nodes := []Node{{Name: "香港 01"}, {Name: "HK 02"}, {Name: "美国 01"}}
	rs := sc.Resolve(nodes, nil)
	byName := map[string]ResolvedGroup{}
	for _, g := range rs {
		byName[g.Name] = g
	}
	if got := byName["🇭🇰 香港节点"].Proxies; len(got) != 2 {
		t.Fatalf("hk filter: %v", got)
	}
	if got := byName["🇯🇵 日本节点"].Proxies; len(got) != 1 || got[0] != "DIRECT" {
		t.Fatalf("empty group should fall back to DIRECT: %v", got)
	}
	if sc.FinalGroup() != "🐟 漏网之鱼" {
		t.Fatal(sc.FinalGroup())
	}
}

func TestRegionsMember(t *testing.T) {
	sc, _ := ParseScheme(MinimalScheme)
	rs := sc.Resolve(sample(), nil)
	main := rs[0]
	found := false
	for _, p := range main.Proxies {
		found = found || p == "🇯🇵 日本"
	}
	if !found || rs[len(rs)-1].Name != "🇯🇵 日本" {
		t.Fatalf("region groups not expanded: %+v", rs)
	}
}

// Every preset renders to every format, and the result is accepted by the
// real clients where they are available.
func TestPresetsRender(t *testing.T) {
	sbBin, mihomoBin := os.Getenv("POLARIS_SINGBOX_BIN"), os.Getenv("POLARIS_MIHOMO_BIN")
	texts := map[string]string{"cm": cmSample}
	for _, p := range Presets() {
		if p.Text != "" {
			texts[p.ID] = p.Text
		}
	}
	for id, text := range texts {
		sc, err := ParseScheme(text)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		sc.RuleSetURL = func(u string) string { return "https://panel.example/sub/rs/x/" + providerName(u) + ".json" }
		info := Info{URL: "https://panel.example/sub/x", Scheme: sc}
		for _, format := range []string{"clash", "singbox", "surge", "loon", "quanx"} {
			r, _ := Get(format)
			b, err := r.Render(sample(), info, "")
			if err != nil {
				t.Fatalf("%s/%s: %v", id, format, err)
			}
			switch format {
			case "clash":
				var doc map[string]any
				if err := yaml.Unmarshal(b, &doc); err != nil {
					t.Fatalf("%s: invalid yaml: %v\n%s", id, err, b)
				}
				if strings.Contains(text, ".list") && doc["rule-providers"] == nil {
					t.Fatalf("%s: no rule-providers", id)
				}
				if mihomoBin != "" {
					dir := t.TempDir()
					f := filepath.Join(dir, "config.yaml")
					os.WriteFile(f, b, 0o600)
					if out, err := exec.Command(mihomoBin, "-t", "-d", dir, "-f", f).CombinedOutput(); err != nil {
						t.Fatalf("%s: mihomo rejected config: %v %s", id, err, out)
					}
				}
			case "singbox":
				var doc map[string]any
				if err := json.Unmarshal(b, &doc); err != nil {
					t.Fatal(err)
				}
				if sbBin != "" {
					f := filepath.Join(t.TempDir(), "c.json")
					os.WriteFile(f, b, 0o600)
					if out, err := exec.Command(sbBin, "check", "-c", f).CombinedOutput(); err != nil {
						t.Fatalf("%s: sing-box rejected config: %v %s\n%s", id, err, out, b)
					}
				}
			default:
				s := string(b)
				if strings.Contains(s, "<no value>") || !strings.Contains(strings.ToLower(s), "final") {
					t.Fatalf("%s/%s:\n%s", id, format, s)
				}
			}
		}
	}
}

func TestSingboxRejectGroup(t *testing.T) {
	sc, _ := ParseScheme(cmSample)
	sc.RuleSetURL = func(u string) string { return u + ".json" }
	_, rules, _, final, main := singboxScheme(sc, []Node{{Name: "HK 1"}})
	if !strings.Contains(rules, `"action":"reject"`) || final != "🐟 漏网之鱼" || main != "🚀 节点选择" {
		t.Fatalf("rules=%s final=%s main=%s", rules, final, main)
	}
	if strings.Contains(rules, "全球拦截") {
		t.Fatal("reject group must not be an outbound")
	}
	if !strings.Contains(rules, `"ip_is_private":true`) {
		t.Fatal("GEOIP,LAN should map to ip_is_private")
	}
}
