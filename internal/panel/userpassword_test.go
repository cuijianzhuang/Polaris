package panel

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// pwUser creates a user with a sign-in password (MinCost hash: the
// verification path is cost-agnostic and the suite stays fast).
func pwUser(t *testing.T, s *Server, id, username, email, password string) store.User {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	u := store.User{ID: id, Username: username, Email: email, SubToken: id + "-sub-token",
		UUID: id + "-uuid", Password: "core-" + id, PasswordHash: string(h), Status: "active"}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

func portalBaseOf(s *Server, admin *apiClient) string {
	return strings.TrimSuffix(admin.base, s.cfg.APIBase())
}

func memberPasswordLogin(t *testing.T, base, username, password string) (int, map[string]any) {
	t.Helper()
	anon := &apiClient{t: t, base: base}
	return anon.do("POST", "/auth/member/password", map[string]any{"username": username, "password": password})
}

func TestPasswordRegisterAndLoginBothFaces(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("register_password", "true")
	browser := newShopClient(t, portalBaseOf(s, admin))

	// Register in the user center: straight in with a portal session.
	if code, body := browser.req("POST", portalPath+"/password/register", map[string]any{
		"username": "PwAlice", "password": "password-123", "email": "pwalice@example.com",
	}, true); code != 200 {
		t.Fatalf("portal password register: %d %s", code, body)
	}
	var u store.User
	if err := s.st.DB.First(&u, "LOWER(username) = ?", "pwalice").Error; err != nil {
		t.Fatalf("registered user missing: %v", err)
	}
	if _, page := browser.req("GET", portalPath, nil, false); !strings.Contains(page, u.SubToken) {
		t.Fatal("portal session after password register does not show the account")
	}
	// The same account signs into the panel, case-insensitively.
	code, out := memberPasswordLogin(t, admin.base, "PWALICE", "password-123")
	if code != 200 {
		t.Fatalf("member password login: %d %v", code, out)
	}
	mc := &apiClient{t: t, base: admin.base, tok: out["token"].(string)}
	if code, out := mc.do("GET", "/auth/me", nil); code != 200 || out["role"] != "member" {
		t.Fatalf("member me: %d %v", code, out)
	}
	// One account only, self-service, and the two passwords are
	// different worlds: the hash verifies the login password, the core
	// credential is the generated one and never the login password.
	var n int64
	s.st.DB.Model(&store.User{}).Where("is_service = ?", false).Count(&n)
	if n != 1 {
		t.Fatalf("user count after both faces: %d", n)
	}
	if !u.SelfService || u.PasswordHash == "" || !passwordMatches(u.PasswordHash, "password-123") {
		t.Fatalf("registered user password state: %+v", u)
	}
	if u.Password == "" || u.Password == "password-123" {
		t.Fatalf("core credential polluted by the login password: %q", u.Password)
	}
	if u.Email != "pwalice@example.com" {
		t.Fatalf("optional email not stored: %q", u.Email)
	}
}

func TestPasswordRegisterUsernameRules(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("register_password", "true")
	browser := newShopClient(t, portalBaseOf(s, admin))
	reg := func(username, password string) (int, string) {
		return browser.req("POST", portalPath+"/password/register", map[string]any{"username": username, "password": password}, true)
	}
	if code, body := reg("Bob2024", "password-123"); code != 200 {
		t.Fatalf("register: %d %s", code, body)
	}
	// Case variant of an existing name: rejected (case-insensitive
	// uniqueness), with a clear message — usernames are public.
	if code, body := reg("bob2024", "password-123"); code != 400 || !strings.Contains(body, "已被注册") {
		t.Fatalf("case variant: %d %s", code, body)
	}
	// Reserved names and the admin's own name are refused.
	for _, name := range []string{"Admin", "support", "ROOT"} {
		if code, body := reg(name, "password-123"); code != 400 {
			t.Fatalf("reserved/admin name %q: %d %s", name, code, body)
		}
	}
	// Format and password rules.
	for _, x := range []struct{ name, pw string }{
		{"ab", "password-123"}, {"a b", "password-123"}, {"alice!", "password-123"},
		{"-alice", "password-123"}, {"alice.", "password-123"}, {"valid_name", "short"},
	} {
		if code, body := reg(x.name, x.pw); code != 400 {
			t.Fatalf("bad registration %q: %d %s", x.name, code, body)
		}
	}

	// register_password off: both registration endpoints refuse, but
	// the already-registered user still signs in at the portal.
	s.st.SetSetting("register_password", "false")
	if code, _ := browser.req("POST", portalPath+"/password/register", map[string]any{"username": "carol", "password": "password-123"}, true); code != 403 {
		t.Fatalf("portal register with switch off: %d", code)
	}
	anon := &apiClient{t: t, base: admin.base}
	if code, _ := anon.do("POST", "/auth/member/register", map[string]any{"username": "carol", "password": "password-123"}); code != 403 {
		t.Fatalf("panel register with switch off: %d", code)
	}
	if code, body := browser.req("POST", portalPath+"/password/login", map[string]any{"username": "Bob2024", "password": "password-123"}, true); code != 200 {
		t.Fatalf("portal password login with register off: %d %s", code, body)
	}

	// Panel admission off: the panel face refuses register and login;
	// the portal face is unaffected.
	s.st.SetSetting("register_password", "true")
	s.st.SetSetting("panel_member_login", "false")
	if code, _ := anon.do("POST", "/auth/member/register", map[string]any{"username": "carol", "password": "password-123"}); code != 403 {
		t.Fatalf("panel register with admission off: %d", code)
	}
	if code, _ := memberPasswordLogin(t, admin.base, "Bob2024", "password-123"); code != 403 {
		t.Fatalf("panel password login with admission off: %d", code)
	}
}

func TestPasswordInfoFlags(t *testing.T) {
	s, admin, _ := memberSetup(t, false)
	anon := &apiClient{t: t, base: admin.base}
	info := func() map[string]any {
		_, out := anon.do("GET", "/info", nil)
		return out
	}
	if info()["memberPasswordLogin"] != false || info()["memberPasswordRegister"] != false {
		t.Fatalf("flags with admission off: %v", info())
	}
	s.st.SetSetting("panel_member_login", "true")
	if info()["memberPasswordLogin"] != true || info()["memberPasswordRegister"] != false {
		t.Fatalf("flags with admission on only: %v", info())
	}
	s.st.SetSetting("register_password", "true")
	if info()["memberPasswordRegister"] != true {
		t.Fatalf("flags with both on: %v", info())
	}
}

func TestPasswordLoginUnifiedFailures(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	memberUser(t, s, "mailonly", "mailonly@example.com") // no sign-in password
	pwUser(t, s, "pwok", "pwok", "", "right-pass-1")
	dis := pwUser(t, s, "pwdis", "pwdis", "", "right-pass-1")
	s.st.DB.Model(&store.User{}).Where("id = ?", dis.ID).Update("status", "disabled")

	for _, x := range []struct{ name, pw string }{
		{"mailonly", "whatever-123"},   // account without a password
		{"ghost-name", "whatever-123"}, // no such user
		{"pwok", "wrong-pass-1"},       // wrong password
		{"pwdis", "right-pass-1"},      // disabled account, right password
	} {
		code, out := memberPasswordLogin(t, admin.base, x.name, x.pw)
		if code != 401 || out["error"] != "用户名或密码错误" {
			t.Fatalf("login %q: %d %v", x.name, code, out)
		}
	}
	var fails int64
	s.st.DB.Model(&store.AuditLog{}).Where("action = ?", "user.password_login_failed").Count(&fails)
	if fails != 4 {
		t.Fatalf("password_login_failed audit rows: %d", fails)
	}
}

func TestPasswordLoginFailuresShareIPBan(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	s.st.SetSetting("login_ban_failures", "3")
	pwUser(t, s, "banuser", "banuser", "", "correct-horse-1")
	anon := &apiClient{t: t, base: admin.base}
	for i := range 3 {
		if code, _ := memberPasswordLogin(t, admin.base, "banuser", "wrong-horse-1"); code != 401 {
			t.Fatalf("failure %d: %d", i, code)
		}
	}
	// The ban pool is shared: the mail-code endpoint (a different face,
	// same loginGuard) now refuses this IP too.
	if code, out := anon.do("POST", "/auth/mail/code", map[string]any{"email": "banuser@example.com"}); code != 429 {
		t.Fatalf("mail code after password ban: %d %v", code, out)
	}
	if code, _ := memberPasswordLogin(t, admin.base, "banuser", "correct-horse-1"); code != 429 {
		t.Fatalf("password login while banned: %d", code)
	}
}

func TestPasswordChangeResetAndSetpw(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("register_password", "true")
	base := portalBaseOf(s, admin)
	browser := newShopClient(t, base)
	if code, body := browser.req("POST", portalPath+"/password/register", map[string]any{
		"username": "carolpw", "password": "first-pass-1", "email": "carol@example.com",
	}, true); code != 200 {
		t.Fatalf("register: %d %s", code, body)
	}
	var carol store.User
	s.st.DB.First(&carol, "username = ?", "carolpw")
	_, out := memberPasswordLogin(t, admin.base, "carolpw", "first-pass-1")
	mc := &apiClient{t: t, base: admin.base, tok: out["token"].(string)}

	// Change the password in the user center (current password proof):
	// every session on both faces ends, this one included.
	if code, body := browser.req("POST", portalPath+"/password/set", map[string]any{
		"currentPassword": "first-pass-1", "newPassword": "second-pass-2",
	}, true); code != 200 {
		t.Fatalf("password set: %d %s", code, body)
	}
	if _, page := browser.req("GET", portalPath, nil, false); strings.Contains(page, carol.SubToken) {
		t.Fatal("portal session survived its own password change")
	}
	if code, _ := mc.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("member session survived password change: %d", code)
	}
	if code, _ := memberPasswordLogin(t, admin.base, "carolpw", "first-pass-1"); code != 401 {
		t.Fatalf("old password still works: %d", code)
	}
	_, out = memberPasswordLogin(t, admin.base, "carolpw", "second-pass-2")
	mc = &apiClient{t: t, base: admin.base, tok: out["token"].(string)}

	// Reset through the mail code (portal face): same invalidation.
	if code, body := browser.req("POST", portalPath+"/email/code", map[string]any{"email": "carol@example.com", "purpose": "reset"}, true); code != 200 {
		t.Fatalf("reset code: %d %s", code, body)
	}
	code := lastCode(t, m, "carol@example.com", 1)
	if st, body := browser.req("POST", portalPath+"/email/verify", map[string]any{
		"email": "carol@example.com", "code": code, "purpose": "reset", "newPassword": "third-pass-3",
	}, true); st != 200 {
		t.Fatalf("reset verify: %d %s", st, body)
	}
	if c, _ := mc.do("GET", "/auth/me", nil); c != 401 {
		t.Fatalf("member session survived password reset: %d", c)
	}
	if c, _ := memberPasswordLogin(t, admin.base, "carolpw", "second-pass-2"); c != 401 {
		t.Fatalf("pre-reset password still works: %d", c)
	}
	if c, _ := memberPasswordLogin(t, admin.base, "carolpw", "third-pass-3"); c != 200 {
		t.Fatalf("reset password does not work: %d", c)
	}

	// A reset code is never mailed to an address without an account,
	// and the answer does not say so.
	if c, body := browser.req("POST", portalPath+"/email/code", map[string]any{"email": "ghost@example.com", "purpose": "reset"}, true); c != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("unknown reset address: %d %s", c, body)
	}
	if n := len(m.To("ghost@example.com")); n != 0 {
		t.Fatalf("reset code mailed to an unknown address (%d)", n)
	}

	// First password via the setpw code: an e-mail user with no
	// password proves mailbox ownership, then sets one.
	dave := memberUser(t, s, "davepw", "dave@example.com")
	daveBrowser := newShopClient(t, base)
	daveBrowser.req("POST", portalPath+"/email/code", map[string]any{"email": "dave@example.com"}, true)
	loginCode := lastCode(t, m, "dave@example.com", 1)
	if st, body := daveBrowser.req("POST", portalPath+"/email/verify", map[string]any{"email": "dave@example.com", "code": loginCode}, true); st != 200 {
		t.Fatalf("dave mail login: %d %s", st, body)
	}
	if st, body := daveBrowser.req("POST", portalPath+"/password/set", map[string]any{"newPassword": "dave-pass-1"}, true); st != 400 {
		t.Fatalf("set without proof: %d %s", st, body)
	}
	daveBrowser.req("POST", portalPath+"/email/code", map[string]any{"email": "dave@example.com", "purpose": "setpw"}, true)
	setCode := lastCode(t, m, "dave@example.com", 2)
	if st, body := daveBrowser.req("POST", portalPath+"/password/set", map[string]any{"newPassword": "dave-pass-1", "code": setCode}, true); st != 200 {
		t.Fatalf("set with code: %d %s", st, body)
	}
	var daveAfter store.User
	s.st.DB.First(&daveAfter, "id = ?", dave.ID)
	if !passwordMatches(daveAfter.PasswordHash, "dave-pass-1") {
		t.Fatal("setpw did not store the password")
	}
	if c, _ := memberPasswordLogin(t, admin.base, "davepw", "dave-pass-1"); c != 200 {
		t.Fatalf("panel login after setpw: %d", c)
	}
}

