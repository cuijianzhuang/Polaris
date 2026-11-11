package panel

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"gorm.io/gorm/logger"
)

// queryCounter wraps the DB logger and counts every SQL statement issued
// while it is installed.
type queryCounter struct {
	logger.Interface
	n *atomic.Int64
}

func (l queryCounter) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	l.n.Add(1)
	if os.Getenv("POLARIS_LOG_SQL") != "" {
		sql, rows := fc()
		fmt.Printf("SQL %d rows=%d: %s\n", l.n.Load(), rows, sql)
	}
	l.Interface.Trace(ctx, begin, fc, err)
}

// seedSubBench builds 2 nodes x 3 inbounds, 3 of them with a CDN endpoint
// sharing one CarrierSplit profile, plus CDN reports for the same
// profile+port+carrier. It returns the user the subscription is built for.
func seedSubBench(t *testing.T, s *Server) *store.User {
	t.Helper()
	db := s.st.DB
	for _, n := range []store.Node{
		{ID: "n1", Name: "tokyo", Domain: "n1.example.com"},
		{ID: "n2", Name: "osaka", Domain: "n2.example.com"},
	} {
		if err := db.Create(&n).Error; err != nil {
			t.Fatal(err)
		}
	}
	u := &store.User{ID: "u1", Username: "bench", UUID: "11111111-1111-1111-1111-111111111111",
		Password: "p", Status: "active", SubToken: "bench-token-1234", TrafficLimit: 1000}
	if err := db.Create(u).Error; err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{"n1", "n2"} {
		if err := db.Create(&store.UserAccess{UserID: u.ID, Kind: "node", TargetID: nodeID}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&store.Core{ID: "core-" + nodeID, NodeID: nodeID, Type: model.CoreXray, Version: "1.8.0"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	p := store.CDNProfile{ID: "prof1", Name: "prof1", Scheme: "https", Provider: "other",
		OriginDomain: "origin.example.com", Ports: store.NewJSON([]int{443, 8443}),
		PreferredIPs: store.NewJSON([]string{"203.0.113.1"}), ExpandPerIP: true, IPTopN: 2,
		CarrierSplit: true}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, src := range []string{"probe-a", "probe-b"} {
		if err := db.Create(&store.CDNIPReport{ProfileID: p.ID, Source: src, Carrier: "ct",
			Port: 8443, Scheme: "https", At: now,
			IPs: store.NewJSON([]store.CDNIPResult{{IP: "203.0.113.1"}, {IP: "203.0.113.2"}})}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 6; i++ {
		nodeID := "n1"
		if i >= 3 {
			nodeID = "n2"
		}
		in := store.Inbound{ID: fmt.Sprintf("in%d", i), NodeID: nodeID, Tag: fmt.Sprintf("ws%d", i),
			Port: 8443, Enabled: true, Core: model.CoreXray,
			Protocol:  store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}),
			Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS, Path: "/ws"}),
			Security:  store.NewJSON(model.SecuritySpec{Type: model.SecNone})}
		if err := db.Create(&in).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&store.Endpoint{ID: fmt.Sprintf("ep-direct-%d", i), InboundID: in.ID, Kind: "direct"}).Error; err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := db.Create(&store.Endpoint{ID: fmt.Sprintf("ep-cdn-%d", i), InboundID: in.ID,
				Kind: "cdn", CDNProfileID: p.ID}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	return u
}

func nodeSummary(nodes []sub.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, fmt.Sprintf("%s|%s|%d|%s", n.Name, n.Server, n.Port, n.Protocol))
	}
	sort.Strings(out)
	return out
}

