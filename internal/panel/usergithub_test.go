package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// useFakeGitHub points the OAuth endpoints at a test server whose
// authorization codes map to fixed GitHub identities:
//
//	good  → 4242 "octo"    good2 → 5555 "hubot"    good3 → 7777 "taken"
func useFakeGitHub(t *testing.T) {
	t.Helper()
	ids := map[string]githubUser{"good": {ID: 4242, Login: "octo"}, "good2": {ID: 5555, Login: "hubot"}, "good3": {ID: 7777, Login: "taken"}}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			r.ParseForm()
			if _, ok := ids[r.Form.Get("code")]; !ok || r.Form.Get("client_secret") != "s3cret" {
				json.NewEncoder(w).Encode(map[string]string{"error_description": "bad code"})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-" + r.Form.Get("code")})
		case "/user":
			code := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
			u, ok := ids[code]
			if !ok {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": u.ID, "login": u.Login})
		}
	}))
	t.Cleanup(gh.Close)
	oldWeb, oldAPI := ghOAuthWeb, ghOAuthAPI
	ghOAuthWeb, ghOAuthAPI = gh.URL, gh.URL
	t.Cleanup(func() { ghOAuthWeb, ghOAuthAPI = oldWeb, oldAPI })
}

func configureGitHub(t *testing.T, admin *apiClient) {
	t.Helper()
	admin.do("PUT", "/settings", map[string]string{"github_client_id": "cid", "github_client_secret": "s3cret"})
}

// ghState extracts the state from an authorize URL this server minted.
func ghState(t *testing.T, raw string) string {
	t.Helper()
	pu, err := url.Parse(raw)
	if err != nil || pu.Query().Get("state") == "" {
		t.Fatalf("authorize url: %s", raw)
	}
	return pu.Query().Get("state")
}

var ghNoFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// ghCallback runs the shared callback and returns the redirect's
// fragment values (panel landings) plus the raw Location.
func ghCallback(t *testing.T, apiBase, state, code string) (url.Values, string) {
	t.Helper()
	resp, err := ghNoFollow.Get(apiBase + "/auth/github/callback?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(code))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	frag, _ := url.ParseQuery(loc[strings.Index(loc, "#")+1:])
	return frag, loc
}

func ghUserRow(t *testing.T, s *Server, id string, ghID int64, ghLogin string) store.User {
	t.Helper()
	u := store.User{ID: id, Username: id, SubToken: id + "-sub-token", UUID: id + "-uuid",
		Password: "pw", Status: "active", GitHubID: ghID, GitHubLogin: ghLogin}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

func TestGitHubUserLoginBothFaces(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true")
	useFakeGitHub(t)
	configureGitHub(t, admin)
	u := ghUserRow(t, s, "ghuser", 4242, "octo")
	anon := &apiClient{t: t, base: admin.base}

	// Panel: the member token lands in the fragment, like the admin flow.
	_, out := anon.do("POST", "/auth/github/user/login-url", nil)
	frag, _ := ghCallback(t, admin.base, ghState(t, out["url"].(string)), "good")
	if frag.Get("token") == "" {
		t.Fatalf("panel github login: %v", frag)
	}
	mc := &apiClient{t: t, base: admin.base, tok: frag.Get("token")}
	if code, me := mc.do("GET", "/auth/me", nil); code != 200 || me["role"] != "member" {
		t.Fatalf("member me after github login: %d %v", code, me)
	}

	// Portal: the cookie is set on the callback and /u shows the account.
	browser := newShopClient(t, portalBaseOf(s, admin))
	_, body := browser.req("POST", portalPath+"/github/login-url", map[string]any{}, true)
	var lr struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &lr); err != nil || lr.URL == "" {
		t.Fatalf("portal github login-url: %s", body)
	}
	resp, err := ghNoFollow.Get(portalBaseOf(s, admin) + s.cfg.APIBase() + "/auth/github/callback?state=" + url.QueryEscape(ghState(t, lr.URL)) + "&code=good")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, portalPath) {
		t.Fatalf("portal github login landing: %s", loc)
	}
	// Hand the callback's cookie to the browser jar the way a real
	// browser would (the callback and /u are the same origin).
	for _, ck := range resp.Cookies() {
		browser.hc.Jar.SetCookies(resp.Request.URL, []*http.Cookie{ck})
	}
	if _, page := browser.req("GET", portalPath, nil, false); !strings.Contains(page, u.SubToken) {
		t.Fatal("portal session after github login does not show the account")
	}
}