func TestPasswordEndpointsNeverCross(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true")
	pwUser(t, s, "crossuser", "crossuser", "", "user-pass-123")
	anon := &apiClient{t: t, base: admin.base}

	// User credentials at the admin endpoint: refused.
	if code, _ := anon.do("POST", "/auth/login", map[string]any{"username": "crossuser", "password": "user-pass-123"}); code != 401 {
		t.Fatalf("user credentials at /auth/login: %d", code)
	}
	// Admin credentials at both user endpoints: refused.
	if code, _ := memberPasswordLogin(t, admin.base, "root", "old-password"); code != 401 {
		t.Fatalf("admin credentials at member endpoint: %d", code)
	}
	browser := newShopClient(t, portalBaseOf(s, admin))
	if code, body := browser.req("POST", portalPath+"/password/login", map[string]any{"username": "root", "password": "old-password"}, true); code != 401 {
		t.Fatalf("admin credentials at portal endpoint: %d %s", code, body)
	}
	// Sanity: the user credentials work where they belong.
	if code, _ := memberPasswordLogin(t, admin.base, "crossuser", "user-pass-123"); code != 200 {
		t.Fatalf("user credentials at member endpoint: %d", code)
	}
}

func TestPasswordLoginCaseDuplicateFailsClosed(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	pwUser(t, s, "dup-a", "DupCase", "", "dup-pass-123")
	pwUser(t, s, "dup-b", "dupcase", "", "dup-pass-123")
	// The case-sensitive unique index let both rows in (legacy state);
	// the password login must refuse rather than pick one.
	if code, out := memberPasswordLogin(t, admin.base, "DUPCASE", "dup-pass-123"); code != 401 || out["error"] != "用户名或密码错误" {
		t.Fatalf("duplicate login: %d %v", code, out)
	}
	var n int64
	s.st.DB.Model(&store.AuditLog{}).Where("action = ? AND detail = ?", "user.password_login_failed", "duplicate").Count(&n)
	if n != 1 {
		t.Fatalf("duplicate audit rows: %d", n)
	}
}

