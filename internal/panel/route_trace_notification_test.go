package panel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func routeEventSink(s *Server) chan Event {
	ch := make(chan Event, 20)
	s.sinks = append(s.sinks, ch)
	return ch
}

func takeRouteEvent(t *testing.T, ch chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	default:
		t.Fatal("missing route event")
		return Event{}
	}
}

func TestRouteChangeEventEvidenceAndCommit(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	ch := routeEventSink(s)
	first := recordTrace(t, s, traceResult("1.1.1.1", "10.0.0.1", "1.1.1.1"))
	if len(ch) != 0 {
		t.Fatal("baseline generated notification")
	}
	rows, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	changed := traceResult("1.1.1.1", "10.0.0.2", "1.1.1.1")
	hook := "test:route-notify-rollback"
	if err := s.st.DB.Callback().Delete().Before("gorm:delete").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "node_route_traces" {
			tx.AddError(errors.New("injected retention failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.finishRouteTrace(rows, []model.RouteTraceResult{changed}, "completed")
	s.st.DB.Callback().Delete().Remove(hook)
	if err == nil || len(ch) != 0 {
		t.Fatal("rolled-back change notified", err, len(ch))
	}
	saved, err := s.finishRouteTrace(rows, []model.RouteTraceResult{changed}, "completed")
	if err != nil {
		t.Fatal(err)
	}
	e := takeRouteEvent(t, ch)
	if e.Kind != "route_changed" || eventType(e) != "node.route_changed" || e.NodeID != "trace" || e.Data["previous_trace_id"] != first.ID || e.Data["trace_id"] != saved[0].ID {
		t.Fatal(e)
	}
	b, _ := json.Marshal(e)
	for _, secret := range []string{"10.0.0.1", "10.0.0.2", "private-network-evidence"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("private evidence in notification", string(b))
		}
	}
	if _, err := s.finishRouteTrace(rows, []model.RouteTraceResult{changed}, "completed"); err != nil {
		t.Fatal(err)
	}
	recordTrace(t, s, changed) // unchanged path
	changed.State = "timeout"
	recordTrace(t, s, changed)
	changed.State = "reached"
	recordTrace(t, s, changed) // do not compare across failure
	if len(ch) != 0 {
		t.Fatal("duplicate/unchanged/incomparable notification", len(ch))
	}
}

func TestRouteChangeEventLateAndConcurrentCompletion(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	ch := routeEventSink(s)
	recordTrace(t, s, traceResult("1.1.1.1", "10.0.0.1", "1.1.1.1"))
	req := model.RouteTraceRequest{IPv4: "1.1.1.1"}
	old, err := s.beginRouteTrace("trace", req)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.beginRouteTrace("trace", req)
	if err != nil {
		t.Fatal(err)
	}
	changed := traceResult("1.1.1.1", "10.0.0.2", "1.1.1.1")
	if _, err := s.finishRouteTrace(newer, []model.RouteTraceResult{changed}, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.finishRouteTrace(old, []model.RouteTraceResult{changed}, "completed"); err != nil {
		t.Fatal(err)
	}
	if len(ch) != 0 {
		t.Fatal("late result notified")
	}
	// Complete a new independent change through two panel instances.
	pending, err := s.beginRouteTrace("trace", req)
	if err != nil {
		t.Fatal(err)
	}
	changed.Hops[0].Addresses = []string{"10.0.0.3"}
	cfg := *s.cfg
	other, err := New(&cfg, s.log, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db, _ := other.st.DB.DB(); db.Close() }()
	other.sinks = append(other.sinks, ch)
	var wg sync.WaitGroup
	for _, server := range []*Server{s, other} {
		wg.Add(1)
		go func(server *Server) {
			defer wg.Done()
			copyRows := append([]store.NodeRouteTrace{}, pending...)
			if _, err := server.finishRouteTrace(copyRows, []model.RouteTraceResult{changed}, "completed"); err != nil {
				t.Error(err)
			}
		}(server)
	}
	wg.Wait()
	if len(ch) != 1 {
		t.Fatal("duplicate cross-instance event", len(ch))
	}
}

func TestRouteChangeNotificationPolicy(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	now := time.Now()
	e := Event{Kind: "route_changed", NodeID: "trace", ResourceID: "target-a", Text: "change A", noticeRecipient: "admin"}
	for _, channel := range notificationChannels {
		if _, allow := s.operationalNotice(channel, e, now); allow {
			t.Fatal("new event enabled by default", channel)
		}
	}
	p := defaultNotificationPolicy()
	p.RepeatWindowSeconds = 300
	p.Channels = map[string]map[string]bool{"email": {"route_changed": true}, "telegram": {"route_changed": true}}
	saveNoticePolicy(t, s, p)
	if _, allow := s.operationalNotice("email", e, now); !allow {
		t.Fatal("opt-in did not enable event")
	}
	e.Text = "another path, another history ID"
	if _, allow := s.operationalNotice("email", e, now.Add(time.Second)); allow {
		t.Fatal("path flapping bypassed cooldown")
	}
	if _, allow := s.operationalNotice("telegram", e, now); !allow {
		t.Fatal("email occupied telegram cooldown")
	}
	other := e
	other.ResourceID = "target-b"
	if _, allow := s.operationalNotice("email", other, now); !allow {
		t.Fatal("target cooldown shared")
	}
	other = e
	other.noticeRecipient = "another-admin"
	if _, allow := s.operationalNotice("email", other, now); !allow {
		t.Fatal("recipient cooldown shared")
	}
	notice, allow := s.operationalNotice("email", e, now.Add(301*time.Second))
	if !allow || notice.noticeMerged < 1 {
		t.Fatal("suppression count lost", notice, allow)
	}
	p.Maintenance = []notificationWindow{{ID: "node-maintenance", NodeIDs: []string{"trace"}, Channels: []string{"email"}, StartsAt: now, EndsAt: now.Add(time.Hour)}}
	saveNoticePolicy(t, s, p)
	if _, allow := s.operationalNotice("email", e, now.Add(10*time.Minute)); allow {
		t.Fatal("maintenance ignored")
	}
	p.Maintenance = nil
	p.RepeatWindowSeconds = 0
	saveNoticePolicy(t, s, p)
	if _, allow := s.operationalNotice("email", e, now); !allow {
		t.Fatal("zero cooldown blocked event")
	}
	if _, allow := s.operationalNotice("email", e, now); !allow {
		t.Fatal("zero cooldown blocked repeated event")
	}
}

func TestRouteChangePolicyUnavailableFailsClosed(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	if err := s.st.SetSetting(notificationPolicyKey, "invalid JSON"); err != nil {
		t.Fatal(err)
	}
	e := Event{Kind: "route_changed", NodeID: "trace", Text: "changed"}
	if _, allow := s.operationalNotice("email", e, time.Now()); allow {
		t.Fatal("invalid policy enabled default-off event")
	}
	d := store.WebhookDelivery{Type: "node.route_changed", Payload: `{"data":{"node_id":"trace"}}`}
	if !s.operationalWebhookMuted(&d, time.Now()) {
		t.Fatal("invalid policy did not pause route webhook")
	}
	// Existing event fallback semantics remain unchanged.
	if _, allow := s.operationalNotice("email", Event{Kind: "node_offline", NodeID: "trace", Text: "down"}, time.Now()); !allow {
		t.Fatal("existing failure fallback changed")
	}
}

func TestRouteChangePolicyAPIAndWebhookRetry(t *testing.T) {
	s, admin := traceHistoryFixture(t)
	p := defaultNotificationPolicy()
	p.Channels = map[string]map[string]bool{"webhook": {"route_changed": true}}
	if st, out := admin.do("PUT", "/notifications/policy", p); st != 200 {
		t.Fatal(st, out)
	} else {
		event := out["events"].(map[string]any)["route_changed"].(map[string]any)
		if event["defaultOff"] != true || event["cooldown"] != true {
			t.Fatal("event defaults not discoverable", event)
		}
	}
	now := time.Now()
	d := store.WebhookDelivery{Type: "node.route_changed", Payload: `{"data":{"node_id":"trace"}}`}
	if s.operationalWebhookMuted(&d, now) {
		t.Fatal("enabled webhook muted")
	}
	p.Channels["webhook"]["route_changed"] = false
	saveNoticePolicy(t, s, p)
	if !s.operationalWebhookMuted(&d, now) {
		t.Fatal("disabled webhook retry allowed")
	}
	p.Channels["webhook"]["route_changed"] = true
	p.Maintenance = []notificationWindow{{ID: "maintenance", NodeIDs: []string{"trace"}, StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Minute)}}
	saveNoticePolicy(t, s, p)
	if !s.operationalWebhookMuted(&d, now) {
		t.Fatal("maintenance webhook retry allowed")
	}
	if s.operationalWebhookMuted(&d, now.Add(2*time.Minute)) {
		t.Fatal("maintenance never ended")
	}
}

func TestRouteChangeMailDeliverySwitches(t *testing.T) {
	s := newTestServer(t)
	mail := useMail(t, s)
	s.st.SetSetting("mail_admin_to", "ops@example.com")
	e := Event{Kind: "route_changed", NodeID: "trace", ResourceID: "target", Text: "route observation"}
	s.mailEvent(context.Background(), e)
	if len(mail.To("ops@example.com")) != 0 {
		t.Fatal("default-off route event sent mail")
	}
	p := defaultNotificationPolicy()
	p.Channels = map[string]map[string]bool{"email": {"route_changed": true}}
	saveNoticePolicy(t, s, p)
	s.mailEvent(context.Background(), e)
	e.Text = "flapping path"
	s.mailEvent(context.Background(), e)
	if len(mail.To("ops@example.com")) != 1 {
		t.Fatal("mail cooldown did not merge flapping")
	}
	p.RepeatWindowSeconds = 0
	saveNoticePolicy(t, s, p)
	s.st.SetSetting("mail_notify_admin", "false")
	s.mailEvent(context.Background(), e)
	if len(mail.To("ops@example.com")) != 1 {
		t.Fatal("administrator mail switch bypassed")
	}
	s.st.SetSetting("mail_notify_admin", "true")
	s.mailEvent(context.Background(), e)
	if len(mail.To("ops@example.com")) != 2 {
		t.Fatal("mail not restored")
	}
	p.Channels["email"]["route_changed"] = false
	saveNoticePolicy(t, s, p)
	s.mailEvent(context.Background(), e)
	if len(mail.To("ops@example.com")) != 2 {
		t.Fatal("event mail switch bypassed")
	}
}