func TestGitHubUserLoginRegistersWhenOpen(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	useFakeGitHub(t)
	configureGitHub(t, admin)
	anon := &apiClient{t: t, base: admin.base}
	login := func(code string) url.Values {
		_, out := anon.do("POST", "/auth/github/user/login-url", nil)
		frag, _ := ghCallback(t, admin.base, ghState(t, out["url"].(string)), code)
		return frag
	}

	// Registration closed on both switches: an unbound GitHub account
	// is refused with the bind-first guidance, and nothing is created.
	if f := login("good"); !strings.Contains(f.Get("gh_error"), "未绑定") || f.Get("token") != "" {
		t.Fatalf("unbound login with registration closed: %v", f)
	}
	var n int64
	s.st.DB.Model(&store.User{}).Where("is_service = ?", false).Count(&n)
	if n != 0 {
		t.Fatalf("registration closed created %d users", n)
	}

	// Open password registration: the first GitHub login registers,
	// binds and signs in; the second lands on the same account.
	s.st.SetSetting("register_password", "true")
	if f := login("good"); f.Get("token") == "" {
		t.Fatalf("github registration login: %v", f)
	}
	var u store.User
	if err := s.st.DB.First(&u, "git_hub_id = ?", 4242).Error; err != nil {
		t.Fatalf("github-registered user missing: %v", err)
	}
	if u.Username != "octo" || !u.SelfService || u.GitHubLogin != "octo" {
		t.Fatalf("github-registered user: %+v", u)
	}
	if f := login("good"); f.Get("token") == "" {
		t.Fatalf("second github login: %v", f)
	}
	s.st.DB.Model(&store.User{}).Where("is_service = ?", false).Count(&n)
	if n != 1 {
		t.Fatalf("second github login created another account: %d users", n)
	}
}

func TestGitHubRegisterUsernameConflictSuffix(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	useFakeGitHub(t)
	configureGitHub(t, admin)
	s.st.SetSetting("register_password", "true")
	memberUser(t, s, "octo", "") // occupies the derived name, no GitHub
	anon := &apiClient{t: t, base: admin.base}
	_, out := anon.do("POST", "/auth/github/user/login-url", nil)
	if frag, _ := ghCallback(t, admin.base, ghState(t, out["url"].(string)), "good"); frag.Get("token") == "" {
		t.Fatalf("github login with taken name: %v", frag)
	}
	var u store.User
	if err := s.st.DB.First(&u, "git_hub_id = ?", 4242).Error; err != nil {
		t.Fatal(err)
	}
	if u.Username == "octo" || !strings.HasPrefix(u.Username, "octo_") {
		t.Fatalf("conflict-derived username: %q", u.Username)
	}
}