func TestAdminSetsUserLoginPassword(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	u := memberUser(t, s, "erin", "erin@example.com")

	code, out := admin.do("PUT", "/users/"+u.ID, map[string]any{"username": "erin", "loginPassword": "erin-pass-99"})
	if code != 200 {
		t.Fatalf("admin set login password: %d %v", code, out)
	}
	if _, leaked := out["passwordHash"]; leaked {
		t.Fatalf("password hash leaked in the response: %v", out)
	}
	if out["hasLoginPassword"] != true {
		t.Fatalf("hasLoginPassword not reported: %v", out)
	}
	if code, _ := memberPasswordLogin(t, admin.base, "erin", "erin-pass-99"); code != 200 {
		t.Fatalf("login with admin-set password: %d", code)
	}
	// The core credential is a different field and did not move.
	if code, out := admin.do("GET", "/users/"+u.ID, nil); code != 200 || out["password"] != u.Password || out["hasLoginPassword"] != true {
		t.Fatalf("user after set: %d %v", code, out)
	}
	// Clearing works the same way, and is audited as a reset.
	if code, out := admin.do("PUT", "/users/"+u.ID, map[string]any{"username": "erin", "loginPassword": ""}); code != 200 || out["hasLoginPassword"] == true {
		t.Fatalf("admin clear login password: %d %v", code, out)
	}
	if code, _ := memberPasswordLogin(t, admin.base, "erin", "erin-pass-99"); code != 401 {
		t.Fatalf("login after clear: %d", code)
	}
	var n int64
	s.st.DB.Model(&store.AuditLog{}).Where("action = ?", "user.password_reset").Count(&n)
	if n != 2 {
		t.Fatalf("user.password_reset audit rows: %d", n)
	}
}

