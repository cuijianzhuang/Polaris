package panel

import (
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

func TestPortalTelegramAvailabilityAndCancellation(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	jar, _ := cookiejar.New(nil)
	page := &portalClient{t: t, base: srv.URL, hc: &http.Client{Jar: jar}}
	_, body := page.req("GET", "/u/login", nil, false)
	if !strings.Contains(body, `id="go" type="button" disabled`) || !strings.Contains(body, "管理员尚未启用 Telegram 机器人登录") {
		t.Fatal("missing disabled Telegram entry")
	}
	s.st.SetSetting("tg_enabled", "true")
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	_, body = page.req("GET", "/u/login", nil, false)
	if strings.Contains(body, `id="go" type="button" disabled`) || !strings.Contains(body, "使用 Telegram 登录") {
		t.Fatal("configured bot disappeared during reconnect")
	}
	if code, body := page.req("POST", "/u/login", nil, true); code != 503 || !strings.Contains(body, "暂时未就绪") {
		t.Fatal(code, body)
	}
	s.tg.mu.Lock()
	s.tg.me, s.tg.running = "polaris_bot", true
	s.tg.mu.Unlock()
	req := page.jsonW("POST", "/u/login", nil)
	if req["id"] == nil {
		t.Fatal(req)
	}
	credentials := map[string]any{"id": req["id"], "secret": req["secret"]}
	if code, _ := page.req("POST", "/u/login/cancel", credentials, false); code != 403 {
		t.Fatal("cancel did not require CSRF header", code)
	}
	page.jsonW("POST", "/u/login/cancel", map[string]any{"id": req["id"], "secret": "wrong"})
	if out := page.jsonW("POST", "/u/poll", credentials); out["status"] != "pending" {
		t.Fatal("wrong secret cancelled another login", out)
	}
	var pending tgWebLogin
	if !s.portalWeb.eph.get(s.portalWeb.key(req["id"].(string)), &pending) {
		t.Fatal("request missing")
	}
	if err := s.tgWeb.eph.put(s.tgWeb.key(req["id"].(string)), pending, tgWebLoginTTL); err != nil {
		t.Fatal(err)
	}
	page.jsonW("POST", "/u/login/cancel", credentials)
	if out := page.jsonW("POST", "/u/poll", credentials); out["status"] != "expired" {
		t.Fatal("cancelled request still usable", out)
	}
	if !s.tgWeb.eph.get(s.tgWeb.key(req["id"].(string)), &pending) {
		t.Fatal("portal cancellation removed admin request")
	}
	req = page.jsonW("POST", "/u/login", nil)
	s.st.SetSetting("tg_enabled", "false") // the cached runtime can still say running
	if code, _ := page.req("POST", "/u/login", nil, true); code != 503 {
		t.Fatal("disabled bot accepted a new login", code)
	}
	if code, _ := page.req("POST", "/u/poll", map[string]any{"id": req["id"], "secret": req["secret"]}, true); code != 503 {
		t.Fatal("disabled bot accepted an old login", code)
	}
}
