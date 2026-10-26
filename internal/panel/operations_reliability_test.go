package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestRolloutCancelWinsInFlightFailure(t *testing.T) {
	s := newTestServer(t)
	rolloutNodesFixture(t, s, 1)
	(&fakeFleet{}).install(t, s)
	r, err := s.newRollout(rolloutReq{Kind: "agent"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	rolloutUpgradeAgent = func(s *Server, _ context.Context, _ *store.Node) error {
		s.st.DB.Model(&store.UpgradeRollout{}).Where("id = ?", r.ID).Update("state", "canceled")
		return errors.New("in-flight command failed after cancel")
	}
	s.runRollouts()
	var got store.UpgradeRollout
	s.st.DB.First(&got, "id = ?", r.ID)
	if got.State != "canceled" {
		t.Fatalf("cancel overwritten: %s", got.State)
	}
	if len(got.Results.V) != 1 {
		t.Fatal("lost in-flight result")
	}
}

func TestRolloutConcurrentAdmission(t *testing.T) {
	s := newTestServer(t)
	rolloutNodesFixture(t, s, 1)
	(&fakeFleet{}).install(t, s)
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.newRollout(rolloutReq{Kind: "agent"}, "admin"); err == nil {
				count.Add(1)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("admitted %d simultaneous rollouts", count.Load())
	}
}

func TestRolloutPauseResumeInvalidatesOldWorker(t *testing.T) {
	s := newTestServer(t)
	rolloutNodesFixture(t, s, 1)
	(&fakeFleet{}).install(t, s)
	r, err := s.newRollout(rolloutReq{Kind: "agent"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	rolloutUpgradeAgent = func(s *Server, _ context.Context, _ *store.Node) error {
		var saved store.UpgradeRollout
		if err := s.st.DB.First(&saved, "id = ?", r.ID).Error; err != nil {
			return err
		}
		if len(saved.Batch.V) != 1 || saved.Cursor != 1 || saved.Results.V[saved.Batch.V[0]].State != "queued" {
			return errors.New("command issued before durable intent")
		}
		// Pause and resume can restore the same state while changing generation.
		s.st.DB.Model(&store.UpgradeRollout{}).Where("id = ?", r.ID).Updates(map[string]any{"state": "running", "control_version": gorm.Expr("control_version + 2"), "healthy_since": nil})
		return errors.New("old in-flight result")
	}
	s.runRollouts()
	var got store.UpgradeRollout
	s.st.DB.First(&got, "id = ?", r.ID)
	if got.State != "running" || got.ControlVersion != 2 || got.HealthySince != nil {
		t.Fatalf("old worker overwrote resumed task: %+v", got)
	}
	if len(got.Results.V) != 1 {
		t.Fatal("in-flight outcome lost")
	}
}

func TestRolloutPausedBetweenClaimAndReloadDoesNotDispatch(t *testing.T) {
	s := newTestServer(t)
	rolloutNodesFixture(t, s, 1)
	f := &fakeFleet{}
	f.install(t, s)
	r, err := s.newRollout(rolloutReq{Kind: "agent"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var changed atomic.Bool
	name := "test:pause-claimed-rollout"
	s.st.DB.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*store.UpgradeRollout); ok && changed.CompareAndSwap(false, true) {
			s.st.DB.Model(&store.UpgradeRollout{}).Where("id = ?", r.ID).Updates(map[string]any{"state": "paused", "control_version": gorm.Expr("control_version + 1")})
		}
	})
	defer s.st.DB.Callback().Query().Remove(name)
	s.runRollouts()
	var got store.UpgradeRollout
	s.st.DB.First(&got, "id = ?", r.ID)
	if !changed.Load() || got.State != "paused" || len(f.upgraded) != 0 || got.LeaseToken != "" {
		t.Fatalf("paused task dispatched: %+v upgraded=%v", got, f.upgraded)
	}
}

func TestUserReminderDurabilityAndIndependentRetry(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.sinks = []chan Event{make(chan Event)} // no sink can accept a notification
	expires := time.Now().Add(5 * 24 * time.Hour)
	u := store.User{ID: "notice-user", Username: "notice", UUID: "x", Status: "active", TelegramID: 42, Email: "buyer@example.com", ExpireAt: &expires, SubToken: "private-subscription-token"}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	s.remindUsers()
	s.remindUsers()
	var jobs []store.UserNotice
	s.st.DB.Where("user_id = ?", u.ID).Find(&jobs)
	if len(jobs) != 2 {
		t.Fatalf("want one durable task per channel, got %d", len(jobs))
	}
	var tg, mail store.UserNotice
	for _, j := range jobs {
		if j.Channel == "telegram" {
			tg = j
		} else {
			mail = j
		}
	}
	var calls atomic.Int32
	var reject atomic.Bool
	reject.Store(true)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if reject.Load() {
			io.WriteString(w, `{"ok":false,"description":"temporary failure"}`)
		} else {
			io.WriteString(w, `{"ok":true,"result":{}}`)
		}
	}))
	defer provider.Close()
	s.tg.mu.Lock()
	s.tg.token = "123:test"
	s.tg.base = provider.URL
	s.tg.mu.Unlock()
	s.deliverUserNotice(tg.ID)
	var got store.UserNotice
	s.st.DB.First(&got, "id = ?", tg.ID)
	if got.Status != "pending" || got.Attempts != 1 || !got.LeaseUntil.IsZero() {
		t.Fatalf("failure not retryable: %+v", got)
	}
	// Retry reads the persisted task; no in-memory event is required.
	s.st.DB.Model(&store.UserNotice{}).Where("id = ?", tg.ID).Update("next_at", time.Now().Add(-time.Minute))
	reject.Store(false)
	s.deliverUserNotice(tg.ID)
	s.deliverUserNotice(tg.ID)
	s.st.DB.First(&got, "id = ?", tg.ID)
	if got.Status != "sent" || calls.Load() != 2 {
		t.Fatalf("delivery/duplicate: %+v calls %d", got, calls.Load())
	}
	got = store.UserNotice{}
	s.st.DB.First(&got, "id = ?", mail.ID)
	if got.Status != "pending" || got.Attempts != 0 {
		t.Fatal("other channel changed")
	}
	// A renewal invalidates the outstanding old-term mail before sending.
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("expire_at", time.Now().Add(30*24*time.Hour))
	s.deliverUserNotice(mail.ID)
	got = store.UserNotice{}
	s.st.DB.First(&got, "id = ?", mail.ID)
	if got.Status != "canceled" {
		t.Fatal("stale reminder delivered after renewal")
	}
	code, out := admin.do("GET", "/user-notices", nil)
	if code != 200 || strings.Contains(fmt.Sprint(out), "buyer@example.com") || strings.Contains(fmt.Sprint(out), "private-subscription-token") {
		t.Fatal("delivery API leaked private payload")
	}
}

