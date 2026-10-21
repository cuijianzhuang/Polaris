package realityscan

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeShodan answers /shodan/host/search and /api-info like the real API.
func fakeShodan(t *testing.T, pages map[string][]map[string]any, gotQuery *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "good-key" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Invalid API key"}`))
			return
		}
		switch r.URL.Path {
		case "/api-info":
			w.Write([]byte(`{"query_credits":97,"plan":"dev"}`))
		case "/shodan/host/search":
			if gotQuery != nil {
				*gotQuery = r.URL.Query().Get("query")
			}
			ms := pages[r.URL.Query().Get("page")]
			json.NewEncoder(w).Encode(map[string]any{"total": 1234, "matches": ms})
		default:
			http.NotFound(w, r)
		}
	}))
	old := ShodanBase
	ShodanBase = srv.URL
	t.Cleanup(func() { ShodanBase = old; srv.Close() })
	return srv
}

func match(ip string, port int, cn string, hostnames ...string) map[string]any {
	m := map[string]any{"ip_str": ip, "port": port, "hostnames": hostnames}
	if cn != "" {
		m["ssl"] = map[string]any{"cert": map[string]any{"subject": map[string]any{"CN": cn}}}
	}
	return m
}

func TestShodanDiscover(t *testing.T) {
	var q string
	fakeShodan(t, map[string][]map[string]any{"1": {
		match("203.0.113.1", 443, "www.example.com"),               // certificate name
		match("203.0.113.2", 443, "*.cdn.example", "edge.example"), // wildcard: reverse DNS instead
		match("203.0.113.3", 8443, ""),                             // nothing: the address
		match("203.0.113.4", 443, "WWW.Example.com"),               // duplicate name
		match("not-an-ip", 443, "bad name"),                        // unusable
	}}, &q)

	res, err := ShodanDiscover(t.Context(), "good-key", ShodanQuery{Country: "jp", Pages: 2})
	if err != nil {
		t.Fatal(err)
	}
	if q != DefaultShodanQuery+" country:JP" || res.Query != q {
		t.Fatalf("query %q / %q", q, res.Query)
	}
	want := []string{"www.example.com:443", "edge.example:443", "203.0.113.3:8443"}
	if strings.Join(res.Hosts, ",") != strings.Join(want, ",") || res.Total != 1234 {
		t.Fatalf("hosts %v total %d", res.Hosts, res.Total)
	}

	// A country filter already in the query is not added twice.
	if _, err := ShodanDiscover(t.Context(), "good-key", ShodanQuery{Query: "ssl.alpn:h2 country:SG", Country: "JP"}); err != nil || q != "ssl.alpn:h2 country:SG" {
		t.Fatalf("custom query %q %v", q, err)
	}
	if _, err := ShodanDiscover(t.Context(), "good-key", ShodanQuery{Country: "JPN"}); err == nil {
		t.Fatal("bad country code accepted")
	}
}

func TestShodanErrorsHideKey(t *testing.T) {
	fakeShodan(t, nil, nil)
	_, err := ShodanDiscover(t.Context(), "wrong-key-123", ShodanQuery{})
	if err == nil || !strings.Contains(err.Error(), "无效") || strings.Contains(err.Error(), "wrong-key-123") {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := ShodanDiscover(t.Context(), "", ShodanQuery{}); err == nil {
		t.Fatal("empty key accepted")
	}
	credits, plan, err := ShodanCredits(t.Context(), "good-key")
	if err != nil || credits != 97 || plan != "dev" {
		t.Fatalf("credits %d %q %v", credits, plan, err)
	}
	// Connection errors include the URL; the key must not leak through it.
	ShodanBase = "http://127.0.0.1:1"
	if _, err := ShodanDiscover(t.Context(), "secret-key-456", ShodanQuery{}); err == nil || strings.Contains(err.Error(), "secret-key-456") {
		t.Fatalf("connection error leaks key: %v", err)
	}
}
