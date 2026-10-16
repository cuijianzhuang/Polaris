package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestInboundSpeedLimit: an inbound's limit is validated, stored and sent to
// its node as a port speed limit.
func TestInboundSpeedLimit(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.9"})
	s.st.DB.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27", InstalledVersions: store.NewJSON([]string{})})
	in := map[string]any{"nodeId": "n1", "core": "xray", "tag": "vless", "port": 2443, "speedLimitMbps": -1,
		"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "tcp"}, "security": map[string]any{"type": "none"}}
	if code, _ := c.do("POST", "/inbounds", in); code != 400 {
		t.Fatalf("negative limit accepted: %d", code)
	}
	in["speedLimitMbps"] = 50
	code, out := c.do("POST", "/inbounds", in)
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.SpeedLimits) != 1 || spec.SpeedLimits[0] != (model.SpeedLimit{Tag: "vless", Port: 2443, Network: "tcp", Mbps: 50}) {
		t.Fatalf("spec limits: %+v", spec.SpeedLimits)
	}
	id := out["inbound"].(map[string]any)["id"].(string)
	in["speedLimitMbps"] = 0
	if code, out := c.do("PUT", "/inbounds/"+id, in); code != 200 {
		t.Fatalf("update %d %v", code, out)
	}
	if spec, _ := s.BuildSpec("n1"); len(spec.SpeedLimits) != 0 {
		t.Fatalf("limit not removed: %+v", spec.SpeedLimits)
	}
}

func TestRateLabel(t *testing.T) {
	for rate, want := range map[float64]string{0: "", 1: "", 2: "×2", 0.5: "×0.5", 1.5: "×1.5"} {
		if got := rateLabel(rate); got != want {
			t.Fatalf("rateLabel(%v) = %q, want %q", rate, got, want)
		}
	}
}