func TestUserReminderSchedulingIsAtomic(t *testing.T) {
	s := newTestServer(t)
	expires := time.Now().Add(5 * 24 * time.Hour)
	u := store.User{ID: "notice-atomic", Username: "atomic", Status: "active", TelegramID: 42, ExpireAt: &expires}
	s.st.DB.Create(&u)
	name := "review:reject-notice"
	s.st.DB.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "user_notices" {
			tx.AddError(errors.New("disk failure"))
		}
	})
	defer s.st.DB.Callback().Create().Remove(name)
	if err := s.queueUserReminders(u.ID, time.Now()); err == nil {
		t.Fatal("task write failure ignored")
	}
	var got store.User
	s.st.DB.First(&got, "id = ?", u.ID)
	if got.WarnedExpire != nil {
		t.Fatal("marker committed without durable task")
	}
}

func TestUserReminderKeepsPluginWebhookEvents(t *testing.T) {
	s := newTestServer(t)
	s.st.SetSetting("tg_notify_users", "false")
	s.st.SetSetting("mail_notify_users", "false")
	expires := time.Now().Add(5 * 24 * time.Hour)
	s.st.DB.Create(&store.User{ID: "hook-reminder", Username: "hook", Status: "active", TelegramID: 42, ExpireAt: &expires})
	s.st.DB.Create(&store.Webhook{ID: "reminder-hook", Enabled: true, Events: store.NewJSON([]string{"user.expire_warn"})})
	s.remindUsers()
	s.remindUsers()
	var deliveries []store.WebhookDelivery
	s.st.DB.Where("webhook_id = ?", "reminder-hook").Find(&deliveries)
	if len(deliveries) != 1 || deliveries[0].Type != "user.expire_warn" || !strings.Contains(deliveries[0].Payload, "到期提醒") {
		t.Fatalf("plugin reminder lost or duplicated: %+v", deliveries)
	}
}

