package panel

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/mailer/mailtest"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// useMail points the panel at an in-memory SMTP server and starts the mail
// sender.
func useMail(t *testing.T, s *Server) *mailtest.Server {
	t.Helper()
	m, err := mailtest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	for k, v := range map[string]string{"smtp_host": "127.0.0.1", "smtp_port": strconv.Itoa(m.Port()), "smtp_security": "none",
		"smtp_from": "noreply@example.com", "smtp_from_name": "Polaris", "site_title": "极光"} {
		s.st.SetSetting(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.runMail(ctx, s.mailEvents)
	return m
}

func TestUserEmailField(t *testing.T) {
	s, _, admin := newAdminClient(t)
	st, out := admin.do("POST", "/users", map[string]any{"username": "alice", "email": " Alice@Example.COM "})
	if st != 201 || out["email"] != "alice@example.com" {
		t.Fatalf("create %d %v", st, out)
	}
	if st, _ := admin.do("POST", "/users", map[string]any{"username": "bob", "email": "alice@example.com"}); st != 400 {
		t.Fatal("duplicate address accepted")
	}
	if st, _ := admin.do("POST", "/users", map[string]any{"username": "bob", "email": "not-mail"}); st != 400 {
		t.Fatal("bad address accepted")
	}
	_, list := admin.do("GET", "/users?q=example.com", nil)
	if list["total"].(float64) != 1 {
		t.Fatalf("search by mail %v", list)
	}
	id := out["id"].(string)
	// Update without email keeps it; "" clears it.
	if st, out := admin.do("PUT", "/users/"+id, map[string]any{"username": "alice"}); st != 200 || out["email"] != "alice@example.com" {
		t.Fatalf("kept %d %v", st, out)
	}
	if st, out := admin.do("PUT", "/users/"+id, map[string]any{"username": "alice", "email": ""}); st != 200 || out["email"] != "" {
		t.Fatalf("cleared %d %v", st, out)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", id)
	if u.Email != "" {
		t.Fatal("not cleared in the database")
	}
}

// A user without Telegram but with an address gets reminders by mail; the
// admin address gets admin notices; a delivered order sends a receipt.
func TestMailNotices(t *testing.T) {
	s, _, admin := newAdminClient(t)
	m := useMail(t, s)
	s.st.SetSetting("mail_admin_to", "ops@example.com")
	u := newUser(&userReq{Username: "carol", TrafficLimit: 100 << 30})
	u.Email, u.UsedDown = "carol@example.org", 85<<30
	s.st.DB.Create(&u)

	s.remindUsers()
	waitFor(t, "reminder mail", func() bool { return len(m.To("carol@example.org")) == 1 })
	got := m.To("carol@example.org")[0]
	if got.Subject != "[极光] 流量提醒" || !strings.Contains(got.Body, "85%") || !strings.Contains(got.Body, s.subURL(u.SubToken)) || strings.Contains(got.Body, "<b>") {
		t.Fatalf("reminder %+v", got)
	}
	if len(m.To("ops@example.com")) != 0 {
		t.Fatal("a user reminder went to the admin")
	}
	s.remindUsers() // once per cycle
	time.Sleep(200 * time.Millisecond)
	if len(m.To("carol@example.org")) != 1 {
		t.Fatal("reminder sent twice")
	}

	// Paused notice with a custom template, as plain text.
	s.st.SetSetting("tpl_user_limited", "{user}，流量用完了 <3")
	s.notify(Event{Kind: "user_limited", UserID: u.ID, Text: "⛔ carol 流量已用尽"})
	waitFor(t, "notice mail", func() bool { return len(m.To("carol@example.org")) == 2 })
	if b := m.To("carol@example.org")[1]; b.Subject != "[极光] 流量已用尽" || !strings.Contains(b.Body, "carol，流量用完了 <3") {
		t.Fatalf("notice %+v", b)
	}
	waitFor(t, "admin copy", func() bool { return len(m.To("ops@example.com")) == 1 })
	if a := m.To("ops@example.com")[0]; !strings.Contains(a.Subject, "carol 流量已用尽") {
		t.Fatalf("admin mail %+v", a)
	}

	// Users can be left out.
	s.st.SetSetting("mail_notify_users", "false")
	s.notify(Event{Kind: "user_active", UserID: u.ID, Text: "✅ carol 已恢复", UserOnly: true})
	time.Sleep(200 * time.Millisecond)
	if len(m.To("carol@example.org")) != 2 {
		t.Fatal("mailed with user notices off")
	}
	s.st.SetSetting("mail_notify_users", "true")

	// Receipt for a delivered order.
	p := store.Group{ID: "p1", Name: "月付", Plan: true, Sale: true, Days: 30}
	s.st.DB.Create(&p)
	now := time.Now()
	o := store.Order{ID: store.NewID(), UserID: u.ID, PlanID: "p1", PlanName: "月付", Method: payManual, Amount: "15.00", Currency: "CNY",
		Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	s.st.DB.Create(&o)
	if _, _, _, err := s.fulfil(o.ID, "manual:x", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "receipt", func() bool {
		for _, x := range m.To("carol@example.org") {
			if x.Subject == "[极光] 订单已开通" && strings.Contains(x.Body, o.ID) {
				return true
			}
		}
		return false
	})

	// Test endpoint: the form's values, the saved password when empty.
	if st, out := admin.do("POST", "/mail/test", map[string]any{"to": "me@example.com", "host": "127.0.0.1", "port": strconv.Itoa(m.Port()), "security": "none", "from": "noreply@example.com"}); st != 200 || out["ok"] != true {
		t.Fatalf("test mail %d %v", st, out)
	}
	waitFor(t, "test mail", func() bool { return len(m.To("me@example.com")) == 1 })
	if st, out := admin.do("POST", "/mail/test", map[string]any{"to": "me@example.com", "host": "127.0.0.1", "port": "1", "security": "none", "from": "noreply@example.com"}); st != 200 || out["ok"] != false {
		t.Fatalf("unreachable server %d %v", st, out)
	}
	if st, _ := admin.do("POST", "/mail/test", map[string]any{"to": "bad", "host": "x", "from": "noreply@example.com"}); st != 400 {
		t.Fatal("bad recipient accepted")
	}
}
