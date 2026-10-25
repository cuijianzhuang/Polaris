package panel

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/mailer/mailtest"
	"github.com/cuijianzhuang/polaris/internal/store"
)

var sixDigits = regexp.MustCompile(`\b\d{6}\b`)

// lastCode is the newest code mailed to addr.
func lastCode(t *testing.T, m *mailtest.Server, addr string, n int) string {
	t.Helper()
	waitFor(t, "code mail", func() bool { return len(m.To(addr)) >= n })
	ms := m.To(addr)
	return sixDigits.FindString(ms[len(ms)-1].Subject)
}

func TestMailLogin(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	m := useMail(t, s)
	s.st.SetSetting("portal_enabled", "true")
	s.mailCodeLimit = &sharedLimiter{s: s.shared, name: "mailcode-test", perMinute: 600000}
	u := newUser(&userReq{Username: "dave"})
	u.Email = "dave@example.com"
	s.st.DB.Create(&u)

	browser := newShopClient(t, srv.URL)
	if _, page := browser.req("GET", portalPath, nil, false); !strings.Contains(page, "发送验证码") {
		t.Fatal("no e-mail sign-in on the login page")
	}
	if code, _ := browser.req("POST", portalPath+"/email/code", map[string]any{"email": "dave@example.com"}, false); code != 403 {
		t.Fatal("request without the portal header accepted")
	}
	// Unknown address, sign-up off: the same answer, no mail.
	if code, body := browser.req("POST", portalPath+"/email/code", map[string]any{"email": "nobody@example.com"}, true); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("unknown address %d %s", code, body)
	}
	// Known address: a code arrives; a second request within a minute waits.
	if code, body := browser.req("POST", portalPath+"/email/code", map[string]any{"email": "Dave@Example.com"}, true); code != 200 {
		t.Fatalf("code %d %s", code, body)
	}
	code := lastCode(t, m, "dave@example.com", 1)
	if code == "" {
		t.Fatalf("no code in %+v", m.To("dave@example.com"))
	}
	if len(m.To("nobody@example.com")) != 0 {
		t.Fatal("mailed an address without an account")
	}
	if st, body := browser.req("POST", portalPath+"/email/code", map[string]any{"email": "dave@example.com"}, true); st != 429 || !strings.Contains(body, "秒后") {
		t.Fatalf("cooldown %d %s", st, body)
	}
	// The unknown address waits the same way: a 429 must not reveal an account.
	if st, body := browser.req("POST", portalPath+"/email/code", map[string]any{"email": "nobody@example.com"}, true); st != 429 || !strings.Contains(body, "秒后") {
		t.Fatalf("unknown address cooldown %d %s", st, body)
	}
	// A wrong code counts; the right one signs in once.
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if st, body := browser.req("POST", portalPath+"/email/verify", map[string]any{"email": "dave@example.com", "code": wrong}, true); st != 400 || !strings.Contains(body, "还能试 4 次") {
		t.Fatalf("wrong code %d %s", st, body)
	}
	if st, body := browser.req("POST", portalPath+"/email/verify", map[string]any{"email": "dave@example.com", "code": code}, true); st != 200 {
		t.Fatalf("verify %d %s", st, body)
	}
	if _, page := browser.req("GET", portalPath, nil, false); !strings.Contains(page, "已通过 邮箱 登录") || !strings.Contains(page, u.SubToken) {
		t.Fatalf("user center after e-mail sign-in: %.300s", page)
	}
	if st, _ := newShopClient(t, srv.URL).req("POST", portalPath+"/email/verify", map[string]any{"email": "dave@example.com", "code": code}, true); st != 400 {
		t.Fatal("a code worked twice")
	}

	// Changing the address ends the session made from the old one.
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("email", "dave2@example.com")
	if _, page := browser.req("GET", portalPath, nil, false); strings.Contains(page, u.SubToken) {
		t.Fatal("session survived an address change")
	}
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("email", "dave@example.com")

	// Five wrong codes burn it.
	other := newShopClient(t, srv.URL)
	s.eph.take(mailKey("login", "dave@example.com"), new(mailCode))
	other.req("POST", portalPath+"/email/code", map[string]any{"email": "dave@example.com"}, true)
	good := lastCode(t, m, "dave@example.com", 2)
	bad := "000000"
	if good == bad {
		bad = "111111"
	}
	for range mailCodeTries {
		other.req("POST", portalPath+"/email/verify", map[string]any{"email": "dave@example.com", "code": bad}, true)
	}
	if st, body := other.req("POST", portalPath+"/email/verify", map[string]any{"email": "dave@example.com", "code": good}, true); st != 400 || !strings.Contains(body, "失效") {
		t.Fatalf("code after too many tries %d %s", st, body)
	}
}

