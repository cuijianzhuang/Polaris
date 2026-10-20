package panel

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestTicketsUserCenterAndAdmin(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	m := useMail(t, s)
	ev := events(s)
	s.st.SetSetting("portal_enabled", "true")
	s.ticketLimit = &sharedLimiter{s: s.shared, name: "ticket-test", perMinute: 600000}
	u := newUser(&userReq{Username: "kate"})
	u.Email = "kate@example.com"
	s.st.DB.Create(&u)
	tok, _ := s.issuePortalToken(&u)
	// A mail session for a user without Telegram.
	tok, _ = s.issuePortalMailToken(&u)
	b := newShopClient(t, srv.URL)
	b.setCookie(portalCookie, tok)

	// Off by default: the page sends people back to /u.
	if code, _ := b.req("GET", portalPath+"/tickets", nil, false); code != 302 {
		t.Fatalf("tickets while off: %d", code)
	}
	if code, _ := b.req("POST", portalPath+"/tickets", map[string]any{"body": "x"}, true); code != 503 {
		t.Fatal("ticket opened while off")
	}
	s.st.SetSetting("tickets_enabled", "true")
	s.st.SetSetting("ticket_max_open", "2")
	if _, page := b.req("GET", portalPath, nil, false); !strings.Contains(page, "/u/tickets") {
		t.Fatal("no tickets link in the user center")
	}

	code, body := b.req("POST", portalPath+"/tickets", map[string]any{"body": "节点连不上\n香港节点一直超时"}, true)
	if code != 200 {
		t.Fatalf("open %d %s", code, body)
	}
	var res struct {
		No  int64
		URL string
	}
	json.Unmarshal([]byte(body), &res)
	if res.No != 1 {
		t.Fatalf("number %d", res.No)
	}
	if got := next(t, ev); !strings.Contains(got, "新工单 #1") || !strings.Contains(got, "kate") {
		t.Fatalf("admin notice %q", got)
	}
	if code, _ := b.req("POST", portalPath+"/tickets", map[string]any{"body": "第二个"}, true); code != 200 {
		t.Fatal("second ticket")
	}
	if code, body := b.req("POST", portalPath+"/tickets", map[string]any{"body": "第三个"}, true); code != 400 || !strings.Contains(body, "最多") {
		t.Fatalf("over the limit %d %s", code, body)
	}

	// The admin sees it, answers, and the user is told by mail.
	_, list := admin.do("GET", "/tickets?status=open", nil)
	items := list["items"].([]any)
	if len(items) != 2 || list["counts"].(map[string]any)["open"].(float64) != 2 {
		t.Fatalf("admin list %v", list)
	}
	var tk store.Ticket
	s.st.DB.First(&tk, "no = ?", 1)
	if !tk.AdminUnread || tk.Subject != "节点连不上" {
		t.Fatalf("ticket %+v", tk)
	}
	_, view := admin.do("GET", "/tickets/"+tk.ID, nil)
	if msgs := view["messages"].([]any); len(msgs) != 1 || view["ticket"].(map[string]any)["username"] != "kate" {
		t.Fatalf("view %v", view)
	}
	if st, out := admin.do("POST", "/tickets/"+tk.ID+"/reply", map[string]any{"body": "请换到日本节点试试"}); st != 200 || out["status"] != ticketAnswered {
		t.Fatalf("reply %d %v", st, out)
	}
	waitFor(t, "reply mail", func() bool { return len(m.To("kate@example.com")) == 1 })
	if r := m.To("kate@example.com")[0]; r.Subject != "[极光] 工单回复" || !strings.Contains(r.Body, "请换到日本节点试试") || strings.Contains(r.Body, "<b>") {
		t.Fatalf("mail %+v", r)
	}

	// The user reads it (unread mark clears), replies (reopens), then closes.
	if _, page := b.req("GET", portalPath, nil, false); !strings.Contains(page, "1 条新回复") {
		t.Fatal("no unread count")
	}
	if _, page := b.req("GET", portalPath+"/tickets/1", nil, false); !strings.Contains(page, "请换到日本节点试试") || !strings.Contains(page, "客服") {
		t.Fatalf("thread page %.300s", page)
	}
	s.st.DB.First(&tk, "id = ?", tk.ID)
	if tk.UserUnread {
		t.Fatal("unread not cleared")
	}
	if code, _ := b.req("POST", portalPath+"/tickets/1/reply", map[string]any{"body": "可以了，谢谢"}, true); code != 200 {
		t.Fatal("user reply")
	}
	s.st.DB.First(&tk, "id = ?", tk.ID)
	if tk.Status != ticketOpen || !tk.AdminUnread {
		t.Fatalf("after user reply %+v", tk)
	}
	if code, _ := b.req("POST", portalPath+"/tickets/1/close", nil, true); code != 200 {
		t.Fatal("close")
	}
	s.st.DB.First(&tk, "id = ?", tk.ID)
	if tk.Status != ticketClosed || tk.ClosedAt == nil {
		t.Fatalf("closed %+v", tk)
	}
	// Someone else's ticket is out of reach.
	other := newUser(&userReq{Username: "leo"})
	other.Email = "leo@example.com"
	s.st.DB.Create(&other)
	otok, _ := s.issuePortalMailToken(&other)
	ob := newShopClient(t, srv.URL)
	ob.setCookie(portalCookie, otok)
	if code, _ := ob.req("POST", portalPath+"/tickets/1/reply", map[string]any{"body": "hi"}, true); code != 404 {
		t.Fatal("replied to another user's ticket")
	}

	// Auto-close: an answered ticket idle for a week.
	var t2 store.Ticket
	s.st.DB.First(&t2, "no = ?", 2)
	admin.do("POST", "/tickets/"+t2.ID+"/reply", map[string]any{"body": "收到"})
	s.st.DB.Model(&store.Ticket{}).Where("id = ?", t2.ID).Update("updated_at", time.Now().AddDate(0, 0, -8))
	s.autoCloseTickets()
	s.st.DB.First(&t2, "id = ?", t2.ID)
	if t2.Status != ticketClosed {
		t.Fatalf("not auto-closed: %s", t2.Status)
	}
	if st, _ := admin.do("DELETE", "/tickets/"+t2.ID, nil); st != 204 {
		t.Fatal("delete")
	}
}

