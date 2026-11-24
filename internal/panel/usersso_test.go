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

// Fake IdP for the user SSO flow. Authorization codes name personas;
// userinfo answers with the persona's claims:
//
//	alice → sub-alice, alice@example.com, verified (bool true)
//	bob   → sub-bob,   bob@example.com,   verified (bool true)
//	bob2  → sub-bob2,  robert@example.com, verified (string "true"), login "bob"
//	carol → sub-carol, carol@example.com, NOT verified
//	dave  → sub-dave,  no e-mail claim at all (OAuth 2.0 style)
type ssoPersona struct {
	sub, login, email string
	verified          any // nil = claim absent
}

var ssoPersonas = map[string]ssoPersona{
	"alice": {sub: "sub-alice", login: "alice", email: "alice@example.com", verified: true},
	"bob":   {sub: "sub-bob", login: "bob", email: "bob@example.com", verified: true},
	"bob2":  {sub: "sub-bob2", login: "bob", email: "robert@example.com", verified: "true"},
	"carol": {sub: "sub-carol", login: "carol", email: "carol@example.com", verified: false},
	"dave":  {sub: "sub-dave", login: "dave"},
}

func useFakeSSOIdP(t *testing.T) string {
	t.Helper()
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
			code := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
			p, ok := ssoPersonas[code]
			if !ok {
				w.WriteHeader(401)
				return
			}
			info := map[string]any{"sub": p.sub, "preferred_username": p.login}
			if p.email != "" {
				info["email"] = p.email
			}
			if p.verified != nil {
				info["email_verified"] = p.verified
			}
			json.NewEncoder(w).Encode(info)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(idp.Close)
	return idp.URL
}

// userSSOSetup builds a panel with member login and user SSO on, one
// OIDC provider against the fake IdP, and returns the provider id.
func userSSOSetup(t *testing.T) (*Server, *apiClient, string) {
	t.Helper()
	s, admin, _ := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("sso_user_login", "true")
	issuer := useFakeSSOIdP(t)
	code, p := admin.do("POST", "/sso/providers", map[string]any{"name": "Keycloak", "kind": "oidc", "issuer": issuer,
		"clientId": "polaris", "clientSecret": "s3cret"})
	if code != 201 {
		t.Fatalf("create provider: %d %v", code, p)
	}
	return s, admin, p["id"].(string)
}

// ssoCallback runs the shared SSO callback and returns the redirect
// Location plus the response cookies.
func ssoCallback(t *testing.T, apiBase, authURL, code string) (string, []*http.Cookie) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || u.Query().Get("state") == "" {
		t.Fatalf("authorize url: %s", authURL)
	}
	resp, err := ghNoFollow.Get(apiBase + "/auth/sso/callback?state=" + url.QueryEscape(u.Query().Get("state")) + "&code=" + url.QueryEscape(code))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.Header.Get("Location"), resp.Cookies()
}

func fragOf(t *testing.T, loc string) url.Values {
	t.Helper()
	i := strings.Index(loc, "#")
	if i < 0 {
		t.Fatalf("no fragment in redirect: %s", loc)
	}
	f, _ := url.ParseQuery(loc[i+1:])
	return f
}

func bindUserSSO(t *testing.T, s *Server, pid, subject, userID, login string) {
	t.Helper()
	if err := s.st.DB.Create(&store.UserSSOBinding{ProviderID: pid, Subject: subject, UserID: userID, Login: login}).Error; err != nil {
		t.Fatal(err)
	}
}

func userSSOBindingCount(t *testing.T, s *Server) int64 {
	t.Helper()
	var n int64
	s.st.DB.Model(&store.UserSSOBinding{}).Count(&n)
	return n
}

