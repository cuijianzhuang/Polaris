package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSSO(t *testing.T) {
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"authorization_endpoint": idp.URL + "/authorize",
				"token_endpoint": idp.URL + "/token", "userinfo_endpoint": idp.URL + "/userinfo"})
		case "/token":
			r.ParseForm()
			if r.Form.Get("code_verifier") == "" || r.Form.Get("client_secret") != "s3cret" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-" + r.Form.Get("code")})
		case "/userinfo":
			who := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
			json.NewEncoder(w).Encode(map[string]string{"sub": "sub-" + who, "preferred_username": who})
		default:
			w.WriteHeader(404)
		}
	}))
	defer idp.Close()

	s, _, admin := newAdminClient(t)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	code, p := admin.do("POST", "/sso/providers", map[string]any{"name": "Keycloak", "kind": "oidc", "issuer": idp.URL,
		"clientId": "polaris", "clientSecret": "s3cret"})
	if code != 201 || p["authUrl"] != idp.URL+"/authorize" {
		t.Fatalf("create %d %v", code, p)
	}
	pid := p["id"].(string)
	if _, info := admin.do("GET", "/info", nil); len(info["sso"].([]any)) != 1 {
		t.Fatalf("login page buttons: %v", info["sso"])
	}
	// callback follows the provider's redirect back to the panel.
	callback := func(authURL, code string) url.Values {
		u, _ := url.Parse(authURL)
		if u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("scope") != "openid profile email" {
			t.Fatalf("authorize url: %s", authURL)
		}
		resp, err := noFollow.Get(admin.base + "/auth/sso/callback?state=" + url.QueryEscape(u.Query().Get("state")) + "&code=" + code)
		if err != nil {
			t.Fatal(err)
		}
		loc := resp.Header.Get("Location")
		f, _ := url.ParseQuery(loc[strings.Index(loc, "#")+1:])
		return f
	}
	anon := &apiClient{t: t, base: admin.base}
	_, out := anon.do("POST", "/auth/sso/"+pid+"/login-url", nil)
	if f := callback(out["url"].(string), "alice"); !strings.Contains(f.Get("gh_error"), "未绑定") {
		t.Fatalf("unbound login: %v", f)
	}
	_, out = admin.do("POST", "/auth/sso/"+pid+"/bind-url", nil)
	if f := callback(out["url"].(string), "alice"); f.Get("sso_bound") != "Keycloak · alice" {
		t.Fatalf("bind: %v", f)
	}
	_, out = anon.do("POST", "/auth/sso/"+pid+"/login-url", nil)
	f := callback(out["url"].(string), "alice")
	if f.Get("token") == "" || f.Get("user") != "root" {
		t.Fatalf("login: %v", f)
	}
	if st, _ := (&apiClient{t: t, base: admin.base, tok: f.Get("token")}).do("GET", "/auth/me", nil); st != 200 {
		t.Fatalf("session: %d", st)
	}
	// Another external account is not this admin.
	_, out = anon.do("POST", "/auth/sso/"+pid+"/login-url", nil)
	if f := callback(out["url"].(string), "mallory"); f.Get("token") != "" {
		t.Fatal("unbound account signed in")
	}
	// A state works once.
	_, out = anon.do("POST", "/auth/sso/"+pid+"/login-url", nil)
	callback(out["url"].(string), "alice")
	if f := callback(out["url"].(string), "alice"); !strings.Contains(f.Get("gh_error"), "过期") {
		t.Fatalf("state reuse: %v", f)
	}
	var n int64
	s.st.DB.Table("sso_bindings").Count(&n)
	if code, _ := admin.do("DELETE", "/sso/providers/"+pid, nil); code != 204 {
		t.Fatal("delete")
	}
	var after int64
	s.st.DB.Table("sso_bindings").Count(&after)
	if n != 1 || after != 0 {
		t.Fatalf("bindings %d -> %d", n, after)
	}
}
