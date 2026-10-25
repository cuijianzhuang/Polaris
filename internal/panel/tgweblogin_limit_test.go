package panel

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
)

// One client cannot open more than a few Telegram sign-ins a minute, so it
// cannot fill the shared cap for everyone else.
func TestTGWebLoginPerIP(t *testing.T) {
	fake := &fakeTG{}
	tgAPI := httptest.NewServer(fake)
	defer tgAPI.Close()
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("tg_enabled", "true")
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.SetSetting("tg_api_base", tgAPI.URL)
	s.st.SetSetting("tg_poll_timeout", "0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	waitUntil(t, "bot running", s.tgWebLoginEnabled)
	jar, _ := cookiejar.New(nil)
	page := &portalClient{t: t, base: srv.URL, hc: &http.Client{Jar: jar}}
	limited := false
	for i := 0; i < 10 && !limited; i++ {
		code, _ := page.req("POST", "/u/login", nil, true)
		limited = code == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("no per-IP limit on sign-in requests")
	}
	if n := s.tgWeb.eph.count(s.portalWeb.name + ":"); n > tgWebLoginPerIP {
		t.Fatalf("%d pending requests from one client", n)
	}
}
