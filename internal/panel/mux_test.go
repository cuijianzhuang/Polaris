package panel

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func muxFixture(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "jp", Address: "203.0.113.5"})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	certPEM, keyPEM, _ := pki.SelfSigned([]string{"b.example.com"}, 1e15)
	db.Create(&store.Certificate{ID: "cert-b", Domain: "b.example.com", Provider: "manual", Cert: string(certPEM), Key: s.st.Box.Seal(string(keyPEM))})
	certPEM2, keyPEM2, _ := pki.SelfSigned([]string{"other.example.com"}, 1e15)
	db.Create(&store.Certificate{ID: "cert-other", Domain: "other.example.com", Provider: "manual", Cert: string(certPEM2), Key: s.st.Box.Seal(string(keyPEM2))})
	db.Create(&store.MuxGroup{ID: "g1", NodeID: "n1", Name: "main", Port: 443, Enabled: true,
		FallbackKind: model.MuxFallbackExternal, FallbackTarget: "www.example.com:443"})
	return s
}

func realityMember(tag, sni string) model.Inbound {
	return model.Inbound{NodeID: "n1", Core: model.CoreXray, Tag: tag, Port: 443, Enabled: true, MuxGroupID: "g1",
		Protocol:  model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport: model.TransportSpec{Type: model.TransTCP},
		Security:  model.SecuritySpec{Type: model.SecReality, RealityDest: sni + ":443", RealityServerNames: []string{sni}}}
}

func trojanMember(tag, sni, certID string) model.Inbound {
	return model.Inbound{NodeID: "n1", Core: model.CoreXray, Tag: tag, Port: 443, Enabled: true, MuxGroupID: "g1",
		Protocol:  model.ProtocolSpec{Type: model.ProtoTrojan},
		Transport: model.TransportSpec{Type: model.TransTCP},
		Security:  model.SecuritySpec{Type: model.SecTLS, ServerName: sni, CertificateID: certID}}
}

func (s *Server) insert(t *testing.T, m model.Inbound) error {
	t.Helper()
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", m.NodeID).Error; err != nil {
		t.Fatal(err)
	}
	_, err := s.insertInbound(&node, &m, inboundMeta{}, []string{})
	return err
}

func TestMuxMembersSharePortAndSpec(t *testing.T) {
	s := muxFixture(t)
	if err := s.insert(t, realityMember("ra", "a.example.com")); err != nil {
		t.Fatalf("reality member: %v", err)
	}
	// Same port, same group: no conflict (portConflict same-group pass).
	if err := s.insert(t, trojanMember("tb", "b.example.com", "cert-b")); err != nil {
		t.Fatalf("trojan member: %v", err)
	}
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Muxes) != 1 {
		t.Fatalf("muxes: %+v", spec.Muxes)
	}
	mx := spec.Muxes[0]
	if mx.Port != 443 || mx.Fallback != "www.example.com:443" {
		t.Fatalf("mux: %+v", mx)
	}
	routes := map[string]string{}
	for _, r := range mx.Routes {
		routes[r.SNI] = r.Upstream
	}
	if routes["a.example.com"] == "" || routes["b.example.com"] == "" || routes["a.example.com"] == routes["b.example.com"] {
		t.Fatalf("routes: %+v", mx.Routes)
	}
	byTag := map[string]model.Inbound{}
	for _, in := range spec.Inbounds {
		byTag[in.Tag] = in
	}
	for tag, sni := range map[string]string{"ra": "a.example.com", "tb": "b.example.com"} {
		in := byTag[tag]
		if in.Listen != "127.0.0.1" || in.Port < frontInternalBase {
			t.Fatalf("member %s not moved to loopback: %+v", tag, in)
		}
		if routes[sni] != "127.0.0.1:"+strconv.Itoa(in.Port) {
			t.Fatalf("route %s -> %s, member %s on %d", sni, routes[sni], tag, in.Port)
		}
	}
}

func TestMuxBaselineUnchanged(t *testing.T) {
	// Red line: a node without mux groups renders exactly as before.
	s := muxFixture(t)
	m := trojanMember("solo", "b.example.com", "cert-b")
	m.MuxGroupID, m.Port = "", 8443
	if err := s.insert(t, m); err != nil {
		t.Fatal(err)
	}
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Muxes) != 0 {
		t.Fatalf("muxes without members: %+v", spec.Muxes)
	}
	if len(spec.Inbounds) != 1 || spec.Inbounds[0].Port != 8443 || spec.Inbounds[0].Listen == "127.0.0.1" {
		t.Fatalf("standalone inbound changed: %+v", spec.Inbounds)
	}
}

func TestMuxMemberFallback(t *testing.T) {
	s := muxFixture(t)
	if err := s.insert(t, realityMember("ra", "a.example.com")); err != nil {
		t.Fatal(err)
	}
	if err := s.insert(t, trojanMember("tb", "b.example.com", "cert-b")); err != nil {
		t.Fatal(err)
	}
	var ra store.Inbound
	s.st.DB.First(&ra, "tag = ?", "ra")
	s.st.DB.Model(&store.MuxGroup{}).Where("id = ?", "g1").Updates(map[string]any{
		"fallback_kind": model.MuxFallbackMember, "fallback_inbound_id": ra.ID, "fallback_target": ""})
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	mx := spec.Muxes[0]
	if mx.FallbackInbound != "ra" || mx.Fallback == "" {
		t.Fatalf("member fallback: %+v", mx)
	}
	for _, r := range mx.Routes {
		if r.Inbound == "ra" && r.Upstream != mx.Fallback {
			t.Fatalf("fallback upstream %s != member upstream %s", mx.Fallback, r.Upstream)
		}
	}
}

