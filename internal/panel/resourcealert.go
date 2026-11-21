package panel

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Node resource threshold alerts: node_offline covers a node that stops
// reporting and node_traffic covers its bandwidth quota, but nothing
// watched the machine itself filling up or pegging. This evaluator runs
// on the leader only (registered with add in startJobs) and reads the
// 5-minute NodeMetric slots from the database — never in-memory samples —
// so in HA it sees every node's data, whichever instance took the
// heartbeat. A metric breaches when the newest slotsNeeded consecutive
// slots are all at or above its threshold (slotsNeeded = ceil(minutes/5),
// so the default 15 minutes is 3 slots); one slot back below the
// threshold ends the breach. State lives in NodeResourceAlert, so a
// leader change neither replays an alert nor loses a recovery.

type resourceAlertConfig struct {
	Enabled bool
	CPU     float64
	Mem     float64
	Disk    float64
	Minutes int
}

func (s *Server) resourceAlertConfig() resourceAlertConfig {
	cfg := resourceAlertConfig{Enabled: true, CPU: 90, Mem: 90, Disk: 90, Minutes: 15}
	cfg.Enabled = s.st.GetSetting("resource_alert_enabled", "true") != "false"
	pct := func(key string, def float64) float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(s.st.GetSetting(key, "")), 64)
		if err != nil {
			return def
		}
		return min(max(v, 50), 100)
	}
	cfg.CPU = pct("resource_alert_cpu", cfg.CPU)
	cfg.Mem = pct("resource_alert_mem", cfg.Mem)
	cfg.Disk = pct("resource_alert_disk", cfg.Disk)
	if v, err := strconv.Atoi(strings.TrimSpace(s.st.GetSetting("resource_alert_minutes", ""))); err == nil {
		cfg.Minutes = min(max(v, 5), 120)
	}
	return cfg
}

type resourceMetric struct {
	key   string
	label string
	value func(store.NodeMetric) float64
	limit func(resourceAlertConfig) float64
}

var resourceMetrics = []resourceMetric{
	{"cpu", "CPU", func(m store.NodeMetric) float64 { return m.CPU }, func(c resourceAlertConfig) float64 { return c.CPU }},
	{"mem", "内存", func(m store.NodeMetric) float64 { return m.Mem }, func(c resourceAlertConfig) float64 { return c.Mem }},
	{"disk", "磁盘", func(m store.NodeMetric) float64 { return m.Disk }, func(c resourceAlertConfig) float64 { return c.Disk }},
}

// resourceBreach reports whether rows (newest first) start with an
// unbroken run of at least slotsNeeded slots, each at or above the
// threshold; it returns the first slot of the run. A missing slot breaks
// the run just like a slot below the threshold: a gap means the panel did
// not observe the node over threshold for the whole window.
func resourceBreach(rows []store.NodeMetric, slotsNeeded int, threshold float64, value func(store.NodeMetric) float64) (time.Time, bool) {
	if len(rows) < slotsNeeded {
		return time.Time{}, false
	}
	for i := 0; i < slotsNeeded; i++ {
		if value(rows[i]) < threshold {
			return time.Time{}, false
		}
		if i > 0 && !rows[i].At.Equal(rows[i-1].At.Add(-metricSlot)) {
			return time.Time{}, false
		}
	}
	return rows[slotsNeeded-1].At, true
}

func (s *Server) checkResourceAlerts() { s.checkResourceAlertsAt(time.Now().UTC()) }

func (s *Server) checkResourceAlertsAt(now time.Time) {
	cfg := s.resourceAlertConfig()
	if !cfg.Enabled {
		return
	}
	slotsNeeded := int(math.Ceil(float64(cfg.Minutes) / metricSlot.Minutes()))
	var nodes []store.Node
	if err := s.st.DB.Select("id", "name").Find(&nodes).Error; err != nil {
		s.log.Error("resource alerts: list nodes", "err", err)
		return
	}
	for _, n := range nodes {
		var rows []store.NodeMetric
		if err := s.st.DB.Where("node_id = ? AND at <= ?", n.ID, now).Order("at DESC").Limit(slotsNeeded).Find(&rows).Error; err != nil {
			s.log.Error("resource alerts: read metrics", "node", n.ID, "err", err)
			continue
		}
		// No fresh slot: the node stopped reporting (node_offline owns
		// that story). Judging stale data would cry wolf on breaches and
		// fabricate recoveries alike.
		if len(rows) == 0 || rows[0].At.Before(now.Add(-2*metricSlot)) {
			continue
		}
		for _, rm := range resourceMetrics {
			s.evalResourceMetric(n, rm, cfg, rows, slotsNeeded, now)
		}
	}
}

func (s *Server) evalResourceMetric(n store.Node, rm resourceMetric, cfg resourceAlertConfig, rows []store.NodeMetric, slotsNeeded int, now time.Time) {
	threshold := rm.limit(cfg)
	latest := rm.value(rows[0])
	since, breach := resourceBreach(rows, slotsNeeded, threshold, rm.value)
	var st store.NodeResourceAlert
	s.st.DB.First(&st, "node_id = ? AND metric = ?", n.ID, rm.key) // absent = never breached
	save := func(active bool, since time.Time) {
		row := store.NodeResourceAlert{NodeID: n.ID, Metric: rm.key, Active: active, Since: since, LastValue: latest, UpdatedAt: now}
		if err := s.st.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
			s.log.Error("resource alerts: save state", "node", n.ID, "metric", rm.key, "err", err)
		}
	}
	switch {
	case breach && !st.Active:
		save(true, since)
		s.notify(Event{Kind: "node_resource", NodeID: n.ID, ResourceID: rm.key,
			Text: fmt.Sprintf("🟠 节点 %s %s持续 %d 分钟超过 %d%%（当前 %d%%）", n.Name, rm.label, cfg.Minutes, int(math.Round(threshold)), int(math.Round(latest))),
			Data: map[string]any{"metric": rm.key, "value": latest, "threshold": threshold}})
	case !breach && st.Active && latest < threshold:
		save(false, st.Since)
		s.notify(Event{Kind: "node_resource_ok", NodeID: n.ID, ResourceID: rm.key,
			Text: fmt.Sprintf("🟢 节点 %s %s已恢复正常（当前 %d%%）", n.Name, rm.label, int(math.Round(latest))),
			Data: map[string]any{"metric": rm.key, "value": latest, "threshold": threshold}})
	case st.Active:
		save(true, st.Since) // still breached or mid-gap: keep LastValue fresh
	}
}