func TestTicketsBot(t *testing.T) {
	fake := &fakeTG{}
	api := httptest.NewServer(fake)
	defer api.Close()
	s := newTestServer(t)
	for k, v := range map[string]string{"tg_enabled": "true", "tg_api_base": api.URL, "tg_poll_timeout": "0", "tickets_enabled": "true"} {
		s.st.SetSetting(k, v)
	}
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.DB.Create(&store.User{ID: "u1", Username: "mia", UUID: "x", Password: "p", Status: "active", SubToken: "mia-token", TelegramID: 700})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	go s.tg.runNotifications(ctx, s.tgEvents)
	waitUntil(t, "bot running", s.tg.isRunning)

	fake.push(msg(700, "/ticket 订阅更新失败"))
	waitUntil(t, "ticket opened", func() bool { return strings.Contains(fake.textsTo(700), "已提交工单 <b>#1</b>") })
	var tk store.Ticket
	s.st.DB.First(&tk, "no = ?", 1)
	if err := s.adminReply(&tk, "admin", "请重置订阅链接后重新导入", false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "reply delivered", func() bool { return strings.Contains(fake.textsTo(700), "请重置订阅链接后重新导入") })
	fake.push(msg(700, "/reply 1 好的已解决"))
	waitUntil(t, "user reply", func() bool { return strings.Contains(fake.textsTo(700), "已回复工单 <b>#1</b>") })
	var n int64
	s.st.DB.Model(&store.TicketMessage{}).Where("ticket_id = ?", tk.ID).Count(&n)
	if n != 3 {
		t.Fatalf("messages %d", n)
	}
	fake.push(msg(999, "/ticket 没绑定"))
	waitUntil(t, "unbound refused", func() bool { return strings.Contains(fake.textsTo(999), "请先绑定账号") })
}
