package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// userNodesClient creates a portal user and signs them in with an e-mail
// token, returning a portal client acting as them.
func userNodesClient(t *testing.T, s *Server, base, id string) (*portalClient, store.User) {
	t.Helper()
	u := store.User{ID: id, Username: id, Email: id + "@example.com", SubToken: id + "-sub-token",
		UUID: id + "-uuid", Password: "pw", Status: "active"}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	token, err := s.issuePortalMailToken(&u)
	if err != nil {
		t.Fatal(err)
	}
	return portalCookieClient(t, base, token), u
}

func createPortalNode(t *testing.T, p *portalClient, displayName string) (int, map[string]any) {
	t.Helper()
	code, body := p.req("POST", "/u/my-nodes", map[string]any{"displayName": displayName, "address": "203.0.113.10"}, true)
	var out map[string]any
	json.Unmarshal([]byte(body), &out)
	return code, out
}

func portalNodeID(t *testing.T, out map[string]any) string {
	t.Helper()
	node, ok := out["node"].(map[string]any)
	if !ok {
		t.Fatalf("no node in response: %v", out)
	}
	id, _ := node["id"].(string)
	if id == "" {
		t.Fatalf("no node id in response: %v", out)
	}
	return id
}

func TestPortalUserNodesCommunityQuota(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	licenseNone(s) // a keyed build with no licence: true community limits
	s.st.SetSetting("portal_enabled", "true")
	p, u := userNodesClient(t, s, srv.URL, "quota-alice")

	var firstID string
	for i := 0; i < 3; i++ {
		code, out := createPortalNode(t, p, "srv")
		if code != http.StatusCreated {
			t.Fatalf("create %d: %d %v", i, code, out)
		}
		if i == 0 {
			firstID = portalNodeID(t, out)
			if cmd, _ := out["command"].(string); !strings.HasSuffix(cmd, "/j/"+out["token"].(string)+" | bash") {
				t.Fatalf("install command: %v", out)
			}
		}
	}
	// Community edition: a 4th own node is refused (402, like limitOK).
	if code, out := createPortalNode(t, p, "srv"); code != http.StatusPaymentRequired {
		t.Fatalf("4th node: %d %v", code, out)
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", firstID).Error; err != nil {
		t.Fatal(err)
	}
	if n.OwnerID != u.ID || n.ReviewState != "pending" || !n.StatusHidden || n.TrafficRate != 1 {
		t.Fatalf("created node fields: %+v", n)
	}
	if n.Name != "un-"+n.ID || n.DisplayName != "srv" {
		t.Fatalf("internal/display names: %q / %q", n.Name, n.DisplayName)
	}
	// Deleting frees a slot again.
	if code, _ := p.req("DELETE", "/u/my-nodes/"+firstID, nil, true); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, out := createPortalNode(t, p, "srv"); code != http.StatusCreated {
		t.Fatalf("create after delete: %d %v", code, out)
	}
}

