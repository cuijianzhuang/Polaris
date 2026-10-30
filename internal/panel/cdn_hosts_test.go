package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestNodeCDNHost(t *testing.T) {
	for _, c := range [][3]string{
		{"cdn.example.com", "ORACLE-SG", "oracle-sg-562f2833d5898890-cdn.example.com"},
		{"cdn.example.com", "BWG HK 香港", "bwg-hk-562f2833d5898890-cdn.example.com"},
		{"example.com", "sg", "sg-562f2833d5898890.example.com"},
		{"cdn.example.com", "香港", "node-562f2833d5898890-cdn.example.com"},
	} {
		if got := nodeCDNHost(c[0], c[1], "test-node"); got != c[2] {
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
	if host("hk1") != "" || host("hk2") != "" || host("sg1") != "oracle-sg-0480a93d2e9b094b-cdn.example.com" {
		t.Fatalf("hosts %q %q %q", host("hk1"), host("hk2"), host("sg1"))
	}
	// Entries made before: syncing splits them.
	var sg store.Inbound
	db.First(&sg, "tag = ?", "sg1")
	db.Model(&store.Endpoint{}).Where("inbound_id = ?", sg.ID).Update("host", "")
	var p store.CDNProfile
	db.First(&p, "id = ?", "cf")
	if notes := s.splitSharedCDNHosts(&p); len(notes) != 1 || host("sg1") != "oracle-sg-0480a93d2e9b094b-cdn.example.com" || host("hk1") != "" {
		t.Fatalf("split %v: %q %q", notes, host("sg1"), host("hk1"))
	}
	// Clients use the node's hostname for the address, SNI and Host.
	var ep store.Endpoint
	db.First(&ep, "inbound_id = ?", sg.ID)
	m := s.toModelInbound(&sg)
	tg := s.cdnTargetFor(&ep, &m, &p)
	if tg.Server != "oracle-sg-0480a93d2e9b094b-cdn.example.com" || tg.SNI != tg.Server || tg.Host != tg.Server {
		t.Fatalf("target %+v", tg)
	}
}

func TestCDNHostNamesRemainUniqueAndStable(t *testing.T) {
	s, admin := cdnFixture(t)
	for _, n := range []store.Node{{ID: "n2", Name: "香港", Address: "203.0.113.6", DefaultCore: model.CoreXray}, {ID: "n3", Name: "新加坡", Address: "203.0.113.7", DefaultCore: model.CoreXray}} {
		if err := s.st.DB.Create(&n).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.st.DB.Create(&store.Core{ID: "c" + n.ID, NodeID: n.ID, Type: model.CoreXray, Version: "26.3.27"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	hosts := map[string]string{}
	for _, id := range []string{"n1", "n2", "n3"} {
		req := map[string]any{"nodeId": id, "core": "xray", "tag": "host-" + id, "port": 2083, "protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "ws", "path": "/ws"}, "security": map[string]any{"type": "tls", "serverName": "cdn.example.com"}, "cdnProfileId": "cf", "cdnOnly": true}
		if code, out := admin.do("POST", "/inbounds", req); code != 201 {
			t.Fatal(code, out)
		}
		var in store.Inbound
		s.st.DB.First(&in, "tag = ?", "host-"+id)
		var ep store.Endpoint
		s.st.DB.First(&ep, "inbound_id = ? AND kind = ?", in.ID, "cdn")
		hosts[id] = ep.Host
	}
	if hosts["n2"] == hosts["n3"] || hosts["n2"] == "" || hosts["n3"] == "" {
		t.Fatal("Chinese names collide", hosts)
	}
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", "cf")
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "n2").Update("name", "renamed").Error; err != nil {
		t.Fatal(err)
	}
	if got := cdnHostFor(s.st.DB, &p, "n2"); got != hosts["n2"] {
		t.Fatal("rename changed published hostname", got, hosts["n2"])
	}
	// Existing colliding names from older versions are split on the next sync.
	if err := s.st.DB.Model(&store.Endpoint{}).Where("host IN ?", []string{hosts["n2"], hosts["n3"]}).Update("host", "node-cdn.example.com").Error; err != nil {
		t.Fatal(err)
	}
	if changes := s.splitSharedCDNHosts(&p); len(changes) != 1 {
		t.Fatal("legacy collision not split", changes)
	}
	for _, id := range []string{"n2", "n3"} {
		hosts[id] = cdnHostFor(s.st.DB, &p, id)
	}
	if hosts["n2"] == hosts["n3"] {
		t.Fatal("collision remains", hosts)
	}
	// Even a long origin label must leave room for a legal generated label.
	long := nodeCDNHost(strings.Repeat("a", 63)+".example.com", strings.Repeat("b", 100), "n2")
	if len(strings.Split(long, ".")[0]) > 63 || !cdnHostRe.MatchString(long) {
		t.Fatal("invalid long hostname", long)
	}
}
