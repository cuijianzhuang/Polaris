package panel

import (
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestCosts(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "hk", Address: "203.0.113.1"})
	s.st.DB.Create(&store.Node{ID: "n2", Name: "us", Address: "203.0.113.2"})
	s.st.DB.Create(&store.Node{ID: "n3", Name: "jp", Address: "203.0.113.3"})
	events := make(chan Event, 8)
	s.sinks = append(s.sinks, events)

	soon := time.Now().AddDate(0, 0, 3).Format("2006-01-02")
	past := time.Now().AddDate(0, 0, -3).Format("2006-01-02")
	if code, out := c.do("PUT", "/nodes/n1/cost", map[string]any{"provider": "Vultr", "amount": 60, "currency": "CNY", "cycle": "month", "expireAt": soon}); code != 200 {
		t.Fatalf("put cost %d %v", code, out)
	}
	c.do("PUT", "/nodes/n2/cost", map[string]any{"amount": 120, "currency": "USD", "cycle": "year", "expireAt": past, "autoRenew": true})
	c.do("PUT", "/nodes/n3/cost", map[string]any{"amount": 10, "currency": "XYZ", "cycle": "month"})
	if code, _ := c.do("PUT", "/nodes/n1/cost", map[string]any{"amount": 1, "cycle": "weekly"}); code != 400 {
		t.Fatal("bad cycle accepted")
	}
	// 10 GB of server traffic on n1 in the last 30 days.
	s.st.DB.Create(&store.NodeTrafficDaily{NodeID: "n1", Day: time.Now().Format("2006-01-02"), Rx: 6 << 30, Tx: 4 << 30})

	code, out := c.do("GET", "/costs", nil)
	if code != 200 {
		t.Fatalf("list %d", code)
	}
	total := out["total"].(map[string]any)
	// 60 CNY/month + 120 USD/year × 7.2 / 12 = 60 + 72 = 132; XYZ has no rate.
	if total["monthly"].(float64) != 132 || total["yearly"].(float64) != 1584 {
		t.Fatalf("totals %v", total)
	}
	if un := total["unconverted"].([]any); len(un) != 1 || un[0] != "XYZ" {
		t.Fatalf("unconverted %v", un)
	}
	if total["dueSoon"].(float64) != 1 {
		t.Fatalf("due soon %v", total["dueSoon"])
	}
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["nodeId"] == "n1" && m["perGb"].(float64) != 6 {
			t.Fatalf("per GB %v", m["perGb"])
		}
	}

	// Daily job: reminder for n1 once; n2 auto-renews past today.
	s.costRenewals()
	e := <-events
	if e.Kind != "server_renewal" || e.NodeID != "n1" {
		t.Fatalf("reminder %+v", e)
	}
	s.costRenewals()
	select {
	case e := <-events:
		t.Fatalf("reminder repeated: %q", e.Text)
	default:
	}
	var n2 store.NodeCost
	s.st.DB.First(&n2, "node_id = ?", "n2")
	if !n2.ExpireAt.After(time.Now()) {
		t.Fatalf("auto renew didn't roll forward: %v", n2.ExpireAt)
	}
	// Manual renewal moves n1 a month on.
	if code, _ := c.do("POST", "/nodes/n1/renew", nil); code != 200 {
		t.Fatal("renew")
	}
	var n1 store.NodeCost
	s.st.DB.First(&n1, "node_id = ?", "n1")
	if got := n1.ExpireAt.Format("2006-01-02"); got != time.Now().AddDate(0, 0, 3).AddDate(0, 1, 0).Format("2006-01-02") {
		t.Fatalf("renewed to %s", got)
	}
	// Base currency switch rebases nothing stored, but rates must be positive.
	if code, _ := c.do("PUT", "/costs/settings", map[string]any{"base": "CNY", "rates": map[string]any{"USD": -1}, "remindDays": 7}); code != 400 {
		t.Fatal("negative rate accepted")
	}
	if code, _ := c.do("PUT", "/costs/settings", map[string]any{"base": "CNY", "rates": map[string]any{"USD": 7, "XYZ": 2}, "remindDays": 5}); code != 200 {
		t.Fatal("settings")
	}
	_, out = c.do("GET", "/costs", nil)
	if m := out["total"].(map[string]any)["monthly"].(float64); m != 60+70+20 {
		t.Fatalf("monthly after rates %v", m)
	}
}

func TestCostsUseNodeBillingModeAndCalendarWindow(t *testing.T) {
	s, _, api := newAdminClient(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	old := now.AddDate(0, 0, -30).Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	future := now.AddDate(0, 0, 1).Format("2006-01-02")
	for _, mode := range []string{"sum", "up", "down", "max"} {
		if e := s.st.DB.Create(&store.Node{ID: mode, Name: mode, NetMode: mode}).Error; e != nil {
			t.Fatal(e)
		}
		if e := s.st.DB.Create(&store.NodeCost{NodeID: mode, Amount: 60, Currency: "CNY", Cycle: "month"}).Error; e != nil {
			t.Fatal(e)
		}
		for _, row := range []store.NodeTrafficDaily{{NodeID: mode, Day: today, Rx: 5 * gib, Tx: 70 * gib, AccountingVersion: 1}, {NodeID: mode, Day: yesterday, Rx: 100 * gib, Tx: 10 * gib}, {NodeID: mode, Day: old, Rx: 1000 * gib, Tx: 1000 * gib}, {NodeID: mode, Day: future, Rx: 1000 * gib, Tx: 1000 * gib}} {
			if e := s.st.DB.Create(&row).Error; e != nil {
				t.Fatal(e)
			}
		}
	}
	code, out := api.do("GET", "/costs", nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	expected := map[string]int64{"sum": 185 * gib, "up": 80 * gib, "down": 105 * gib, "max": 105 * gib}
	for _, row := range out["items"].([]any) {
		m := row.(map[string]any)
		id := m["nodeId"].(string)
		if m["traffic30"] != float64(expected[id]) || m["trafficRx30"] != float64(105*gib) || m["trafficTx30"] != float64(80*gib) || m["trafficMode"] != id || m["trafficDays"] != float64(2) || m["trafficLegacyDays"] != float64(1) {
			t.Fatal("wrong billing aggregation", m)
		}
		want := 60 / (float64(expected[id]) / gib)
		if diff := m["perGb"].(float64) - want; diff > 1e-9 || diff < -1e-9 {
			t.Fatal("cost per GB uses wrong bytes", m)
		}
	}
}
