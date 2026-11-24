package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Unified login-page dispatch (anylogin.go): one entry per method,
// the callback resolves admin first, then registered user. These
// tests pin the resolution order and the fall-through rules; the
// dedicated flows keep their own suites.

func bindAdminGitHub(t *testing.T, s *Server, ghID int64, login string) {
	t.Helper()
	if err := s.st.DB.Model(&store.Admin{}).Where("id = ?", "a1").Updates(map[string]any{"git_hub_id": ghID, "git_hub_login": login}).Error; err != nil {
		t.Fatal(err)
	}
}

func anyGitHubCallback(t *testing.T, base string, code string) (frag map[string]string, raw string) {
	t.Helper()
	anon := &apiClient{t: t, base: base}
	_, out := anon.do("POST", "/auth/github/any/login-url", nil)
	f, loc := ghCallback(t, base, ghState(t, out["url"].(string)), code)
	m := map[string]string{}
	for k := range f {
		m[k] = f.Get(k)
	}
	return m, loc
}

func meRole(t *testing.T, base, token string) (int, map[string]any) {
	t.Helper()
	mc := &apiClient{t: t, base: base, tok: token}
	return mc.do("GET", "/auth/me", nil)
}

func TestGitHubAnyAdminFirstThenUser(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	useFakeGitHub(t)
	configureGitHub(t, admin)

	// Only the admin holds 4242: the unified button signs the admin in.
	bindAdminGitHub(t, s, 4242, "octo")
	frag, _ := anyGitHubCallback(t, admin.base, "good")
	if frag["token"] == "" {
		t.Fatalf("any github admin login: %v", frag)
	}
	if code, me := meRole(t, admin.base, frag["token"]); code != 200 || me["username"] != "root" || me["role"] == "member" {
		t.Fatalf("any github admin me: %d %v", code, me)
	}

	// Only a user holds 5555: the same button signs the member in.
	ghUserRow(t, s, "ghuser", 5555, "hubot")
	frag, _ = anyGitHubCallback(t, admin.base, "good2")
	if frag["token"] == "" {
		t.Fatalf("any github user login: %v", frag)
	}
	if code, me := meRole(t, admin.base, frag["token"]); code != 200 || me["role"] != "member" {
		t.Fatalf("any github user me: %d %v", code, me)
	}

	// Both sides hold 4242: the admin binding wins.
	ghUserRow(t, s, "ghboth", 4242, "octo")
	frag, _ = anyGitHubCallback(t, admin.base, "good")
	if frag["token"] == "" {
		t.Fatalf("any github both-bound login: %v", frag)
	}
	if code, me := meRole(t, admin.base, frag["token"]); code != 200 || me["username"] != "root" || me["role"] == "member" {
		t.Fatalf("any github both-bound me: %d %v", code, me)
	}
}

func TestGitHubAnyUnboundRegistersOrRefuses(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	useFakeGitHub(t)
	configureGitHub(t, admin)

	// Registration closed: an unbound GitHub is refused, no account.
	frag, _ := anyGitHubCallback(t, admin.base, "good3")
	if frag["token"] != "" || !strings.Contains(frag["gh_error"], "未绑定") {
		t.Fatalf("any github unbound closed: %v", frag)
	}
	var n int64
	s.st.DB.Model(&store.User{}).Where("git_hub_id = ?", 7777).Count(&n)
	if n != 0 {
		t.Fatalf("refused any github login created %d users", n)
	}

	// Registration open: the same identity registers and signs in.
	s.st.SetSetting("portal_email_register", "true")
	frag, _ = anyGitHubCallback(t, admin.base, "good3")
	if frag["token"] == "" {
		t.Fatalf("any github unbound open: %v", frag)
	}
	if code, me := meRole(t, admin.base, frag["token"]); code != 200 || me["role"] != "member" {
		t.Fatalf("any github registered me: %d %v", code, me)
	}
	s.st.DB.Model(&store.User{}).Where("git_hub_id = ?", 7777).Count(&n)
	if n != 1 {
		t.Fatalf("any github registration created %d users", n)
	}
}

func anySSOCallback(t *testing.T, base, pid, code string) map[string]string {
	t.Helper()
	anon := &apiClient{t: t, base: base}
	_, out := anon.do("POST", "/auth/sso/any/login-url", map[string]any{"providerId": pid})
	loc, _ := ssoCallback(t, base, out["url"].(string), code)
	f := fragOf(t, loc)
	m := map[string]string{}
	for k := range f {
		m[k] = f.Get(k)
	}
	return m
}

func TestSSOAnyAdminFirstThenUser(t *testing.T) {
	s, admin, pid := userSSOSetup(t)
	u := memberUser(t, s, "ssoalice", "alice@example.com")
	bindUserSSO(t, s, pid, "sub-alice", u.ID, "alice")
	if err := s.st.DB.Create(&store.SSOBinding{ProviderID: pid, Subject: "sub-alice", AdminID: "a1", Login: "alice"}).Error; err != nil {
		t.Fatal(err)
	}

	// Both sides bound: the admin binding wins.
	frag := anySSOCallback(t, admin.base, pid, "alice")
	if frag["token"] == "" {
		t.Fatalf("any sso both-bound login: %v", frag)
	}
	if code, me := meRole(t, admin.base, frag["token"]); code != 200 || me["username"] != "root" || me["role"] == "member" {
		t.Fatalf("any sso both-bound me: %d %v", code, me)
	}

	// Admin binding removed: the same identity signs the member in.
	s.st.DB.Where("provider_id = ? AND subject = ?", pid, "sub-alice").Delete(&store.SSOBinding{})
	frag = anySSOCallback(t, admin.base, pid, "alice")
	if frag["token"] == "" {
		t.Fatalf("any sso user login: %v", frag)
	}
	if code, me := meRole(t, admin.base, frag["token"]); code != 200 || me["role"] != "member" {
		t.Fatalf("any sso user me: %d %v", code, me)
	}
}