func TestSSOUserLoginBothFaces(t *testing.T) {
	s, admin, pid := userSSOSetup(t)
	u := memberUser(t, s, "ssoalice", "alice@example.com")
	bindUserSSO(t, s, pid, "sub-alice", u.ID, "alice")
	anon := &apiClient{t: t, base: admin.base}

	if _, info := anon.do("GET", "/info", nil); info["memberSSOLogin"] != true {
		t.Fatalf("info memberSSOLogin: %v", info)
	}

	// Panel: the member token lands in the fragment, like GitHub.
	_, out := anon.do("POST", "/auth/sso/user/login-url", map[string]any{"providerId": pid})
	loc, _ := ssoCallback(t, admin.base, out["url"].(string), "alice")
	frag := fragOf(t, loc)
	if frag.Get("token") == "" {
		t.Fatalf("panel sso login: %s", loc)
	}
	mc := &apiClient{t: t, base: admin.base, tok: frag.Get("token")}
	if code, me := mc.do("GET", "/auth/me", nil); code != 200 || me["role"] != "member" {
		t.Fatalf("member me after sso login: %d %v", code, me)
	}
	// The bindings view shows the SSO method and counts it.
	if code, view := mc.do("GET", "/auth/bindings", nil); code != 200 || len(view["sso"].([]any)) != 1 || view["count"].(float64) < 2 {
		t.Fatalf("bindings view after sso login: %d %v", code, view)
	}

	// Portal: the cookie is set on the callback and the session works.
	browser := newShopClient(t, portalBaseOf(s, admin))
	_, body := browser.req("POST", portalPath+"/sso/login-url", map[string]any{"providerId": pid}, true)
	var lr struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &lr); err != nil || lr.URL == "" {
		t.Fatalf("portal sso login-url: %s", body)
	}
	loc, cookies := ssoCallback(t, portalBaseOf(s, admin)+s.cfg.APIBase(), lr.URL, "alice")
	if !strings.HasSuffix(loc, portalPath) {
		t.Fatalf("portal sso login landing: %s", loc)
	}
	pu, _ := url.Parse(portalBaseOf(s, admin) + portalPath)
	browser.hc.Jar.SetCookies(pu, cookies)
	if code, body := browser.req("GET", portalPath+"/bindings", nil, false); code != 200 || !strings.Contains(body, `"providerId":"`+pid+`"`) {
		t.Fatalf("portal bindings after sso login: %d %s", code, body)
	}
}

func TestSSOUserLoginUnboundNoMerge(t *testing.T) {
	s, admin, pid := userSSOSetup(t)
	memberUser(t, s, "existing", "alice@example.com") // alice's verified e-mail already has an account
	memberUser(t, s, "caroluser", "carol@example.com")
	anon := &apiClient{t: t, base: admin.base}
	login := func(code string) url.Values {
		_, out := anon.do("POST", "/auth/sso/user/login-url", map[string]any{"providerId": pid})
		loc, _ := ssoCallback(t, admin.base, out["url"].(string), code)
		return fragOf(t, loc)
	}

	// Verified e-mail matching an existing account: refused with the
	// bind-after-login guidance — no session, no binding, no merge.
	f := login("alice")
	if f.Get("token") != "" || !strings.Contains(f.Get("gh_error"), "绑定管理中绑定") {
		t.Fatalf("email-matched unbound login: %v", f)
	}
	if n := userSSOBindingCount(t, s); n != 0 {
		t.Fatalf("email-matched login created %d bindings", n)
	}

	// Unverified e-mail: refused outright, even though it matches.
	f = login("carol")
	if f.Get("token") != "" || !strings.Contains(f.Get("gh_error"), "未提供已验证的邮箱") {
		t.Fatalf("unverified unbound login: %v", f)
	}
	// No e-mail claim at all (OAuth 2.0 style): refused the same way.
	f = login("dave")
	if f.Get("token") != "" || !strings.Contains(f.Get("gh_error"), "未提供已验证的邮箱") {
		t.Fatalf("email-less unbound login: %v", f)
	}
	if n := userSSOBindingCount(t, s); n != 0 {
		t.Fatalf("refused logins created %d bindings", n)
	}

	// A binding hit signs in on the subject alone: carol's unverified
	// e-mail does not matter once the binding exists.
	carol := memberUser(t, s, "carolbound", "")
	bindUserSSO(t, s, pid, "sub-carol", carol.ID, "carol")
	if f := login("carol"); f.Get("token") == "" {
		t.Fatalf("bound login with unverified email: %v", f)
	}
}