func TestMuxValidation(t *testing.T) {
	s := muxFixture(t)
	var node store.Node
	s.st.DB.First(&node, "id = ?", "n1")

	// Wrong port for the group.
	m := realityMember("bad-port", "a.example.com")
	m.Port = 8443
	if err := s.insert(t, m); err == nil || !strings.Contains(err.Error(), "组端口") {
		t.Fatalf("wrong port: %v", err)
	}
	// HTTP-shaped transport cannot join.
	m = realityMember("bad-trans", "a.example.com")
	m.Transport = model.TransportSpec{Type: model.TransWS, Path: "/ws"}
	m.Security = model.SecuritySpec{Type: model.SecTLS, ServerName: "a.example.com", CertificateID: "cert-other"}
	if err := s.insert(t, m); err == nil || !strings.Contains(err.Error(), "只有 TCP") {
		t.Fatalf("ws member: %v", err)
	}
	// TLS member whose certificate does not cover its SNI.
	m = trojanMember("bad-cert", "a.example.com", "cert-other")
	if err := s.insert(t, m); err == nil || !strings.Contains(err.Error(), "覆盖范围") {
		t.Fatalf("cert mismatch: %v", err)
	}
	// Duplicate SNI inside the group.
	if err := s.insert(t, realityMember("ra", "a.example.com")); err != nil {
		t.Fatal(err)
	}
	m = realityMember("ra2", "a.example.com")
	if err := s.insert(t, m); err == nil || !strings.Contains(err.Error(), "已被同组成员") {
		t.Fatalf("dup sni: %v", err)
	}
	// A standalone inbound may not take the group's port.
	m = trojanMember("solo", "b.example.com", "cert-b")
	m.MuxGroupID = ""
	if err := s.insert(t, m); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("standalone on group port: %v", err)
	}
	// A second group may not take the same port.
	g2 := store.MuxGroup{ID: "g2", NodeID: "n1", Name: "second", Port: 443, Enabled: true}
	if err := s.validateMuxGroup(&g2, "g2"); err == nil || !strings.Contains(err.Error(), "至多一组") {
		t.Fatalf("second group: %v", err)
	}
	// A group may not take a port a standalone inbound holds.
	solo := trojanMember("holder", "b.example.com", "cert-b")
	solo.MuxGroupID, solo.Port = "", 8443
	if err := s.insert(t, solo); err != nil {
		t.Fatal(err)
	}
	g3 := store.MuxGroup{ID: "g3", NodeID: "n1", Name: "third", Port: 8443, Enabled: true}
	if err := s.validateMuxGroup(&g3, "g3"); err == nil || !strings.Contains(err.Error(), "已被入站") {
		t.Fatalf("group on held port: %v", err)
	}
	_ = node
}

func TestMuxFallbackChildCannotJoin(t *testing.T) {
	s := muxFixture(t)
	db := s.st.DB
	// Child: loopback, no security; master: xray fallback to the child.
	db.Create(&store.Inbound{ID: "child", NodeID: "n1", Tag: "child", Port: 10001, Core: model.CoreXray, Enabled: true, Listen: "127.0.0.1",
		Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS, Path: "/ws"}),
		Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone})})
	master := trojanMember("master", "b.example.com", "cert-b")
	master.MuxGroupID, master.Port = "", 8443
	master.Protocol.Fallbacks = []model.Fallback{{InboundID: "child"}}
	if err := s.insert(t, master); err != nil {
		t.Fatal(err)
	}
	var node store.Node
	db.First(&node, "id = ?", "n1")
	child := trojanMember("child", "b.example.com", "cert-b")
	child.ID = "child"
	if err := s.validateMuxMember(&node, &child, "child"); err == nil || !strings.Contains(err.Error(), "回落子入站") {
		t.Fatalf("fallback child join: %v", err)
	}
}

func TestMuxUserNodeRejected(t *testing.T) {
	s := muxFixture(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "un1", Name: "un-x", OwnerID: "u1", ReviewState: "approved"})
	db.Create(&store.Core{ID: "c2", NodeID: "un1", Type: model.CoreXray, Version: "26.3.27"})
	g := store.MuxGroup{ID: "gu", NodeID: "un1", Name: "x", Port: 443, Enabled: true}
	if err := s.validateMuxGroup(&g, "gu"); err == nil || !strings.Contains(err.Error(), "用户自带节点") {
		t.Fatalf("group on user node: %v", err)
	}
	// Even with a group row forced into the DB, BuildSpec dispatches no
	// muxes to a user-owned node and leaves its inbounds untouched.
	db.Create(&g)
	db.Create(&store.Inbound{ID: "um", NodeID: "un1", Tag: "um", Port: 443, Core: model.CoreXray, Enabled: true, MuxGroupID: "gu",
		Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}),
		Security: store.NewJSON(model.SecuritySpec{Type: model.SecReality, RealityDest: "a.example.com:443", RealityServerNames: []string{"a.example.com"}})})
	spec, err := s.BuildSpec("un1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Muxes) != 0 {
		t.Fatalf("user node muxes: %+v", spec.Muxes)
	}
	for _, in := range spec.Inbounds {
		if in.Port != 443 || in.Listen == "127.0.0.1" {
			t.Fatalf("user node inbound rewritten: %+v", in)
		}
	}
	// The portal sanitizer strips membership from owner-managed inbounds.
	m := realityMember("x", "a.example.com")
	sanitizePortalInbound(&m)
	if m.MuxGroupID != "" {
		t.Fatal("sanitizePortalInbound kept MuxGroupID")
	}
}
