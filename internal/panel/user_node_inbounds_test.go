package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Batch 3: portal inbound management on user-owned nodes. The gates are
// portalOwnedNode (another user's node is a 404) plus the review state
// (only approved nodes manage inbounds), and the binding is a single
// direct UserAccess grant for the owner — never a group, never anyone
// else.

func approveUserNode(t *testing.T, admin *apiClient, id string) {
	t.Helper()
	if code, out := admin.do("POST", "/nodes/"+id+"/review", map[string]any{"action": "approve"}); code != http.StatusOK {
		t.Fatalf("approve %s: %d %v", id, code, out)
	}
}

func createPortalInbound(t *testing.T, p *portalClient, nodeID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	code, raw := p.req("POST", "/u/my-nodes/"+nodeID+"/inbounds", body, true)
	var out map[string]any
	json.Unmarshal([]byte(raw), &out)
	return code, out
}

func vlessInboundBody(tag string, port int) map[string]any {
	return map[string]any{
		"tag": tag, "port": port,
		"protocol":  map[string]any{"type": "vless", "flow": "xtls-rprx-vision"},
		"transport": map[string]any{"type": "tcp"},
		"security":  map[string]any{"type": "reality", "serverName": "www.example.com", "realityDest": "www.example.com:443"},
		"enabled":   true,
	}
}

func portalInboundID(t *testing.T, out map[string]any) string {
	t.Helper()
	in, ok := out["inbound"].(map[string]any)
	if !ok {
		t.Fatalf("no inbound in response: %v", out)
	}
	id, _ := in["id"].(string)
	if id == "" {
		t.Fatalf("no inbound id in response: %v", out)
	}
	return id
}

func grantsFor(t *testing.T, s *Server, userID, inboundID string) int {
	t.Helper()
	var n int
	if err := s.st.DB.Raw("SELECT COUNT(*) FROM ("+store.GrantsSQL+") g WHERE g.user_id = ? AND g.inbound_id = ?", userID, inboundID).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPortalNodeInboundsPendingGate(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	p, _ := userNodesClient(t, s, srv.URL, "pend-gina")
	_, out := createPortalNode(t, p, "gina-node")
	id := portalNodeID(t, out) // pending under the default approval mode

	if code, out := createPortalInbound(t, p, id, vlessInboundBody("v1", 24443)); code != http.StatusForbidden {
		t.Fatalf("create on pending node: %d %v", code, out)
	}
	if code, _ := p.req("GET", "/u/my-nodes/"+id+"/inbounds", nil, false); code != http.StatusForbidden {
		t.Fatalf("list on pending node: %d", code)
	}
	if code, _ := p.req("PATCH", "/u/my-nodes/"+id+"/inbounds/whatever", vlessInboundBody("v1", 24443), true); code != http.StatusForbidden {
		t.Fatalf("update on pending node: %d", code)
	}
	if code, _ := p.req("DELETE", "/u/my-nodes/"+id+"/inbounds/whatever", nil, true); code != http.StatusForbidden {
		t.Fatalf("delete on pending node: %d", code)
	}
	var n int64
	s.st.DB.Model(&store.Inbound{}).Where("node_id = ?", id).Count(&n)
	if n != 0 {
		t.Fatalf("pending node gained %d inbounds", n)
	}
}

func TestPortalNodeInboundsIsolation(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	pa, _ := userNodesClient(t, s, srv.URL, "ib-alice")
	pb, _ := userNodesClient(t, s, srv.URL, "ib-bob")
	_, out := createPortalNode(t, pa, "alice-node")
	nodeID := portalNodeID(t, out)
	approveUserNode(t, admin, nodeID)
	_, created := createPortalInbound(t, pa, nodeID, vlessInboundBody("alice-vless", 24443))
	inID := portalInboundID(t, created)

	// Every one of Bob's operations on Alice's node inbounds is a 404.
	if code, _ := pb.req("GET", "/u/my-nodes/"+nodeID+"/inbounds", nil, false); code != http.StatusNotFound {
		t.Fatalf("bob list: %d", code)
	}
	if code, out := createPortalInbound(t, pb, nodeID, vlessInboundBody("bob-vless", 24444)); code != http.StatusNotFound {
		t.Fatalf("bob create: %d %v", code, out)
	}
	if code, _ := pb.req("PATCH", "/u/my-nodes/"+nodeID+"/inbounds/"+inID, vlessInboundBody("hijack", 24445), true); code != http.StatusNotFound {
		t.Fatalf("bob update: %d", code)
	}
	if code, _ := pb.req("DELETE", "/u/my-nodes/"+nodeID+"/inbounds/"+inID, nil, true); code != http.StatusNotFound {
		t.Fatalf("bob delete: %d", code)
	}
	// And a made-up inbound id on Alice's own node is equally a 404.
	if code, _ := pa.req("DELETE", "/u/my-nodes/"+nodeID+"/inbounds/nope", nil, true); code != http.StatusNotFound {
		t.Fatalf("alice delete missing inbound: %d", code)
	}
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", inID).Error; err != nil || row.Tag != "alice-vless" || row.Port != 24443 {
		t.Fatalf("inbound after bob's attempts: %+v %v", row, err)
	}
}

