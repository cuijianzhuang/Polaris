package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

type fakeTG struct {
	mu      sync.Mutex
	queue   []map[string]any
	nextID  int64
	sent    []map[string]any // sendMessage / editMessageText payloads
	answers []string
	calls   map[string][]map[string]any // every call by method
}

func (f *fakeTG) push(u map[string]any) {
	f.mu.Lock()
	f.nextID++
	u["update_id"] = f.nextID
	f.queue = append(f.queue, u)
	f.mu.Unlock()
}

func msg(chat int64, text string) map[string]any {
	return map[string]any{"message": map[string]any{"message_id": 1, "from": map[string]any{"id": chat, "username": "u"},
		"chat": map[string]any{"id": chat, "type": "private"}, "text": text}}
}

func cb(chat int64, data string) map[string]any {
	return map[string]any{"callback_query": map[string]any{"id": "cb", "from": map[string]any{"id": chat}, "data": data,
		"message": map[string]any{"message_id": 9, "chat": map[string]any{"id": chat, "type": "private"}}}}
}

func (f *fakeTG) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var body map[string]any
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
	}
	var result any = true
	f.mu.Lock()
	if method != "getUpdates" {
		if f.calls == nil {
			f.calls = map[string][]map[string]any{}
		}
		f.calls[method] = append(f.calls[method], body)
	}
	switch method {
	case "getMe":
		result = map[string]any{"id": 1, "username": "polaris_bot"}
	case "getUpdates":
		off, _ := body["offset"].(float64)
		var out []map[string]any
		for _, u := range f.queue {
			if u["update_id"].(int64) >= int64(off) {
				out = append(out, u)
			}
		}
		result = out
	case "sendMessage", "editMessageText":
		f.sent = append(f.sent, body)
	case "createInvoiceLink":
		result = "https://t.me/$" + body["payload"].(string)
	case "answerCallbackQuery":
		f.answers = append(f.answers, body["text"].(string))
	}
	f.mu.Unlock()
	if method == "getUpdates" && result == nil {
		time.Sleep(20 * time.Millisecond)
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeTG) textsTo(chat int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.sent {
		if int64(m["chat_id"].(float64)) == chat {
			out = append(out, m["text"].(string))
		}
	}
	return strings.Join(out, "\n---\n")
}

func waitUntil(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func TestTelegramBot(t *testing.T) {
	fake := &fakeTG{}
	api := httptest.NewServer(fake)
	defer api.Close()
	s := newTestServer(t)
	licenseAll(s)
	s.st.SetSetting("tg_enabled", "true")
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.SetSetting("tg_api_base", api.URL)
	s.st.SetSetting("tg_poll_timeout", "0")
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "tokyo", Status: "online"})
	db.Create(&store.Node{ID: "n2", Name: "hk", Status: "offline"})
	db.Create(&store.User{ID: "u1", Username: "alice", UUID: "x", Password: "p", Status: "active", SubToken: "alice-subscription-token", TrafficLimit: 100})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	go s.tg.runNotifications(ctx, s.tgEvents)
	waitUntil(t, "bot running", s.tg.isRunning)

	s.tg.codes["OWNERCODE1"] = bindCode{Role: "owner", Name: "boss", Expires: time.Now().Add(time.Minute)}
	s.tg.codes["OPERCODE01"] = bindCode{Role: "operator", NodeIDs: []string{"n2"}, Expires: time.Now().Add(time.Minute)}
	fake.push(msg(100, "/bind OWNERCODE1"))
	fake.push(msg(300, "/bind opercode01"))
	fake.push(msg(200, "/bind https://panel.example.com/sub/alice-subscription-token"))
	waitUntil(t, "bindings", func() bool {
		var n int64
		db.Model(&store.TGAdmin{}).Count(&n)
		var u store.User
		db.First(&u, "id = ?", "u1")
		return n == 2 && u.TelegramID == 200
	})

	fake.push(msg(200, "/me"))
	fake.push(msg(100, "/status"))
	fake.push(msg(300, "/status"))
	if proBuild {
		adminMenusPro(t, fake, db)
	} else {
		// Community build: the admin menus answer with the professional
		// edition notice; nothing changes.
		fake.push(cb(100, "u:d:u1"))
		waitUntil(t, "replies", func() bool {
			fake.mu.Lock()
			n := len(fake.answers)
			fake.mu.Unlock()
			return strings.Contains(fake.textsTo(200), "alice") && strings.Contains(fake.textsTo(100), "专业版") &&
				strings.Contains(fake.textsTo(300), "专业版") && n >= 1
		})
		var u store.User
		db.First(&u, "id = ?", "u1")
		if u.Status != "active" || strings.Contains(fake.textsTo(100), "tokyo") {
			t.Fatalf("community build ran an admin action: %s", u.Status)
		}
	}

	// Events: users get a user-facing text, operators only their nodes' events.
	s.notify(Event{Kind: "user_limited", UserID: "u1", Text: "admin text"})
	s.notify(Event{Kind: "node_offline", NodeID: "n1", Text: "tokyo offline"})
	s.notify(Event{Kind: "node_offline", NodeID: "n2", Text: "hk offline"})
	waitUntil(t, "notifications", func() bool {
		return strings.Contains(fake.textsTo(200), "流量已用尽") && strings.Contains(fake.textsTo(100), "tokyo offline") &&
			strings.Contains(fake.textsTo(300), "hk offline")
	})
	if strings.Contains(fake.textsTo(300), "tokyo offline") {
		t.Fatal("operator received an out-of-scope event")
	}

	// 80% traffic reminder is sent once.
	db.Model(&store.User{}).Where("id = ?", "u1").Updates(map[string]any{"status": "active", "used_down": 85})
	s.remindUsers()
	s.remindUsers()
	waitUntil(t, "reminder", func() bool { return strings.Contains(fake.textsTo(200), "85%") })
	time.Sleep(100 * time.Millisecond)
	if strings.Count(fake.textsTo(200), "流量提醒") != 1 {
		t.Fatal("reminder must be sent once per cycle")
	}
	if strings.Contains(fake.textsTo(100), "流量提醒") {
		t.Fatal("per-user reminders go to the user only")
	}

	// Daily report reaches owners once, with the attention list.
	s.st.SetSetting("tg_daily_report_hour", fmt.Sprint(time.Now().Hour()))
	db.Model(&store.User{}).Where("id = ?", "u1").Update("status", "limited")
	s.dailyReport()
	s.dailyReport()
	waitUntil(t, "daily report", func() bool { return strings.Contains(fake.textsTo(100), "每日报告") })
	time.Sleep(100 * time.Millisecond)
	if r := fake.textsTo(100); strings.Count(r, "每日报告") != 1 || !strings.Contains(r, "已超额 1：alice") {
		t.Fatalf("daily report:\n%s", r)
	}
	if strings.Contains(fake.textsTo(300), "每日报告") {
		t.Fatal("daily report is for owners only")
	}
}
