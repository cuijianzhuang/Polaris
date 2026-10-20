package panel

import (
	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestCycleStart(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		day       int
		now, want string
	}{
		{1, "2026-10-17", "2026-10-01"},
		{20, "2026-10-17", "2026-09-20"},
		{31, "2026-11-15", "2026-10-31"},
		{31, "2026-12-01", "2026-11-30"}, // November has 30 days
		{17, "2026-10-17", "2026-10-17"},
	}
	for _, c := range cases {
		now, _ := time.ParseInLocation("2006-01-02", c.now, loc)
		now = now.Add(10 * time.Hour)
		if got := cycleStart(c.day, now).Format("2006-01-02"); got != c.want {
			t.Errorf("cycleStart(%d, %s) = %s, want %s", c.day, c.now, got, c.want)
		}
	}
	now, _ := time.ParseInLocation("2006-01-02", "2027-01-31", loc)
	if got := nextNetReset(31, now).Format("2006-01-02"); got != "2027-02-28" {
		t.Errorf("next reset after Jan 31 = %s", got)
	}
}

func TestNodeTraffic(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "hk", Address: "203.0.113.1"})
	events := make(chan Event, 8)
	s.sinks = append(s.sinks, events)
	load := func() store.Node {
		var n store.Node
		s.st.DB.First(&n, "id = ?", "n1")
		return n
	}
	beat := func(rx, tx uint64) {
		if e := s.accountNodeNet("n1", &polarisv1.SystemStats{RxBytes: rx, TxBytes: tx}, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	// 10 GB limit, both directions count, warn at 80%.
	if code, out := c.do("PUT", "/nodes/n1/net", map[string]any{"limitGb": 10, "mode": "sum", "warnPct": 80, "resetDay": 1}); code != 200 {
		t.Fatalf("put net %d %v", code, out)
	}
	beat(5*gib, 1*gib) // baseline only
	if n := load(); n.NetRx != 0 || n.NetTx != 0 {
		t.Fatalf("first report must only set the baseline: %+v", n)
	}
	beat(8*gib, 3*gib) // +3 rx +2 tx = 5 GB
	if n := load(); n.NetRx != 3*gib || n.NetTx != 2*gib || netUsed(&n) != 5*gib {
		t.Fatalf("after delta: rx %d tx %d", n.NetRx, n.NetTx)
	}
	select {
	case e := <-events:
		t.Fatalf("no alert expected at 50%%, got %q", e.Text)
	default:
	}
	beat(10*gib, 4*gib) // +2 +1 = 8 GB → 80%
	if e := <-events; e.Kind != "node_traffic" || load().NetWarned != 1 {
		t.Fatalf("threshold alert: %+v warned=%d", e, load().NetWarned)
	}
	beat(10*gib+gib/2, 4*gib) // 8.5 GB: no repeat
	select {
	case e := <-events:
		t.Fatalf("threshold alert repeated: %q", e.Text)
	default:
	}
	// A decrease only establishes a baseline: it might be a replaced NIC.
	beat(1*gib, gib/2)
	if netUsedPtr := load(); netUsed(&netUsedPtr) != gib*8+gib/2 {
		t.Fatal("counter reset charged lifetime bytes")
	}
	beat(2*gib, 1*gib) // +1 +0.5 = 10 GB → over the limit
	if e := <-events; load().NetWarned != 2 || e.Text == "" {
		t.Fatalf("over-limit alert missing, warned=%d", load().NetWarned)
	}
	// Daily table has the same totals.
	var d store.NodeTrafficDaily
	s.st.DB.First(&d, "node_id = ?", "n1")
	if d.Rx+d.Tx != 10*gib {
		t.Fatalf("daily total %d", d.Rx+d.Tx)
	}
	// Calibration to the provider's figure, then a manual reset.
	if code, _ := c.do("PUT", "/nodes/n1/net", map[string]any{"usedGb": 2}); code != 200 {
		t.Fatal("calibrate")
	}
	if n := load(); netUsed(&n) != 2*gib || n.NetWarned != 0 {
		t.Fatalf("calibrated used %d warned %d", netUsed(&n), n.NetWarned)
	}
	if code, _ := c.do("POST", "/nodes/n1/net-reset", nil); code != 204 {
		t.Fatal("reset")
	}
	if n := load(); netUsed(&n) != 0 {
		t.Fatalf("after reset %d", netUsed(&n))
	}
	// Only-upload billing.
	c.do("PUT", "/nodes/n1/net", map[string]any{"mode": "up"})
	beat(3*gib, 5*gib/2) // +1 rx, +1.5 tx
	if n := load(); netUsed(&n) != n.NetTx {
		t.Fatalf("up mode counts tx only: used %d tx %d", netUsed(&n), n.NetTx)
	}
	if code, _ := c.do("PUT", "/nodes/n1/net", map[string]any{"mode": "bogus"}); code != 400 {
		t.Fatal("bad mode accepted")
	}
}
