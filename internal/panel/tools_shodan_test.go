package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/realityscan"
)

func TestRealityDiscover(t *testing.T) {
	// A local TLS 1.3 + h2 server stands in for a public site.
	site := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	site.EnableHTTP2 = true
	site.StartTLS()
	defer site.Close()
	port, _ := strconv.Atoi(site.URL[strings.LastIndex(site.URL, ":")+1:])

	var calls int
	shodan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("key") != "sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api-info" {
			w.Write([]byte(`{"query_credits":42,"plan":"membership"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"total": 1, "matches": []map[string]any{{"ip_str": "127.0.0.1", "port": port}}})
	}))
	defer shodan.Close()
	old := realityscan.ShodanBase
	realityscan.ShodanBase = shodan.URL
	defer func() { realityscan.ShodanBase = old }()

	s, _, admin := newAdminClient(t)

	// No key yet: a clear error, and Shodan is not called.
	if st, out := admin.do("GET", "/tools/shodan", nil); st != 200 || out["configured"] != false {
		t.Fatalf("info without key: %d %v", st, out)
	}
	if st, out := admin.do("POST", "/tools/reality-discover", map[string]any{}); st != 400 || !strings.Contains(out["error"].(string), "Shodan API Key") {
		t.Fatalf("discover without key: %d %v", st, out)
	}
	if calls != 0 {
		t.Fatal("Shodan called without a key")
	}

	// The key is stored sealed and never returned.
	s.st.SetSetting("shodan_api_key", s.st.Box.Seal("sk-test"))
	if raw := s.st.GetSetting("shodan_api_key", ""); raw == "sk-test" {
		t.Fatal("key stored in clear")
	}
	st, out := admin.do("GET", "/settings", nil)
	if st != 200 || out["shodan_api_key_set"] != true || strings.Contains(toJSONString(out), "sk-test") {
		t.Fatalf("settings: %d %v", st, out)
	}
	if st, out := admin.do("GET", "/tools/shodan", nil); st != 200 || out["credits"].(float64) != 42 {
		t.Fatalf("info: %d %v", st, out)
	}

	// Discovery hands Shodan's hosts to the normal TLS check.
	st, out = admin.do("POST", "/tools/reality-discover", map[string]any{"country": "JP"})
	if st != 200 {
		t.Fatalf("discover: %d %v", st, out)
	}
	if out["query"] != realityscan.DefaultShodanQuery+" country:JP" {
		t.Fatalf("query %v", out["query"])
	}
	cands := out["candidates"].([]any)
	if len(cands) != 1 || cands[0] != "127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("candidates %v", cands)
	}
	rs := out["results"].([]any)
	if len(rs) != 1 {
		t.Fatalf("results %v", rs)
	}
	r := rs[0].(map[string]any)
	if r["tls"] != "1.3" || r["alpn"] != "h2" || out["from"] != "panel" {
		t.Fatalf("checked result %v from %v", r, out["from"])
	}
	if strings.Contains(toJSONString(out), "sk-test") {
		t.Fatal("key in the response")
	}
}
