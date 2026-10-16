package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

type portalClient struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func (p *portalClient) req(method, path string, body any, csrf bool) (int, string) {
	p.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, p.base+path, rd)
	r.Header.Set("Content-Type", "application/json")
	if csrf {
		r.Header.Set(portalHeader, "1")
	}
	resp, err := p.hc.Do(r)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (p *portalClient) json(method, path string, body any) map[string]any {
	p.t.Helper()
	_, b := p.req(method, path, body, false)
	var out map[string]any
	json.Unmarshal([]byte(b), &out)
	return out
}

// TestPortal: a user signs in to the user center through the bot, sees their
// own page, can reset the link; the session never opens the admin API and
// ends when the user unbinds Telegram.
func TestPortal(t *testing.T) {
	fake := &fakeTG{}
	tgAPI := httptest.NewServer(fake)
	defer tgAPI.Close()
	s, srv, _ := newAdminClient(t)
	jar, _ := cookiejar.New(nil)
	page := &portalClient{t: t, base: srv.URL, hc: &http.Client{Jar: jar}}

	if st, body := page.req("GET", "/u", nil, false); st != 404 || strings.Contains(body, "用户中心") {
		t.Fatalf("user center must be off by default: %d", st)
	}
	s.st.SetSetting("portal_enabled", "true")
	if _, body := page.req("GET", "/u", nil, false); !strings.Contains(body, "尚未启用 Telegram 机器人") {
		t.Fatalf("without the bot: %s", body)
	}

	s.st.SetSetting("tg_enabled", "true")
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.SetSetting("tg_api_base", tgAPI.URL)
	s.st.SetSetting("tg_poll_timeout", "0")
	db := s.st.DB
	db.Create(&store.User{ID: "u1", Username: "alice", UUID: "x", Password: "p", Status: "active", SubToken: "alice-subscription-token", TelegramID: 2001})
	db.Create(&store.User{ID: "u2", Username: "bob", UUID: "y", Password: "q", Status: "active", SubToken: "bob-subscription-token-x"})
	db.Create(&store.TGAdmin{ID: "tgown", ChatID: 1002, Name: "boss", Role: "owner"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	waitUntil(t, "bot running", s.tgWebLoginEnabled)
	if _, body := page.req("GET", "/u", nil, false); !strings.Contains(body, "使用 Telegram 登录") {
		t.Fatalf("login page: %s", body)
	}

	start := func() (id, secret string, code int) {
		out := page.json("POST", "/u/login", nil)
		if !strings.HasPrefix(fmt.Sprint(out["url"]), "https://t.me/polaris_bot?start="+portalLoginPrefix) {
			t.Fatalf("start: %v", out)
		}
		return out["id"].(string), out["secret"].(string), int(out["code"].(float64))
	}
	open := func(chat int64, id string) {
		n := len(fake.textsTo(chat))
		fake.push(msg(chat, "/start "+portalLoginPrefix+id))
		waitUntil(t, "prompt", func() bool { return len(fake.textsTo(chat)) != n })
	}
	approve := func(chat int64, id, secret, pick string) map[string]any {
		fake.push(cb(chat, "ul:"+id+":"+pick))
		var out map[string]any
		waitUntil(t, "answer", func() bool {
			out = page.json("POST", "/u/poll", map[string]string{"id": id, "secret": secret})
			return out["status"] != "pending"
		})
		return out
	}
	// An unbound chat (even an admin's) gets no choices.
	id, _, code := start()
	open(1002, id)
	if !strings.Contains(fake.textsTo(1002), "还没有绑定账号") {
		t.Fatalf("admin without a user binding: %s", fake.textsTo(1002))
	}
	// The admin login callback cannot answer a user center request.
	fake.push(cb(1002, fmt.Sprintf("wl:%s:%d", id, code)))

	// Alice signs in.
	secret := ""
	id, secret, code = start()
	open(2001, id)
	if !strings.Contains(fake.textsTo(2001), "用户中心登录请求") || !strings.Contains(fake.textsTo(2001), "alice") {
		t.Fatalf("prompt: %s", fake.textsTo(2001))
	}
	if out := approve(2001, id, secret, fmt.Sprint(code)); out["status"] != "ok" {
		t.Fatalf("approve: %v", out)
	}
	st, body := page.req("GET", "/u", nil, false)
	if st != 200 || !strings.Contains(body, "alice-subscription-token") || !strings.Contains(body, "已通过 Telegram 登录") || strings.Contains(body, "bob") {
		t.Fatalf("user page %d", st)
	}

	// The cookie is no admin session; its JWT is no bearer token either.
	u, _ := http.NewRequest("GET", srv.URL+s.cfg.APIBase()+"/users", nil)
	for _, ck := range jar.Cookies(u.URL) {
		if ck.Name == portalCookie {
			u.Header.Set("Authorization", "Bearer "+ck.Value)
		}
	}
	if resp, _ := http.DefaultClient.Do(u); resp.StatusCode != 401 {
		t.Fatalf("portal token on the admin API: %d", resp.StatusCode)
	}

	// Writes need the portal header (CSRF).
	if st, _ := page.req("POST", "/u/reset", nil, false); st != 403 {
		t.Fatalf("reset without header: %d", st)
	}
	if st, _ := page.req("POST", "/u/reset", nil, true); st != 200 {
		t.Fatalf("reset: %d", st)
	}
	var a store.User
	db.First(&a, "id = ?", "u1")
	if a.SubToken == "alice-subscription-token" {
		t.Fatal("token not reset")
	}
	if _, body := page.req("GET", "/u", nil, false); !strings.Contains(body, a.SubToken) {
		t.Fatal("session lost after the user's own reset")
	}

	// Unbinding Telegram ends the session.
	db.Model(&store.User{}).Where("id = ?", "u1").Update("telegram_id", 0)
	if _, body := page.req("GET", "/u", nil, false); !strings.Contains(body, "使用 Telegram 登录") {
		t.Fatal("session survived unbinding")
	}

	// A wrong number denies.
	db.Model(&store.User{}).Where("id = ?", "u1").Update("telegram_id", 2001)
	id, secret, code = start()
	open(2001, id)
	if out := approve(2001, id, secret, fmt.Sprint((code-10+1)%90+10)); out["status"] != "denied" {
		t.Fatalf("wrong number: %v", out)
	}
	if st, _ := page.req("POST", "/u/logout", nil, true); st != 200 {
		t.Fatalf("logout: %d", st)
	}
}

// TestResetTokenRotatesCredentials: resetting a user's link also replaces the
// node credentials, so nodes imported from the old link stop working.
func TestResetTokenRotatesCredentials(t *testing.T) {
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.User{ID: "u1", Username: "alice", UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", Password: "old-password",
		SSSeed: "old-seed", Status: "active", SubToken: "alice-subscription-token"})
	st, out := admin.do("POST", "/users/u1/reset-token", nil)
	if st != 200 {
		t.Fatalf("reset: %d %v", st, out)
	}
	var u store.User
	db.First(&u, "id = ?", "u1")
	if u.SubToken == "alice-subscription-token" || out["subToken"] != u.SubToken {
		t.Fatalf("token: %+v %v", u, out)
	}
	if u.UUID == "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01" || u.Password == "old-password" || u.SSSeed == "old-seed" || len(u.UUID) != 36 || u.Password == "" {
		t.Fatalf("node credentials not rotated: %+v", u)
	}
}