func TestExternalMetricsRejectOlderAndEqualSamples(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "metric-node", Name: "metric"})
	now := time.Now().UTC().Truncate(time.Second)
	for i, at := range []time.Time{now, now.Add(-time.Hour), now} {
		code, out := admin.do("POST", "/nodes/metric-node/metrics", map[string]any{"metrics": []map[string]any{{"name": "latency.review", "unit": "ms", "value": 30 + i, "at": at.Format(time.RFC3339)}}})
		if code != 200 {
			t.Fatalf("post: %d %v", code, out)
		}
		want := float64(0)
		if i == 0 {
			want = 1
		}
		if out["stored"] != want {
			t.Fatalf("stored: %v", out)
		}
	}
	var got store.NodeExternalMetric
	s.st.DB.First(&got, "node_id = ?", "metric-node")
	if got.Value != 30 {
		t.Fatalf("new value overwritten: %v", got.Value)
	}
}

func TestExternalMetricsTokenNamesAreDisplayOnly(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "metric-source", Name: "metric"})
	var firstID string
	for i := 0; i < 2; i++ {
		row, secret, err := s.issueAPIToken("same name", "", []string{ScopeNodesMetrics}, 0, "admin")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = row.ID
		}
		client := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: secret}
		if code, out := client.do("POST", "/nodes/metric-source/metrics", map[string]any{"metrics": []map[string]any{{"name": "latency.source", "unit": "ms", "value": i + 1}}}); code != 200 {
			t.Fatalf("post %d %v", code, out)
		}
		if i == 0 {
			s.st.DB.Model(&store.APIToken{}).Where("id = ?", row.ID).Update("name", "renamed")
			if code, out := client.do("POST", "/nodes/metric-source/metrics", map[string]any{"metrics": []map[string]any{{"name": "latency.source", "unit": "ms", "value": 10}}}); code != 200 {
				t.Fatalf("rename post %d %v", code, out)
			}
		}
	}
	var rows []store.NodeExternalMetric
	s.st.DB.Where("node_id = ?", "metric-source").Find(&rows)
	if len(rows) != 2 {
		t.Fatalf("same-named sources merged: %+v", rows)
	}
	for _, row := range rows {
		if row.Source == "token:"+firstID && (row.SourceLabel != "renamed" || row.Value != 10) {
			t.Fatalf("renaming changed identity: %+v", row)
		}
	}
}

func TestRenderCacheBoundsAndExpiry(t *testing.T) {
	c := renderCache{}
	now := time.Now()
	body := make([]byte, pluginRenderMax)
	for i := 0; i < 12; i++ {
		c.put(fmt.Sprint(i), renderEntry{body: body, until: now.Add(time.Minute)}, now.Add(time.Duration(i)*time.Millisecond))
	}
	if c.bytes > pluginRenderBytes || len(c.m) != 8 {
		t.Fatalf("unbounded cache: bytes=%d entries=%d", c.bytes, len(c.m))
	}
	if _, ok := c.get("0", now); ok {
		t.Fatal("oldest item was not evicted")
	}
	c.mu.Lock()
	c.expire(now.Add(time.Minute))
	c.mu.Unlock()
	if c.bytes != 0 || len(c.m) != 0 {
		t.Fatal("expired bodies retained")
	}
	c.put("x", renderEntry{body: body, until: now.Add(time.Minute)}, now)
	c.put("x", renderEntry{body: []byte("small"), until: now.Add(time.Minute)}, now)
	if c.bytes != 5 {
		t.Fatal("replacement byte accounting is wrong")
	}
}
