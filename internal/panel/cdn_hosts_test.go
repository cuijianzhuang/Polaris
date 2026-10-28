package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestNodeCDNHost(t *testing.T) {
	for _, c := range [][3]string{
		{"cdn.example.com", "ORACLE-SG", "oracle-sg-cdn.example.com"},
		{"cdn.example.com", "BWG HK 香港", "bwg-hk-cdn.example.com"},
		{"example.com", "sg", "sg.example.com"},
		{"cdn.example.com", "香港", "node-cdn.example.com"},
	} {
		if got := nodeCDNHost(c[0], c[1]); got != c[2] {
			t.Errorf("%s %s: %s, want %s", c[0], c[1], got, c[2])
		}
	}
}

// Nodes sharing a profile get their own hostnames: one DNS record leads to
// one node only.
func TestCDNHostsPerNode(t *testing.T) {
	s, admin := cdnFixture(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n2", Name: "ORACLE-SG", Address: "203.0.113.6", DefaultCore: model.CoreXray})
	db.Create(&store.Core{ID: "c2", NodeID: "n2", Type: model.CoreXray, Version: "26.3.27"})
	ws := func(node, tag string, port int) map[string]any {
		return map[string]any{"nodeId": node, "core": "xray", "tag": tag, "port": port, "protocol": map[string]any{"type": "vless"},
			"transport": map[string]any{"type": "ws", "path": "/" + tag}, "security": map[string]any{"type": "tls", "serverName": "cdn.example.com"},
			"cdnProfileId": "cf", "cdnOnly": true}
	}
	host := func(tag string) string {
		var in store.Inbound
		db.First(&in, "tag = ?", tag)
		var ep store.Endpoint
		db.First(&ep, "inbound_id = ? AND kind = ?", in.ID, "cdn")
		return ep.Host
	}
	for _, r := range []map[string]any{ws("n1", "hk1", 2083), ws("n1", "hk2", 2087), ws("n2", "sg1", 2083)} {
		if code, out := admin.do("POST", "/inbounds", r); code != 201 {
			t.Fatalf("create %v: %d %v", r["tag"], code, out)
		}
	}
	if host("hk1") != "" || host("hk2") != "" || host("sg1") != "oracle-sg-cdn.example.com" {
		t.Fatalf("hosts %q %q %q", host("hk1"), host("hk2"), host("sg1"))
	}
	// Entries made before: syncing splits them.
	var sg store.Inbound
	db.First(&sg, "tag = ?", "sg1")
	db.Model(&store.Endpoint{}).Where("inbound_id = ?", sg.ID).Update("host", "")
	var p store.CDNProfile
	db.First(&p, "id = ?", "cf")
	if notes := s.splitSharedCDNHosts(&p); len(notes) != 1 || host("sg1") != "oracle-sg-cdn.example.com" || host("hk1") != "" {
		t.Fatalf("split %v: %q %q", notes, host("sg1"), host("hk1"))
	}
	// Clients use the node's hostname for the address, SNI and Host.
	var ep store.Endpoint
	db.First(&ep, "inbound_id = ?", sg.ID)
	m := s.toModelInbound(&sg)
	tg := s.cdnTargetFor(&ep, &m, &p)
	if tg.Server != "oracle-sg-cdn.example.com" || tg.SNI != tg.Server || tg.Host != tg.Server {
		t.Fatalf("target %+v", tg)
	}
}
