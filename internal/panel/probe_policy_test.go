package panel

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/backupfmt"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestNodeProbePolicyScheduling(t *testing.T) {
	s, _, admin := newAdminClient(t)
	for _, id := range []string{"inherit", "custom", "off"} {
		if err := s.st.DB.Create(&store.Node{ID: id, Name: id, Status: "online"}).Error; err != nil {
			t.Fatal(err)
		}
		defer fakeProber(t, s, id, map[string]string{"zstaticcdn": "10", "custom.example": "20"})()
	}
	st, out := admin.do("GET", "/nodes/inherit/probe-policy", nil)
	if st != 200 || out["mode"] != "inherit" || len(out["effectiveTargets"].([]any)) != 3 {
		t.Fatalf("default %d %v", st, out)
	}
	st, out = admin.do("PUT", "/nodes/custom/probe-policy", map[string]any{"mode": "custom", "targets": []map[string]any{{"name": "独立测试", "addr": "custom.example:443", "carrier": "ct"}}})
	if st != 200 {
		t.Fatalf("custom %d %v", st, out)
	}
	id := out["targets"].([]any)[0].(map[string]any)["id"].(string)
	st, out = admin.do("PUT", "/nodes/off/probe-policy", map[string]any{"mode": "off"})
	if st != 200 {
		t.Fatalf("off %d %v", st, out)
	}
	s.runLatencyProbes()
	var rows []store.NodeLatency
	if err := s.st.DB.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.NodeID]++
		if r.Lost != 0 || (r.NodeID == "custom" && r.TargetID != id) {
			t.Fatalf("unexpected sample %+v", r)
		}
	}
	if counts["inherit"] != 3 || counts["custom"] != 1 || counts["off"] != 0 {
		t.Fatalf("schedule %v", counts)
	}
	// Global pause overrides custom policies too.
	if err := s.st.SetSetting("probe_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	s.runLatencyProbes()
	var repeated []store.NodeLatency
	s.st.DB.Find(&repeated)
	for _, r := range repeated {
		if r.Sent != 1 {
			t.Fatalf("global off ignored: %+v", r)
		}
	}
	st, out = admin.do("GET", "/nodes/custom/latency", nil)
	if st != 200 || len(out["targets"].([]any)) != 1 || len(out["points"].([]any)) != 1 {
		t.Fatalf("history %d %v", st, out)
	}
	// Public metadata contains neither custom addresses nor hidden node targets.
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "custom").Update("status_hidden", true).Error; err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s.probeOverview(true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "custom.example") || strings.Contains(string(b), "独立测试") {
		t.Fatalf("public leak %s", b)
	}
	b, _ = json.Marshal(s.probeOverview(false))
	if strings.Contains(string(b), "custom.example") || !strings.Contains(string(b), "独立测试") {
		t.Fatalf("admin metadata %s", b)
	}
	// An empty global library must not prevent custom probes from running.
	for _, target := range s.latencyTargets(false) {
		if st, out := admin.do("DELETE", "/probe/targets/"+target.ID, nil); st != 204 {
			t.Fatalf("remove global %d %v", st, out)
		}
	}
	if err := s.st.SetSetting("probe_enabled", "true"); err != nil {
		t.Fatal(err)
	}
	s.runLatencyProbes()
	var custom store.NodeLatency
	if err := s.st.DB.First(&custom, "node_id = ? AND target_id = ?", "custom", id).Error; err != nil {
		t.Fatal(err)
	}
	if custom.Sent != 2 {
		t.Fatalf("empty global library suppressed custom probing: %+v", custom)
	}
}

