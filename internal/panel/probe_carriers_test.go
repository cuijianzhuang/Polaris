package panel

import (
	"math"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

func carrierOf(t *testing.T, out gin.H, key string) carrierView {
	t.Helper()
	cs, ok := out["carriers"].(gin.H)
	if !ok {
		t.Fatalf("carriers missing: %v", out)
	}
	v, ok := cs[key].(carrierView)
	if !ok {
		t.Fatalf("carrier %s missing: %v", key, cs)
	}
	return v
}

func approx(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 0.01 {
		t.Fatalf("%s: got %v, want %v", name, got, want)
	}
}

// Two telecom targets fold into one carrier column, weighted by sample
// count; unicom has a target but no samples; mobile has no target at all.
func TestCarrierSummaryWeighted(t *testing.T) {
	s := newTestServer(t)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, tg := range []store.ProbeTarget{
		{ID: "ct-a", Name: "上海电信", Carrier: "ct", Addr: "sh-ct-v4.example:80", Enabled: true},
		{ID: "ct-b", Name: "北京电信", Carrier: "ct", Addr: "bj-ct-v4.example:80", Enabled: true, SortOrder: 1},
		{ID: "cu-a", Name: "上海联通", Carrier: "cu", Addr: "sh-cu-v4.example:80", Enabled: true},
	} {
		if err := s.st.DB.Create(&tg).Error; err != nil {
			t.Fatal(err)
		}
	}
	node := store.Node{ID: "n1", Name: "n1", Status: "online"}
	if err := s.st.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	row := func(target string, at time.Time, sent, lost int, sum, last float64, lastAt time.Time) store.NodeLatency {
		return store.NodeLatency{NodeID: "n1", TargetID: target, At: at, Sent: sent, Lost: lost, SumMs: sum, LastMs: last, LastAt: lastAt}
	}
	rows := []store.NodeLatency{
		row("ct-a", now, 10, 1, 450, 48, now.Add(-10*time.Second)),                      // slot avg 50, loss 10%
		row("ct-a", now.Add(-30*time.Minute), 10, 5, 200, 40, now.Add(-30*time.Minute)), // slot avg 40, loss 50%
		row("ct-b", now, 30, 3, 2700, 99, now.Add(-20*time.Second)),                     // slot avg 100, loss 10%
	}
	for i := range rows {
		if err := s.st.DB.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	out := s.carrierSummary(&node, now)

	ct := carrierOf(t, out, "ct")
	if ct.State != "ok" {
		t.Fatalf("ct state %q, want ok", ct.State)
	}
	if ct.Current == nil || ct.Current.Ms != 48 {
		t.Fatalf("ct current %+v, want the freshest sample (48ms)", ct.Current)
	}
	// Latest slot across both targets: (450+2700) / (36 successes), 4/40 lost.
	approx(t, "ct current avg", ct.Current.Avg, 87.5)
	approx(t, "ct current loss", ct.Current.Loss, 10)
	// Last hour adds the older ct-a slot: 3350 / 41 successes, 9/50 lost.
	approx(t, "ct hour avg", ct.HourAvg, 3350.0/41)
	approx(t, "ct hour loss", ct.Loss1h, 18)
	if len(ct.Targets) != 2 {
		t.Fatalf("ct targets %d, want 2 (per-target breakdown)", len(ct.Targets))
	}
	approx(t, "ct-a hour loss", ct.Targets[0].Loss1h, 30)
	// Trend: 289 slots; the latest has the weighted figures, a slot two
	// hours back is a gap (nil), not a fabricated zero.
	if len(ct.Trend) != 289 {
		t.Fatalf("trend points %d, want 289", len(ct.Trend))
	}
	last := ct.Trend[len(ct.Trend)-1]
	approx(t, "trend last avg", last.Avg, 87.5)
	approx(t, "trend last loss", last.Loss, 10)
	gap := ct.Trend[len(ct.Trend)-1-24]
	if gap.Avg != nil || gap.Loss != nil {
		t.Fatalf("gap slot should stay empty: %+v", gap)
	}
	mid := ct.Trend[len(ct.Trend)-1-6] // 30 minutes back
	approx(t, "trend mid loss", mid.Loss, 50)

	if cu := carrierOf(t, out, "cu"); cu.State != "no_data" || len(cu.Targets) != 1 || cu.Targets[0].State != "no_data" {
		t.Fatalf("cu should be no_data with one target: %+v", cu)
	}
	if cm := carrierOf(t, out, "cm"); cm.State != "no_target" || len(cm.Targets) != 0 {
		t.Fatalf("cm should be no_target: %+v", cm)
	}
}

// A node whose newest sample is older than the freshness window reports
// stale: history stays, but there is no "current" reading.
func TestCarrierSummaryStale(t *testing.T) {
	s := newTestServer(t)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	if err := s.st.DB.Create(&store.ProbeTarget{ID: "ct-a", Name: "上海电信", Carrier: "ct", Addr: "sh-ct-v4.example:80", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	node := store.Node{ID: "n1", Name: "n1", Status: "online"}
	if err := s.st.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	old := now.Add(-10 * time.Minute)
	if err := s.st.DB.Create(&store.NodeLatency{NodeID: "n1", TargetID: "ct-a", At: old, Sent: 10, Lost: 2, SumMs: 400, LastMs: 50, LastAt: old}).Error; err != nil {
		t.Fatal(err)
	}
	ct := carrierOf(t, s.carrierSummary(&node, now), "ct")
	if ct.State != "stale" {
		t.Fatalf("ct state %q, want stale", ct.State)
	}
	if ct.Current != nil {
		t.Fatalf("stale carrier must not present a current reading: %+v", ct.Current)
	}
	approx(t, "stale hour loss", ct.Loss1h, 20) // history is still history
	if ct.Targets[0].State != "stale" || ct.Targets[0].Current != nil {
		t.Fatalf("target should be stale without current: %+v", ct.Targets[0])
	}
}

func TestCarrierSummaryDisabledAndV6(t *testing.T) {
	s := newTestServer(t)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	if err := s.st.DB.Create(&store.ProbeTarget{ID: "ct-a", Name: "上海电信", Carrier: "ct", Addr: "sh-ct-v4.example:80", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	off := store.Node{ID: "off", Name: "off", ProbeMode: "off"}
	if err := s.st.DB.Create(&off).Error; err != nil {
		t.Fatal(err)
	}
	for _, k := range carrierKeys {
		if v := carrierOf(t, s.carrierSummary(&off, now), k); v.State != "disabled" {
			t.Fatalf("probe-off node carrier %s state %q, want disabled", k, v.State)
		}
	}
	// An IPv6-only telecom target does not enter the IPv4 aggregation.
	v6 := store.Node{ID: "v6", Name: "v6", ProbeMode: "custom",
		ProbeTargets: store.NewJSON([]store.ProbeTarget{{ID: "ct6", Name: "电信 v6", Carrier: "ct", Addr: "[2001:db8::1]:80", Enabled: true}})}
	if err := s.st.DB.Create(&v6).Error; err != nil {
		t.Fatal(err)
	}
	if v := carrierOf(t, s.carrierSummary(&v6, now), "ct"); v.State != "no_target" {
		t.Fatalf("v6-only target: ct state %q, want no_target", v.State)
	}
	// Globally disabled probing reads as disabled too.
	if err := s.st.SetSetting("probe_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	on := store.Node{ID: "on", Name: "on"}
	if v := carrierOf(t, s.carrierSummary(&on, now), "ct"); v.State != "disabled" {
		t.Fatalf("global probe off: ct state %q, want disabled", v.State)
	}
}

func TestNodeCarriersEndpoint(t *testing.T) {
	s, _, c := newAdminClient(t)
	if err := s.st.DB.Create(&store.ProbeTarget{ID: "ct-a", Name: "上海电信", Carrier: "ct", Addr: "sh-ct-v4.example:80", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.Node{ID: "n1", Name: "n1", Status: "online"}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	slot := now.Truncate(latencySlot)
	if err := s.st.DB.Create(&store.NodeLatency{NodeID: "n1", TargetID: "ct-a", At: slot, Sent: 10, Lost: 1, SumMs: 450, LastMs: 48, LastAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	code, out := c.do("GET", "/nodes/n1/carriers", nil)
	if code != 200 {
		t.Fatalf("carriers %d %v", code, out)
	}
	cs, ok := out["carriers"].(map[string]any)
	if !ok {
		t.Fatalf("response shape: %v", out)
	}
	ct := cs["ct"].(map[string]any)
	if ct["state"] != "ok" || ct["current"].(map[string]any)["ms"] != float64(48) {
		t.Fatalf("ct over HTTP: %v", ct)
	}
	if _, ok := cs["cu"]; !ok {
		t.Fatalf("all three carriers are always present: %v", cs)
	}
	if code, _ := c.do("GET", "/nodes/missing/carriers", nil); code != 404 {
		t.Fatalf("missing node: %d, want 404", code)
	}
}