func TestGitHubUserBindPanelFlow(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	useFakeGitHub(t)
	configureGitHub(t, admin)
	pwUser(t, s, "binder", "binder", "", "password-123")
	pwUser(t, s, "other", "other", "", "password-123")
	ghUserRow(t, s, "occupier", 7777, "taken")
	login := func(name string) *apiClient {
		code, out := memberPasswordLogin(t, admin.base, name, "password-123")
		if code != 200 {
			t.Fatalf("member login %s: %d %v", name, code, out)
		}
		return &apiClient{t: t, base: admin.base, tok: out["token"].(string)}
	}
	mc, oc := login("binder"), login("other")
	startBind := func(mc *apiClient, code string) (url.Values, string) {
		_, out := mc.do("POST", "/auth/github/user/bind-url", nil)
		return ghCallback(t, admin.base, ghState(t, out["url"].(string)), code)
	}

	// A GitHub account bound to someone else refuses in the callback,
	// before any ticket exists; nothing changes.
	if frag, _ := startBind(mc, "good3"); !strings.Contains(frag.Get("gh_error"), "已绑定其他账号") {
		t.Fatalf("occupied bind: %v", frag)
	}
	var chk store.User
	s.st.DB.First(&chk, "id = ?", "binder")
	if chk.GitHubID != 0 {
		t.Fatalf("occupied bind wrote GitHubID: %+v", chk)
	}

	// The redemption must happen under the session that started the
	// flow: another member's session voids the ticket.
	frag, _ := startBind(mc, "good")
	ticket := frag.Get("gh_bind_ticket")
	if ticket == "" {
		t.Fatalf("bind callback: %v", frag)
	}
	if code, out := oc.do("POST", "/auth/github/user/bind-complete", map[string]any{"ticket": ticket}); code != 409 {
		t.Fatalf("cross-session redemption: %d %v", code, out)
	}
	if code, out := mc.do("POST", "/auth/github/user/bind-complete", map[string]any{"ticket": ticket}); code != 400 {
		t.Fatalf("voided ticket reuse: %d %v", code, out)
	}

	// Fresh flow, redeemed by its owner: bound, and GitHub sign-in
	// now lands on this account.
	frag, _ = startBind(mc, "good")
	if code, out := mc.do("POST", "/auth/github/user/bind-complete", map[string]any{"ticket": frag.Get("gh_bind_ticket")}); code != 200 || out["githubLogin"] != "octo" {
		t.Fatalf("bind complete: %d %v", code, out)
	}
	s.st.DB.First(&chk, "id = ?", "binder")
	if chk.GitHubID != 4242 || chk.GitHubLogin != "octo" {
		t.Fatalf("bound user: %+v", chk)
	}
	anon := &apiClient{t: t, base: admin.base}
	_, out := anon.do("POST", "/auth/github/user/login-url", nil)
	frag, _ = ghCallback(t, admin.base, ghState(t, out["url"].(string)), "good")
	mc2 := &apiClient{t: t, base: admin.base, tok: frag.Get("token")}
	if code, me := mc2.do("GET", "/auth/me", nil); code != 200 || me["role"] != "member" || me["username"] != "binder" {
		t.Fatalf("github login after bind: %d %v", code, me)
	}
	// The member bindings view reflects the new method.
	if code, view := mc.do("GET", "/auth/bindings", nil); code != 200 || view["github"] != true || view["githubLogin"] != "octo" {
		t.Fatalf("member bindings view: %d %v", code, view)
	}
}

func TestGitHubUserBindPortalFlow(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true")
	useFakeGitHub(t)
	configureGitHub(t, admin)
	pwUser(t, s, "pa", "pa", "", "password-123")
	pwUser(t, s, "pb", "pb", "", "password-123")
	base := portalBaseOf(s, admin)
	signIn := func(name string) *portalClient {
		b := newShopClient(t, base)
		if code, body := b.req("POST", portalPath+"/password/login", map[string]any{"username": name, "password": "password-123"}, true); code != 200 {
			t.Fatalf("portal login %s: %d %s", name, code, body)
		}
		return b
	}
	a, b := signIn("pa"), signIn("pb")

	// A starts the bind; the callback parks a ticket in the query.
	_, body := a.req("POST", portalPath+"/github/bind-url", map[string]any{}, true)
	var lr struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &lr); err != nil || lr.URL == "" {
		t.Fatalf("portal bind-url: %s", body)
	}
	resp, err := ghNoFollow.Get(base + s.cfg.APIBase() + "/auth/github/callback?state=" + url.QueryEscape(ghState(t, lr.URL)) + "&code=good2")
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	pu, _ := url.Parse(loc)
	ticket := pu.Query().Get("ghbind")
	if ticket == "" {
		t.Fatalf("portal bind landing: %s", loc)
	}
	// B's session cannot redeem A's ticket, and the attempt voids it.
	if code, body := b.req("POST", portalPath+"/bindings/github/complete", map[string]any{"ticket": ticket}, true); code != 409 {
		t.Fatalf("cross-session portal redemption: %d %s", code, body)
	}
	if code, body := a.req("POST", portalPath+"/bindings/github/complete", map[string]any{"ticket": ticket}, true); code != 400 {
		t.Fatalf("voided portal ticket reuse: %d %s", code, body)
	}
	// Fresh flow redeemed by A: bound.
	_, body = a.req("POST", portalPath+"/github/bind-url", map[string]any{}, true)
	json.Unmarshal([]byte(body), &lr)
	resp, err = ghNoFollow.Get(base + s.cfg.APIBase() + "/auth/github/callback?state=" + url.QueryEscape(ghState(t, lr.URL)) + "&code=good2")
	if err != nil {
		t.Fatal(err)
	}
	loc = resp.Header.Get("Location")
	resp.Body.Close()
	pu, _ = url.Parse(loc)
	if code, body := a.req("POST", portalPath+"/bindings/github/complete", map[string]any{"ticket": pu.Query().Get("ghbind")}, true); code != 200 {
		t.Fatalf("portal bind complete: %d %s", code, body)
	}
	var chk store.User
	s.st.DB.First(&chk, "id = ?", "pa")
	if chk.GitHubID != 5555 || chk.GitHubLogin != "hubot" {
		t.Fatalf("portal bound user: %+v", chk)
	}
	if code, body := a.req("GET", portalPath+"/bindings", nil, false); code != 200 || !strings.Contains(body, `"github":true`) {
		t.Fatalf("portal bindings view: %d %s", code, body)
	}
}

