package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func saveNoticePolicy(t *testing.T, s *Server, p notificationPolicy) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetSetting(notificationPolicyKey, string(b)); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationIncidentMerging(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	down := Event{Kind: "node_offline", NodeID: "n", Text: "down"}
	up := Event{Kind: "node_online", NodeID: "n", Text: "up"}
	if _, ok := s.operationalNotice("email", up, now); ok {
		t.Fatal("orphan recovery sent")
	}
	if _, ok := s.operationalNotice("email", down, now); !ok {
		t.Fatal("first fault suppressed")
	}
	for i := 0; i < 3; i++ {
		if _, ok := s.operationalNotice("email", down, now.Add(time.Duration(i+1)*time.Hour)); ok {
			t.Fatal("ongoing fault repeated")
		}
	}
	// A new server object sharing the database models a restarted/second sink.
	restarted := &Server{st: s.st, log: s.log}
	result, ok := restarted.operationalNotice("email", up, now.Add(4*time.Hour))
	if !ok || result.Data["notification_suppressed"] != int64(3) || !strings.Contains(result.Text, "3") {
		t.Fatalf("recovery summary %+v %v", result, ok)
	}
	if _, ok := restarted.operationalNotice("email", up, now.Add(5*time.Hour)); ok {
		t.Fatal("duplicate recovery")
	}
	if _, ok := restarted.operationalNotice("email", down, now.Add(6*time.Hour)); !ok {
		t.Fatal("new incident suppressed")
	}
	if _, ok := restarted.operationalNotice("telegram", down, now); !ok {
		t.Fatal("channels shared dedup state")
	}
	if _, ok := restarted.operationalNotice("email", Event{Kind: "node_offline", NodeID: "other", Text: "down"}, now); !ok {
		t.Fatal("nodes shared dedup state")
	}
}

func TestNotificationMaintenanceAndSwitches(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	p := defaultNotificationPolicy()
	p.Maintenance = []notificationWindow{{ID: "work", NodeIDs: []string{"n"}, Channels: []string{"email"}, StartsAt: now, EndsAt: now.Add(time.Hour)}}
	saveNoticePolicy(t, s, p)
	down := Event{Kind: "node_offline", NodeID: "n", Text: "down"}
	up := Event{Kind: "node_online", NodeID: "n", Text: "up"}
	if _, ok := s.operationalNotice("email", down, now.Add(-time.Second)); !ok {
		t.Fatal("window started early")
	}
	if _, ok := s.operationalNotice("email", up, now); ok {
		t.Fatal("maintenance recovery sent")
	}
	if _, ok := s.operationalNotice("email", down, now.Add(time.Minute)); ok {
		t.Fatal("maintenance fault sent")
	}
	if _, ok := s.operationalNotice("email", up, now.Add(2*time.Minute)); ok {
		t.Fatal("silent incident recovery sent")
	}
	if _, ok := s.operationalNotice("email", down, now.Add(time.Hour)); !ok {
		t.Fatal("window did not expire")
	}
	if _, ok := s.operationalNotice("telegram", down, now); !ok {
		t.Fatal("channel scope ignored")
	}
	if _, ok := s.operationalNotice("email", Event{Kind: "node_offline", NodeID: "other", Text: "down"}, now); !ok {
		t.Fatal("node scope ignored")
	}
	if _, ok := s.operationalNotice("email", Event{Kind: "backup_failed", Text: "backup failed"}, now); !ok {
		t.Fatal("node window muted global task")
	}
	p.Channels = map[string]map[string]bool{"webhook": {"node_offline": false}}
	saveNoticePolicy(t, s, p)
	if _, ok := s.operationalNotice("webhook", down, now); ok {
		t.Fatal("event switch ignored")
	}
	if _, ok := s.operationalNotice("webhook", up, now.Add(time.Minute)); ok {
		t.Fatal("muted failure generated orphan recovery")
	}
}

