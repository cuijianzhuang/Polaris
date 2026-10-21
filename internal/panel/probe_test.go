package panel

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// fakeProber answers the agent "probe" commands sent to nodeID: targets
// whose address contains a key of ms get that latency, others fail.
func fakeProber(t *testing.T, s *Server, nodeID string, ms map[string]string) func() {
	conn := s.hub.register(nodeID)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case m := <-conn.out:
				cmd := m.GetCommand()
				if cmd == nil || cmd.Kind != "probe" {
					continue
				}
				res := &polarisv1.CommandResult{Id: cmd.Id, Ok: false, Error: "connection refused"}
				for k, v := range ms {
					if strings.Contains(cmd.Args["tcp"], k) {
						res = &polarisv1.CommandResult{Id: cmd.Id, Ok: true, Output: v}
					}
				}
				s.hub.resolve(nodeID, res)
			}
		}
	}()
	return func() { close(stop); s.hub.unregister(conn) }
}

func TestLatencyProbes(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.9", Status: "online", Flag: "JP"})
	s.st.DB.Create(&store.Node{ID: "n2", Name: "hidden", Address: "203.0.113.10", Status: "online", StatusHidden: true})
	s.st.DB.Create(&store.Node{ID: "n3", Name: "down", Status: "offline"})
	defer fakeProber(t, s, "n1", map[string]string{"-ct-": "30", "-cm-": "50"})()

	// Defaults are seeded once.
	st, out := admin.do("GET", "/probe", nil)
	if st != 200 || len(out["targets"].([]any)) != 3 {
		t.Fatalf("probe page: %d %v", st, out)
	}
	s.runLatencyProbes()
	s.runLatencyProbes()
	var rows []store.NodeLatency
	s.st.DB.Where("node_id = ?", "n1").Find(&rows)
	if len(rows) != 3 {
		t.Fatalf("latency rows: %+v", rows)
	}
	var lost, ok int
	for _, r := range rows {
		if r.Sent != 2 {
			t.Fatalf("samples not folded into one slot: %+v", r)
		}
		if r.Lost == 2 {
			lost++
		} else if r.Lost == 0 && r.SumMs > 0 {
			ok++
		}
	}
	if lost != 1 || ok != 2 {
		t.Fatalf("lost %d ok %d: %+v", lost, ok, rows)
	}
	var n int64
	s.st.DB.Model(&store.NodeLatency{}).Where("node_id IN ?", []string{"n2", "n3"}).Count(&n)
	if n != 0 {
		t.Fatal("probed a node without a connection")
	}

	// Current figures.
	sum := s.latencySummary([]string{"n1"}, time.Now())["n1"]
	var ct, cu *latencyNow
	for _, tg := range s.latencyTargets(true) {
		switch tg.Carrier {
		case "ct":
			ct = sum[tg.ID]
		case "cu":
			cu = sum[tg.ID]
		}
	}
	if ct == nil || ct.Ms != 30 || ct.Avg == nil || *ct.Avg != 30 || ct.Loss != 0 {
		t.Fatalf("telecom: %+v", ct)
	}
	if cu == nil || cu.Ms != 0 || cu.Avg != nil || cu.Loss != 100 {
		t.Fatalf("unicom: %+v", cu)
	}

	// History endpoint.
	if st, out := admin.do("GET", "/nodes/n1/latency?hours=6", nil); st != 200 || len(out["points"].([]any)) != 3 {
		t.Fatalf("history: %d %v", st, out)
	}

	// Targets: validation, edit, delete.
	if st, _ := admin.do("POST", "/probe/targets", map[string]any{"name": "x", "addr": "no-port"}); st != 400 {
		t.Fatal("address without a port accepted")
	}
	if st, _ := admin.do("POST", "/probe/targets", map[string]any{"name": "x", "carrier": "zz", "addr": "a.example:80"}); st != 400 {
		t.Fatal("unknown carrier accepted")
	}
	st, out = admin.do("POST", "/probe/targets", map[string]any{"name": "广东电信", "carrier": "ct", "addr": "gd-ct-v4.ip.zstaticcdn.com:80"})
	if st != 200 {
		t.Fatalf("create target: %d %v", st, out)
	}
	id := out["id"].(string)
	if st, out := admin.do("POST", "/probe/targets/test", map[string]any{"nodeId": "n1", "addr": "gd-ct-v4.ip.zstaticcdn.com:80"}); st != 200 || out["ok"] != true || out["ms"].(float64) != 30 {
		t.Fatalf("test target: %d %v", st, out)
	}
	if st, _ := admin.do("DELETE", "/probe/targets/"+id, nil); st != 204 {
		t.Fatal("delete target")
	}

	// Public page: off by default (the decoy answers), then on.
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if _, body := get("/status/data"); strings.Contains(body, "tokyo") {
		t.Fatal("status data served while the page is off")
	}
	s.st.SetSetting("status_page", "true")
	s.st.SetSetting("status_title", "Acme 状态")
	if st, body := get("/status"); st != 200 || !strings.Contains(body, "Acme 状态") {
		t.Fatalf("status page: %d", st)
	}
	st2, body := get("/status/data")
	if st2 != 200 || !strings.Contains(body, "tokyo") {
		t.Fatalf("status data: %d %s", st2, body)
	}
	for _, leak := range []string{"203.0.113", "hidden", "agentVersion", "netLimit\":1"} {
		if strings.Contains(body, leak) {
			t.Fatalf("public data has %q: %s", leak, body)
		}
	}

	// Hiding a node takes effect right away.
	if st, _ := admin.do("PUT", "/probe/nodes/n1", map[string]any{"hidden": true}); st != 204 {
		t.Fatal("hide node")
	}
	if _, body := get("/status/data"); strings.Contains(body, "tokyo") {
		t.Fatal("hidden node still public")
	}
}

func TestRecordLatencyMinAfterLoss(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC().Truncate(latencySlot).Add(time.Minute)
	s.recordLatency("n", "t", 0, false, now)
	s.recordLatency("n", "t", 80, true, now.Add(time.Second))
	s.recordLatency("n", "t", 40, true, now.Add(2*time.Second))
	var m store.NodeLatency
	s.st.DB.First(&m)
	p := pointOf(&m)
	if m.Sent != 3 || m.Lost != 1 || m.MinMs != 40 || m.MaxMs != 80 || m.LastMs != 40 || p.Avg == nil || *p.Avg != 60 {
		t.Fatalf("%+v avg %v", m, p.Avg)
	}
}