func TestGitHubUnbindAndPasswordClearRedLine(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true")
	useFakeGitHub(t)
	configureGitHub(t, admin)
	base := portalBaseOf(s, admin)

	// A GitHub-only account: unbinding its last method is refused.
	ghUserRow(t, s, "ghonly", 4242, "octo")
	b := newShopClient(t, base)
	_, body := b.req("POST", portalPath+"/github/login-url", map[string]any{}, true)
	var lr struct {
		URL string `json:"url"`
	}
	json.Unmarshal([]byte(body), &lr)
	resp, err := ghNoFollow.Get(base + s.cfg.APIBase() + "/auth/github/callback?state=" + url.QueryEscape(ghState(t, lr.URL)) + "&code=good")
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range resp.Cookies() {
		b.hc.Jar.SetCookies(resp.Request.URL, []*http.Cookie{ck})
	}
	resp.Body.Close()
	if code, body := b.req("POST", portalPath+"/bindings/github/unbind", map[string]any{}, true); code != 409 || !strings.Contains(body, "至少保留一种登录方式") {
		t.Fatalf("unbind last method: %d %s", code, body)
	}

	// With a password added, unbind needs the password as second
	// factor, then succeeds — and the GitHub session dies with it.
	u := pwUser(t, s, "multi", "multi", "multi@example.com", "password-123")
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"git_hub_id": 5555, "git_hub_login": "hubot"})
	b2 := newShopClient(t, base)
	if code, body := b2.req("POST", portalPath+"/password/login", map[string]any{"username": "multi", "password": "password-123"}, true); code != 200 {
		t.Fatalf("portal login multi: %d %s", code, body)
	}
	if code, body := b2.req("POST", portalPath+"/bindings/github/unbind", map[string]any{}, true); code != 400 {
		t.Fatalf("unbind without second factor: %d %s", code, body)
	}
	if code, body := b2.req("POST", portalPath+"/bindings/github/unbind", map[string]any{"currentPassword": "wrong-password"}, true); code != 400 {
		t.Fatalf("unbind with wrong password: %d %s", code, body)
	}
	if code, body := b2.req("POST", portalPath+"/bindings/github/unbind", map[string]any{"currentPassword": "password-123"}, true); code != 200 {
		t.Fatalf("unbind github: %d %s", code, body)
	}
	var chk store.User
	s.st.DB.First(&chk, "id = ?", "multi")
	if chk.GitHubID != 0 || chk.GitHubLogin != "" {
		t.Fatalf("after unbind: %+v", chk)
	}
	// The session that unbound is itself over (epoch bumped): the
	// next call is anonymous again, and signing back in works.
	if code, _ := b2.req("GET", portalPath+"/bindings", nil, false); code != 401 {
		t.Fatalf("session after unbind: %d", code)
	}
	if code, body := b2.req("POST", portalPath+"/password/login", map[string]any{"username": "multi", "password": "password-123"}, true); code != 200 {
		t.Fatalf("re-login after unbind: %d %s", code, body)
	}
	// Password clear: e-mail remains, so it may go — with the current
	// password as proof. Then password login is over for good.
	if code, body := b2.req("POST", portalPath+"/bindings/password/clear", map[string]any{"currentPassword": "nope-nope"}, true); code != 400 {
		t.Fatalf("clear with wrong password: %d %s", code, body)
	}
	if code, body := b2.req("POST", portalPath+"/bindings/password/clear", map[string]any{"currentPassword": "password-123"}, true); code != 200 {
		t.Fatalf("clear password: %d %s", code, body)
	}
	s.st.DB.First(&chk, "id = ?", "multi")
	if chk.PasswordHash != "" || chk.Password == "" {
		t.Fatalf("after clear: hash %q core %q", chk.PasswordHash, chk.Password)
	}
	b3 := newShopClient(t, base)
	if code, _ := b3.req("POST", portalPath+"/password/login", map[string]any{"username": "multi", "password": "password-123"}, true); code != 401 {
		t.Fatalf("password login after clear: %d", code)
	}

	// A password-only account cannot clear its last method.
	pwUser(t, s, "pwonly", "pwonly", "", "password-123")
	b4 := newShopClient(t, base)
	if code, body := b4.req("POST", portalPath+"/password/login", map[string]any{"username": "pwonly", "password": "password-123"}, true); code != 200 {
		t.Fatalf("portal login pwonly: %d %s", code, body)
	}
	if code, body := b4.req("POST", portalPath+"/bindings/password/clear", map[string]any{"currentPassword": "password-123"}, true); code != 409 {
		t.Fatalf("clear last method: %d %s", code, body)
	}
}