func TestNotificationProtectedEvents(t *testing.T) {
	s := newTestServer(t)
	p := defaultNotificationPolicy()
	noRecovery := false
	p.NotifyRecovery = &noRecovery
	p.EnabledChannels = map[string]bool{"telegram": false, "email": false, "webhook": false}
	p.Maintenance = []notificationWindow{{ID: "global", StartsAt: time.Now().Add(-time.Hour), EndsAt: time.Now().Add(time.Hour)}}
	saveNoticePolicy(t, s, p)
	for _, event := range []Event{
		{Kind: "admin_login_new_ip"}, {Kind: "admin_recovery_code"}, {Kind: "login_ban"},
		{Kind: "order_delivered"}, {Type: "order.paid"}, {Type: "order.failed"}, {Kind: "ticket_reply"},
		{Kind: "user_limited", UserID: "u"}, {Kind: "expire_warn", UserOnly: true}, {Kind: "unknown_new_event"},
		{Kind: "operational_alert", ResourceID: "payment-pending"}, {Kind: "operational_alert", ResourceID: "refund-pending"},
		{Kind: "node_offline", Type: "admin.login_new_ip"},
	} {
		for _, channel := range notificationChannels {
			for i := 0; i < 2; i++ {
				if _, ok := s.operationalNotice(channel, event, time.Now()); !ok {
					t.Fatalf("protected event muted: %s %+v", channel, event)
				}
			}
		}
	}
	var n int64
	s.st.DB.Model(&store.NotificationState{}).Count(&n)
	if n != 0 {
		t.Fatal("protected events consumed dedup state")
	}
}

func TestNotificationRecoveryGateClosesEveryIncident(t *testing.T) {
	for _, events := range [][2]Event{
		{{Kind: "node_offline", NodeID: "n"}, {Kind: "node_online", NodeID: "n"}},
		{{Kind: "inbound_down", NodeID: "n", ResourceID: "in"}, {Kind: "inbound_up", NodeID: "n", ResourceID: "in"}},
		{{Kind: "plugin_offline", ResourceID: "plugin"}, {Kind: "plugin_online", ResourceID: "plugin"}},
		{{Kind: "health", NodeID: "n", ResourceID: "route", Data: map[string]any{"down": true}}, {Kind: "health", NodeID: "n", ResourceID: "route", Data: map[string]any{"down": false}}},
	} {
		t.Run(events[0].Kind, func(t *testing.T) {
			s := newTestServer(t)
			p := defaultNotificationPolicy()
			off := false
			p.NotifyRecovery = &off
			saveNoticePolicy(t, s, p)
			now := time.Now()
			for _, channel := range notificationChannels {
				if _, ok := s.operationalNotice(channel, events[0], now); !ok {
					t.Fatal("fault was silenced by recovery gate")
				}
				if _, ok := s.operationalNotice(channel, events[1], now.Add(time.Second)); ok {
					t.Fatal("recovery ignored its global gate")
				}
				if _, ok := s.operationalNotice(channel, events[0], now.Add(2*time.Second)); !ok {
					t.Fatal("muted recovery did not close the incident")
				}
			}
		})
	}
}

func TestNotificationChannelGatePreservesDetailRules(t *testing.T) {
	s := newTestServer(t)
	p := defaultNotificationPolicy()
	p.Channels = map[string]map[string]bool{"email": {"backup_failed": false}, "telegram": {"route_changed": true}}
	saveNoticePolicy(t, s, p)
	now := time.Now()
	down := Event{Kind: "node_offline", NodeID: "n"}
	up := Event{Kind: "node_online", NodeID: "n"}
	if _, ok := s.operationalNotice("email", down, now); !ok {
		t.Fatal("initial fault not sent")
	}
	p.EnabledChannels = map[string]bool{"email": false}
	saveNoticePolicy(t, s, p)
	if _, ok := s.operationalNotice("email", up, now.Add(time.Second)); ok {
		t.Fatal("disabled channel sent recovery")
	}
	if _, ok := s.operationalNotice("email", Event{Kind: "apply_failed", NodeID: "n"}, now); ok {
		t.Fatal("disabled channel sent a new fault")
	}
	if _, ok := s.operationalNotice("telegram", down, now); !ok {
		t.Fatal("channel gate leaked to another destination")
	}
	p.EnabledChannels["email"] = true
	saveNoticePolicy(t, s, p)
	if _, ok := s.operationalNotice("email", down, now.Add(2*time.Second)); !ok {
		t.Fatal("reenabled channel lost the next fault")
	}
	if _, ok := s.operationalNotice("email", Event{Kind: "backup_failed"}, now); ok {
		t.Fatal("channel enable discarded the backup override")
	}
	loaded, err := s.loadNotificationPolicy()
	if err != nil || loaded.Channels["email"]["backup_failed"] || !loaded.Channels["telegram"]["route_changed"] {
		t.Fatal("detail preferences were replaced")
	}
}

