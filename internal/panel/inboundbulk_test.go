package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestBulkInbounds(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.9"})
	s.st.DB.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27", InstalledVersions: store.NewJSON([]string{})})
	var ids []string
	for i, tag := range []string{"a", "b", "c"} {
		code, out := c.do("POST", "/inbounds", map[string]any{"nodeId": "n1", "core": "xray", "tag": tag, "port": 20001 + i,
			"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "tcp"}, "security": map[string]any{"type": "none"}})
		if code != 201 {
			t.Fatalf("create %d %v", code, out)
		}
		ids = append(ids, out["inbound"].(map[string]any)["id"].(string))
	}
	bulk := func(body map[string]any) []map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", c.base+"/inbounds/bulk", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+c.tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out []map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != 200 {
			t.Fatalf("bulk %d", resp.StatusCode)
		}
		return out
	}
	port := func(id string) int {
		var in store.Inbound
		s.st.DB.First(&in, "id = ?", id)
		return in.Port
	}
	bulk(map[string]any{"ids": ids, "action": "port", "mode": "sequential", "start": 30000})
	if port(ids[0]) != 30000 || port(ids[2]) != 30002 {
		t.Fatalf("sequential: %d %d", port(ids[0]), port(ids[2]))
	}
	bulk(map[string]any{"ids": ids[:2], "action": "port", "mode": "offset", "offset": 100})
	if port(ids[0]) != 30100 || port(ids[2]) != 30002 {
		t.Fatal("offset")
	}
	// A clash is reported per inbound and leaves that one alone.
	res := bulk(map[string]any{"ids": []string{ids[2]}, "action": "port", "mode": "sequential", "start": 30100})
	if res[0]["error"] == nil || port(ids[2]) != 30002 {
		t.Fatalf("conflict not reported: %v", res)
	}
	bulk(map[string]any{"ids": ids, "action": "port", "mode": "random", "min": 40000, "max": 41000})
	for _, id := range ids {
		if p := port(id); p < 40000 || p >= 41000 {
			t.Fatalf("random port %d", p)
		}
	}
	bulk(map[string]any{"ids": ids, "action": "speed", "mbps": 30})
	bulk(map[string]any{"ids": ids[:1], "action": "disable"})
	spec, _ := s.BuildSpec("n1")
	if len(spec.SpeedLimits) != 2 { // the disabled one is not shaped
		t.Fatalf("speed limits: %+v", spec.SpeedLimits)
	}
}
