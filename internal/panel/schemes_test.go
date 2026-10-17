package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestRuleSchemes(t *testing.T) {
	lists := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/scheme.ini":
			w.Write([]byte("[custom]\nruleset=🚀 节点选择," + "http://" + r.Host + "/proxy.list\nruleset=DIRECT,[]GEOIP,CN\nruleset=🚀 节点选择,[]FINAL\n" +
				"custom_proxy_group=🚀 节点选择`select`.*\n"))
		case "/proxy.list":
			w.Write([]byte("# comment\nDOMAIN-SUFFIX,google.com\nDOMAIN,x.com\nIP-CIDR,8.8.8.0/24,no-resolve\nPROCESS-NAME,curl\nUSER-AGENT,foo*\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer lists.Close()

	s := newTestServer(t)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()
	s.cfg.PublicURL = srv.URL
	s.st.SetSetting("allow_private_sources", "true")
	// A template saved before schemes existed: no {{ .ProxyGroups }}.
	s.st.DB.Model(&store.SubTemplate{}).Where("format = ?", "clash").Update("content", "proxies:\n{{ .Proxies }}\nlegacy: true\n")
	s.st.DB.Create(&store.User{ID: "u1", Username: "alice", UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", Status: "active", SubToken: "alice-subscription-token"})
	skip := true
	s.st.DB.Create(&store.SubProfile{ID: "p1", Name: "p", Token: "profile-token-abcdefgh", Enabled: true,
		SourceIDs: store.NewJSON([]string{"m"}), Templates: store.NewJSON(map[string]string{}), Scheme: "builtin:media", SkipCert: &skip})
	s.st.DB.Create(&store.SubSource{ID: "m", Name: "m", Kind: "manual", Enabled: true, Content: "trojan://pw@hk.example.com:443?sni=hk.example.com#香港 01"})

	get := func(path, ua string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("User-Agent", ua)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Subscription groups (/sub/p/) are a paid feature: only in pro builds.
	if proBuild {
		// No scheme: the legacy template is used untouched.
		if _, body := get("/sub/p/profile-token-abcdefgh", "clash-verge"); !strings.Contains(body, "🤖 AI 服务") || !strings.Contains(body, "rule-providers:") ||
			strings.Contains(body, "legacy: true") || !strings.Contains(body, "skip-cert-verify: true") {
			t.Fatalf("profile scheme not applied:\n%s", body)
		}
		s.st.DB.Model(&store.SubProfile{}).Where("id = ?", "p1").Update("scheme", "none")
		if _, body := get("/sub/p/profile-token-abcdefgh", "clash-verge"); !strings.Contains(body, "legacy: true") {
			t.Fatalf("legacy template should stay without a scheme:\n%s", body)
		}
	} else if code, _ := get("/sub/p/profile-token-abcdefgh", "clash-verge"); code != http.StatusNotFound {
		t.Fatalf("community build served a subscription group: %d", code)
	}

	// Global custom ini URL; sing-box gets a signed, panel-converted rule set.
	s.st.SetSetting("sub_scheme", lists.URL+"/scheme.ini")
	_, body := get("/sub/alice-subscription-token", "sing-box")
	var cfg struct {
		Route struct {
			RuleSet []struct{ Tag, URL string } `json:"rule_set"`
			Final   string                      `json:"final"`
		} `json:"route"`
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	rsURL := ""
	for _, r := range cfg.Route.RuleSet {
		if strings.Contains(r.URL, "/sub/rs/") {
			rsURL = r.URL
		}
	}
	if rsURL == "" || cfg.Route.Final != "🚀 节点选择" {
		t.Fatalf("no converted rule set:\n%s", body)
	}
	code, rs := get(strings.TrimPrefix(rsURL, srv.URL), "sing-box")
	if code != 200 || !strings.Contains(rs, `"domain_suffix":["google.com"]`) || !strings.Contains(rs, `"ip_cidr":["8.8.8.0/24"]`) ||
		!strings.Contains(rs, `"process_name":["curl"]`) {
		t.Fatalf("rule set: %d %s", code, rs)
	}
	// Tampered signature: not an open converter.
	if code, _ := get(strings.Replace(strings.TrimPrefix(rsURL, srv.URL), "/sub/rs/", "/sub/rs/0", 1), "x"); code != 404 {
		t.Fatalf("tampered rule-set URL: %d", code)
	}
	// Built-in presets are published as ini for third-party backends.
	if code, ini := get("/sub/scheme/full.ini", "subconverter"); code != 200 || !strings.Contains(ini, "custom_proxy_group=") {
		t.Fatalf("preset ini: %d", code)
	}
	cc := s.converterFor(nil)
	if cc.Config != lists.URL+"/scheme.ini" {
		t.Fatalf("backend config: %q", cc.Config)
	}
}

func TestExternalParams(t *testing.T) {
	got := externalParams("emoji=true&scv=false", renderOpts{SkipCert: true, ForceUDP: true})
	if !strings.Contains(got, "scv=true") || !strings.Contains(got, "udp=true") || !strings.Contains(got, "emoji=true") || strings.Contains(got, "scv=false") {
		t.Fatal(got)
	}
}
