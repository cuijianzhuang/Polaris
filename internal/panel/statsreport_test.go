package panel

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestStatsReport(t *testing.T) {
	s, _, c := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "tokyo", TrafficRate: 1})
	db.Create(&store.Node{ID: "n2", Name: "hk", TrafficRate: 2})
	db.Create(&store.User{ID: "u1", Username: "alice", UUID: "a", SubToken: "alice-subscription-token", Status: "active"})
	db.Create(&store.User{ID: "u2", Username: "bob", UUID: "b", SubToken: "bob-subscription-token-x", Status: "active"})
	s.traffic.Add("n1", []model.TrafficDelta{{UserID: "u1", Up: 10, Down: 90}, {UserID: "u2", Up: 1, Down: 9}, {UserID: "inbound>>>vless", Up: 11, Down: 99}})
	s.traffic.Add("n2", []model.TrafficDelta{{UserID: "u2", Up: 50, Down: 50}})
	if err := s.settleTraffic(); err != nil {
		t.Fatal(err)
	}
	// An older day from before the per-day table: backfilled from hourly rows.
	old := time.Now().AddDate(0, 0, -3)
	db.Create(&store.TrafficHourly{UserID: "u1", NodeID: "n1", Hour: old.UTC().Truncate(time.Hour), Up: 5, Down: 5})
	db.Exec("DELETE FROM traffic_dailies") // empty table: the backfill rebuilds it
	s.backfillDaily()
	code, out := c.do("GET", "/stats/report?from="+old.AddDate(0, 0, -1).Format(dayLayout), nil)
	if code != 200 {
		t.Fatalf("report %d %v", code, out)
	}
	total := out["total"].(map[string]any)
	if total["up"].(float64)+total["down"].(float64) != 100+10+200+10 {
		t.Fatalf("total %v", total)
	}
	if n := len(out["trend"].([]any)); n != 5 {
		t.Fatalf("trend days %d", n)
	}
	nodes := out["nodes"].([]any)
	if len(nodes) != 2 || nodes[0].(map[string]any)["name"] != "hk" {
		t.Fatalf("nodes (hk ×2 = 200 first): %v", nodes)
	}
	users := out["users"].([]any)
	if len(users) != 2 || users[0].(map[string]any)["name"] != "bob" {
		t.Fatalf("users: %v", users)
	}
	ins := out["inbounds"].([]any)
	if len(ins) != 1 || ins[0].(map[string]any)["tag"] != "vless" || ins[0].(map[string]any)["down"].(float64) != 99 {
		t.Fatalf("inbounds: %v", ins)
	}
	// Filtered by node; the previous period holds the backfilled day.
	_, out = c.do("GET", "/stats/report?node=n1&from="+time.Now().AddDate(0, 0, -1).Format(dayLayout), nil)
	if tot := out["total"].(map[string]any); tot["up"].(float64)+tot["down"].(float64) != 110 || out["activeUsers"].(float64) != 2 {
		t.Fatalf("node filter: %v", out)
	}
	if len(out["nodes"].([]any)) != 1 || out["filter"].(map[string]any)["nodeName"] != "tokyo" {
		t.Fatalf("node filter nodes: %v", out["nodes"])
	}
	_, out = c.do("GET", "/stats/report?user=u1&from="+time.Now().AddDate(0, 0, -2).Format(dayLayout), nil)
	prev := out["prev"].(map[string]any)["total"].(map[string]any)
	if prev["up"].(float64)+prev["down"].(float64) != 10 {
		t.Fatalf("previous period: %v", out["prev"])
	}
	if code, _ := c.do("GET", "/stats/report?from=2026-13-01", nil); code != 400 {
		t.Fatalf("bad date accepted: %d", code)
	}

	req, _ := http.NewRequest("GET", c.base+"/stats/export?from="+old.Format(dayLayout), nil)
	req.Header.Set("Authorization", "Bearer "+c.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csv := string(b)
	if !strings.HasPrefix(csv, "\xEF\xBB\xBF用户,节点") || !strings.Contains(csv, "bob,hk,100,100,200") || !strings.Contains(csv, "alice,（合计）,,,110") {
		t.Fatalf("csv:\n%s", csv)
	}
}

func TestTrafficSpike(t *testing.T) {
	s := newTestServer(t)
	ch := events(s)
	db := s.st.DB
	db.Create(&store.User{ID: "u1", Username: "alice", UUID: "a", SubToken: "alice-subscription-token", Status: "active"})
	db.Create(&store.User{ID: "u2", Username: "bob", UUID: "b", SubToken: "bob-subscription-token-x", Status: "active"})
	now := time.Now()
	gb := int64(1 << 30)
	for d := 2; d <= 8; d++ { // a week of 1 GB days for both
		h := now.Add(-time.Duration(d)*24*time.Hour + 2*time.Hour).UTC().Truncate(time.Hour)
		db.Create(&store.TrafficHourly{UserID: "u1", NodeID: "n1", Hour: h, Down: gb})
		db.Create(&store.TrafficHourly{UserID: "u2", NodeID: "n1", Hour: h, Down: gb})
	}
	h := now.Add(-time.Hour).UTC().Truncate(time.Hour)
	db.Create(&store.TrafficHourly{UserID: "u1", NodeID: "n1", Hour: h, Down: 8 * gb}) // 8× the average
	db.Create(&store.TrafficHourly{UserID: "u2", NodeID: "n1", Hour: h, Down: 2 * gb}) // normal
	s.checkTrafficSpikes()
	if txt := next(t, ch); !strings.Contains(txt, "alice") || !strings.Contains(txt, "8.0 倍") {
		t.Fatalf("spike: %q", txt)
	}
	s.checkTrafficSpikes()
	select {
	case e := <-ch:
		t.Fatalf("unexpected second alert: %q", e.Text)
	default:
	}
}

func TestNodeMetrics(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo"})
	now := time.Now().UTC().Truncate(metricSlot).Add(time.Minute)
	s.recordMetric("n1", store.SystemInfo{CPU: 10, Mem: 50, RxBps: 100}, now)
	s.recordMetric("n1", store.SystemInfo{CPU: 30, Mem: 50, RxBps: 300}, now.Add(time.Second))
	code, out := c.do("GET", "/nodes/n1/metrics?hours=1", nil)
	pts := out["points"].([]any)
	if code != 200 || len(pts) != 1 {
		t.Fatalf("metrics %d %v", code, out)
	}
	p := pts[0].(map[string]any)
	if p["cpu"].(float64) != 20 || p["rx"].(float64) != 200 {
		t.Fatalf("slot average: %v", p)
	}
}
