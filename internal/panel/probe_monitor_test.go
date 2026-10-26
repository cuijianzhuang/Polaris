package panel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestProbeMonitorSamplingDistinguishesFailureAndMissing(t *testing.T) {
	s := newTestServer(t)
	s.seedProbeTargets()
	now := time.Now().UTC()
	s.st.DB.Create(&store.Node{ID: "monitor", Name: "monitor", Status: "online", LastSeen: &now})
	target := s.latencyTargets(true)[0]
	previous := now.Add(-latencySlot)
	for i := 0; i < 2; i++ {
		if err := recordLatencyDB(s.st.DB, "monitor", target.ID, 0, false, previous); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordLatencyDB(s.st.DB, "monitor", target.ID, 12.5, true, now); err != nil {
		t.Fatal(err)
	}
	l := s.latencySummary([]string{"monitor"}, now)["monitor"][target.ID]
	if l == nil || len(l.Spark) != 25 || len(l.SparkLoss) != 25 || l.SparkStart.IsZero() {
		t.Fatalf("incomplete timeline: %+v", l)
	}
	last := len(l.Spark) - 1
	if l.Spark[last] == nil || *l.Spark[last] != 12.5 || l.SparkLoss[last] == nil || *l.SparkLoss[last] != 0 {
		t.Fatal("successful sample was lost")
	}
	if l.Spark[last-1] != nil || l.SparkLoss[last-1] == nil || *l.SparkLoss[last-1] != 100 {
		t.Fatal("all-failed interval is indistinguishable from missing data")
	}
	if l.Spark[last-2] != nil || l.SparkLoss[last-2] != nil {
		t.Fatal("missing sample invented a zero or failed measurement")
	}
	point := pointOf(&store.NodeLatency{Sent: 10, Lost: 9, SumMs: 100, MinMs: 100, MaxMs: 100})
	if point.Sent != 10 || point.Lost != 9 || point.Avg == nil || *point.Avg != 100 {
		t.Fatal("history lacks sample weights")
	}
}

func TestProbeMonitorPublicPrivacyAndFreshness(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	now, old := time.Now(), time.Now().Add(-10*time.Minute)
	for _, n := range []store.Node{
		{ID: "fresh", Name: "fresh", Status: "online", LastSeen: &now, Address: "198.51.100.9", AgentVersion: "private-agent-version", System: store.NewJSON(store.SystemInfo{DiskTotal: 1000}), NetRx: 12345, NetTx: 67890},
		{ID: "stale", Name: "stale", Status: "online", LastSeen: &old},
		{ID: "hidden", Name: "hidden-secret", StatusHidden: true, Status: "online", LastSeen: &now},
		{ID: "revoked", Name: "revoked-secret", Revoked: true, Status: "online", LastSeen: &now},
	} {
		if err := s.st.DB.Create(&n).Error; err != nil {
			t.Fatal(err)
		}
	}
	code, out := admin.do("GET", "/probe", nil)
	if code != 200 || len(out["nodes"].([]any)) != 3 {
		t.Fatalf("admin nodes: %d %v", code, out)
	}
	for _, raw := range out["nodes"].([]any) {
		n := raw.(map[string]any)
		if n["id"] == "fresh" && (n["telemetryFresh"] != true || n["cpu"] != float64(0) || n["disk"] != float64(0)) {
			t.Fatal("zero telemetry classified as unknown", n)
		}
		if n["id"] == "stale" && n["telemetryFresh"] != false {
			t.Fatal("stale telemetry classified as fresh")
		}
	}
	if code, _ := admin.do("PUT", "/settings", map[string]string{"status_page": "true"}); code != 200 {
		t.Fatal("enable status")
	}
	for i := 0; i < 2; i++ {
		resp, err := http.Get(srv.URL + "/status/data")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("cached status response permits browser storage")
		}
		for _, private := range []string{"198.51.100.9", "private-agent-version", "hidden-secret", "revoked-secret", "ip.zstaticcdn.com", "\"netRx\"", "\"netTx\""} {
			if strings.Contains(string(body), private) {
				t.Fatalf("public feed leaked %s", private)
			}
		}
		var data struct {
			Nodes       []probeNode
			ShowTraffic bool
		}
		if err := json.Unmarshal(body, &data); err != nil || len(data.Nodes) != 2 || data.ShowTraffic {
			t.Fatal("invalid public feed", err, string(body))
		}
		if len(data.Nodes[0].ProbeTargets) != 3 {
			t.Fatal("missing node target metadata")
		}
	}
}

func TestProbeMonitorTemplate(t *testing.T) {
	var body strings.Builder
	if err := statusTpl.Execute(&body, map[string]any{"Title": "<script>unexpected()</script>", "Data": "/status/data"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.String(), "<script>unexpected()") || !strings.Contains(body.String(), "v-ring") || !strings.Contains(body.String(), "visibilitychange") {
		t.Fatal("unsafe or incomplete public template")
	}
	// Optional local artifact for manual browser checks, never used by the app.
	if path := os.Getenv("POLARIS_PROBE_PREVIEW_HTML"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := statusTpl.Execute(file, map[string]any{"Title": "Polaris · 服务器状态", "Data": "/status/data"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProbeMonitorReadFailureDoesNotInventEmptyFleet(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("status_page", "true")
	name := "test:unavailable-probe-nodes"
	s.st.DB.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "nodes" {
			tx.AddError(errors.New("temporary database failure"))
		}
	})
	defer s.st.DB.Callback().Query().Remove(name)
	for _, client := range []*apiClient{admin, {t: t, base: srv.URL}} {
		path := "/probe"
		if client != admin {
			path = "/status/data"
		}
		code, out := client.do("GET", path, nil)
		if code != http.StatusServiceUnavailable || out["nodes"] != nil {
			t.Fatalf("read failure returned a valid empty fleet: %d %v", code, out)
		}
	}
	if s.statusCache.Load() != nil {
		t.Fatal("failed query poisoned public cache")
	}
}