func TestPortalNodeInboundCreateBindsOwnerOnly(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	pa, alice := userNodesClient(t, s, srv.URL, "own-hank")
	_, bob := userNodesClient(t, s, srv.URL, "own-ivan")
	_, out := createPortalNode(t, pa, "hank-node")
	nodeID := portalNodeID(t, out)
	approveUserNode(t, admin, nodeID)

	code, created := createPortalInbound(t, pa, nodeID, vlessInboundBody("hank-vless", 24443))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, created)
	}
	inID := portalInboundID(t, created)
	// The portal view never carries server secrets.
	if raw, _ := json.Marshal(created); strings.Contains(string(raw), "PrivateKey") || strings.Contains(string(raw), "serverKey") {
		t.Fatalf("view leaks secrets: %s", raw)
	}

	// Exactly one grant exists for the inbound: the owner's direct access.
	var grants []store.UserAccess
	s.st.DB.Where("target_id = ?", inID).Find(&grants)
	if len(grants) != 1 || grants[0].UserID != alice.ID || grants[0].Kind != "inbound" {
		t.Fatalf("grants: %+v", grants)
	}
	var groupBinds int64
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", inID).Count(&groupBinds)
	if groupBinds != 0 {
		t.Fatalf("portal inbound got %d group bindings", groupBinds)
	}
	// The grant drives both directions of the existing model: the owner
	// is the inbound's only user, and the subscription grant query sees
	// the inbound for the owner and nobody else.
	users, err := s.inboundUsers([]string{inID})
	if err != nil {
		t.Fatal(err)
	}
	if len(users[inID]) != 1 || users[inID][0].ID != alice.ID {
		t.Fatalf("inbound users: %+v", users[inID])
	}
	if grantsFor(t, s, alice.ID, inID) != 1 || grantsFor(t, s, bob.ID, inID) != 0 {
		t.Fatal("subscription grants leak beyond the owner")
	}
	spec, err := s.BuildSpec(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Inbounds) != 1 || len(spec.Inbounds[0].Users) != 1 || spec.Inbounds[0].Users[0].ID != alice.ID {
		t.Fatalf("spec: %+v", spec.Inbounds)
	}
	// The owner lists it back.
	if code, body := pa.req("GET", "/u/my-nodes/"+nodeID+"/inbounds", nil, false); code != http.StatusOK || !strings.Contains(body, "hank-vless") {
		t.Fatalf("owner list: %d %s", code, body)
	}
}

func TestPortalNodeInboundStripsManagementFields(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	pa, _ := userNodesClient(t, s, srv.URL, "strip-judy")
	_, out := createPortalNode(t, pa, "judy-node")
	nodeID := portalNodeID(t, out)
	approveUserNode(t, admin, nodeID)

	cert := store.Certificate{ID: "cert-admin", Domain: "admin.example.com", Provider: "manual", Cert: "CERT", Key: "KEY"}
	if err := s.st.DB.Create(&cert).Error; err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"tag": "judy-tls", "port": 24446,
		"nodeId": "someone-elses-node", "groupIds": []string{"g1"},
		"relayOnly": true, "relayAccess": map[string]any{"enabled": true, "proxyProtocol": 1},
		"templateId": "tpl-x", "cdnProfileId": "cdn-x", "cdnOnly": true,
		"outboundTag": "direct", "speedLimitMbps": 50, "extra": map[string]any{"foo": 1},
		"protocol": map[string]any{"type": "vless", "snellUserId": "someone-else",
			"fallbacks": []map[string]any{{"inboundId": "other-inbound"}}},
		"transport": map[string]any{"type": "tcp", "acceptProxyProtocol": true},
		"security":  map[string]any{"type": "tls", "serverName": "judy.example.com", "certificateId": "cert-admin"},
		"enabled":   true,
	}
	code, created := createPortalInbound(t, pa, nodeID, body)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, created)
	}
	inID := portalInboundID(t, created)
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", inID).Error; err != nil {
		t.Fatal(err)
	}
	if row.NodeID != nodeID {
		t.Fatalf("nodeId not forced: %q", row.NodeID)
	}
	if row.TemplateID != "" || row.RelayOnly || row.RelayAccess.V.Enabled || row.OutboundTag != "" || row.SpeedLimitMbps != 0 || len(row.Extra) != 0 {
		t.Fatalf("management fields survived: %+v", row)
	}
	if row.Security.V.CertificateID == "cert-admin" {
		t.Fatal("foreign certificate reference survived")
	}
	if len(row.Protocol.V.Fallbacks) != 0 || row.Protocol.V.SnellUserID != "" {
		t.Fatalf("protocol references survived: %+v", row.Protocol.V)
	}
	if row.Transport.V.AcceptProxyProtocol {
		t.Fatal("acceptProxyProtocol survived")
	}
	var groupBinds int64
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", inID).Count(&groupBinds)
	if groupBinds != 0 {
		t.Fatalf("groupIds survived: %d bindings", groupBinds)
	}
}

