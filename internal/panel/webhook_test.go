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

type hookCall struct {
	header http.Header
	body   []byte
}

// hookReceiver records calls; the first `fail` calls answer 500.
func hookReceiver(t *testing.T, fail int) (*httptest.Server, func() []hookCall) {
	var mu sync.Mutex
	var calls []hookCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, hookCall{r.Header.Clone(), b})
		n := len(calls)
		mu.Unlock()
		if n <= fail {
			http.Error(w, "boom", 500)
			return
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []hookCall { mu.Lock(); defer mu.Unlock(); return append([]hookCall{}, calls...) }
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestWebhookUserEvents: creating a user sends a signed user.created event
// to a webhook subscribed to user.*, and nothing it did not subscribe to.
func TestWebhookUserEvents(t *testing.T) {
	s, _, admin := newAdminClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runWebhooks(ctx, s.hookEvents)
	recv, calls := hookReceiver(t, 0)

	code, out := admin.do("POST", "/webhooks", map[string]any{"name": "plugin", "url": recv.URL, "events": []string{"user.*"}})
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	secret, _ := out["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("secret %q", secret)
	}
	if code, out := admin.do("POST", "/users", map[string]any{"username": "bob"}); code != 201 {
		t.Fatalf("create user %d %v", code, out)
	}
	s.notify(Event{Kind: "node_offline", NodeID: "n1", Text: "offline"}) // not subscribed
	waitFor(t, "delivery", func() bool { return len(calls()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	cs := calls()
	if len(cs) != 1 {
		t.Fatalf("%d calls, want 1 (node event must be filtered)", len(cs))
	}
	c := cs[0]
	if c.header.Get("X-Polaris-Event") != "user.created" {
		t.Fatalf("event header %q", c.header.Get("X-Polaris-Event"))
	}
	sig := c.header.Get("X-Polaris-Signature") // t=<ts>,v1=<hex>
	ts, v1, _ := strings.Cut(strings.TrimPrefix(sig, "t="), ",v1=")
	if v1 == "" || v1 != signWebhook(secret, ts, c.body) {
		t.Fatalf("bad signature %q", sig)
	}
	var p struct {
		Type string `json:"type"`
		Data struct {
			User map[string]any `json:"user"`
		} `json:"data"`
	}
	json.Unmarshal(c.body, &p)
	if p.Type != "user.created" || p.Data.User["username"] != "bob" {
		t.Fatalf("payload %s", c.body)
	}
}

// TestWebhookRetry: a failed delivery is retried with backoff and marked
// successful once the receiver answers 2xx. A test message is not retried.
func TestWebhookRetry(t *testing.T) {
	s, _, admin := newAdminClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runWebhooks(ctx, s.hookEvents)
	recv, calls := hookReceiver(t, 2)
	_, out := admin.do("POST", "/webhooks", map[string]any{"url": recv.URL})
	id := out["webhook"].(map[string]any)["id"].(string)

	if code, d := admin.do("POST", "/webhooks/"+id+"/test", nil); code != 200 || d["status"] != "failed" {
		t.Fatalf("test message %d %v", code, d)
	}
	s.notify(Event{Kind: "node_offline", NodeID: "n1", Text: "offline"})
	var row store.WebhookDelivery
	waitFor(t, "first attempt", func() bool {
		return s.st.DB.First(&row, "webhook_id = ? AND type = ? AND attempts = 1 AND status = ?", id, "node.offline", "pending").Error == nil
	})
	if wait := time.Until(row.NextAt); wait < 50*time.Second || wait > 70*time.Second {
		t.Fatalf("next attempt in %v, want ~1m", wait)
	}
	s.st.DB.Model(&row).Update("next_at", time.Now().Add(-time.Second))
	s.retryWebhooks()
	s.st.DB.First(&row, "id = ?", row.ID)
	if row.Status != "success" || row.Attempts != 2 || len(calls()) != 3 {
		t.Fatalf("after retry: %s attempts=%d calls=%d", row.Status, row.Attempts, len(calls()))
	}
}

func TestDisabledWebhookDefersQueuedDeliveries(t *testing.T) {
	for _, typ := range []string{"user.expire_warn", "node.offline"} {
		for _, attempts := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s_attempts_%d", typ, attempts), func(t *testing.T) {
				s, _, admin := newAdminClient(t)
				recv, calls := hookReceiver(t, 0)
				code, out := admin.do("POST", "/webhooks", map[string]any{"url": recv.URL})
				if code != http.StatusCreated {
					t.Fatalf("create webhook: %d %v", code, out)
				}
				id := out["webhook"].(map[string]any)["id"].(string)
				d := store.WebhookDelivery{ID: store.NewID(), WebhookID: id, EventID: "evt-original", Type: typ,
					Payload: `{"data":{"node_id":"n1","user_id":"u1"}}`, Status: "pending", Attempts: attempts, NextAt: time.Now()}
				if err := s.st.DB.Create(&d).Error; err != nil {
					t.Fatal(err)
				}
				setEnabled := func(enabled bool) {
					t.Helper()
					if code, out := admin.do("PUT", "/webhooks/"+id, map[string]any{"url": recv.URL, "format": "json", "enabled": enabled}); code != http.StatusOK {
						t.Fatalf("update webhook: %d %v", code, out)
					}
				}
				setEnabled(false)
				s.retryWebhooks()
				var paused store.WebhookDelivery
				if err := s.st.DB.First(&paused, "id = ?", d.ID).Error; err != nil {
					t.Fatal(err)
				}
				if len(calls()) != 0 || paused.Status != "pending" || paused.Attempts != attempts || !paused.NextAt.After(time.Now()) || paused.LastError == "" {
					t.Fatalf("disabled webhook sent or consumed retry: %+v calls=%d", paused, len(calls()))
				}
				setEnabled(true)
				s.st.DB.Model(&paused).Update("next_at", time.Now().Add(-time.Second))
				s.retryWebhooks()
				var delivered store.WebhookDelivery
				s.st.DB.First(&delivered, "id = ?", d.ID)
				if len(calls()) != 1 || delivered.Status != "success" || delivered.Attempts != attempts+1 || delivered.EventID != d.EventID || delivered.Payload != d.Payload || delivered.LastError != "" {
					t.Fatalf("re-enable failed to deliver original event once: %+v calls=%d", delivered, len(calls()))
				}
			})
		}
	}
}

func TestMatchEventAndChatBody(t *testing.T) {
	for _, x := range []struct {
		subs []string
		typ  string
		want bool
	}{
		{nil, "user.created", true},
		{[]string{"user.*"}, "user.created", true},
		{[]string{"user.*"}, "node.offline", false},
		{[]string{"node.offline"}, "node.offline", true},
		{[]string{"node.online"}, "node.offline", false},
	} {
		if matchEvent(x.subs, x.typ) != x.want {
			t.Fatalf("matchEvent(%v, %s) != %v", x.subs, x.typ, x.want)
		}
	}
	b, target, _ := chatBody("dingtalk", "hi", "https://oapi.dingtalk.com/robot/send?access_token=x", "SECabc")
	if !strings.Contains(target, "sign=") || !strings.Contains(target, "timestamp=") || !strings.Contains(string(b), `"content":"hi"`) {
		t.Fatalf("dingtalk %s %s", target, b)
	}
	if b, _, _ := chatBody("discord", "hi", "https://x", ""); string(b) != `{"content":"hi"}` {
		t.Fatalf("discord %s", b)
	}
	if eventType(Event{Kind: "user_expired"}) != "user.expired" || eventType(Event{Kind: "user.created"}) != "user.created" {
		t.Fatal("eventType")
	}
	for k, v := range kindTypes { // every mapped type is subscribable
		found := false
		for _, e := range EventTypes {
			found = found || e == v
		}
		if !found {
			t.Fatalf("%s → %s missing from EventTypes", k, v)
		}
	}
}