func TestPasswordRegisterTrialAndSharedIPLimit(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	for k, v := range map[string]string{
		"portal_enabled": "true", "register_password": "true", "portal_email_register": "true", "register_ip_daily": "2",
	} {
		s.st.SetSetting(k, v)
	}
	code, out := admin.do("POST", "/plans", map[string]any{"name": "试用", "trafficLimit": 5 << 30, "days": 3})
	if code != 200 {
		t.Fatalf("plan %d %v", code, out)
	}
	s.st.SetSetting("register_trial_plan", out["id"].(string))
	base := portalBaseOf(s, admin)

	// A password registration gets the trial plan like a mail one.
	b1 := newShopClient(t, base)
	if code, body := b1.req("POST", portalPath+"/password/register", map[string]any{"username": "trialuser", "password": "password-123"}, true); code != 200 {
		t.Fatalf("trial register: %d %s", code, body)
	}
	var tu store.User
	s.st.DB.First(&tu, "username = ?", "trialuser")
	if tu.PlanID != out["id"].(string) || tu.ExpireAt == nil {
		t.Fatalf("trial plan not applied: %+v", tu)
	}

	// The daily IP budget is one counter for both registration ways:
	// one mail registration fills it, the next password one is refused.
	b2 := newShopClient(t, base)
	b2.req("POST", portalPath+"/email/code", map[string]any{"email": "second@example.com", "purpose": "register"}, true)
	mailCode := lastCode(t, m, "second@example.com", 1)
	if st, body := b2.req("POST", portalPath+"/email/verify", map[string]any{"email": "second@example.com", "code": mailCode, "purpose": "register"}, true); st != 200 {
		t.Fatalf("mail register: %d %s", st, body)
	}
	b3 := newShopClient(t, base)
	if code, body := b3.req("POST", portalPath+"/password/register", map[string]any{"username": "thirduser", "password": "password-123"}, true); code != 400 || !strings.Contains(body, "太多了") {
		t.Fatalf("third registration over the shared cap: %d %s", code, body)
	}
}