func TestNotificationRepeatWindowAndResourceIsolation(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	e := Event{Kind: "apply_failed", NodeID: "n", Text: "rev 10 failed", Data: map[string]any{"revision": 10}}
	if _, ok := s.operationalNotice("email", e, now); !ok {
		t.Fatal("first failure")
	}
	if _, ok := s.operationalNotice("email", e, now.Add(time.Minute)); ok {
		t.Fatal("repeat not merged")
	}
	out, ok := s.operationalNotice("email", e, now.Add(5*time.Minute))
	if !ok || out.Data["notification_suppressed"] != int64(1) {
		t.Fatalf("repeat summary %+v", out)
	}
	if len(e.Data) != 1 {
		t.Fatal("policy mutated shared event data")
	}
	e.Text = "rev 11 failed"
	if _, ok := s.operationalNotice("email", e, now.Add(6*time.Minute)); !ok {
		t.Fatal("new revision suppressed")
	}
	for _, id := range []string{"in1", "in2"} {
		if _, ok := s.operationalNotice("email", Event{Kind: "inbound_down", NodeID: "n", ResourceID: id, Text: "failed"}, now); !ok {
			t.Fatal("inbounds merged together")
		}
	}
}

func TestNotificationConcurrentInstances(t *testing.T) {
	dsn := ""
	if os.Getenv("POLARIS_TEST_ALL_POSTGRES") != "" {
		dsn = pgOnly(t)
	}
	instances := clusterServers(t, dsn, 2)
	s, other := instances[0], instances[1]
	var sent atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			server := s
			if i%2 == 1 {
				server = other
			}
			if _, ok := server.operationalNotice("email", Event{Kind: "node_offline", NodeID: "n", Text: "down"}, time.Now()); ok {
				sent.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if sent.Load() != 1 {
		t.Fatalf("concurrent sends %d", sent.Load())
	}
}

func TestNotificationPolicyAPI(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n", Name: "node"})
	if st, out := admin.do("GET", "/notifications/policy", nil); st != 200 || out["policy"].(map[string]any)["repeatWindowSeconds"] != float64(300) {
		t.Fatalf("default %d %v", st, out)
	}
	p := defaultNotificationPolicy()
	noRecovery := false
	p.NotifyRecovery = &noRecovery
	p.EnabledChannels = map[string]bool{"email": false}
	p.Channels = map[string]map[string]bool{"email": {"apply_failed": false}}
	p.Maintenance = []notificationWindow{{ID: "planned", NodeIDs: []string{"n"}, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour)}}
	if st, out := admin.do("PUT", "/notifications/policy", p); st != 200 {
		t.Fatalf("save %d %v", st, out)
	} else {
		saved := out["policy"].(map[string]any)
		if saved["notifyRecovery"] != false || saved["enabledChannels"].(map[string]any)["email"] != false {
			t.Fatal("simple notification controls were not persisted", saved)
		}
	}
	before := s.st.GetSetting(notificationPolicyKey, "")
	for _, body := range []any{
		nil, map[string]any{"unknown": true}, map[string]any{"repeatWindowSeconds": -1},
		map[string]any{"enabledChannels": map[string]bool{"unknown": false}},
		map[string]any{"notifyRecovery": "false"},
		map[string]any{"channels": map[string]any{"sms": map[string]bool{}}},
		map[string]any{"channels": map[string]any{"email": map[string]bool{"login_ban": false}}},
		map[string]any{"maintenance": []notificationWindow{{ID: "bad", StartsAt: time.Now(), EndsAt: time.Now().Add(8 * 24 * time.Hour)}}},
		map[string]any{"maintenance": []notificationWindow{{ID: "bad", NodeIDs: []string{"missing"}, StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour)}}},
	} {
		if st, out := admin.do("PUT", "/notifications/policy", body); st != 400 {
			t.Fatalf("invalid %d %v", st, out)
		}
		if s.st.GetSetting(notificationPolicyKey, "") != before {
			t.Fatal("invalid request changed policy")
		}
	}
	// New policy cannot be bypassed through the generic settings endpoint.
	admin.do("PUT", "/settings", map[string]string{notificationPolicyKey: "{}"})
	if s.st.GetSetting(notificationPolicyKey, "") != before {
		t.Fatal("generic settings bypass")
	}
	if st, _ := (&apiClient{t: t, base: admin.base}).do("GET", "/notifications/policy", nil); st != 401 {
		t.Fatalf("anonymous %d", st)
	}
	// Bodies are bounded and trailing values are rejected.
	for _, raw := range []string{"{} {}", `{"channels":` + strings.Repeat(" ", 65536) + `{}}`} {
		req, _ := http.NewRequest("PUT", admin.base+"/notifications/policy", bytes.NewBufferString(raw))
		req.Header.Set("Authorization", "Bearer "+admin.tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("raw validation %d", resp.StatusCode)
		}
	}
}

