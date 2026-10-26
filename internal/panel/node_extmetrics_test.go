package panel

import (
	"net/http"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// A metrics token writes labelled readings and nothing else; the node's own
// state is untouched.
func TestNodeExternalMetrics(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.9", Status: "offline"})
	_, w, err := s.issueAPIToken("监控", "", []string{ScopeNodesMetrics}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	_, r, _ := s.issueAPIToken("只读", "", []string{ScopeNodesRead}, 0, "root")
	wc := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: w}
	rc := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: r}

	ok := map[string]any{"metrics": []map[string]any{
		{"name": "latency.sh-ct", "label": "上海电信", "unit": "ms", "value": 38.5},
		{"name": "speed", "unit": "Mbps", "value": 920, "at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)},
	}}
	if code, out := wc.do("POST", "/nodes/n1/metrics", ok); code != 200 || out["stored"] != float64(2) {
		t.Fatalf("post %d %v", code, out)
	}
	// Updating replaces the value, keyed by source and name.
	ok["metrics"].([]map[string]any)[0]["value"] = 41.0
	wc.do("POST", "/nodes/n1/metrics", ok)
	code, out := rc.do("GET", "/nodes/n1/metrics/external", nil)
	ms, _ := out["metrics"].([]any)
	if code != 200 || len(ms) != 2 {
		t.Fatalf("list %d %v", code, out)
	}
	first := ms[0].(map[string]any)
	if first["source"] != "token:监控" || first["name"] != "latency.sh-ct" || first["value"] != 41.0 || first["label"] != "上海电信" {
		t.Fatalf("row %v", first)
	}

	// The node's own state is not something a metric can change.
	var n store.Node
	s.st.DB.First(&n, "id = ?", "n1")
	if n.Status != "offline" || n.LastSeen != nil {
		t.Fatalf("node state changed: %s %v", n.Status, n.LastSeen)
	}

	for name, body := range map[string]map[string]any{
		"reserved":   {"metrics": []map[string]any{{"name": "cpu", "unit": "%", "value": 1}}},
		"online":     {"metrics": []map[string]any{{"name": "online", "unit": "bool", "value": 1}}},
		"bad unit":   {"metrics": []map[string]any{{"name": "x", "unit": "GB", "value": 1}}},
		"no value":   {"metrics": []map[string]any{{"name": "x", "unit": "ms"}}},
		"huge":       {"metrics": []map[string]any{{"name": "x", "unit": "ms", "value": 1e13}}},
		"old":        {"metrics": []map[string]any{{"name": "x", "unit": "ms", "value": 1, "at": time.Now().Add(-48 * time.Hour).Format(time.RFC3339)}}},
		"future":     {"metrics": []map[string]any{{"name": "x", "unit": "ms", "value": 1, "at": time.Now().Add(time.Hour).Format(time.RFC3339)}}},
		"bool range": {"metrics": []map[string]any{{"name": "x", "unit": "bool", "value": 2}}},
		"dup":        {"metrics": []map[string]any{{"name": "x", "unit": "ms", "value": 1}, {"name": "x", "unit": "ms", "value": 2}}},
		"empty":      {"metrics": []map[string]any{}},
	} {
		if code, _ := wc.do("POST", "/nodes/n1/metrics", body); code != http.StatusBadRequest {
			t.Fatalf("%s accepted: %d", name, code)
		}
	}
	if code, _ := wc.do("POST", "/nodes/nope/metrics", ok); code != http.StatusNotFound {
		t.Fatal("unknown node accepted")
	}
	// Scopes: a read token cannot write, the metrics token cannot read or touch the node.
	if code, _ := rc.do("POST", "/nodes/n1/metrics", ok); code != http.StatusForbidden {
		t.Fatalf("read token wrote: %d", code)
	}
	for _, path := range []string{"/nodes", "/nodes/n1"} {
		if code, _ := wc.do("GET", path, nil); code != http.StatusForbidden {
			t.Fatalf("metrics token read %s: %d", path, code)
		}
	}
	if code, _ := wc.do("PUT", "/nodes/n1", map[string]any{"name": "x"}); code != http.StatusForbidden {
		t.Fatalf("metrics token edited the node: %d", code)
	}
	// Deleting the node removes its metrics.
	if code, out := admin.do("DELETE", "/nodes/n1", nil); code != 200 && code != 204 {
		t.Fatalf("delete node %d %v", code, out)
	}
	var left int64
	s.st.DB.Model(&store.NodeExternalMetric{}).Count(&left)
	if left != 0 {
		t.Fatalf("%d metrics left after node deletion", left)
	}
}
