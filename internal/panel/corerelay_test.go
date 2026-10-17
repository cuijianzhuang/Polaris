package panel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestRelayTarget(t *testing.T) {
	for u, want := range map[string]bool{
		"https://github.com/XTLS/Xray-core/releases/download/v26.3.27/Xray-linux-64.zip":     true,
		"https://github.com/apernet/hysteria/releases/download/app/v2.6.2/hashes.txt":        true,
		"https://api.github.com/repos/SagerNet/sing-box/releases/tags/v1.13.5":               true,
		"https://github.com/evil/repo/releases/download/v1/x":                                false,
		"https://github.com/XTLS/Xray-core/archive/refs/heads/main.zip":                      false,
		"https://github.com/XTLS/Xray-core/releases/download/../../../../evil/x":             false,
		"https://api.github.com/repos/SagerNet/sing-box/releases":                            false,
		"https://example.com/XTLS/Xray-core/releases/download/v1/x":                          false,
		"https://github.com/XTLS/Xray-core/releases/download/v1/x?redirect=https://evil.com": false,
	} {
		if _, ok := relayTarget(u); ok != want {
			t.Errorf("relayTarget(%q) = %v, want %v", u, ok, want)
		}
	}
}

func TestRelayCachesReleases(t *testing.T) {
	s := newTestServer(t)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/XTLS/Xray-core/releases/download/v1.0.0/Xray-linux-64.zip":
			w.Write([]byte("zipdata"))
		case "/repos/XTLS/Xray-core/releases/tags/v1.0.0":
			w.Write([]byte(`{"assets":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	githubAPI, githubWeb = up.URL, up.URL
	defer func() { githubAPI, githubWeb = "https://api.github.com", "https://github.com" }()

	get := func(u string) (int, string) {
		w := httptest.NewRecorder()
		s.relayCore(w, httptest.NewRequest(http.MethodGet, "/agent/core?u="+url.QueryEscape(u), nil))
		return w.Code, w.Body.String()
	}
	asset := "https://github.com/XTLS/Xray-core/releases/download/v1.0.0/Xray-linux-64.zip"
	for i := 0; i < 2; i++ {
		if code, body := get(asset); code != 200 || body != "zipdata" {
			t.Fatalf("asset #%d: %d %q", i, code, body)
		}
	}
	api := "https://api.github.com/repos/XTLS/Xray-core/releases/tags/v1.0.0"
	for i := 0; i < 2; i++ {
		if code, body := get(api); code != 200 || body != `{"assets":[]}` {
			t.Fatalf("api #%d: %d %q", i, code, body)
		}
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("upstream hit %d times, want 2 (both cached)", n)
	}
	if code, _ := get("https://github.com/XTLS/Xray-core/releases/download/v9/missing.zip"); code != http.StatusBadGateway {
		t.Errorf("missing asset: %d", code)
	}
	if code, _ := get("https://github.com/evil/x/releases/download/v1/a"); code != http.StatusBadRequest {
		t.Errorf("foreign repo: %d", code)
	}
}