func TestNotificationMailIntegration(t *testing.T) {
	s := newTestServer(t)
	m := useMail(t, s)
	s.st.SetSetting("mail_admin_to", "ops@example.com")
	down := Event{Kind: "node_offline", NodeID: "n", Text: "offline"}
	up := Event{Kind: "node_online", NodeID: "n", Text: "online"}
	for _, e := range []Event{down, down, up, up} {
		s.mailEvent(context.Background(), e)
	}
	if got := len(m.To("ops@example.com")); got != 2 {
		t.Fatalf("mail fault/recovery count %d", got)
	}
	p := defaultNotificationPolicy()
	p.Channels = map[string]map[string]bool{"email": {"node_offline": false}}
	saveNoticePolicy(t, s, p)
	s.mailEvent(context.Background(), down)
	s.mailEvent(context.Background(), up)
	if got := len(m.To("ops@example.com")); got != 2 {
		t.Fatal("disabled fault/recovery leaked")
	}
	for i := 0; i < 2; i++ {
		s.mailEvent(context.Background(), Event{Kind: "admin_login_new_ip", Text: "security"})
	}
	if got := len(m.To("ops@example.com")); got != 4 {
		t.Fatal("security messages were deduplicated")
	}
	// The old administrator master switch still tracks recovery, so enabling
	// it again cannot leave a later incident permanently suppressed.
	saveNoticePolicy(t, s, defaultNotificationPolicy())
	s.mailEvent(context.Background(), down)
	s.st.SetSetting("mail_notify_admin", "false")
	s.mailEvent(context.Background(), up)
	s.st.SetSetting("mail_notify_admin", "true")
	s.mailEvent(context.Background(), down)
	if got := len(m.To("ops@example.com")); got != 6 {
		t.Fatalf("re-enabled channel lost new incident: %d", got)
	}
}

func TestNotificationStateFailureAndBackup(t *testing.T) {
	s := newTestServer(t)
	e := Event{Kind: "node_offline", NodeID: "n", Text: "private fault text"}
	const callback = "test:notification-write-failure"
	s.st.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "notification_states" {
			tx.AddError(errors.New("injected"))
		}
	})
	if _, ok := s.operationalNotice("email", e, time.Now()); !ok {
		t.Fatal("DB failure silently swallowed fault")
	}
	s.st.DB.Callback().Create().Remove(callback)
	if _, ok := s.operationalNotice("email", e, time.Now()); !ok {
		t.Fatal("failed transaction consumed attempt")
	}
	files, _, err := exportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["notification_states"]; ok {
		t.Fatal("runtime dedup state exported")
	}
	var states []store.NotificationState
	s.st.DB.Find(&states)
	b, _ := json.Marshal(states)
	if strings.Contains(string(b), e.Text) {
		t.Fatal("raw message persisted")
	}
}

