package panel

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// buttonsTo returns the callback data of the last message sent to chat.
func (f *fakeTG) buttonsTo(chat int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for i := len(f.sent) - 1; i >= 0; i-- {
		m := f.sent[i]
		if int64(m["chat_id"].(float64)) != chat {
			continue
		}
		rm, _ := m["reply_markup"].(map[string]any)
		rows, _ := rm["inline_keyboard"].([]any)
		for _, r := range rows {
			for _, b := range r.([]any) {
				out = append(out, b.(map[string]any)["callback_data"].(string))
			}
		}
		return out
	}
	return nil
}

func TestTelegramWebLogin(t *testing.T) {
	fake := &fakeTG{}
	tgAPI := httptest.NewServer(fake)
	defer tgAPI.Close()
	s, srv, _ := newAdminClient(t)
	page := &apiClient{t: t, base: srv.URL + s.cfg.APIBase()}

	if st, _ := page.do("POST", "/auth/tg/start", nil); st != 503 {
		t.Fatalf("start without bot: %d", st)
	}
	s.st.SetSetting("tg_enabled", "true")
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.SetSetting("tg_api_base", tgAPI.URL)
	s.st.SetSetting("tg_poll_timeout", "0")
	s.st.DB.Create(&store.TGAdmin{ID: "tgop", ChatID: 1001, Name: "alice", Role: "operator", NodeIDs: store.JSON[[]string]{V: []string{"n1"}}})
	s.st.DB.Create(&store.TGAdmin{ID: "tgown", ChatID: 1002, Name: "boss", Role: "owner"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	waitUntil(t, "bot running", s.tgWebLoginEnabled)
	if _, info := page.do("GET", "/info", nil); info["telegramLogin"] != true {
		t.Fatalf("info: %v", info)
	}

	start := func() (id, secret string, code int) {
		st, out := page.do("POST", "/auth/tg/start", nil)
		if st != 200 || !strings.HasPrefix(out["url"].(string), "https://t.me/polaris_bot?start=weblogin_") {
			t.Fatalf("start %d %v", st, out)
		}
		return out["id"].(string), out["secret"].(string), int(out["code"].(float64))
	}
	poll := func(id, secret string) map[string]any {
		_, out := page.do("POST", "/auth/tg/poll", map[string]string{"id": id, "secret": secret})
		return out
	}
	open := func(chat int64, id string) []string {
		n := len(fake.buttonsTo(chat))
		fake.push(msg(chat, "/start weblogin_"+id))
		var bs []string
		waitUntil(t, "prompt", func() bool {
			bs = fake.buttonsTo(chat)
			return len(bs) != n || strings.Contains(fake.textsTo(chat), id)
		})
		time.Sleep(50 * time.Millisecond)
		return fake.buttonsTo(chat)
	}

	// Owner taps the matching number: page gets a full session, once.
	id, secret, code := start()
	if out := poll(id, "wrong"); out["status"] != "expired" {
		t.Fatalf("bad secret: %v", out)
	}
	if out := poll(id, secret); out["status"] != "pending" || out["opened"] != false {
		t.Fatalf("pending: %v", out)
	}
	bs := open(1002, id)
	if len(bs) != tgWebLoginChoices+1 || !strings.Contains(fake.textsTo(1002), "网页登录请求") {
		t.Fatalf("prompt buttons %v\n%s", bs, fake.textsTo(1002))
	}
	if out := poll(id, secret); out["opened"] != true {
		t.Fatalf("opened: %v", out)
	}
	// Another admin cannot answer a request opened by someone else.
	if b := open(1001, id); len(b) == tgWebLoginChoices+1 {
		t.Fatalf("second admin got choices: %v", b)
	}
	fake.push(cb(1002, fmt.Sprintf("wl:%s:%d", id, code)))
	var out map[string]any
	waitUntil(t, "approved", func() bool { out = poll(id, secret); return out["status"] != "pending" })
	if out["status"] != "ok" || out["username"] != "tg:boss" {
		t.Fatalf("approve: %v", out)
	}
	c := &apiClient{t: t, base: page.base, tok: out["token"].(string)}
	if st, _ := c.do("GET", "/users", nil); st != 200 {
		t.Fatalf("owner session: %d", st)
	}
	if out := poll(id, secret); out["status"] != "expired" {
		t.Fatalf("reuse: %v", out)
	}

	// A wrong number denies; so does "x".
	for _, pick := range []string{"wrong", "x"} {
		id, secret, code = start()
		open(1001, id)
		p := "x"
		if pick == "wrong" {
			p = fmt.Sprint((code-10+1)%90 + 10)
		}
		fake.push(cb(1001, "wl:"+id+":"+p))
		waitUntil(t, "denied", func() bool { out = poll(id, secret); return out["status"] != "pending" })
		if out["status"] != "denied" {
			t.Fatalf("%s: %v", pick, out)
		}
	}

	// Operator session is scoped; unbound chats get nothing.
	id, secret, code = start()
	open(1001, id)
	fake.push(cb(1001, fmt.Sprintf("wl:%s:%d", id, code)))
	waitUntil(t, "operator", func() bool { out = poll(id, secret); return out["status"] != "pending" })
	opc := &apiClient{t: t, base: page.base, tok: out["token"].(string)}
	if st, _ := opc.do("GET", "/users", nil); st != 403 {
		t.Fatalf("operator /users: %d", st)
	}
	id, _, _ = start()
	open(4242, id)
	if !strings.Contains(fake.textsTo(4242), "只有已绑定的管理员") {
		t.Fatalf("stranger: %s", fake.textsTo(4242))
	}
}