func TestMailRegister(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	m := useMail(t, s)
	s.mailCodeLimit = &sharedLimiter{s: s.shared, name: "mailcode-test", perMinute: 600000}
	code, out := admin.do("POST", "/plans", map[string]any{"name": "试用", "trafficLimit": 5 << 30, "days": 3})
	if code != 200 {
		t.Fatalf("plan %d %v", code, out)
	}
	trial := out["id"].(string)
	for k, v := range map[string]string{"portal_enabled": "true", "portal_email_register": "true", "register_email_domains": "gmail.com, qq.com", "register_ip_daily": "2"} {
		s.st.SetSetting(k, v)
	}
	signUp := func(addr string) (int, string) {
		b := newShopClient(t, srv.URL)
		n := len(m.To(addr))
		b.req("POST", portalPath+"/email/code", map[string]any{"email": addr}, true)
		if len(m.To(addr)) == n {
			time.Sleep(200 * time.Millisecond)
			if len(m.To(addr)) == n {
				return 0, "no mail"
			}
		}
		return b.req("POST", portalPath+"/email/verify", map[string]any{"email": addr, "code": lastCode(t, m, addr, n+1)}, true)
	}
	// Domain not allowed: no mail at all.
	if st, body := signUp("eve@example.com"); st != 0 {
		t.Fatalf("other domain %d %s", st, body)
	}
	// No trial plan: the account exists but has no service yet.
	if st, body := signUp("eve@gmail.com"); st != 200 || !strings.Contains(body, `"created":true`) {
		t.Fatalf("register %d %s", st, body)
	}
	var u store.User
	s.st.DB.First(&u, "email = ?", "eve@gmail.com")
	if !strings.HasPrefix(u.Username, "eve_") || !u.SelfService || u.Status != "expired" || u.Remark != "邮箱注册" {
		t.Fatalf("new account %+v", u)
	}
	// With a trial plan.
	s.st.SetSetting("register_trial_plan", trial)
	if st, body := signUp("frank@qq.com"); st != 200 {
		t.Fatalf("register with trial %d %s", st, body)
	}
	u = store.User{}
	s.st.DB.First(&u, "email = ?", "frank@qq.com")
	if u.PlanID != trial || u.Status != "active" || u.TrafficLimit != 5<<30 || u.ExpireAt == nil {
		t.Fatalf("trial account %+v", u)
	}
	// Two sign-ups per address a day.
	if st, body := signUp("gina@qq.com"); st != 400 || !strings.Contains(body, "太多") {
		t.Fatalf("third sign-up %d %s", st, body)
	}
	// Sign-up off: the code still checks out, but no account is made.
	s.st.SetSetting("portal_email_register", "false")
	if st, _ := signUp("hank@qq.com"); st != 0 {
		t.Fatal("mailed with sign-up off")
	}
}

// A user signed in with Telegram binds an address; the session stays.
func TestMailBind(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	m := useMail(t, s)
	s.st.SetSetting("portal_enabled", "true")
	s.mailCodeLimit = &sharedLimiter{s: s.shared, name: "mailcode-test", perMinute: 600000}
	u := newUser(&userReq{Username: "ivy"})
	u.TelegramID = 4242
	s.st.DB.Create(&u)
	taken := newUser(&userReq{Username: "jay"})
	taken.Email = "jay@example.com"
	s.st.DB.Create(&taken)
	tok, _ := s.issuePortalToken(&u)
	b := newShopClient(t, srv.URL)
	b.setCookie(portalCookie, tok)
	if _, page := b.req("GET", portalPath, nil, false); !strings.Contains(page, "绑定邮箱") {
		t.Fatal("no bind button")
	}
	if st, body := b.req("POST", portalPath+"/email/code", map[string]any{"email": "jay@example.com", "purpose": "bind"}, true); st != 400 || !strings.Contains(body, "其他用户") {
		t.Fatalf("taken address %d %s", st, body)
	}
	if st, _ := b.req("POST", portalPath+"/email/code", map[string]any{"email": "ivy@example.com", "purpose": "bind"}, true); st != 200 {
		t.Fatal("bind code")
	}
	code := lastCode(t, m, "ivy@example.com", 1)
	// The code only binds: it does not sign anyone in.
	if st, _ := newShopClient(t, srv.URL).req("POST", portalPath+"/email/verify", map[string]any{"email": "ivy@example.com", "code": code}, true); st != 400 {
		t.Fatal("bind code used to sign in")
	}
	if st, body := b.req("POST", portalPath+"/email/verify", map[string]any{"email": "ivy@example.com", "code": code, "purpose": "bind"}, true); st != 200 {
		t.Fatalf("bind %d %s", st, body)
	}
	s.st.DB.First(&u, "id = ?", u.ID)
	if u.Email != "ivy@example.com" {
		t.Fatal("not bound")
	}
	if _, page := b.req("GET", portalPath, nil, false); !strings.Contains(page, "已通过 Telegram 登录 · ivy@example.com") || !strings.Contains(page, "更换邮箱") {
		t.Fatalf("after binding: %.400s", page)
	}
}

func (p *portalClient) setCookie(name, value string) {
	u, _ := url.Parse(p.base + portalPath)
	p.hc.Jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: portalPath}})
}