func TestPortalUserNodesPlanCap(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	plan := store.Group{ID: "plan-one", Name: "plan-one", Plan: true, NodeLimit: 1}
	if err := s.st.DB.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	p, u := userNodesClient(t, s, srv.URL, "plan-bob")
	if err := s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("plan_id", plan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if code, out := createPortalNode(t, p, "one"); code != http.StatusCreated {
		t.Fatalf("first: %d %v", code, out)
	}
	// The plan tightens the community cap of 3 down to 1.
	if code, out := createPortalNode(t, p, "two"); code != http.StatusPaymentRequired {
		t.Fatalf("second over plan cap: %d %v", code, out)
	}
}

func TestPortalUserNodesIsolation(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	pa, _ := userNodesClient(t, s, srv.URL, "iso-alice")
	pb, _ := userNodesClient(t, s, srv.URL, "iso-bob")
	_, out := createPortalNode(t, pa, "alice-node")
	id := portalNodeID(t, out)

	// Bob's list is empty, and every by-id operation on Alice's node is a
	// 404 — indistinguishable from a nonexistent node.
	if _, body := pb.req("GET", "/u/my-nodes", nil, false); !strings.Contains(body, `"nodes":[]`) {
		t.Fatalf("bob list: %s", body)
	}
	if code, _ := pb.req("GET", "/u/my-nodes/"+id+"/install", nil, false); code != http.StatusNotFound {
		t.Fatalf("bob install: %d", code)
	}
	if code, _ := pb.req("PATCH", "/u/my-nodes/"+id, map[string]any{"displayName": "hijack"}, true); code != http.StatusNotFound {
		t.Fatalf("bob patch: %d", code)
	}
	if code, _ := pb.req("POST", "/u/my-nodes/"+id+"/reset-access", nil, true); code != http.StatusNotFound {
		t.Fatalf("bob reset: %d", code)
	}
	if code, _ := pb.req("DELETE", "/u/my-nodes/"+id, nil, true); code != http.StatusNotFound {
		t.Fatalf("bob delete: %d", code)
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", id).Error; err != nil || n.DisplayName != "alice-node" {
		t.Fatalf("node after bob's attempts: %+v %v", n, err)
	}
	// Alice sees her node under its display name; the internal name
	// ("un-<id>") never leaks into the portal.
	_, body := pa.req("GET", "/u/my-nodes", nil, false)
	if !strings.Contains(body, `"displayName":"alice-node"`) || strings.Contains(body, "un-"+id) {
		t.Fatalf("alice list: %s", body)
	}
	// An admin node is equally invisible through the portal.
	admin := store.Node{ID: "adminnode1", Name: "admin-hk", Status: "online"}
	if err := s.st.DB.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	if code, _ := pa.req("GET", "/u/my-nodes/adminnode1/install", nil, false); code != http.StatusNotFound {
		t.Fatalf("admin node via portal: %d", code)
	}
}

func TestPortalUserNodesSameDisplayName(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	pa, _ := userNodesClient(t, s, srv.URL, "name-alice")
	pb, _ := userNodesClient(t, s, srv.URL, "name-bob")
	codeA, outA := createPortalNode(t, pa, "hk-1")
	codeB, outB := createPortalNode(t, pb, "hk-1")
	if codeA != http.StatusCreated || codeB != http.StatusCreated {
		t.Fatalf("same display name: %d %v / %d %v", codeA, outA, codeB, outB)
	}
	idA, idB := portalNodeID(t, outA), portalNodeID(t, outB)
	var a, b store.Node
	if err := s.st.DB.First(&a, "id = ?", idA).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.First(&b, "id = ?", idB).Error; err != nil {
		t.Fatal(err)
	}
	if a.Name == b.Name || a.DisplayName != "hk-1" || b.DisplayName != "hk-1" {
		t.Fatalf("names: %q/%q vs %q/%q", a.Name, a.DisplayName, b.Name, b.DisplayName)
	}
}

func TestUserNodesModeOff(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	p, _ := userNodesClient(t, s, srv.URL, "off-carol")
	_, out := createPortalNode(t, p, "before-off")
	id := portalNodeID(t, out)

	s.st.SetSetting("user_nodes_mode", "off")
	if code, out := createPortalNode(t, p, "after-off"); code != http.StatusForbidden {
		t.Fatalf("create while off: %d %v", code, out)
	}
	// Nodes that already exist stay manageable while the feature is off.
	if code, _ := p.req("GET", "/u/my-nodes", nil, false); code != http.StatusOK {
		t.Fatalf("list while off: %d", code)
	}
	if code, _ := p.req("DELETE", "/u/my-nodes/"+id, nil, true); code != http.StatusNoContent {
		t.Fatalf("delete while off: %d", code)
	}
}

func TestUserNodesOpenModeNeedsFeature(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	licenseNone(s) // community: no paid features at all
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("user_nodes_mode", "open")
	p, _ := userNodesClient(t, s, srv.URL, "open-dave")

	// The community edition has no user_nodes_open feature: the open
	// setting degrades to approval.
	if got := s.effectiveUserNodesMode(); got != "approval" {
		t.Fatalf("effective mode without feature: %q", got)
	}
	_, out := createPortalNode(t, p, "dave-node")
	id := portalNodeID(t, out)
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if n.ReviewState != "pending" {
		t.Fatalf("node created under degraded open: reviewState %q", n.ReviewState)
	}

	// With the feature licensed, open takes effect.
	s.lic.Store(&licenseState{st: license.State{Plan: "pro", Features: []string{license.FeatureUserNodesOpen}}})
	if got := s.effectiveUserNodesMode(); got != "open" {
		t.Fatalf("effective mode with feature: %q", got)
	}
	_, out = createPortalNode(t, p, "dave-node-2")
	id2 := portalNodeID(t, out)
	var n2 store.Node // fresh struct: reusing n would keep its primary key as a query condition
	if err := s.st.DB.First(&n2, "id = ?", id2).Error; err != nil {
		t.Fatal(err)
	}
	if n2.ReviewState != "approved" {
		t.Fatalf("node created under open: reviewState %q", n2.ReviewState)
	}
}

func TestUserNodesModeSettingValidation(t *testing.T) {
	_, _, admin := newAdminClient(t)
	if code, out := admin.do("PUT", "/settings", map[string]string{"user_nodes_mode": "bogus"}); code != http.StatusBadRequest {
		t.Fatalf("bogus mode: %d %v", code, out)
	}
	if code, out := admin.do("PUT", "/settings", map[string]string{"user_nodes_mode": "open"}); code != http.StatusOK {
		t.Fatalf("open mode: %d %v", code, out)
	}
}

func TestUserNodeReviewGateAndFlow(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	p, _ := userNodesClient(t, s, srv.URL, "gate-erin")
	_, out := createPortalNode(t, p, "erin-node")
	id := portalNodeID(t, out)

	// Give the node an inbound directly: while it is pending, BuildSpec
	// must still hand the agent an empty configuration.
	in := store.Inbound{ID: "erin-in", NodeID: id, Core: model.CoreXray, Tag: "erin-in", Port: 24443, Enabled: true,
		Protocol:  store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}),
		Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}),
		Security:  store.NewJSON(model.SecuritySpec{Type: model.SecNone})}
	if err := s.st.DB.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	spec, err := s.BuildSpec(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Inbounds) != 0 {
		t.Fatalf("pending node spec has %d inbounds", len(spec.Inbounds))
	}

	// The review endpoint refuses admin nodes.
	adminNode := store.Node{ID: "adminnode2", Name: "admin-jp", Status: "online"}
	if err := s.st.DB.Create(&adminNode).Error; err != nil {
		t.Fatal(err)
	}
	if code, out := admin.do("POST", "/nodes/adminnode2/review", map[string]any{"action": "approve"}); code != http.StatusBadRequest {
		t.Fatalf("review admin node: %d %v", code, out)
	}

	// Approve: the gate opens and the inbound flows.
	if code, out := admin.do("POST", "/nodes/"+id+"/review", map[string]any{"action": "approve"}); code != http.StatusOK {
		t.Fatalf("approve: %d %v", code, out)
	}
	spec, err = s.BuildSpec(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Inbounds) != 1 {
		t.Fatalf("approved node spec has %d inbounds", len(spec.Inbounds))
	}

	// Suspend closes the gate again; approving once more reopens it.
	if code, out := admin.do("POST", "/nodes/"+id+"/review", map[string]any{"action": "suspend"}); code != http.StatusOK {
		t.Fatalf("suspend: %d %v", code, out)
	}
	spec, err = s.BuildSpec(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Inbounds) != 0 {
		t.Fatalf("suspended node spec has %d inbounds", len(spec.Inbounds))
	}
	if code, out := admin.do("POST", "/nodes/"+id+"/review", map[string]any{"action": "approve"}); code != http.StatusOK {
		t.Fatalf("re-approve: %d %v", code, out)
	}

	// Reject: the state flips and the agent certificate is revoked.
	s.st.DB.Model(&store.Node{}).Where("id = ?", id).Update("cert_serial", "abc123")
	if code, out := admin.do("POST", "/nodes/"+id+"/review", map[string]any{"action": "reject"}); code != http.StatusOK {
		t.Fatalf("reject: %d %v", code, out)
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if n.ReviewState != "rejected" || !n.Revoked || n.CertSerial != "" {
		t.Fatalf("after reject: %+v", n)
	}
	// Reset-access must not resurrect a rejected node (Enroll would clear
	// the revoked flag).
	if code, _ := p.req("POST", "/u/my-nodes/"+id+"/reset-access", nil, true); code != http.StatusBadRequest {
		t.Fatalf("reset rejected node: %d", code)
	}
}