// TestSubNodesForQueryCount measures how many SQL statements one
// subscription render issues, and pins its output so a prefetch refactor
// cannot silently change what clients receive.
func TestSubNodesForQueryCount(t *testing.T) {
	s := newTestServer(t)
	u := seedSubBench(t, s)
	var n atomic.Int64
	old := s.st.DB.Logger
	s.st.DB.Logger = queryCounter{Interface: old, n: &n}
	t.Cleanup(func() { s.st.DB.Logger = old })

	// Warm the settings cache, then measure the steady state.
	if _, err := s.subNodesForTrace(u, "ct", "", nil); err != nil {
		t.Fatal(err)
	}
	n.Store(0)
	nodes, err := s.subNodesForTrace(u, "ct", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	count := n.Load()
	t.Logf("subNodesForTrace steady-state queries: %d, nodes: %d", count, len(nodes))
	if len(nodes) == 0 {
		t.Fatal("no nodes rendered")
	}
	want := nodeSummary(nodes)
	// A second render must produce identical output.
	n.Store(0)
	nodes2, err := s.subNodesForTrace(u, "ct", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeSummary(nodes2); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("second render differs:\n%v\n%v", got, want)
	}
	t.Logf("subNodesForTrace second render queries: %d", n.Load())
	if count > 20 {
		t.Fatalf("too many queries per subscription render: %d (want <= 20)", count)
	}
}

// TestStatsOverviewCacheQueryCount measures the SQL statements behind
// two consecutive /stats/overview calls: the second must reuse the
// cached aggregates (live node status and the operational overview are
// still computed fresh) and return identical aggregate values.
func TestStatsOverviewCacheQueryCount(t *testing.T) {
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	soon := time.Now().Add(48 * time.Hour)
	db.Create(&store.User{ID: "u1", Username: "heavy", UUID: "11111111-1111-1111-1111-111111111111", Password: "p", Status: "active", SubToken: "t1", TrafficLimit: 100, UsedDown: 90})
	db.Create(&store.User{ID: "u2", Username: "light", UUID: "22222222-2222-2222-2222-222222222222", Password: "p", Status: "active", SubToken: "t2", ExpireAt: &soon})
	hour := time.Now().UTC().Truncate(time.Hour)
	db.Create(&store.TrafficHourly{UserID: "u1", NodeID: "n1", Hour: hour, Up: 10, Down: 500})
	db.Create(&store.TrafficHourly{UserID: "u2", NodeID: "n1", Hour: hour, Up: 5, Down: 50})
	var n atomic.Int64
	old := s.st.DB.Logger
	s.st.DB.Logger = queryCounter{Interface: old, n: &n}
	t.Cleanup(func() { s.st.DB.Logger = old })
	n.Store(0)
	code, out1 := admin.do("GET", "/stats/overview?days=7", nil)
	if code != 200 {
		t.Fatalf("overview %d %v", code, out1)
	}
	first := n.Load()
	n.Store(0)
	code, out2 := admin.do("GET", "/stats/overview?days=7", nil)
	if code != 200 {
		t.Fatalf("overview %d %v", code, out2)
	}
	second := n.Load()
	t.Logf("statsOverview queries: first %d, second (cached) %d", first, second)
	for _, k := range []string{"users", "today", "month", "trend", "topUsers", "attention", "cores"} {
		if fmt.Sprint(out1[k]) != fmt.Sprint(out2[k]) {
			t.Fatalf("cached overview %s differs:\n%v\n%v", k, out1[k], out2[k])
		}
	}
	if second >= first {
		t.Fatalf("second overview call did not use the cache: first %d, second %d", first, second)
	}
}
func TestStatsTrafficQueryCount(t *testing.T) {
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	hour := time.Now().UTC().Truncate(time.Hour)
	for ui := 0; ui < 3; ui++ {
		uid := fmt.Sprintf("u%d", ui)
		db.Create(&store.User{ID: uid, Username: fmt.Sprintf("user%d", ui),
			UUID: fmt.Sprintf("11111111-1111-1111-1111-11111111111%d", ui), Password: "p",
			Status: "active", SubToken: fmt.Sprintf("tok%d", ui)})
		for h := 0; h < 48; h++ {
			// Distinct totals per user so the top order is deterministic.
			db.Create(&store.TrafficHourly{UserID: uid, NodeID: "n1", Hour: hour.Add(time.Duration(-h) * time.Hour),
				Up: int64((h + 1) * (ui + 1)), Down: int64((h + 1) * 10 * (ui + 1))})
		}
	}
	from := hour.Add(-48 * time.Hour).Format(time.RFC3339)
	to := hour.Add(time.Hour).Format(time.RFC3339)
	url := "/stats/traffic?from=" + from + "&to=" + to + "&bucket=hour&top=10"
	var n atomic.Int64
	old := s.st.DB.Logger
	s.st.DB.Logger = queryCounter{Interface: old, n: &n}
	t.Cleanup(func() { s.st.DB.Logger = old })
	code, out := admin.do("GET", url, nil)
	if code != 200 {
		t.Fatalf("traffic %d %v", code, out)
	}
	n.Store(0)
	code, out2 := admin.do("GET", url, nil)
	if code != 200 {
		t.Fatalf("traffic %d %v", code, out2)
	}
	count := n.Load()
	series := out2["series"].([]any)
	top := out2["top"].([]any)
	t.Logf("statsTraffic steady-state queries: %d, series: %d, top: %d",
		count, len(series), len(top))
	if fmt.Sprint(out["series"]) != fmt.Sprint(out2["series"]) || fmt.Sprint(out["top"]) != fmt.Sprint(out2["top"]) {
		t.Fatal("second statsTraffic call differs")
	}
	// Totals per user are 12936*(ui+1); top order must be u2, u1, u0.
	if len(top) != 3 || top[0].(map[string]any)["userId"] != "u2" ||
		top[0].(map[string]any)["total"].(float64) != 38808 ||
		top[2].(map[string]any)["userId"] != "u0" {
		t.Fatalf("top users wrong: %v", top)
	}
	// One series point per hour: hour h sums to up (h+1)*6, down (h+1)*60.
	if len(series) != 48 {
		t.Fatalf("series points = %d", len(series))
	}
	last := series[47].(map[string]any)
	if last["up"].(float64) != 6 || last["down"].(float64) != 60 {
		t.Fatalf("last series point wrong: %v", last)
	}
	// The day bucket must sum to the same totals as the hour bucket
	// (the bucketing itself is unchanged; only the rows feeding it are
	// now pre-aggregated per hour).
	code, outDay := admin.do("GET", "/stats/traffic?from="+from+"&to="+to+"&bucket=day&top=10", nil)
	if code != 200 {
		t.Fatalf("traffic day %d %v", code, outDay)
	}
	sum := func(v any) (up, down float64) {
		for _, p := range v.([]any) {
			m := p.(map[string]any)
			up += m["up"].(float64)
			down += m["down"].(float64)
		}
		return up, down
	}
	hu, hd := sum(out2["series"])
	du, dd := sum(outDay["series"])
	if hu != du || hd != dd {
		t.Fatalf("day bucket totals (%v,%v) != hour bucket totals (%v,%v)", du, dd, hu, hd)
	}
	if fmt.Sprint(outDay["top"]) != fmt.Sprint(out2["top"]) {
		t.Fatal("day bucket top differs from hour bucket top")
	}
	if count > 5 {
		t.Fatalf("too many queries per statsTraffic call: %d (want <= 5)", count)
	}
}
