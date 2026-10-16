package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Standalone forward rules: port conflicts, rendering into the node spec,
// tunnels and failover (down remotes left out, all down keeps all).
func TestForwardRules(t *testing.T) {
	s, _, c := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "relay", Address: "203.0.113.9"})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27", InstalledVersions: store.NewJSON([]string{})})
	code, out := c.do("POST", "/inbounds", map[string]any{"nodeId": "n1", "core": "xray", "tag": "r", "port": 443,
		"protocol": map[string]any{"type": "vless", "flow": "xtls-rprx-vision"}, "transport": map[string]any{"type": "tcp"},
		"security": map[string]any{"type": "reality", "realityDest": "www.example.com:443"}})
	if code != 201 {
		t.Fatalf("inbound %d %v", code, out)
	}
	rule := map[string]any{"nodeId": "n1", "name": "hk", "listenPort": 443, "network": "tcp",
		"remotes": []map[string]any{{"addr": "198.51.100.1:443", "enabled": true, "weight": 2}, {"addr": "198.51.100.2:443", "enabled": true}},
		"balance": "roundrobin", "failover": true, "tunnelRole": "client", "tunnelType": "wss", "tunnelHost": "cdn.example", "tunnelPath": "/t", "sendProxy": 2}
	if code, out = c.do("POST", "/forwards", rule); code != 400 {
		t.Fatalf("port 443 is taken by the inbound: %d %v", code, out)
	}
	rule["listenPort"] = 30443
	if code, out = c.do("POST", "/forwards", rule); code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	id := out["id"].(string)
	// The inbound side now sees the forward's port.
	code, _ = c.do("POST", "/inbounds", map[string]any{"nodeId": "n1", "core": "xray", "tag": "x", "port": 30443,
		"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "tcp"}, "security": map[string]any{"type": "none"}})
	if code == 201 {
		t.Fatal("inbound on a forward's port must be refused")
	}

	fw := s.standaloneForwards("n1")
	if len(fw) != 1 || fw[0].Target != "198.51.100.1:443" || len(fw[0].ExtraTargets) != 1 || fw[0].Weights[0] != 2 ||
		fw[0].RemoteTransport != "ws;host=cdn.example;path=/t;tls;sni=cdn.example;insecure" || fw[0].SendProxy != 2 {
		t.Fatalf("rendered %+v", fw)
	}
	db.Model(&store.ForwardRule{}).Where("id = ?", id).Update("health", store.NewJSON(map[string]bool{"198.51.100.1:443": true}))
	if fw = s.standaloneForwards("n1"); fw[0].Target != "198.51.100.2:443" || len(fw[0].ExtraTargets) != 0 {
		t.Fatalf("failover must drop the down remote: %+v", fw)
	}
	db.Model(&store.ForwardRule{}).Where("id = ?", id).Update("health", store.NewJSON(map[string]bool{"198.51.100.1:443": true, "198.51.100.2:443": true}))
	if fw = s.standaloneForwards("n1"); len(fw[0].ExtraTargets) != 1 {
		t.Fatalf("all down keeps every remote: %+v", fw)
	}
	rule["enabled"] = false
	if code, out = c.do("PUT", "/forwards/"+id, rule); code != 200 {
		t.Fatalf("disable %d %v", code, out)
	}
	if fw = s.standaloneForwards("n1"); len(fw) != 0 {
		t.Fatal("disabled rule rendered")
	}
}
