package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestGitHubLogin(t *testing.T) {
	// Fake GitHub: code "good" belongs to user 4242 (octo).
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			r.ParseForm()
			if r.Form.Get("client_secret") != "s3cret" || r.Form.Get("code") != "good" {
				json.NewEncoder(w).Encode(map[string]string{"error_description": "bad code"})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
		case "/user":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 4242, "login": "octo"})
		}
	}))
	defer gh.Close()
	ghOAuthWeb, ghOAuthAPI = gh.URL, gh.URL
	defer func() { ghOAuthWeb, ghOAuthAPI = "https://github.com", "https://api.github.com" }()

	s, srv, c := newAdminClient(t)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Not configured yet.
	if code, _ := c.do("POST", "/auth/github/login-url", nil); code != 400 {
		t.Fatalf("unconfigured: %d", code)
	}
	c.do("PUT", "/settings", map[string]string{"github_client_id": "cid", "github_client_secret": "s3cret"})
	_, info := c.do("GET", "/settings", nil)
	if v, ok := info["github_client_secret"]; ok && v != "" {
		t.Fatalf("secret leaked through settings: %v", v)
	}
	// callback follows the GitHub redirect: returns the fragment the browser lands on.
	callback := func(state, code string) url.Values {
		resp, err := noFollow.Get(c.base + "/auth/github/callback?state=" + url.QueryEscape(state) + "&code=" + code)
		if err != nil {
			t.Fatal(err)
		}
		loc := resp.Header.Get("Location")
		frag, _ := url.ParseQuery(loc[strings.Index(loc, "#")+1:])
		return frag
	}
	stateOf := func(u string) string {
		pu, _ := url.Parse(u)
		if pu.Query().Get("client_id") != "cid" || !strings.HasSuffix(pu.Query().Get("redirect_uri"), "/api/v1/auth/github/callback") {
			t.Fatalf("authorize url: %s", u)
		}
		return pu.Query().Get("state")
	}

	// Login before binding: refused with a clear reason.
	_, out := c.do("POST", "/auth/github/login-url", nil)
	if f := callback(stateOf(out["url"].(string)), "good"); !strings.Contains(f.Get("gh_error"), "未绑定") {
		t.Fatalf("unbound login: %v", f)
	}
	// Bind while signed in.
	_, out = c.do("POST", "/auth/github/bind-url", nil)
	st := stateOf(out["url"].(string))
	if f := callback(st, "good"); f.Get("gh_bound") != "octo" {
		t.Fatalf("bind: %v", f)
	}
	// A state works once.
	if f := callback(st, "good"); !strings.Contains(f.Get("gh_error"), "过期") {
		t.Fatalf("state reuse: %v", f)
	}
	// Login now issues a working session.
	_, out = c.do("POST", "/auth/github/login-url", nil)
	f := callback(stateOf(out["url"].(string)), "good")
	if f.Get("token") == "" || f.Get("user") != "root" {
		t.Fatalf("login: %v", f)
	}
	gc := &apiClient{t: t, base: c.base, tok: f.Get("token")}
	if code, _ := gc.do("GET", "/auth/me", nil); code != 200 {
		t.Fatalf("github session rejected: %d", code)
	}
	// A bad code fails cleanly.
	_, out = c.do("POST", "/auth/github/login-url", nil)
	if f := callback(stateOf(out["url"].(string)), "bad"); !strings.Contains(f.Get("gh_error"), "授权失败") {
		t.Fatalf("bad code: %v", f)
	}

	// With 2FA on, GitHub alone is not enough.
	secret := "JBSWY3DPEHPK3PXP"
	s.st.DB.Model(&store.Admin{}).Where("id = ?", "a1").Updates(map[string]any{"totp_enabled": true, "totp_secret": s.st.Box.Seal(secret)})
	_, out = c.do("POST", "/auth/github/login-url", nil)
	f = callback(stateOf(out["url"].(string)), "good")
	if f.Get("token") != "" || f.Get("gh_ticket") == "" {
		t.Fatalf("2FA bypassed: %v", f)
	}
	anon := &apiClient{t: t, base: c.base}
	if code, _ := anon.do("POST", "/auth/github/totp", map[string]string{"ticket": f.Get("gh_ticket"), "totp": "000000"}); code != 401 {
		t.Fatalf("wrong code accepted: %d", code)
	}
	_, out = c.do("POST", "/auth/github/login-url", nil)
	f = callback(stateOf(out["url"].(string)), "good")
	code6, _ := totp.GenerateCode(secret, time.Now())
	if code, out := anon.do("POST", "/auth/github/totp", map[string]string{"ticket": f.Get("gh_ticket"), "totp": code6}); code != 200 || out["token"] == "" {
		t.Fatalf("totp completion: %d %v", code, out)
	}

	// Unbind.
	c.do("POST", "/auth/github/unbind", nil)
	_, out = c.do("POST", "/auth/github/login-url", nil)
	if f := callback(stateOf(out["url"].(string)), "good"); !strings.Contains(f.Get("gh_error"), "未绑定") {
		t.Fatalf("after unbind: %v", f)
	}
	_ = srv
}