func TestSSOAnyUserSideOffKeepsAdminSemantics(t *testing.T) {
	s, admin, pid := userSSOSetup(t)
	u := memberUser(t, s, "ssoalice", "alice@example.com")
	bindUserSSO(t, s, pid, "sub-alice", u.ID, "alice")
	s.st.SetSetting("sso_user_login", "false")

	// Bound only on the user side, user SSO off: refused like any
	// unbound admin identity — no member session, no merge.
	frag := anySSOCallback(t, admin.base, pid, "alice")
	if frag["token"] != "" || !strings.Contains(frag["gh_error"], "未绑定任何管理员") {
		t.Fatalf("any sso with user side off: %v", frag)
	}

	// The admin side still signs in through the unified button.
	if err := s.st.DB.Create(&store.SSOBinding{ProviderID: pid, Subject: "sub-alice", AdminID: "a1", Login: "alice"}).Error; err != nil {
		t.Fatal(err)
	}
	frag = anySSOCallback(t, admin.base, pid, "alice")
	if frag["token"] == "" {
		t.Fatalf("any sso admin login with user side off: %v", frag)
	}
	if code, me := meRole(t, admin.base, frag["token"]); code != 200 || me["username"] != "root" {
		t.Fatalf("any sso admin me: %d %v", code, me)
	}
}

func TestSSOAnyUnboundNoMerge(t *testing.T) {
	s, admin, pid := userSSOSetup(t)
	memberUser(t, s, "existing", "alice@example.com")
	frag := anySSOCallback(t, admin.base, pid, "alice")
	if frag["token"] != "" || !strings.Contains(frag["gh_error"], "绑定管理中绑定") {
		t.Fatalf("any sso email-matched unbound: %v", frag)
	}
}

func anyPasskeyLogin(t *testing.T, base string, auth *softAuth, handle []byte, counter uint32) (int, map[string]any) {
	t.Helper()
	anon := &apiClient{t: t, base: base}
	code, out := anon.do("POST", "/auth/passkeys/any/login/begin", nil)
	if code != 200 {
		t.Fatalf("any passkey begin: %d %v", code, out)
	}
	cred := auth.assertionPayload(t, "https://panel.example.com", challengeOf(t, out), handle, counter)
	return anon.do("POST", "/auth/passkeys/any/login/finish", map[string]any{"challengeId": out["challengeId"], "credential": cred})
}

func TestPasskeyAnyDispatchesByCredentialTable(t *testing.T) {
	_, admin, mc, u := userPasskeySetup(t)

	// An admin key, registered through the admins' own ceremony.
	adminAuth := newSoftAuth(t)
	if code, out := admin.do("POST", "/auth/sudo", map[string]any{"password": "old-password"}); code != 200 {
		t.Fatalf("sudo: %d %v", code, out)
	}
	code, out := admin.do("POST", "/auth/passkeys/register/begin", map[string]any{"name": "管理员密钥"})
	if code != 200 {
		t.Fatalf("admin register begin: %d %v", code, out)
	}
	cred := adminAuth.createPayload("https://panel.example.com", challengeOf(t, out))
	if code, out := admin.do("POST", "/auth/passkeys/register/finish", map[string]any{"challengeId": out["challengeId"], "credential": cred}); code != 201 {
		t.Fatalf("admin register finish: %d %v", code, out)
	}
	// A user key for the member.
	userAuth := newSoftAuth(t)
	registerPanelPasskey(t, mc, userAuth, "用户密钥")

	// The unified ceremony signs the admin in with the admin's key…
	code, out = anyPasskeyLogin(t, admin.base, adminAuth, []byte("a1"), 2)
	if code != 200 || out["token"] == "" {
		t.Fatalf("any passkey admin login: %d %v", code, out)
	}
	if c, me := meRole(t, admin.base, out["token"].(string)); c != 200 || me["username"] != "root" || me["role"] == "member" {
		t.Fatalf("any passkey admin me: %d %v", c, me)
	}
	// …and the member in with the member's key.
	code, out = anyPasskeyLogin(t, admin.base, userAuth, []byte(u.ID), 2)
	if code != 200 || out["token"] == "" {
		t.Fatalf("any passkey user login: %d %v", code, out)
	}
	if c, me := meRole(t, admin.base, out["token"].(string)); c != 200 || me["role"] != "member" {
		t.Fatalf("any passkey user me: %d %v", c, me)
	}
	// An unknown credential is refused with the one vague answer.
	stranger := newSoftAuth(t)
	if code, _ := anyPasskeyLogin(t, admin.base, stranger, []byte(u.ID), 2); code != 401 {
		t.Fatalf("any passkey unknown credential: %d", code)
	}
	// The dedicated endpoints are untouched by the unified pair.
	if code, _ := panelPasskeyLogin(t, admin.base, userAuth, []byte(u.ID), 3); code != 200 {
		t.Fatalf("dedicated user passkey login after any: %d", code)
	}
}
