package panel

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestParseRuleList(t *testing.T) {
	l, err := parseRuleList(strings.NewReader(`# comment
payload:
  - '+.google.com'
  - DOMAIN,exact.example.com
  - DOMAIN-SUFFIX,suffix.example.com
  - DOMAIN-KEYWORD,ads
  - IP-CIDR,1.2.3.0/24,no-resolve
  - IP-CIDR6,2001:db8::/32
full:full.example.com
domain:v2.example.com
keyword:track
regexp:^x$
plain.example.com @cn
.dot.example.com
9.9.9.9
// trailing comment
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"full":    {"exact.example.com", "full.example.com"},
		"suffix":  {"dot.example.com", "google.com", "plain.example.com", "suffix.example.com", "v2.example.com"},
		"keyword": {"ads", "track"},
		"cidr":    {"1.2.3.0/24", "2001:db8::/32", "9.9.9.9/32"},
	}
	got := map[string][]string{"full": l.Full, "suffix": l.Suffix, "keyword": l.Keyword, "cidr": l.CIDR}
	for k, v := range want {
		if !slices.Equal(got[k], v) {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, err := parseRuleList(strings.NewReader("# nothing\n")); err == nil {
		t.Fatal("empty list should fail")
	}
}

func TestRuleSetsAndDNS(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.9"})
	s.st.DB.Create(&store.Node{ID: "n2", Name: "osaka", Address: "203.0.113.10"})
	s.st.DB.Create(&store.Outbound{ID: "o1", NodeID: "n1", Tag: "warp", Type: model.OutSOCKS, Settings: []byte(`{"server":"127.0.0.1","port":1}`)})

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DOMAIN-SUFFIX,remote.example\nIP-CIDR,10.9.0.0/16\n"))
	}))
	defer remote.Close()

	code, out := c.do("POST", "/rulesets", map[string]any{"tag": "ai", "name": "AI", "domainSuffix": []string{"openai.com", "+.claude.ai"}, "ipCidr": []string{"1.1.1.1"}})
	if code != 200 {
		t.Fatalf("create %d %v", code, out)
	}
	if code, out = c.do("POST", "/rulesets", map[string]any{"tag": "empty"}); code != 200 {
		t.Fatalf("create empty %d %v", code, out)
	}
	code, out = c.do("POST", "/rulesets", map[string]any{"tag": "remote", "sourceUrl": remote.URL})
	if code != 200 || out["fetchError"] != "" {
		t.Fatalf("create remote %d %v", code, out)
	}
	if code, _ := c.do("POST", "/rulesets", map[string]any{"tag": "Bad Tag"}); code != 400 {
		t.Fatal("bad tag accepted")
	}

	// Global rules: rule sets expand into a domain rule and an IP rule; an
	// empty set must not become a catch-all.
	code, _ = c.do("PUT", "/nodes/*/routing", []map[string]any{
		{"match": map[string]any{"ruleSets": []string{"ai", "remote"}}, "outboundTag": "warp", "priority": 10},
		{"match": map[string]any{"ruleSets": []string{"empty"}}, "outboundTag": "block", "priority": 20},
	})
	if code != 200 {
		t.Fatalf("put routing %d", code)
	}
	if code, _ := c.do("PUT", "/nodes/*/routing", []map[string]any{{"match": map[string]any{"ruleSets": []string{"nope"}}, "outboundTag": "direct"}}); code != 400 {
		t.Fatal("unknown rule set accepted")
	}
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	var dom, ip *model.RouteRule
	for i, r := range spec.Routes {
		if strings.HasSuffix(r.ID, "#d") {
			dom = &spec.Routes[i]
		}
		if strings.HasSuffix(r.ID, "#i") {
			ip = &spec.Routes[i]
		}
		if r.Outbound == "block" {
			t.Fatalf("empty rule set produced a rule: %+v", r)
		}
	}
	if dom == nil || ip == nil || !slices.Contains(dom.Match.DomainSuffix, "claude.ai") || !slices.Contains(dom.Match.DomainSuffix, "remote.example") ||
		!slices.Contains(ip.Match.IPCIDR, "1.1.1.1/32") || !slices.Contains(ip.Match.IPCIDR, "10.9.0.0/16") || len(dom.Match.IPCIDR) > 0 {
		t.Fatalf("expansion wrong: %+v", spec.Routes)
	}

	// DNS: off by default, global on, per-node off.
	if spec.DNS != nil {
		t.Fatal("DNS should be off by default")
	}
	dns := map[string]any{"servers": []map[string]any{
		{"tag": "ai", "address": "https://1.1.1.1/dns-query", "domains": []string{"ruleset:ai"}, "detour": "warp"},
		{"tag": "def", "address": "8.8.8.8"},
	}, "strategy": "ipv4_only"}
	if code, out := c.do("PUT", "/dns/*", map[string]any{"mode": "on", "spec": dns}); code != 200 {
		t.Fatalf("put dns %d %v", code, out)
	}
	spec, _ = s.BuildSpec("n1")
	if spec.DNS == nil || spec.DNS.Servers[0].Detour != "warp" || !slices.Contains(spec.DNS.Servers[0].Domains, "domain:openai.com") {
		t.Fatalf("n1 dns: %+v", spec.DNS)
	}
	spec, _ = s.BuildSpec("n2")
	if spec.DNS == nil || spec.DNS.Servers[0].Detour != "" {
		t.Fatalf("n2 lacks warp, the detour should be dropped: %+v", spec.DNS)
	}
	if code, _ := c.do("PUT", "/dns/n2", map[string]any{"mode": "off"}); code != 200 {
		t.Fatal("node off")
	}
	if spec, _ = s.BuildSpec("n2"); spec.DNS != nil {
		t.Fatal("n2 override off ignored")
	}
	if code, _ := c.do("PUT", "/dns/n2", map[string]any{"mode": "inherit"}); code != 200 {
		t.Fatal("node inherit")
	}
	if spec, _ = s.BuildSpec("n2"); spec.DNS == nil {
		t.Fatal("n2 inherit should get global")
	}
	bad := map[string]any{"servers": []map[string]any{{"tag": "x", "address": "ftp://1.1.1.1"}}}
	if code, _ := c.do("PUT", "/dns/*", map[string]any{"mode": "on", "spec": bad}); code != 400 {
		t.Fatal("bad address accepted")
	}
	split := map[string]any{"servers": []map[string]any{{"tag": "x", "address": "1.1.1.1", "domains": []string{"a.com"}}}}
	if code, _ := c.do("PUT", "/dns/*", map[string]any{"mode": "on", "spec": split}); code != 400 {
		t.Fatal("spec without a default server accepted")
	}

	// A referenced rule set can't be deleted.
	var ai store.RuleSet
	s.st.DB.First(&ai, "tag = ?", "ai")
	if code, _ := c.do("DELETE", "/rulesets/"+ai.ID, nil); code != 400 {
		t.Fatal("referenced rule set deleted")
	}
}