func TestUserNodeEnrollSkipsBoundTemplates(t *testing.T) {
	s, _, _ := newAdminClient(t)
	item := store.TemplateItem{Key: "entry", Core: model.CoreXray, Protocol: model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport: model.TransportSpec{Type: model.TransTCP}, Security: model.SecuritySpec{Type: model.SecNone},
		Port: store.PortPolicy{Mode: "fixed", Fixed: 24448}, Cert: "none", TagPattern: "tpl-{node.name}"}
	tpl := store.InboundTemplate{ID: "tpl-bound", Name: "bound", Items: store.NewJSON([]store.TemplateItem{item}),
		BoundLabels: store.NewJSON([]string{"fleet"})}
	if err := s.st.DB.Create(&tpl).Error; err != nil {
		t.Fatal(err)
	}
	mkNode := func(id, owner string) string {
		t.Helper()
		tok := "tok-" + id
		exp := time.Now().Add(time.Hour)
		n := store.Node{ID: id, Name: "n-" + id, OwnerID: owner, Status: "pending",
			Labels: store.NewJSON([]string{"fleet"}), TokenHash: hashToken(tok), TokenExpires: &exp,
			DefaultCore: model.CoreXray, TrafficRate: 1}
		if owner != "" {
			n.Name, n.ReviewState = "un-"+id, "approved"
		}
		if err := s.st.DB.Create(&n).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.st.DB.Create(&store.Core{ID: "core-" + id, NodeID: id, Type: model.CoreXray, Version: "26.3.27",
			Status: "installing", IsDefault: true, InstalledVersions: store.NewJSON([]string{})}).Error; err != nil {
			t.Fatal(err)
		}
		return tok
	}
	enroll := func(tok string) {
		t.Helper()
		_, csrPEM, err := pki.NewCSR("host")
		if err != nil {
			t.Fatal(err)
		}
		a := &agentService{s: s}
		if _, err := a.Enroll(context.Background(), connect.NewRequest(&polarisv1.EnrollRequest{
			Token: tok, CsrPem: csrPEM, Hostname: "host", AgentVersion: "test"})); err != nil {
			t.Fatal(err)
		}
	}
	count := func(nodeID string) int64 {
		var n int64
		s.st.DB.Model(&store.Inbound{}).Where("node_id = ?", nodeID).Count(&n)
		return n
	}
	// Control: an admin node carrying the label gets the bound template
	// applied at enrolment (autoBindAfterEnroll is a no-op in this build,
	// so the template is the observable half of the enrolment hooks).
	enroll(mkNode("adminnode3", ""))
	if count("adminnode3") == 0 {
		t.Fatal("control failed: bound template did not land on the admin node")
	}
	// A user-owned node with the same label gets nothing.
	enroll(mkNode("usernode3", "some-user"))
	if got := count("usernode3"); got != 0 {
		t.Fatalf("user node received %d template inbounds", got)
	}
}

