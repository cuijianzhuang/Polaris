package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/warp"
)

func TestAddWARP(t *testing.T) {
	var regs int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/reg" {
			regs++
			w.Write([]byte(`{"id":"d1","token":"t1","account":{"account_type":"free"},"config":{"client_id":"AQID",
				"peers":[{"public_key":"bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=","endpoint":{"v4":"162.159.192.1:0"}}],
				"interface":{"addresses":{"v4":"172.16.0.2","v6":"2606:4700:110:8a36::1"}}}}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer api.Close()
	old := warp.API
	warp.API = api.URL
	defer func() { warp.API = old }()

	s, _, admin := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "hk"})
	s.st.DB.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	s.st.DB.Create(&store.Node{ID: "n2", Name: "hy"})
	s.st.DB.Create(&store.Core{ID: "c2", NodeID: "n2", Type: model.CoreHysteria, Version: "2.6.0"})

	// A node that cannot carry WireGuard is refused before registering.
	if st, _ := admin.do("POST", "/nodes/n2/warp", map[string]any{}); st != 400 || regs != 0 {
		t.Fatalf("hysteria-only node: %d, %d registrations", st, regs)
	}

	st, out := admin.do("POST", "/nodes/n1/warp", map[string]any{"rules": map[string]any{"geosite": []string{"openai", "netflix"}}})
	if st != 200 || out["ruleAdded"] != true {
		t.Fatalf("add warp: %d %v", st, out)
	}
	var o store.Outbound
	if err := s.st.DB.First(&o, "node_id = ? AND tag = ?", "n1", "warp").Error; err != nil || o.Type != model.OutWireGuard {
		t.Fatalf("outbound: %+v %v", o, err)
	}
	var set model.OutboundSettings
	json.Unmarshal(o.Settings, &set)
	if set.Server != "162.159.192.1" || set.Port != 2408 || set.PrivateKey == "" || len(set.Reserved) != 3 || len(set.LocalAddress) != 2 || set.MTU != 1280 {
		t.Fatalf("settings %+v", set)
	}
	var rules []store.RoutingRule
	s.st.DB.Where("node_id = ?", "n1").Find(&rules)
	if len(rules) != 1 || rules[0].OutboundTag != "warp" || strings.Join(rules[0].Match.V.Geosite, ",") != "openai,netflix" {
		t.Fatalf("rules %+v", rules)
	}
	if strings.Contains(toJSONString(out["device"]), set.PrivateKey) {
		t.Fatal("private key echoed in the device summary")
	}

	// The tag is taken now: refused without another registration.
	if st, out := admin.do("POST", "/nodes/n1/warp", map[string]any{}); st != 400 || regs != 1 || !strings.Contains(out["error"].(string), "占用") {
		t.Fatalf("duplicate tag: %d %v regs=%d", st, out, regs)
	}
	// Without rules only the outbound is added.
	st, out = admin.do("POST", "/nodes/n1/warp", map[string]any{"tag": "warp2"})
	if st != 200 || out["ruleAdded"] != false {
		t.Fatalf("no rules: %d %v", st, out)
	}
}
