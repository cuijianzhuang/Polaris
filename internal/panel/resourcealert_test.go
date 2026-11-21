package panel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func resourceAlertSetup(t *testing.T) (*Server, chan Event) {
	t.Helper()
	s := newTestServer(t)
	events := make(chan Event, 32)
	s.sinks = append(s.sinks, events)
	if err := s.st.DB.Create(&store.Node{ID: "n1", Name: "hk"}).Error; err != nil {
		t.Fatal(err)
	}
	return s, events
}

func drainEvents(ch chan Event) []Event {
	var out []Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// putMetricSlot writes one NodeMetric row `back` slots before base.
func putMetricSlot(t *testing.T, s *Server, base time.Time, back int, cpu, mem, disk float64) {
	t.Helper()
	row := store.NodeMetric{NodeID: "n1", At: base.Add(-time.Duration(back) * metricSlot), CPU: cpu, Mem: mem, Disk: disk, N: 1}
	if err := s.st.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

func alertState(t *testing.T, s *Server, metric string) store.NodeResourceAlert {
	t.Helper()
	var st store.NodeResourceAlert
	if err := s.st.DB.First(&st, "node_id = ? AND metric = ?", "n1", metric).Error; err != nil {
		t.Fatal(err)
	}
	return st
}

func TestResourceAlertBreachAndRecovery(t *testing.T) {
	s, events := resourceAlertSetup(t)
	base := time.Now().UTC().Truncate(metricSlot)
	now := base.Add(2 * time.Minute)
	for back := 0; back < 3; back++ {
		putMetricSlot(t, s, base, back, 10, 95, 10)
	}
	s.checkResourceAlertsAt(now)
	got := drainEvents(events)
	if len(got) != 1 || got[0].Kind != "node_resource" || got[0].NodeID != "n1" || got[0].ResourceID != "mem" {
		t.Fatalf("want one mem breach event, got %+v", got)
	}
	if want := "🟠 节点 hk 内存持续 15 分钟超过 90%（当前 95%）"; got[0].Text != want {
		t.Fatalf("text = %q, want %q", got[0].Text, want)
	}
	if st := alertState(t, s, "mem"); !st.Active || !st.Since.Equal(base.Add(-2*metricSlot)) || st.LastValue != 95 {
		t.Fatalf("state after breach: %+v", st)
	}
	// Same data again (also what a restarted evaluator on a new leader
	// would see): the persisted state must prevent a duplicate alert.
	s.checkResourceAlertsAt(now)
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("duplicate alert after re-evaluation: %+v", got)
	}
	// A fresh slot below the threshold ends the breach with a recovery.
	putMetricSlot(t, s, base.Add(metricSlot), 0, 10, 60, 10)
	s.checkResourceAlertsAt(base.Add(metricSlot + 2*time.Minute))
	got = drainEvents(events)
	if len(got) != 1 || got[0].Kind != "node_resource_ok" || got[0].ResourceID != "mem" {
		t.Fatalf("want one mem recovery event, got %+v", got)
	}
	if want := "🟢 节点 hk 内存已恢复正常（当前 60%）"; got[0].Text != want {
		t.Fatalf("text = %q, want %q", got[0].Text, want)
	}
	if st := alertState(t, s, "mem"); st.Active {
		t.Fatalf("state still active after recovery: %+v", st)
	}
	s.checkResourceAlertsAt(base.Add(metricSlot + 2*time.Minute))
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("duplicate recovery: %+v", got)
	}
}

func TestResourceAlertNeedsFullDuration(t *testing.T) {
	s, events := resourceAlertSetup(t)
	base := time.Now().UTC().Truncate(metricSlot)
	for back := 0; back < 2; back++ { // 10 minutes over, needs 15
		putMetricSlot(t, s, base, back, 99, 10, 10)
	}
	s.checkResourceAlertsAt(base.Add(2 * time.Minute))
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("alerted before the duration elapsed: %+v", got)
	}
}

func TestResourceAlertFlapAndGapBreakTheRun(t *testing.T) {
	s, events := resourceAlertSetup(t)
	base := time.Now().UTC().Truncate(metricSlot)
	now := base.Add(2 * time.Minute)
	// Middle slot dips below the threshold.
	putMetricSlot(t, s, base, 0, 99, 10, 10)
	putMetricSlot(t, s, base, 1, 40, 10, 10)
	putMetricSlot(t, s, base, 2, 99, 10, 10)
	s.checkResourceAlertsAt(now)
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("flapping value alerted: %+v", got)
	}
	// A missing slot (node unobserved for 5 minutes) breaks the run too.
	s.st.DB.Delete(&store.NodeMetric{}, "node_id = ?", "n1")
	putMetricSlot(t, s, base, 0, 99, 10, 10)
	putMetricSlot(t, s, base, 2, 99, 10, 10)
	putMetricSlot(t, s, base, 3, 99, 10, 10)
	s.checkResourceAlertsAt(now)
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("gapped run alerted: %+v", got)
	}
}

