package panel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestRecordMetricFoldsNewFields(t *testing.T) {
	s := newTestServer(t)
	// Anchor inside one slot (same pattern as TestNodeMetrics): a bare
	// time.Now() can sit in the slot's last minute, where now+1m falls into
	// the next slot and the two samples land in different rows.
	now := time.Now().UTC().Truncate(metricSlot).Add(time.Minute)
	s.recordMetric("n1", store.SystemInfo{CPU: 10, Load1: 1, Load5: 2, Load15: 4, Swap: 20}, now)
	s.recordMetric("n1", store.SystemInfo{CPU: 30, Load1: 3, Load5: 4, Load15: 8, Swap: 40}, now.Add(time.Minute))
	var m store.NodeMetric
	if err := s.st.DB.First(&m, "node_id = ?", "n1").Error; err != nil {
		t.Fatal(err)
	}
	if m.N != 2 || m.Load5 != 3 || m.Load15 != 6 || m.Swap != 30 {
		t.Fatalf("new fields not averaged into the slot: %+v", m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"load5":3`, `"load15":6`, `"swap":30`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("metric JSON missing %s: %s", want, b)
		}
	}
}

func overviewNode(t *testing.T, s *Server, public bool, id string) probeNode {
	t.Helper()
	out := s.probeOverview(public)
	if out == nil {
		t.Fatal("overview failed")
	}
	for _, n := range out["nodes"].([]probeNode) {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %s missing from overview (public=%v)", id, public)
	return probeNode{}
}

func TestProbeOverviewNewMetricsAndPrivacy(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	for _, n := range []store.Node{
		{ID: "new", Name: "new", Status: "online", LastSeen: &now, System: store.NewJSON(store.SystemInfo{
			Load1: 1, Load5: 2, Load15: 3, Swap: 25, SwapTotal: 1 << 30, Processes: 123, TCPConns: 45, UDPConns: 6})},
		{ID: "old", Name: "old", Status: "online", LastSeen: &now, System: store.NewJSON(store.SystemInfo{Load1: 1})},
	} {
		if err := s.st.DB.Create(&n).Error; err != nil {
			t.Fatal(err)
		}
	}
	at := now.UTC().Truncate(metricSlot)
	for _, m := range []store.NodeMetric{
		{NodeID: "new", At: at, CPU: 10, Mem: 20, RxBps: 100, TxBps: 200, N: 1},
		{NodeID: "new", At: at.Add(-time.Hour), CPU: 30, Mem: 40, RxBps: 300, TxBps: 400, N: 1},
	} {
		if err := s.st.DB.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
	}

	admin := overviewNode(t, s, false, "new")
	if admin.Load5 == nil || *admin.Load5 != 2 || admin.Load15 == nil || *admin.Load15 != 3 {
		t.Fatalf("admin overview lost load5/load15: %+v", admin)
	}
	if admin.Swap == nil || *admin.Swap != 25 || admin.SwapTotal != 1<<30 {
		t.Fatalf("admin overview lost swap: %+v", admin)
	}
	if admin.Processes != 123 || admin.TCPConns != 45 || admin.UDPConns != 6 {
		t.Fatalf("admin overview lost processes/connections: %+v", admin)
	}
	if admin.ResSpark == nil || admin.ResSpark.SlotSeconds != 300 ||
		len(admin.ResSpark.CPU) != 25 || len(admin.ResSpark.Mem) != 25 || len(admin.ResSpark.Rx) != 25 || len(admin.ResSpark.Tx) != 25 {
		t.Fatalf("admin overview lost resource spark: %+v", admin.ResSpark)
	}
	points := 0
	for i, v := range admin.ResSpark.CPU {
		if v == nil {
			continue
		}
		points++
		if *v != 10 && *v != 30 {
			t.Fatalf("spark point %d has unexpected cpu %v", i, *v)
		}
		if admin.ResSpark.Rx[i] == nil || admin.ResSpark.Tx[i] == nil || admin.ResSpark.Mem[i] == nil {
			t.Fatalf("spark point %d missing mem/rx/tx", i)
		}
	}
	if points != 2 {
		t.Fatalf("spark has %d points, want the 2 inserted slots", points)
	}

	old := overviewNode(t, s, false, "old")
	if old.Load5 != nil || old.Load15 != nil || old.Swap != nil || old.SwapTotal != 0 {
		t.Fatalf("old agent's zero metrics were not omitted: %+v", old)
	}
	if old.ResSpark != nil {
		t.Fatalf("node without metrics got a spark: %+v", old.ResSpark)
	}

	pub := overviewNode(t, s, true, "new")
	if pub.Processes != 0 || pub.TCPConns != 0 || pub.UDPConns != 0 {
		t.Fatalf("public overview leaked processes/connections: %+v", pub)
	}
	b, err := json.Marshal(pub)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{`"processes"`, `"tcpConns"`, `"udpConns"`} {
		if strings.Contains(string(b), private) {
			t.Fatalf("public JSON leaked %s: %s", private, b)
		}
	}
	if pub.Load5 == nil || pub.Swap == nil || pub.ResSpark == nil {
		t.Fatalf("public overview lost the non-private new metrics: %+v", pub)
	}
}