func TestUserDisableDeleteCascadesToOwnedNodes(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	pd, ud := userNodesClient(t, s, srv.URL, "d6-dave")
	_, out := createPortalNode(t, pd, "dave-node")
	daveNode := portalNodeID(t, out)
	pe, ue := userNodesClient(t, s, srv.URL, "d6-erin")
	_, out = createPortalNode(t, pe, "erin-node")
	erinNode := portalNodeID(t, out)
	// Both nodes hold live agent certificates.
	for _, id := range []string{daveNode, erinNode} {
		s.st.DB.Model(&store.Node{}).Where("id = ?", id).Updates(map[string]any{"cert_serial": "serial-" + id, "revoked": false})
	}

	// Disabling Dave revokes his node's certificate; Erin's is untouched.
	if code, out := admin.do("POST", "/users/bulk", map[string]any{"action": "disable", "ids": []string{ud.ID}}); code != http.StatusOK {
		t.Fatalf("bulk disable: %d %v", code, out)
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", daveNode).Error; err != nil {
		t.Fatal(err)
	}
	if !n.Revoked || n.CertSerial != "" {
		t.Fatalf("disabled user's node: %+v", n)
	}
	var en store.Node // fresh struct, see above
	if err := s.st.DB.First(&en, "id = ?", erinNode).Error; err != nil || en.Revoked {
		t.Fatalf("other user's node after disable: %+v %v", en, err)
	}

	// Deleting Erin (single delete) removes her node with her account.
	if code, out := admin.do("DELETE", "/users/"+ue.ID, nil); code != http.StatusNoContent {
		t.Fatalf("delete user: %d %v", code, out)
	}
	var cnt int64
	s.st.DB.Model(&store.Node{}).Where("id = ?", erinNode).Count(&cnt)
	if cnt != 0 {
		t.Fatal("deleted user's node survived")
	}
	// Dave's node still exists — he is only disabled.
	s.st.DB.Model(&store.Node{}).Where("id = ?", daveNode).Count(&cnt)
	if cnt != 1 {
		t.Fatal("disabled user's node was deleted")
	}
	// Bulk-deleting Dave removes his node too.
	if code, out := admin.do("POST", "/users/bulk", map[string]any{"action": "delete", "ids": []string{ud.ID}}); code != http.StatusOK {
		t.Fatalf("bulk delete: %d %v", code, out)
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", daveNode).Count(&cnt)
	if cnt != 0 {
		t.Fatal("bulk-deleted user's node survived")
	}
}
