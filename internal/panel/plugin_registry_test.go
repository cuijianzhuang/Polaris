package panel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestParseRegistry(t *testing.T) {
	data := `{"version":1,"plugins":[
	 {"id":"good","name":"Good","description":"` + strings.Repeat("长", 400) + `","author":"A","repo":"https://github.com/a/b","type":"plugin","kind":"official","category":"monitor",
	  "scopes":["nodes:read","nodes:read","root:all"],"events":["user.created","bad event"],"minPanel":"0.1.149","deploy":{"command":"run","port":70000}},
	 {"id":"tool","name":"Tool","repo":"https://github.com/a/c","type":"tool","kind":"weird","category":"weird"},
	 {"id":"good","name":"Dup","repo":"https://github.com/a/d","type":"plugin"},
	 {"id":"Bad ID","name":"x","repo":"https://github.com/a/e","type":"plugin"},
	 {"id":"insecure","name":"x","repo":"http://github.com/a/f","type":"plugin"},
	 {"id":"notype","name":"x","repo":"https://github.com/a/g"},
	 "not an object"
	]}`
	got, skipped, err := parseRegistry([]byte(data), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || skipped != 5 {
		t.Fatalf("kept %d skipped %d: %+v", len(got), skipped, got)
	}
	g := got[0]
	if g.Kind != "community" {
		t.Fatalf("a custom registry must not mark plugins official: %q", g.Kind)
	}
	if strings.Join(g.Scopes, ",") != "nodes:read" || strings.Join(g.UnknownScopes, ",") != "root:all" {
		t.Fatalf("scopes %v unknown %v", g.Scopes, g.UnknownScopes)
	}
	if strings.Join(g.Events, ",") != "user.created" || g.Deploy == nil || g.Deploy.Port != 0 || g.Deploy.Command != "run" {
		t.Fatalf("events %v deploy %+v", g.Events, g.Deploy)
	}
	if n := len([]rune(g.Description)); n != 301 {
		t.Fatalf("description not clipped: %d runes", n)
	}
	if got[1].Kind != "community" || got[1].Category != "other" {
		t.Fatalf("unknown kind/category not normalised: %+v", got[1])
	}
	if off, _, _ := parseRegistry([]byte(data), true); off[0].Kind != "official" {
		t.Fatal("the default registry may mark plugins official")
	}
	for _, bad := range []string{`{"version":2,"plugins":[]}`, `[]`, `nope`} {
		if _, _, err := parseRegistry([]byte(bad), true); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestPluginRegistryAPI(t *testing.T) {
	s, _, admin := newAdminClient(t)
	oldVersion := Version
	Version = "0.1.150"
	defer func() { Version = oldVersion }()
	var hits atomic.Int32
	body := `{"version":1,"plugins":[
	 {"id":"mine","name":"Mine","description":"d","author":"a","repo":"https://github.com/a/mine","type":"plugin","kind":"example","category":"monitor","minPanel":"0.0.1"},
	 {"id":"future","name":"Future","description":"d","author":"a","repo":"https://github.com/a/f","type":"plugin","kind":"community","category":"other","minPanel":"99999.0.0"},
	 {"id":"probe","name":"Probe","description":"d","author":"a","repo":"https://github.com/a/p","type":"tool","kind":"official","category":"cdn","scopes":["cdn:ips:write"]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/big" {
			io.WriteString(w, `{"version":1,"plugins":[`+strings.Repeat(" ", registryMaxBytes)+`]}`)
			return
		}
		io.WriteString(w, body)
	}))
	defer srv.Close()
	registryCache.Lock()
	registryCache.last = registryFetch{}
	registryCache.Unlock()

	// The address must be https.
	if code, _ := admin.do("PUT", "/plugin-registry", map[string]any{"url": srv.URL + "/index.json"}); code != 400 {
		t.Fatalf("http registry accepted: %d", code)
	}
	// Tests serve plain HTTP on loopback: set it directly and allow private sources.
	s.st.SetSetting("plugin_registry_url", srv.URL+"/index.json")
	if code, out := admin.do("GET", "/plugin-registry", nil); code != 200 || out["error"] == nil {
		t.Fatalf("loopback registry fetched without allow_private_sources: %d %v", code, out)
	}
	s.st.SetSetting("allow_private_sources", "true")
	s.st.DB.Create(&store.Plugin{ID: "mine", Name: "Mine", BaseURL: "http://127.0.0.1:1"})

	code, out := admin.do("GET", "/plugin-registry?refresh=1", nil)
	if code != 200 || out["error"] != nil || out["custom"] != true {
		t.Fatalf("registry %d %v", code, out)
	}
	list := out["plugins"].([]any)
	if len(list) != 3 {
		t.Fatalf("plugins %v", list)
	}
	byID := map[string]map[string]any{}
	for _, p := range list {
		m := p.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if byID["mine"]["installed"] != true || byID["future"]["installed"] != false || byID["probe"]["installed"] != false {
		t.Fatalf("installed flags %v", byID)
	}
	if byID["mine"]["compatible"] != true || byID["future"]["compatible"] != false {
		t.Fatalf("compatible flags %v", byID)
	}
	if byID["probe"]["kind"] != "community" {
		t.Fatalf("custom registry kept official: %v", byID["probe"])
	}
	// Cached: a second read does not refetch.
	before := hits.Load()
	admin.do("GET", "/plugin-registry", nil)
	if hits.Load() != before {
		t.Fatal("registry refetched inside the cache window")
	}
	// A failed refresh keeps the last good list and reports the error.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 502) })
	code, out = admin.do("GET", "/plugin-registry?refresh=1", nil)
	if code != 200 || out["error"] == nil || len(out["plugins"].([]any)) != 3 {
		t.Fatalf("failed refresh lost the list: %d %v", code, out)
	}
	// Oversized registries are refused.
	s.st.SetSetting("plugin_registry_url", srv.URL+"/big")
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"version":1,"plugins":[`+strings.Repeat(" ", registryMaxBytes)+`]}`)
	})
	if _, out = admin.do("GET", "/plugin-registry?refresh=1", nil); out["error"] == nil || len(out["plugins"].([]any)) != 0 {
		t.Fatalf("oversized registry accepted: %v", out)
	}
	// Restoring the default clears the setting.
	if code, _ := admin.do("PUT", "/plugin-registry", map[string]any{"url": ""}); code != 200 || s.st.GetSetting("plugin_registry_url", "x") != "" {
		t.Fatalf("reset %d", code)
	}
	if code, _ := admin.do("PUT", "/plugin-registry", map[string]any{"url": "https://example.com/r.json"}); code != 200 || s.st.GetSetting("plugin_registry_url", "") != "https://example.com/r.json" {
		t.Fatalf("set %d", code)
	}
	// With a GitHub mirror, the default registry goes through it.
	s.st.SetSetting("plugin_registry_url", "")
	s.st.SetSetting("github_mirror", "https://mirror.example/")
	if u, custom := s.registryURL(); custom || u != "https://mirror.example/"+defaultPluginRegistry {
		t.Fatalf("mirror url %q %v", u, custom)
	}
}