func TestResourceAlertStaleDataIgnored(t *testing.T) {
	s, events := resourceAlertSetup(t)
	base := time.Now().UTC().Truncate(metricSlot)
	for back := 0; back < 3; back++ {
		putMetricSlot(t, s, base, back, 99, 10, 10)
	}
	// An hour later the newest slot is long stale: no breach from old data.
	s.checkResourceAlertsAt(base.Add(time.Hour))
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("stale data alerted: %+v", got)
	}
	// Breach while fresh, then the node goes silent: no fake recovery.
	s.checkResourceAlertsAt(base.Add(2 * time.Minute))
	if got := drainEvents(events); len(got) != 1 || got[0].Kind != "node_resource" {
		t.Fatalf("want the breach, got %+v", got)
	}
	s.checkResourceAlertsAt(base.Add(2 * time.Hour))
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("silence fabricated a recovery: %+v", got)
	}
	if st := alertState(t, s, "cpu"); !st.Active {
		t.Fatalf("breach state lost while node silent: %+v", st)
	}
}

func TestResourceAlertDisabledAndMetricsIndependent(t *testing.T) {
	s, events := resourceAlertSetup(t)
	base := time.Now().UTC().Truncate(metricSlot)
	now := base.Add(2 * time.Minute)
	for back := 0; back < 3; back++ {
		putMetricSlot(t, s, base, back, 95, 95, 10)
	}
	s.st.SetSetting("resource_alert_enabled", "false")
	s.checkResourceAlertsAt(now)
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("alerted while disabled: %+v", got)
	}
	s.st.SetSetting("resource_alert_enabled", "true")
	s.st.SetSetting("resource_alert_mem", "99") // memory no longer breaches at 95
	s.checkResourceAlertsAt(now)
	got := drainEvents(events)
	if len(got) != 1 || got[0].Kind != "node_resource" || got[0].ResourceID != "cpu" {
		t.Fatalf("want only the cpu breach, got %+v", got)
	}
	var count int64
	s.st.DB.Model(&store.NodeResourceAlert{}).Where("active = ?", true).Count(&count)
	if count != 1 {
		t.Fatalf("active states = %d, want 1 (cpu only)", count)
	}
}

func TestResourceAlertConfigParsing(t *testing.T) {
	s := newTestServer(t)
	cfg := s.resourceAlertConfig()
	if !cfg.Enabled || cfg.CPU != 90 || cfg.Mem != 90 || cfg.Disk != 90 || cfg.Minutes != 15 {
		t.Fatalf("defaults: %+v", cfg)
	}
	s.st.SetSetting("resource_alert_cpu", "80")
	s.st.SetSetting("resource_alert_mem", "junk")
	s.st.SetSetting("resource_alert_disk", "10") // below range: clamped
	s.st.SetSetting("resource_alert_minutes", "1000")
	cfg = s.resourceAlertConfig()
	if cfg.CPU != 80 || cfg.Mem != 90 || cfg.Disk != 50 || cfg.Minutes != 120 {
		t.Fatalf("parsed/clamped: %+v", cfg)
	}
	s.st.SetSetting("resource_alert_enabled", "false")
	if s.resourceAlertConfig().Enabled {
		t.Fatal("enabled=false not honored")
	}
}

func TestResourceAlertSettingsValidation(t *testing.T) {
	_, _, c := newAdminClient(t)
	cases := []struct {
		body map[string]string
		want int
	}{
		{map[string]string{"resource_alert_cpu": "49"}, 400},
		{map[string]string{"resource_alert_cpu": "101"}, 400},
		{map[string]string{"resource_alert_mem": "abc"}, 400},
		{map[string]string{"resource_alert_minutes": "4"}, 400},
		{map[string]string{"resource_alert_minutes": "121"}, 400},
		{map[string]string{"resource_alert_enabled": "yes"}, 400},
		{map[string]string{"resource_alert_cpu": "90", "resource_alert_mem": "85", "resource_alert_disk": "95", "resource_alert_minutes": "30", "resource_alert_enabled": "true"}, 200},
	}
	for _, tc := range cases {
		if code, out := c.do("PUT", "/settings", tc.body); code != tc.want {
			t.Fatalf("PUT %v = %d (%v), want %d", tc.body, code, out, tc.want)
		}
	}
}

func TestRecordMetricFoldsDisk(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC().Truncate(metricSlot).Add(time.Minute)
	s.recordMetric("n1", store.SystemInfo{Disk: 80}, now)
	s.recordMetric("n1", store.SystemInfo{Disk: 100}, now.Add(time.Minute))
	// An old agent's zero must not drag the average down.
	s.recordMetric("n1", store.SystemInfo{Disk: 0}, now.Add(2*time.Minute))
	var m store.NodeMetric
	if err := s.st.DB.First(&m, "node_id = ?", "n1").Error; err != nil {
		t.Fatal(err)
	}
	if m.Disk != 90 || m.N != 3 {
		t.Fatalf("disk fold: %+v", m)
	}
	if !strings.Contains(metricJSON(t, m), `"disk":90`) {
		t.Fatal("disk missing from metric JSON")
	}
}

func metricJSON(t *testing.T, m store.NodeMetric) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