func TestSSOUserLoginRegistersWhenOpen(t *testing.T) {
	s, admin, pid := userSSOSetup(t)
	anon := &apiClient{t: t, base: admin.base}
	login := func(code string) url.Values {
		_, out := anon.do("POST", "/auth/sso/user/login-url", map[string]any{"providerId": pid})
		loc, _ := ssoCallback(t, admin.base, out["url"].(string), code)
		return fragOf(t, loc)
	}
	userCount := func() int64 {
		var n int64
		s.st.DB.Model(&store.User{}).Where("is_service = ?", false).Count(&n)
		return n
	}

	// Registration closed: bob's verified, unmatched e-mail is not
	// enough — refused, nothing created.
	if f := login("bob"); f.Get("token") != "" || !strings.Contains(f.Get("gh_error"), "未绑定") {
		t.Fatalf("unbound login with registration closed: %v", f)
	}
	if n := userCount(); n != 0 {
		t.Fatalf("registration closed created %d users", n)
	}

	// Registration open: the first login registers, binds and signs
	// in; the second lands on the same account through the binding.
	s.st.SetSetting("portal_email_register", "true")
	if f := login("bob"); f.Get("token") == "" {
		t.Fatalf("sso registration login: %v", f)
	}
	var u store.User
	if err := s.st.DB.First(&u, "email = ?", "bob@example.com").Error; err != nil {
		t.Fatalf("sso-registered user missing: %v", err)
	}
	if u.Username != "bob" || !u.SelfService {
		t.Fatalf("sso-registered user: %+v", u)
	}
	var b store.UserSSOBinding
	if err := s.st.DB.First(&b, "provider_id = ? AND subject = ?", pid, "sub-bob").Error; err != nil || b.UserID != u.ID {
		t.Fatalf("sso binding after registration: %v %+v", err, b)
	}
	if f := login("bob"); f.Get("token") == "" {
		t.Fatalf("second sso login: %v", f)
	}
	if n := userCount(); n != 1 {
		t.Fatalf("second sso login created another account: %d users", n)
	}

	// Username conflict: "bob" is taken now, so bob2 (same IdP login,
	// different e-mail) registers with a suffixed name.
	if f := login("bob2"); f.Get("token") == "" {
		t.Fatalf("sso registration with taken name: %v", f)
	}
	var u2 store.User
	if err := s.st.DB.First(&u2, "email = ?", "robert@example.com").Error; err != nil {
		t.Fatalf("second sso-registered user missing: %v", err)
	}
	if u2.Username == "bob" || !strings.HasPrefix(u2.Username, "bob_") {
		t.Fatalf("conflicting username not suffixed: %q", u2.Username)
	}
}

func TestSSOUserBindTicketFlow(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("sso_user_login", "true")
	issuer := useFakeSSOIdP(t)
	_, p := admin.do("POST", "/sso/providers", map[string]any{"name": "Keycloak", "kind": "oidc", "issuer": issuer,
		"clientId": "polaris", "clientSecret": "s3cret"})
	pid := p["id"].(string)
	ua := memberUser(t, s, "binda", "binda@example.com")
	ub := memberUser(t, s, "bindb", "bindb@example.com")
	ma := memberMailLogin(t, admin.base, m, ua.Email, 1)
	mb := memberMailLogin(t, admin.base, m, ub.Email, 1)

	bindTicket := func(mc *apiClient) string {
		_, out := mc.do("POST", "/auth/sso/user/bind-url", map[string]any{"providerId": pid})
		loc, _ := ssoCallback(t, admin.base, out["url"].(string), "alice")
		ticket := fragOf(t, loc).Get("sso_bind_ticket")
		if ticket == "" {
			t.Fatalf("bind landing: %s", loc)
		}
		return ticket
	}

	// B cannot redeem A's ticket, and the attempt voids it for A too.
	ticket := bindTicket(ma)
	if code, out := mb.do("POST", "/auth/bindings/sso/complete", map[string]any{"ticket": ticket}); code != 409 {
		t.Fatalf("cross-session redemption: %d %v", code, out)
	}
	if code, out := ma.do("POST", "/auth/bindings/sso/complete", map[string]any{"ticket": ticket}); code != 400 {
		t.Fatalf("voided ticket reuse: %d %v", code, out)
	}

	// Fresh flow redeemed by A: bound, and SSO login now lands on A.
	ticket = bindTicket(ma)
	if code, out := ma.do("POST", "/auth/bindings/sso/complete", map[string]any{"ticket": ticket}); code != 200 {
		t.Fatalf("bind complete: %d %v", code, out)
	}
	var b store.UserSSOBinding
	if err := s.st.DB.First(&b, "provider_id = ? AND subject = ?", pid, "sub-alice").Error; err != nil || b.UserID != ua.ID {
		t.Fatalf("binding after complete: %v %+v", err, b)
	}

	// Occupancy: B's bind attempt for the same subject is refused at
	// the callback; A's binding is untouched.
	_, out := mb.do("POST", "/auth/sso/user/bind-url", map[string]any{"providerId": pid})
	loc, _ := ssoCallback(t, admin.base, out["url"].(string), "alice")
	if f := fragOf(t, loc); !strings.Contains(f.Get("gh_error"), "已绑定其他账号") {
		t.Fatalf("occupied bind: %s", loc)
	}
	if err := s.st.DB.First(&b, "provider_id = ? AND subject = ?", pid, "sub-alice").Error; err != nil || b.UserID != ua.ID {
		t.Fatalf("binding after occupied attempt: %v %+v", err, b)
	}
}