func TestPortalNodeInboundUpdateDeleteSync(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	pa, alice := userNodesClient(t, s, srv.URL, "sync-kyle")
	_, out := createPortalNode(t, pa, "kyle-node")
	nodeID := portalNodeID(t, out)
	approveUserNode(t, admin, nodeID)
	_, created := createPortalInbound(t, pa, nodeID, vlessInboundBody("kyle-vless", 24447))
	inID := portalInboundID(t, created)

	clearDirty := func() {
		s.dirtyMu.Lock()
		delete(s.dirty, nodeID)
		s.dirtyMu.Unlock()
	}
	dirty := func() bool {
		s.dirtyMu.Lock()
		defer s.dirtyMu.Unlock()
		_, ok := s.dirty[nodeID]
		return ok
	}

	// Update: the port moves, the binding stays the owner's alone, and
	// the node is marked dirty (the same signal the admin path raises).
	clearDirty()
	upd := vlessInboundBody("kyle-vless", 24555)
	code, raw := pa.req("PATCH", "/u/my-nodes/"+nodeID+"/inbounds/"+inID, upd, true)
	if code != http.StatusOK {
		t.Fatalf("update: %d %s", code, raw)
	}
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", inID).Error; err != nil || row.Port != 24555 {
		t.Fatalf("after update: %+v %v", row, err)
	}
	if grantsFor(t, s, alice.ID, inID) != 1 {
		t.Fatal("update lost the owner grant")
	}
	if !dirty() {
		t.Fatal("update did not mark the node dirty")
	}
	spec, err := s.BuildSpec(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Inbounds) != 1 || spec.Inbounds[0].Port != 24555 {
		t.Fatalf("spec after update: %+v", spec.Inbounds)
	}

	// Delete: row, grant and spec entry all go, and the node is dirty.
	clearDirty()
	if code, _ := pa.req("DELETE", "/u/my-nodes/"+nodeID+"/inbounds/"+inID, nil, true); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if err := s.st.DB.First(&row, "id = ?", inID).Error; err == nil {
		t.Fatal("inbound row survived delete")
	}
	var grants int64
	s.st.DB.Model(&store.UserAccess{}).Where("target_id = ?", inID).Count(&grants)
	if grants != 0 {
		t.Fatalf("grant survived delete: %d", grants)
	}
	if !dirty() {
		t.Fatal("delete did not mark the node dirty")
	}
	spec, err = s.BuildSpec(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Inbounds) != 0 {
		t.Fatalf("spec after delete: %d inbounds", len(spec.Inbounds))
	}
}

func TestPortalMyNodesPageAndEntry(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	p, _ := userNodesClient(t, s, srv.URL, "page-lena")
	_, out := createPortalNode(t, p, "lena-node")
	nodeID := portalNodeID(t, out)

	if code, body := p.req("GET", "/u/my-nodes-page", nil, false); code != http.StatusOK ||
		!strings.Contains(body, "我的节点") || !strings.Contains(body, "lena-node") {
		t.Fatalf("page: %d", code)
	}
	// With an approved node and an inbound, the page renders the inbound
	// list and both inbound forms (the template's map-field paths).
	approveUserNode(t, admin, nodeID)
	if code, created := createPortalInbound(t, p, nodeID, vlessInboundBody("lena-vless", 24448)); code != http.StatusCreated {
		t.Fatalf("inbound for page: %d %v", code, created)
	}
	if code, body := p.req("GET", "/u/my-nodes-page", nil, false); code != http.StatusOK ||
		!strings.Contains(body, "lena-vless") || !strings.Contains(body, "ib-form") {
		t.Fatalf("page with inbound: %d", code)
	}
	// The user center links the page while the feature is on…
	if code, body := p.req("GET", "/u", nil, false); code != http.StatusOK || !strings.Contains(body, "/u/my-nodes-page") {
		t.Fatalf("entry link: %d", code)
	}
	// …and stops linking it once the admin turns the feature off.
	s.st.SetSetting("user_nodes_mode", "off")
	if _, body := p.req("GET", "/u", nil, false); strings.Contains(body, "/u/my-nodes-page") {
		t.Fatal("entry link shown while off")
	}
}
