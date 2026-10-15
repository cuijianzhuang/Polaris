package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestFallbackValidationSpecAndLinks(t *testing.T) {
	s := newTestServer(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.5"})
	db.Create(&store.Node{ID: "n2", Name: "hk"})
	mk := func(id, node, listen string, port int, p model.ProtocolSpec, tr model.TransportSpec, sec model.SecuritySpec) {
		db.Create(&store.Inbound{ID: id, NodeID: node, Tag: id, Core: model.CoreXray, Listen: listen, Port: port, Enabled: true,
			Protocol: store.NewJSON(p), Transport: store.NewJSON(tr), Security: store.NewJSON(sec), Sniffing: store.NewJSON(model.SniffSpec{})})
	}
	tlsSec := model.SecuritySpec{Type: model.SecTLS, ServerName: "a.example.com"}
	mk("ws", "n1", "127.0.0.1", 2001, model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{Type: model.TransWS, Path: "/ws"}, model.SecuritySpec{})
	mk("pub", "n1", "", 2002, model.ProtocolSpec{Type: model.ProtoVMess}, model.TransportSpec{Type: model.TransWS, Path: "/v"}, model.SecuritySpec{})
	mk("other", "n2", "127.0.0.1", 2003, model.ProtocolSpec{Type: model.ProtoTrojan}, model.TransportSpec{}, model.SecuritySpec{})

	master := model.Inbound{NodeID: "n1", Core: model.CoreXray, Port: 443, Protocol: model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport: model.TransportSpec{Type: model.TransTCP}, Security: tlsSec}
	cases := map[string]struct {
		fbs  []model.Fallback
		edit func(*model.Inbound)
	}{
		"public child":    {fbs: []model.Fallback{{InboundID: "pub"}}},
		"other node":      {fbs: []model.Fallback{{InboundID: "other"}}},
		"bad path":        {fbs: []model.Fallback{{InboundID: "ws", Path: "ws"}}},
		"two defaults":    {fbs: []model.Fallback{{InboundID: "ws"}, {InboundID: "ws", ALPN: "h2"}, {InboundID: "ws", Path: "/a"}, {InboundID: "ws"}}},
		"not capable":     {fbs: []model.Fallback{{InboundID: "ws"}}, edit: func(m *model.Inbound) { m.Transport.Type = model.TransWS }},
		"sing-box master": {fbs: []model.Fallback{{InboundID: "ws"}}, edit: func(m *model.Inbound) { m.Core = model.CoreSingbox }},
	}
	for name, c := range cases {
		m := master
		m.Protocol.Fallbacks = c.fbs
		if c.edit != nil {
			c.edit(&m)
		}
		if err := s.validateFallbacks(&m, ""); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	m := master
	m.Protocol.Fallbacks = []model.Fallback{{InboundID: "ws", Path: "/ws"}}
	if err := s.validateFallbacks(&m, ""); err != nil {
		t.Fatal(err)
	}
	mk("master", "n1", "", 443, m.Protocol, m.Transport, m.Security)

	// The spec resolves the child to its loopback address.
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range spec.Inbounds {
		if in.ID == "master" && (len(in.Protocol.Fallbacks) != 1 || in.Protocol.Fallbacks[0].Dest != "127.0.0.1:2001") {
			t.Fatalf("fallback not resolved: %+v", in.Protocol.Fallbacks)
		}
	}
	// The child cannot be deleted while referenced.
	if got := s.referencingMaster("ws"); got != "master" {
		t.Fatalf("referencingMaster = %q", got)
	}

	// A user of the child gets a link on the master's port with its TLS.
	db.Create(&store.User{ID: "u1", Username: "alice", UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", Status: "active", SubToken: "alice-token-0000000000"})
	db.Create(&store.UserAccess{UserID: "u1", Kind: "inbound", TargetID: "ws"})
	var u store.User
	db.First(&u, "id = ?", "u1")
	nodes, err := s.subNodes(&u)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("%v %+v", err, nodes)
	}
	n := nodes[0]
	if n.Port != 443 || n.TLS.Type != model.SecTLS || n.TLS.ServerName != "a.example.com" || n.Transport.Path != "/ws" || !strings.Contains(n.Server, "203.0.113.5") {
		t.Fatalf("child link not rewritten to the master: %+v", n)
	}
}