func TestGitHubAdminUserNamespacesIsolated(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	useFakeGitHub(t)
	configureGitHub(t, admin)
	anon := &apiClient{t: t, base: admin.base}

	// The admin binds GitHub 4242 through the admin flow.
	_, out := admin.do("POST", "/auth/github/bind-url", nil)
	if frag, _ := ghCallback(t, admin.base, ghState(t, out["url"].(string)), "good"); frag.Get("gh_bound") != "octo" {
		t.Fatalf("admin bind: %v", frag)
	}
	// The user flow must not see the admin's binding: with
	// registration closed it answers "unbound", never a session.
	_, out = anon.do("POST", "/auth/github/user/login-url", nil)
	if frag, _ := ghCallback(t, admin.base, ghState(t, out["url"].(string)), "good"); frag.Get("token") != "" || !strings.Contains(frag.Get("gh_error"), "未绑定") {
		t.Fatalf("user login hit the admin binding: %v", frag)
	}
	// And the admin flow must not see a user's binding either.
	ghUserRow(t, s, "someuser", 5555, "hubot")
	_, out = anon.do("POST", "/auth/github/login-url", nil)
	if frag, _ := ghCallback(t, admin.base, ghState(t, out["url"].(string)), "good2"); frag.Get("token") != "" || !strings.Contains(frag.Get("gh_error"), "未绑定") {
		t.Fatalf("admin login hit the user binding: %v", frag)
	}
	// The admin's own GitHub login still works.
	_, out = anon.do("POST", "/auth/github/login-url", nil)
	if frag, _ := ghCallback(t, admin.base, ghState(t, out["url"].(string)), "good"); frag.Get("token") == "" || frag.Get("user") != "root" {
		t.Fatalf("admin github login: %v", frag)
	}
}

func TestMemberBindingsRouteGuard(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	useFakeGitHub(t)
	configureGitHub(t, admin)
	pwUser(t, s, "guarded", "guarded", "", "password-123")
	_, out := memberPasswordLogin(t, admin.base, "guarded", "password-123")
	mc := &apiClient{t: t, base: admin.base, tok: out["token"].(string)}

	// The bindings group is open to the member; the rest of the admin
	// self-service surface stays shut (three-table isolation).
	if code, view := mc.do("GET", "/auth/bindings", nil); code != 200 || view["count"] == nil {
		t.Fatalf("member bindings: %d %v", code, view)
	}
	for _, p := range []string{"/auth/github", "/auth/passkeys", "/settings", "/users"} {
		if code, _ := mc.do("GET", p, nil); code != 403 {
			t.Fatalf("member GET %s: %d", p, code)
		}
	}
	// The admin principal gets nothing from the member handlers.
	if code, _ := admin.do("GET", "/auth/bindings", nil); code != 403 {
		t.Fatalf("admin on member bindings: %d", code)
	}
	if code, _ := admin.do("POST", "/auth/github/user/bind-url", nil); code != 403 {
		t.Fatalf("admin on member bind-url: %d", code)
	}
	// Member admission off: the public user login entry closes too.
	s.st.SetSetting("panel_member_login", "false")
	anon := &apiClient{t: t, base: admin.base}
	if code, _ := anon.do("POST", "/auth/github/user/login-url", nil); code != 403 {
		t.Fatalf("user login-url with admission off: %d", code)
	}
}