func TestNotificationFailedAttemptRelease(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	down := Event{Kind: "node_offline", NodeID: "n", Text: "down"}
	up := Event{Kind: "node_online", NodeID: "n", Text: "up"}
	first, ok := s.operationalNotice("email", down, now)
	if !ok {
		t.Fatal("first attempt")
	}
	s.retryOperationalNotice(first)
	second, ok := s.operationalNotice("email", down, now.Add(time.Second))
	if !ok {
		t.Fatal("failed fault cannot retry")
	}
	s.retryOperationalNotice(first) // late failure must not undo the newer reservation
	if _, ok := s.operationalNotice("email", down, now.Add(2*time.Second)); ok {
		t.Fatal("stale failure undid new reservation")
	}
	recovered, ok := s.operationalNotice("email", up, now.Add(3*time.Second))
	if !ok {
		t.Fatal("recovery")
	}
	if _, ok := s.operationalNotice("email", up, now.Add(4*time.Second)); ok {
		t.Fatal("recovery duplicate")
	}
	s.retryOperationalNotice(recovered)
	if _, ok := s.operationalNotice("email", up, now.Add(5*time.Second)); !ok {
		t.Fatal("failed recovery cannot retry")
	}
	s.retryOperationalNotice(second)
	if _, ok := s.operationalNotice("email", up, now.Add(6*time.Second)); ok {
		t.Fatal("stale fault failure corrupted closed incident")
	}
}