func TestNodeProbePolicyHistoryAndLateResults(t *testing.T) {
	s, _, admin := newAdminClient(t)
	if err := s.st.DB.Create(&store.Node{ID: "n", Name: "node"}).Error; err != nil {
		t.Fatal(err)
	}
	set := func(addr, name string) store.ProbeTarget {
		t.Helper()
		st, out := admin.do("PUT", "/nodes/n/probe-policy", map[string]any{"mode": "custom", "targets": []map[string]any{{"name": name, "addr": addr}}})
		if st != 200 {
			t.Fatalf("set %d %v", st, out)
		}
		var n store.Node
		if err := s.st.DB.First(&n, "id = ?", "n").Error; err != nil {
			t.Fatal(err)
		}
		return n.ProbeTargets.V[0]
	}
	old := set("old.example:80", "old")
	if err := s.recordNodeProbe("n", old, 10, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	renamed := set("old.example:80", "renamed")
	if renamed.ID != old.ID {
		t.Fatal("rename discarded history identity")
	}
	var count int64
	s.st.DB.Model(&store.NodeLatency{}).Count(&count)
	if count != 1 {
		t.Fatal("rename lost history")
	}
	current := set("new.example:80", "new")
	if current.ID == old.ID {
		t.Fatal("changed address reused history")
	}
	if err := s.recordNodeProbe("n", old, 10, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.NodeLatency{}).Count(&count)
	if count != 0 {
		t.Fatal("late sample resurrected old target")
	}
	if err := s.recordNodeProbe("n", current, 10, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	st, out := admin.do("PUT", "/nodes/n/probe-policy", map[string]any{"mode": "off"})
	if st != 200 {
		t.Fatalf("off %d %v", st, out)
	}
	if err := s.recordNodeProbe("n", current, 10, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.NodeLatency{}).Count(&count)
	if count != 0 {
		t.Fatal("off accepted pending sample")
	}
	set("new.example:80", "new")
	st, out = admin.do("DELETE", "/nodes/n", nil)
	if st != 204 {
		t.Fatalf("delete %d %v", st, out)
	}
	if err := s.recordNodeProbe("n", current, 10, true, time.Now()); err == nil {
		t.Fatal("deleted node accepted sample")
	}
	s.st.DB.Model(&store.NodeLatency{}).Count(&count)
	if count != 0 {
		t.Fatal("deleted node left history")
	}
}

func TestNodeProbePolicyValidation(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n", Name: "node"})
	for _, body := range []map[string]any{
		{}, {"mode": "bad"}, {"mode": "custom"},
		{"mode": "off", "targets": []map[string]any{{"name": "a", "addr": "a:80"}}},
		{"mode": "custom", "targets": []map[string]any{{"name": "a", "addr": "https://example.com"}}},
		{"mode": "custom", "targets": []map[string]any{{"name": "a", "addr": "a:80"}, {"name": "b", "addr": "A:80"}}},
		{"mode": "custom", "targets": []map[string]any{{"name": "a", "addr": "a:80", "enabled": false}}},
	} {
		if st, out := admin.do("PUT", "/nodes/n/probe-policy", body); st != 400 {
			t.Fatalf("validation %d %v", st, out)
		}
	}
	if st, _ := admin.do("GET", "/nodes/missing/probe-policy", nil); st != 404 {
		t.Fatalf("missing GET %d", st)
	}
	if st, _ := admin.do("PUT", "/nodes/missing/probe-policy", map[string]any{"mode": "off"}); st != 404 {
		t.Fatalf("missing PUT %d", st)
	}
	// Invalid requests must leave the inherited policy unchanged.
	var n store.Node
	s.st.DB.First(&n, "id = ?", "n")
	if n.ProbeMode != "" || len(n.ProbeTargets.V) != 0 {
		t.Fatalf("invalid request mutated policy %+v", n)
	}
}

func TestNodeProbePolicyRollbackAndBackup(t *testing.T) {
	s, _, admin := newAdminClient(t)
	if err := s.st.DB.Create(&store.Node{ID: "n", Name: "node"}).Error; err != nil {
		t.Fatal(err)
	}
	st, out := admin.do("PUT", "/nodes/n/probe-policy", map[string]any{"mode": "custom", "targets": []map[string]any{{"name": "IPv6", "addr": "[2001:db8::1]:443"}}})
	if st != 200 {
		t.Fatalf("set %d %v", st, out)
	}
	var n store.Node
	s.st.DB.First(&n, "id = ?", "n")
	target := n.ProbeTargets.V[0]
	if err := s.recordNodeProbe("n", target, 12, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	const callback = "test:probe-policy-failure"
	if err := s.st.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "nodes" {
			if values, ok := tx.Statement.Dest.(map[string]any); ok {
				if _, exists := values["probe_mode"]; exists {
					tx.AddError(errors.New("injected write failure"))
				}
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	st, _ = admin.do("PUT", "/nodes/n/probe-policy", map[string]any{"mode": "off"})
	s.st.DB.Callback().Update().Remove(callback)
	if st != 500 {
		t.Fatalf("write failure status %d", st)
	}
	var count int64
	s.st.DB.Model(&store.NodeLatency{}).Count(&count)
	if count != 1 {
		t.Fatal("failed policy update erased history")
	}
	tables, _, err := backupfmt.ExportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "n").Update("probe_mode", "off").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Transaction(func(tx *gorm.DB) error { return backupfmt.ImportAll(tx, tables, func(v string) string { return v }) }); err != nil {
		t.Fatal(err)
	}
	var restored store.Node
	if err := s.st.DB.First(&restored, "id = ?", "n").Error; err != nil {
		t.Fatal(err)
	}
	if restored.ProbeMode != "custom" || len(restored.ProbeTargets.V) != 1 || restored.ProbeTargets.V[0].ID != target.ID || restored.ProbeTargets.V[0].Addr != target.Addr {
		t.Fatalf("backup lost policy %+v", restored)
	}
	if summary := s.latencySummary([]string{"n"}, time.Now()); summary["n"][target.ID] == nil {
		t.Fatal("restored policy lost measurements")
	}
}

func TestNodeProbePolicyGlobalTargetChange(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n", Name: "node"})
	s.seedProbeTargets()
	old := s.latencyTargets(true)[0]
	if err := s.recordNodeProbe("n", old, 12, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	st, out := admin.do("PUT", "/probe/targets/"+old.ID, map[string]any{"name": old.Name, "addr": "changed.example:443", "carrier": old.Carrier})
	if st != 200 {
		t.Fatalf("change %d %v", st, out)
	}
	if err := s.recordNodeProbe("n", old, 12, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	var count int64
	s.st.DB.Model(&store.NodeLatency{}).Count(&count)
	if count != 0 {
		t.Fatal("old global target measurement survived address change")
	}
	var current store.ProbeTarget
	s.st.DB.First(&current, "id = ?", old.ID)
	if err := s.recordNodeProbe("n", current, 12, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st, out := admin.do("DELETE", "/probe/targets/"+old.ID, nil); st != 204 {
		t.Fatalf("delete %d %v", st, out)
	}
	if err := s.recordNodeProbe("n", current, 12, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.NodeLatency{}).Count(&count)
	if count != 0 {
		t.Fatal("deleted global target resurrected")
	}
}
