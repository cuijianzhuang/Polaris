package panel

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestSelfRename: an account the shop created can rename itself in the bot
// and in the user center, once a week, to a free valid name; accounts the
// admin made cannot, nor can anyone when the shop turns it off.
func TestSelfRename(t *testing.T) {
	fake := &fakeTG{}
	api := httptest.NewServer(fake)
	defer api.Close()
	s, srv, _ := newAdminClient(t)
	for k, v := range map[string]string{"tg_enabled": "true", "tg_api_base": api.URL, "tg_poll_timeout": "0", "portal_enabled": "true"} {
		s.st.SetSetting(k, v)
	}
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	db := s.st.DB
	db.Create(&store.User{ID: "u1", Username: "tg700", UUID: "u1-uuid", Status: "active", SubToken: "u1-subscription-token", TelegramID: 700, SelfService: true})
	db.Create(&store.User{ID: "u2", Username: "Taken", UUID: "u2-uuid", Status: "active", SubToken: "u2-subscription-token"})
	db.Create(&store.User{ID: "u3", Username: "vip", UUID: "u3-uuid", Status: "active", SubToken: "u3-subscription-token", TelegramID: 701})
	name := func(id string) string {
		var u store.User
		db.First(&u, "id = ?", id)
		return u.Username
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	waitUntil(t, "bot running", s.tg.isRunning)

	fake.push(cb(700, "v:me"))
	waitUntil(t, "rename button", func() bool { return hasButton(fake, "v:rename") })
	for _, bad := range []string{"ab", "-dash", "has space", "taken", "Admin", "tg700"} {
		fake.push(msg(700, "/rename "+bad))
	}
	waitUntil(t, "refusals", func() bool { return strings.Count(fake.textsTo(700), "❌") == 6 })
	if name("u1") != "tg700" {
		t.Fatal("bad name accepted")
	}
	fake.push(msg(700, "/rename Alice_01"))
	waitUntil(t, "renamed", func() bool { return name("u1") == "Alice_01" })
	if !strings.Contains(fake.textsTo(700), "用户名已改为") {
		t.Fatal("no confirmation")
	}
	fake.push(msg(700, "/rename alice02"))
	waitUntil(t, "cooldown", func() bool { return strings.Contains(fake.textsTo(700), "7 天内只能修改一次") })
	if name("u1") != "Alice_01" {
		t.Fatal("renamed twice in a week")
	}
	fake.push(msg(701, "/rename vip2"))
	waitUntil(t, "admin account refused", func() bool { return strings.Contains(fake.textsTo(701), "管理员创建") })

	// User center: the button shows for the shop account; the cooldown over,
	// it renames through /u/rename.
	db.Model(&store.User{}).Where("id = ?", "u1").Update("renamed_at", time.Now().Add(-8*24*time.Hour))
	var u store.User
	db.First(&u, "id = ?", "u1")
	tok, err := s.issuePortalToken(&u)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse(srv.URL + portalPath)
	jar.SetCookies(base, []*http.Cookie{{Name: portalCookie, Value: tok, Path: portalPath}})
	page := &portalClient{t: t, base: srv.URL, hc: &http.Client{Jar: jar}}
	if _, body := page.req("GET", "/u", nil, false); !strings.Contains(body, `data-portal="rename"`) {
		t.Fatal("no rename button in the user center")
	}
	if st, _ := page.req("POST", "/u/rename", map[string]string{"username": "carol"}, false); st != 403 {
		t.Fatalf("rename without the portal header: %d", st)
	}
	if st, body := page.req("POST", "/u/rename", map[string]string{"username": "taken"}, true); st != 400 || !strings.Contains(body, "已被使用") {
		t.Fatalf("duplicate: %d %s", st, body)
	}
	if st, body := page.req("POST", "/u/rename", map[string]string{"username": "carol"}, true); st != 200 || name("u1") != "carol" {
		t.Fatalf("rename: %d %s", st, body)
	}
	// The session survives the rename (it is bound to the id, not the name).
	if _, body := page.req("GET", "/u", nil, false); !strings.Contains(body, "carol") {
		t.Fatal("signed out by the rename")
	}

	s.st.SetSetting("shop_user_rename", "false")
	if _, body := page.req("GET", "/u", nil, false); strings.Contains(body, `data-portal="rename"`) {
		t.Fatal("rename offered while off")
	}
	db.Model(&store.User{}).Where("id = ?", "u1").Update("renamed_at", nil)
	if st, _ := page.req("POST", "/u/rename", map[string]string{"username": "dave"}, true); st != 400 || name("u1") != "carol" {
		t.Fatal("renamed while off")
	}
}