func TestNotificationTelegramIntegration(t *testing.T) {
	fake := &fakeTG{}
	api := httptest.NewServer(fake)
	defer api.Close()
	s := newTestServer(t)
	s.st.SetSetting("tg_enabled", "true")
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.SetSetting("tg_api_base", api.URL)
	s.st.SetSetting("tg_poll_timeout", "0")
	s.st.DB.Create(&store.TGAdmin{ID: "owner", ChatID: 100, Role: "owner", Notify: true})
	s.st.DB.Create(&store.TGAdmin{ID: "other", ChatID: 200, Role: "owner", Notify: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	go s.tg.runNotifications(ctx, s.tgEvents)
	waitUntil(t, "bot running", s.tg.isRunning)
	for _, e := range []Event{{Kind: "node_offline", NodeID: "n", Text: "FAULT"}, {Kind: "node_offline", NodeID: "n", Text: "FAULT"}, {Kind: "node_online", NodeID: "n", Text: "RECOVERED"}, {Kind: "node_online", NodeID: "n", Text: "RECOVERED"}, {Kind: "admin_login_new_ip", Text: "BARRIER"}} {
		s.tgEvents <- e
	}
	waitUntil(t, "telegram notices processed", func() bool { return strings.Contains(fake.textsTo(200), "BARRIER") })
	for _, id := range []int64{100, 200} {
		text := fake.textsTo(id)
		if strings.Count(text, "FAULT") != 1 || strings.Count(text, "RECOVERED") != 1 {
			t.Fatalf("recipient %d notices %s", id, text)
		}
	}
}

func TestNotificationWebhookIntegration(t *testing.T) {
	s, _, admin := newAdminClient(t)
	receiver, calls := hookReceiver(t, 0)
	for _, name := range []string{"a", "b"} {
		if st, out := admin.do("POST", "/webhooks", map[string]any{"name": name, "url": receiver.URL, "events": []string{"node.*", "order.paid"}}); st != 201 {
			t.Fatalf("hook %d %v", st, out)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runWebhooks(ctx, s.hookEvents)
	for _, e := range []Event{{Kind: "node_offline", NodeID: "n", Text: "down"}, {Kind: "node_offline", NodeID: "n", Text: "down"}, {Kind: "node_online", NodeID: "n", Text: "up"}, {Kind: "node_online", NodeID: "n", Text: "up"}, {Type: "order.paid"}} {
		s.hookEvents <- e
	}
	waitFor(t, "webhook deliveries", func() bool { return len(calls()) >= 6 })
	var deliveries []store.WebhookDelivery
	s.st.DB.Find(&deliveries)
	if len(deliveries) != 6 {
		t.Fatalf("expected one fault/recovery/business event per hook, got %d", len(deliveries))
	}
	counts := map[string]int{}
	for _, d := range deliveries {
		counts[d.WebhookID+":"+d.Type]++
	}
	for key, count := range counts {
		if count != 1 {
			t.Fatalf("duplicate %s %d", key, count)
		}
	}
	p := defaultNotificationPolicy()
	p.Channels = map[string]map[string]bool{"webhook": {"node_offline": false}}
	saveNoticePolicy(t, s, p)
	for _, e := range []Event{{Kind: "node_offline", NodeID: "n", Text: "down again"}, {Kind: "node_online", NodeID: "n", Text: "up again"}, {Type: "order.paid"}} {
		s.hookEvents <- e
	}
	waitFor(t, "protected order event", func() bool { return len(calls()) >= 8 })
	s.st.DB.Find(&deliveries)
	if len(deliveries) != 8 {
		t.Fatal("disabled operational events or orphan recovery sent")
	}
}

func TestNotificationPolicyCorruptionAndSaveFailure(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.SetSetting(notificationPolicyKey, "invalid JSON")
	if st, _ := admin.do("GET", "/notifications/policy", nil); st != 500 {
		t.Fatal("corrupt configuration hidden")
	}
	if _, ok := s.operationalNotice("email", Event{Kind: "backup_failed", Text: "failure"}, time.Now()); !ok {
		t.Fatal("corrupt policy swallowed alert")
	}
	saveNoticePolicy(t, s, defaultNotificationPolicy())
	before := s.st.GetSetting(notificationPolicyKey, "")
	const callback = "test:policy-failure"
	s.st.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "settings" {
			tx.AddError(errors.New("injected"))
		}
	})
	st, _ := admin.do("PUT", "/notifications/policy", map[string]any{"repeatWindowSeconds": 600})
	s.st.DB.Callback().Create().Remove(callback)
	if st != 500 || s.st.GetSetting(notificationPolicyKey, "") != before {
		t.Fatal("failed policy save was not atomic")
	}
}

func TestNotificationWebhookRetrySilence(t *testing.T) {
	s := newTestServer(t)
	receiver, calls := hookReceiver(t, 0)
	h := store.Webhook{ID: "h", Name: "hook", URL: receiver.URL, Enabled: true}
	if err := s.st.DB.Create(&h).Error; err != nil {
		t.Fatal(err)
	}
	for _, d := range []store.WebhookDelivery{
		{ID: "ops", WebhookID: h.ID, EventID: "event-ops", Type: "node.offline", Payload: `{"type":"node.offline","data":{"node_id":"n","message":"down"}}`, Status: "pending"},
		{ID: "payment", WebhookID: h.ID, EventID: "event-payment", Type: "order.paid", Payload: `{"type":"order.paid","data":{}}`, Status: "pending"},
	} {
		if err := s.st.DB.Create(&d).Error; err != nil {
			t.Fatal(err)
		}
	}
	p := defaultNotificationPolicy()
	p.Maintenance = []notificationWindow{{ID: "work", StartsAt: time.Now().Add(-time.Hour), EndsAt: time.Now().Add(time.Hour)}}
	saveNoticePolicy(t, s, p)
	s.deliverWebhook("ops")
	var delayed store.WebhookDelivery
	s.st.DB.First(&delayed, "id = ?", "ops")
	if len(calls()) != 0 || delayed.Attempts != 0 || delayed.Status != "pending" {
		t.Fatalf("operational retry bypassed maintenance %+v", delayed)
	}
	s.deliverWebhook("payment")
	if len(calls()) != 1 {
		t.Fatal("maintenance blocked payment webhook")
	}
	saveNoticePolicy(t, s, defaultNotificationPolicy())
	s.deliverWebhook("ops")
	if len(calls()) != 2 {
		t.Fatal("delayed webhook did not resume")
	}
	s.st.DB.First(&delayed, "id = ?", "ops")
	if delayed.Status != "success" || delayed.EventID != "event-ops" {
		t.Fatal("retry changed identity or failed")
	}
}