func TestSSOUserBindPortalFlow(t *testing.T) {
	s, admin, pid := userSSOSetup(t)
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
	bindTicket := func(br *portalClient) string {
		_, body := br.req("POST", portalPath+"/sso/bind-url", map[string]any{"providerId": pid}, true)
		var lr struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(body), &lr); err != nil || lr.URL == "" {
			t.Fatalf("portal sso bind-url: %s", body)
		}
		loc, _ := ssoCallback(t, base+s.cfg.APIBase(), lr.URL, "alice")
		pu, _ := url.Parse(loc)
		ticket := pu.Query().Get("ssobind")
		if ticket == "" {
			t.Fatalf("portal bind landing: %s", loc)
		}
		return ticket
	}
	ticket := bindTicket(a)
	if code, body := b.req("POST", portalPath+"/bindings/sso/complete", map[string]any{"ticket": ticket}, true); code != 409 {
		t.Fatalf("cross-session portal redemption: %d %s", code, body)
	}
	if code, body := a.req("POST", portalPath+"/bindings/sso/complete", map[string]any{"ticket": ticket}, true); code != 400 {
		t.Fatalf("voided portal ticket reuse: %d %s", code, body)
	}
	ticket = bindTicket(a)
	if code, body := a.req("POST", portalPath+"/bindings/sso/complete", map[string]any{"ticket": ticket}, true); code != 200 {
		t.Fatalf("portal bind complete: %d %s", code, body)
	}
	var row store.UserSSOBinding
	if err := s.st.DB.First(&row, "provider_id = ? AND subject = ?", pid, "sub-alice").Error; err != nil || row.UserID != "pa" {
		t.Fatalf("portal binding row: %v %+v", err, row)
	}
}

func TestSSOUnbindRedLineAndSessionDeath(t *testing.T) {
	s, admin, pid := userSSOSetup(t)
	// The user's only method is the SSO binding (no e-mail, no
	// password): sign in through SSO itself.
	u := memberUser(t, s, "onlysso", "")
	bindUserSSO(t, s, pid, "sub-alice", u.ID, "alice")
	anon := &apiClient{t: t, base: admin.base}
	_, out := anon.do("POST", "/auth/sso/user/login-url", map[string]any{"providerId": pid})
	loc, _ := ssoCallback(t, admin.base, out["url"].(string), "alice")
	mc := &apiClient{t: t, base: admin.base, tok: fragOf(t, loc).Get("token")}
	if mc.tok == "" {
		t.Fatalf("sso login for unbind test: %s", loc)
	}
	// Last method: unbind refused.
	if code, out := mc.do("POST", "/auth/bindings/sso/unbind", map[string]any{"providerId": pid}); code != 409 || out["code"] != "last_login_method" {
		t.Fatalf("last-method unbind: %d %v", code, out)
	}
	// With a password as a second method, unbind needs it (second
	// factor) and then ends the member session it rode in on.
	hash, err := hashUserPassword("password-123")
	if err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("password_hash", hash)
	if code, out := mc.do("POST", "/auth/bindings/sso/unbind", map[string]any{"providerId": pid, "currentPassword": "wrong-password"}); code != 400 {
		t.Fatalf("unbind with wrong password: %d %v", code, out)
	}
	if code, out := mc.do("POST", "/auth/bindings/sso/unbind", map[string]any{"providerId": pid, "currentPassword": "password-123"}); code != 200 {
		t.Fatalf("unbind: %d %v", code, out)
	}
	if n := userSSOBindingCount(t, s); n != 0 {
		t.Fatalf("binding survived unbind: %d", n)
	}
	if code, _ := mc.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("member session after unbind: %d", code)
	}

	// Portal: an SSO session dies the moment its binding row is gone,
	// even without the epoch bump (the per-request fingerprint
	// recheck, design D7.6).
	bindUserSSO(t, s, pid, "sub-alice", u.ID, "alice")
	browser := newShopClient(t, portalBaseOf(s, admin))
	_, body := browser.req("POST", portalPath+"/sso/login-url", map[string]any{"providerId": pid}, true)
	var lr struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &lr); err != nil || lr.URL == "" {
		t.Fatalf("portal sso login-url: %s", body)
	}
	_, cookies := ssoCallback(t, portalBaseOf(s, admin)+s.cfg.APIBase(), lr.URL, "alice")
	pu, _ := url.Parse(portalBaseOf(s, admin) + portalPath)
	browser.hc.Jar.SetCookies(pu, cookies)
	if code, _ := browser.req("GET", portalPath+"/bindings", nil, false); code != 200 {
		t.Fatalf("portal sso session before row deletion: %d", code)
	}
	s.st.DB.Where("provider_id = ? AND user_id = ?", pid, u.ID).Delete(&store.UserSSOBinding{})
	if code, _ := browser.req("GET", portalPath+"/bindings", nil, false); code != 401 {
		t.Fatalf("portal sso session after binding row deleted: %d", code)
	}
}

