package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestBusinessEventsSurviveQueueSaturationAndRestart(t *testing.T) {
	s, _, admin := newAdminClient(t)
	receiver, calls := hookReceiver(t, 0)
	if code, out := admin.do("POST", "/webhooks", map[string]any{"name": "durable", "url": receiver.URL, "events": []string{"user.*"}}); code != 201 {
		t.Fatalf("hook: %d %v", code, out)
	}
	for len(s.hookEvents) < cap(s.hookEvents) {
		s.hookEvents <- Event{Kind: "unknown"}
	}
	if code, out := admin.do("POST", "/users", map[string]any{"username": "original"}); code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	var d store.WebhookDelivery
	if err := s.st.DB.First(&d, "type = ?", "user.created").Error; err != nil {
		t.Fatal(err)
	}
	if d.Status != "pending" || len(calls()) != 0 {
		t.Fatal("event must be committed before any worker runs")
	}
	if err := s.st.DB.Model(&store.User{}).Where("username = ?", "original").Update("username", "later").Error; err != nil {
		t.Fatal(err)
	}
	// A fresh consumer needs only the committed DB; none of the old channels.
	restarted := &Server{st: s.st, cfg: s.cfg, log: s.log, shared: s.shared}
	restarted.retryWebhooks()
	restarted.retryWebhooks()
	if got := calls(); len(got) != 1 || !strings.Contains(string(got[0].body), `"username":"original"`) || strings.Contains(string(got[0].body), `"uuid"`) || strings.Contains(string(got[0].body), `"password"`) {
		t.Fatalf("immutable redacted snapshot not delivered exactly once by worker: %d", len(got))
	}
}

func TestBusinessEventInsertFailureRollsBackMutation(t *testing.T) {
	s, _, admin := newAdminClient(t)
	receiver, _ := hookReceiver(t, 0)
	if code, _ := admin.do("POST", "/webhooks", map[string]any{"name": "durable", "url": receiver.URL}); code != 201 {
		t.Fatal(code)
	}
	if err := s.st.DB.Callback().Create().Before("gorm:create").Register("test:fail_business_outbox", func(tx *gorm.DB) {
		if tx.Statement.Table == "webhook_deliveries" {
			tx.AddError(errors.New("synthetic outbox failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Create().Remove("test:fail_business_outbox") })
	if code, _ := admin.do("POST", "/users", map[string]any{"username": "must-rollback"}); code < 400 {
		t.Fatal("mutation succeeded without its event")
	}
	var count int64
	s.st.DB.Model(&store.User{}).Where("username = ?", "must-rollback").Count(&count)
	if count != 0 {
		t.Fatal("user survived failed outbox insert")
	}
}

func TestBusinessCommandReplayDoesNotEnqueueTwice(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	receiver, _ := hookReceiver(t, 0)
	admin.do("POST", "/webhooks", map[string]any{"name": "durable", "url": receiver.URL, "events": []string{"user.renewed"}})
	exp := time.Now().Add(24 * time.Hour)
	u := store.User{ID: "outbox-user", Username: "outbox-user", Status: "active", ExpireAt: &exp}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	code, token := admin.do("POST", "/tokens", map[string]any{"name": "durable", "scopes": []string{"users:write"}})
	if code != 201 {
		t.Fatal(code)
	}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", srv.URL+s.cfg.APIBase()+"/users/"+u.ID+"/renew", bytes.NewBufferString(`{"days":2}`))
		req.Header.Set("Authorization", "Bearer "+token["secret"].(string))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "same-business-order")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatal(resp.StatusCode)
		}
	}
	var events []store.WebhookDelivery
	s.st.DB.Where("type = ?", "user.renewed").Find(&events)
	if len(events) != 1 {
		t.Fatalf("replayed command enqueued %d events", len(events))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(events[0].Payload), &payload); err != nil || payload["id"] != events[0].EventID {
		t.Fatal("event id not preserved")
	}
}

func TestBusinessEventCleanupKeepsUnresolvedDeliveries(t *testing.T) {
	s, _, _ := newAdminClient(t)
	old := time.Now().AddDate(0, 0, -10)
	for _, status := range []string{"pending", "sending", "failed", "success"} {
		if err := s.st.DB.Create(&store.WebhookDelivery{ID: status, Status: status, CreatedAt: old}).Error; err != nil {
			t.Fatal(err)
		}
	}
	s.cleanup()
	var ds []store.WebhookDelivery
	if err := s.st.DB.Find(&ds).Error; err != nil {
		t.Fatal(err)
	}
	if len(ds) != 3 {
		t.Fatalf("cleanup retained %d rows, want 3 unresolved deliveries", len(ds))
	}
	for _, d := range ds {
		if d.Status == "success" {
			t.Fatal("expired success retained")
		}
	}
}