func TestSSOUserSwitchOff(t *testing.T) {
	s, admin, pid := userSSOSetup(t)
	u := memberUser(t, s, "switched", "sw@example.com")
	bindUserSSO(t, s, pid, "sub-alice", u.ID, "alice")
	anon := &apiClient{t: t, base: admin.base}

	// Mint a user login state while the switch is on, then turn it
	// off: entry points refuse, /info hides the method, and the
	// in-flight callback refuses too.
	_, out := anon.do("POST", "/auth/sso/user/login-url", map[string]any{"providerId": pid})
	authURL := out["url"].(string)
	s.st.SetSetting("sso_user_login", "false")
	if code, _ := anon.do("POST", "/auth/sso/user/login-url", map[string]any{"providerId": pid}); code != 403 {
		t.Fatalf("panel login-url with switch off: %d", code)
	}
	browser := newShopClient(t, portalBaseOf(s, admin))
	if code, body := browser.req("POST", portalPath+"/sso/login-url", map[string]any{"providerId": pid}, true); code != 403 {
		t.Fatalf("portal login-url with switch off: %d %s", code, body)
	}
	if _, info := anon.do("GET", "/info", nil); info["memberSSOLogin"] != false {
		t.Fatalf("info with switch off: %v", info)
	}
	loc, _ := ssoCallback(t, admin.base, authURL, "alice")
	if f := fragOf(t, loc); f.Get("token") != "" || !strings.Contains(f.Get("gh_error"), "未开启") {
		t.Fatalf("callback with switch off: %s", loc)
	}
	// The admin face never reads the switch.
	if code, out := anon.do("POST", "/auth/sso/"+pid+"/login-url", nil); code != 200 || out["url"] == "" {
		t.Fatalf("admin login-url with user switch off: %d %v", code, out)
	}
}

func TestSSOAdminUserNamespacesIsolated(t *testing.T) {
	s, admin, pid := userSSOSetup(t)
	u := memberUser(t, s, "nsuser", "ns@example.com")
	bindUserSSO(t, s, pid, "sub-alice", u.ID, "alice")
	anon := &apiClient{t: t, base: admin.base}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	adminCallback := func(authURL, code string) url.Values {
		uu, _ := url.Parse(authURL)
		resp, err := noFollow.Get(admin.base + "/auth/sso/callback?state=" + url.QueryEscape(uu.Query().Get("state")) + "&code=" + code)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		loc := resp.Header.Get("Location")
		f, _ := url.ParseQuery(loc[strings.Index(loc, "#")+1:])
		return f
	}

	// The admin binds the same (provider, subject) in the admin
	// namespace — the user's binding neither blocks nor leaks.
	_, out := admin.do("POST", "/auth/sso/"+pid+"/bind-url", nil)
	if f := adminCallback(out["url"].(string), "alice"); f.Get("sso_bound") != "Keycloak · alice" {
		t.Fatalf("admin bind: %v", f)
	}
	// Admin login with alice lands on the admin; user login with
	// alice lands on the user.
	_, out = anon.do("POST", "/auth/sso/"+pid+"/login-url", nil)
	f := adminCallback(out["url"].(string), "alice")
	if f.Get("token") == "" || f.Get("user") != "root" {
		t.Fatalf("admin login: %v", f)
	}
	_, out = anon.do("POST", "/auth/sso/user/login-url", map[string]any{"providerId": pid})
	loc, _ := ssoCallback(t, admin.base, out["url"].(string), "alice")
	f = fragOf(t, loc)
	if f.Get("token") == "" {
		t.Fatalf("user login: %s", loc)
	}
	mc := &apiClient{t: t, base: admin.base, tok: f.Get("token")}
	if code, me := mc.do("GET", "/auth/me", nil); code != 200 || me["role"] != "member" {
		t.Fatalf("user session after sso login: %d %v", code, me)
	}
	// Deleting the provider clears both namespaces' bindings.
	if code, _ := admin.do("DELETE", "/sso/providers/"+pid, nil); code != 204 {
		t.Fatalf("delete provider: %d", code)
	}
	if n := userSSOBindingCount(t, s); n != 0 {
		t.Fatalf("user bindings after provider delete: %d", n)
	}
	var an int64
	s.st.DB.Table("sso_bindings").Count(&an)
	if an != 0 {
		t.Fatalf("admin bindings after provider delete: %d", an)
	}
}
