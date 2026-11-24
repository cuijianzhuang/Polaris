package panel

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Batch 4: isolation close-out. The save-time checks keep user-owned nodes
// out of every shared pool (groups/plans, direct bindings to other users,
// relays, chains, template group bindings), and GrantsSQL — the one grant
// query behind subscription rendering and BuildSpec — filters them again
// at read time, so even rows written directly into the tables (legacy or
// corrupt data) cannot leak a user node into someone else's subscription.

// ownedNode creates a portal user and one node for them, optionally
// approved, and returns the portal client, the user and the node ID.
func ownedNode(t *testing.T, s *Server, srvURL string, admin *apiClient, username string, approve bool) (*portalClient, store.User, string) {
	t.Helper()
	p, u := userNodesClient(t, s, srvURL, username)
	_, out := createPortalNode(t, p, username+"-box")
	id := portalNodeID(t, out)
	if approve {
		approveUserNode(t, admin, id)
	}
	return p, u, id
}

func adminNode(t *testing.T, s *Server, id, name string) store.Node {
	t.Helper()
	n := store.Node{ID: id, Name: name, Address: "203.0.113.20", Status: "online", DefaultCore: model.CoreXray}
	if err := s.st.DB.Create(&n).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.Core{ID: id + "-core", NodeID: id, Type: model.CoreXray, Version: "26.3.27", IsDefault: true}).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func adminInboundBody(nodeID, tag string, port int, extra map[string]any) map[string]any {
	body := vlessInboundBody(tag, port)
	body["nodeId"] = nodeID
	body["core"] = "xray"
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestGroupSaveRejectsUserNodes(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	adminNode(t, s, "adm-g1", "adm-g1")
	_, _, nodeID := ownedNode(t, s, srv.URL, admin, "grp-alice", true)

	// Give the user node an inbound via the admin API (no groups: allowed).
	code, out := admin.do("POST", "/inbounds", adminInboundBody(nodeID, "alice-in", 24450, nil))
	if code != http.StatusCreated {
		t.Fatalf("admin inbound on user node: %d %v", code, out)
	}
	inID := portalInboundID(t, out)

	// A group holding only the admin node saves fine.
	if code, out := admin.do("POST", "/groups", map[string]any{"name": "g-ok", "nodeIds": []string{"adm-g1"}}); code != http.StatusOK {
		t.Fatalf("group with admin node: %d %v", code, out)
	}
	// The user node itself may not enter a group.
	code, out = admin.do("POST", "/groups", map[string]any{"name": "g-bad-node", "nodeIds": []string{nodeID}})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "自带节点") {
		t.Fatalf("group with user node: %d %v", code, out)
	}
	// Nor may an inbound living on it.
	code, out = admin.do("POST", "/groups", map[string]any{"name": "g-bad-in", "inboundIds": []string{inID}})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "自带节点") {
		t.Fatalf("group with user-node inbound: %d %v", code, out)
	}
	// Plans share the same save path and the same rule.
	code, out = admin.do("POST", "/plans", map[string]any{"name": "p-bad", "nodeIds": []string{nodeID}})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "自带节点") {
		t.Fatalf("plan with user node: %d %v", code, out)
	}
	// Nothing was written for the rejected saves.
	var gn, gi int64
	s.st.DB.Model(&store.GroupNode{}).Where("node_id = ?", nodeID).Count(&gn)
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", inID).Count(&gi)
	if gn != 0 || gi != 0 {
		t.Fatalf("rejected bindings leaked: groupNodes=%d groupInbounds=%d", gn, gi)
	}
}

func TestDirectBindingRejectsForeignUserNode(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	_, owner, nodeID := ownedNode(t, s, srv.URL, admin, "bind-alice", true)
	code, out := admin.do("POST", "/inbounds", adminInboundBody(nodeID, "alice-in", 24451, nil))
	if code != http.StatusCreated {
		t.Fatalf("inbound: %d %v", code, out)
	}
	inID := portalInboundID(t, out)

	// Creating another user with the node bound is refused.
	code, out = admin.do("POST", "/users", map[string]any{"username": "bind-bob", "nodeIds": []string{nodeID}})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "只能绑定给其主人") {
		t.Fatalf("create user with foreign node: %d %v", code, out)
	}
	// So is updating one with the inbound bound.
	code, out = admin.do("POST", "/users", map[string]any{"username": "bind-carol"})
	if code != http.StatusCreated {
		t.Fatalf("create carol: %d %v", code, out)
	}
	carolID := out["id"].(string)
	code, out = admin.do("PUT", "/users/"+carolID, map[string]any{"username": "bind-carol", "inboundIds": []string{inID}})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "只能绑定给其主人") {
		t.Fatalf("update user with foreign inbound: %d %v", code, out)
	}
	// Bulk attach and the node-page binding endpoint refuse too.
	code, out = admin.do("POST", "/users/bulk", map[string]any{"action": "attach-nodes", "ids": []string{carolID}, "nodeIds": []string{nodeID}})
	if code != http.StatusBadRequest {
		t.Fatalf("bulk attach foreign node: %d %v", code, out)
	}
	code, out = admin.do("POST", "/nodes/"+nodeID+"/users", map[string]any{"userIds": []string{carolID}})
	if code != http.StatusBadRequest {
		t.Fatalf("bind node users foreign: %d %v", code, out)
	}
	// The owner themself may be bound — both kinds.
	code, out = admin.do("PUT", "/users/"+owner.ID, map[string]any{"username": owner.Username, "nodeIds": []string{nodeID}})
	if code != http.StatusOK {
		t.Fatalf("bind owner to own node: %d %v", code, out)
	}
	if got := grantsFor(t, s, owner.ID, inID); got == 0 {
		t.Fatalf("owner has no grant on own inbound after node binding")
	}
	if got := grantsFor(t, s, carolID, inID); got != 0 {
		t.Fatalf("carol gained a grant on alice's inbound")
	}
}

func TestRelayForRejectsUserNodes(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	adminNode(t, s, "adm-r1", "adm-r1")
	adminNode(t, s, "adm-r2", "adm-r2")
	_, _, nodeID := ownedNode(t, s, srv.URL, admin, "relay-alice", true)
	var un store.Node
	if err := s.st.DB.First(&un, "id = ?", nodeID).Error; err != nil {
		t.Fatal(err)
	}

	// A user node as a relayFor target is refused at creation…
	code, out := admin.do("POST", "/nodes", map[string]any{"name": "relay-src", "relayFor": []string{nodeID}})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "用户自带节点") {
		t.Fatalf("create relay to user node: %d %v", code, out)
	}
	// …and on update of an existing admin node.
	code, out = admin.do("PUT", "/nodes/adm-r1", map[string]any{"name": "adm-r1", "relayFor": []string{nodeID}})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "用户自带节点") {
		t.Fatalf("update relay to user node: %d %v", code, out)
	}
	// A user node may not declare relay targets of its own either.
	code, out = admin.do("PUT", "/nodes/"+nodeID, map[string]any{"name": un.Name, "relayFor": []string{"adm-r1"}})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "中转来源") {
		t.Fatalf("user node as relay source: %d %v", code, out)
	}
	// Admin-to-admin relay metadata still saves.
	code, out = admin.do("PUT", "/nodes/adm-r1", map[string]any{"name": "adm-r1", "relayFor": []string{"adm-r2"}})
	if code != http.StatusOK {
		t.Fatalf("admin relay: %d %v", code, out)
	}
}

func TestChainRejectsUserNodeHop(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	adminNode(t, s, "adm-c1", "adm-c1")
	adminNode(t, s, "adm-c2", "adm-c2")
	_, _, nodeID := ownedNode(t, s, srv.URL, admin, "chain-alice", true)
	code, out := admin.do("POST", "/inbounds", adminInboundBody("adm-c2", "landing", 24452, nil))
	if code != http.StatusCreated {
		t.Fatalf("landing inbound: %d %v", code, out)
	}
	landingID := portalInboundID(t, out)

	chain := func(name string, hops []map[string]any) (int, map[string]any) {
		return admin.do("POST", "/chains", map[string]any{"name": name, "mode": "l4", "hops": hops})
	}
	// A user node as the relay hop is refused.
	code, out = chain("ch-bad-hop", []map[string]any{
		{"nodeId": nodeID, "port": 31001},
		{"nodeId": "adm-c2", "inboundId": landingID},
	})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "用户自带节点") {
		t.Fatalf("chain with user hop: %d %v", code, out)
	}
	// A user node as the landing node is refused too.
	code, out = admin.do("POST", "/inbounds", adminInboundBody(nodeID, "alice-landing", 24453, nil))
	if code != http.StatusCreated {
		t.Fatalf("user landing inbound: %d %v", code, out)
	}
	userLanding := portalInboundID(t, out)
	code, out = chain("ch-bad-land", []map[string]any{
		{"nodeId": "adm-c1", "port": 31002},
		{"nodeId": nodeID, "inboundId": userLanding},
	})
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "用户自带节点") {
		t.Fatalf("chain landing on user node: %d %v", code, out)
	}
	// An all-admin chain still validates.
	code, out = chain("ch-ok", []map[string]any{
		{"nodeId": "adm-c1", "port": 31003},
		{"nodeId": "adm-c2", "inboundId": landingID},
	})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("admin chain: %d %v", code, out)
	}
}

func TestAdminInboundOnUserNodeGetsNoGroups(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	adminNode(t, s, "adm-i1", "adm-i1")
	_, _, nodeID := ownedNode(t, s, srv.URL, admin, "inb-alice", true)
	if code, out := admin.do("POST", "/groups", map[string]any{"name": "g-pool"}); code != http.StatusOK {
		t.Fatalf("group: %d %v", code, out)
	}
	var g store.Group
	if err := s.st.DB.First(&g, "name = ?", "g-pool").Error; err != nil {
		t.Fatal(err)
	}

	// Explicit group bindings on a user node's inbound are refused.
	code, out := admin.do("POST", "/inbounds", adminInboundBody(nodeID, "alice-in", 24454, map[string]any{"groupIds": []string{g.ID}}))
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "不能绑定分组") {
		t.Fatalf("create with groups: %d %v", code, out)
	}
	// Without groups it is created — and the default-group fallback must
	// not claim it either.
	code, out = admin.do("POST", "/inbounds", adminInboundBody(nodeID, "alice-in", 24454, nil))
	if code != http.StatusCreated {
		t.Fatalf("create without groups: %d %v", code, out)
	}
	inID := portalInboundID(t, out)
	var n int64
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", inID).Count(&n)
	if n != 0 {
		t.Fatalf("user-node inbound landed in %d groups", n)
	}
	// Nor can groups be attached afterwards through the update path.
	var in store.Inbound
	if err := s.st.DB.First(&in, "id = ?", inID).Error; err != nil {
		t.Fatal(err)
	}
	body := adminInboundBody(nodeID, "alice-in", 24454, map[string]any{"groupIds": []string{g.ID}})
	code, out = admin.do("PUT", "/inbounds/"+inID, body)
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "不能绑定分组") {
		t.Fatalf("update with groups: %d %v", code, out)
	}
}

func TestTemplateApplyToUserNodeBindsNoGroups(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	_, _, nodeID := ownedNode(t, s, srv.URL, admin, "tpl-alice", true)
	var tpl store.InboundTemplate
	if err := s.st.DB.First(&tpl, "name = ?", "全协议").Error; err != nil {
		t.Fatal(err)
	}
	code, out := admin.do("POST", "/templates/"+tpl.ID+"/apply", map[string]any{"nodeIds": []string{nodeID}, "autoPort": true, "reassign": true})
	if code != http.StatusOK {
		t.Fatalf("apply: %d %v", code, out)
	}
	var ins []store.Inbound
	if err := s.st.DB.Where("node_id = ?", nodeID).Find(&ins).Error; err != nil {
		t.Fatal(err)
	}
	if len(ins) == 0 {
		t.Fatalf("template generated nothing on the user node: %v", out)
	}
	ids := make([]string, len(ins))
	for i := range ins {
		ids[i] = ins[i].ID
	}
	var n int64
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id IN ?", ids).Count(&n)
	if n != 0 {
		t.Fatalf("template bound %d user-node inbounds into groups", n)
	}
	var relays int64
	s.st.DB.Model(&store.Endpoint{}).Where("inbound_id IN ? AND kind = ?", ids, "relay").Count(&relays)
	if relays != 0 {
		t.Fatalf("template created %d relay endpoints on a user node", relays)
	}
}

// TestGrantsSQLIsolatesUserNodes writes the forbidden bindings straight
// into the tables — bypassing every save-time check, as legacy or corrupt
// data could — and verifies the read side still isolates the node.
func TestGrantsSQLIsolatesUserNodes(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	pa, owner, nodeID := ownedNode(t, s, srv.URL, admin, "sql-alice", true)
	_, created := createPortalInbound(t, pa, nodeID, vlessInboundBody("alice-vless", 24455))
	inID := portalInboundID(t, created)
	_, bob, _ := ownedNode(t, s, srv.URL, admin, "sql-bob", false)

	db := s.st.DB
	// Dirty data: bob is in a group that holds the node and the inbound,
	// and holds direct bindings of both kinds as well.
	g := store.Group{ID: "dirty-g", Name: "dirty-g"}
	if err := db.Create(&g).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{
		&store.UserGroup{UserID: bob.ID, GroupID: g.ID},
		&store.GroupNode{GroupID: g.ID, NodeID: nodeID},
		&store.GroupInbound{GroupID: g.ID, InboundID: inID},
		&store.UserAccess{UserID: bob.ID, Kind: "node", TargetID: nodeID},
		&store.UserAccess{UserID: bob.ID, Kind: "inbound", TargetID: inID},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if got := grantsFor(t, s, bob.ID, inID); got != 0 {
		t.Fatalf("bob has %d grants on alice's inbound via dirty rows", got)
	}
	if got := grantsFor(t, s, owner.ID, inID); got == 0 {
		t.Fatalf("owner lost the grant on their own inbound")
	}
	// The subscription surface agrees: bob's node list has nothing of
	// alice's, the owner's does.
	subHas := func(u *store.User) bool {
		nodes, err := s.subNodesFor(u, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			if n.PanelInboundID == inID {
				return true
			}
		}
		return false
	}
	if subHas(&bob) {
		t.Fatalf("alice's inbound appears in bob's subscription")
	}
	if !subHas(&owner) {
		t.Fatalf("alice's inbound missing from her own subscription")
	}

	// The level path is filtered too: a levelled group matching the
	// node's (dirty) level still grants bob nothing.
	if err := db.Model(&store.Node{}).Where("id = ?", nodeID).Update("level", 5).Error; err != nil {
		t.Fatal(err)
	}
	g2 := store.Group{ID: "dirty-g5", Name: "dirty-g5", Level: 5}
	if err := db.Create(&g2).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.UserGroup{UserID: bob.ID, GroupID: g2.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if got := grantsFor(t, s, bob.ID, inID); got != 0 {
		t.Fatalf("bob gained a grant through the level path")
	}
}

// TestGrantsSQLHidesUnapprovedUserNodes: even the owner sees nothing of
// their node in subscriptions until it is approved, and nothing again
// once it is suspended or rejected.
func TestGrantsSQLHidesUnapprovedUserNodes(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	_, owner, nodeID := ownedNode(t, s, srv.URL, admin, "gate-alice", false) // stays pending
	db := s.st.DB
	// A pending node cannot get inbounds through the portal, so write one
	// directly, with the owner's direct binding in place.
	in := store.Inbound{ID: "gate-in", NodeID: nodeID, Tag: "gate-vless", Port: 24456, Core: model.CoreXray, Enabled: true,
		Protocol:  store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}),
		Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}),
		Security:  store.NewJSON(model.SecuritySpec{Type: model.SecNone})}
	if err := db.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.UserAccess{UserID: owner.ID, Kind: "inbound", TargetID: in.ID}).Error; err != nil {
		t.Fatal(err)
	}
	review := func(action string) {
		t.Helper()
		if code, out := admin.do("POST", "/nodes/"+nodeID+"/review", map[string]any{"action": action}); code != http.StatusOK {
			t.Fatalf("review %s: %d %v", action, code, out)
		}
	}
	if got := grantsFor(t, s, owner.ID, in.ID); got != 0 {
		t.Fatalf("pending node grants its owner %d", got)
	}
	review("approve")
	if got := grantsFor(t, s, owner.ID, in.ID); got == 0 {
		t.Fatalf("approved node grants its owner nothing")
	}
	review("suspend")
	if got := grantsFor(t, s, owner.ID, in.ID); got != 0 {
		t.Fatalf("suspended node still grants its owner %d", got)
	}
	review("approve")
	review("reject")
	if got := grantsFor(t, s, owner.ID, in.ID); got != 0 {
		t.Fatalf("rejected node still grants its owner %d", got)
	}
}

func TestUserNodeStaysOffPublicStatusPage(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	adminNode(t, s, "adm-s1", "adm-s1")
	p, _, nodeID := ownedNode(t, s, srv.URL, admin, "stat-alice", true)

	pub := s.probeOverview(true)
	for _, n := range pub["nodes"].([]probeNode) {
		if n.ID == nodeID {
			t.Fatalf("user node appears on the public status page")
		}
	}
	adm := s.probeOverview(false)
	found := false
	for _, n := range adm["nodes"].([]probeNode) {
		if n.ID == nodeID {
			found = true
		}
	}
	if !found {
		t.Fatalf("user node missing from the admin probe overview")
	}
	// The portal cannot talk the node onto the page: statusHidden is not
	// among the fields a portal PATCH may touch.
	if code, _ := p.req("PATCH", "/u/my-nodes/"+nodeID, map[string]any{"displayName": "stat-box", "statusHidden": false}, true); code != http.StatusOK {
		t.Fatalf("portal patch: %d", code)
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", nodeID).Error; err != nil {
		t.Fatal(err)
	}
	if !n.StatusHidden {
		t.Fatalf("portal PATCH cleared StatusHidden")
	}
}

func TestAdminEditsUserNodeDisplayName(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	_, _, nodeID := ownedNode(t, s, srv.URL, admin, "name-alice", true)
	var before store.Node
	if err := s.st.DB.First(&before, "id = ?", nodeID).Error; err != nil {
		t.Fatal(err)
	}
	// The edit form sends the internal name back unchanged plus the new
	// display name; only the display name may move.
	body := map[string]any{"name": before.Name, "address": before.Address, "displayName": "爱丽丝的机器"}
	if code, out := admin.do("PUT", "/nodes/"+nodeID, body); code != http.StatusOK {
		t.Fatalf("update: %d %v", code, out)
	}
	var after store.Node
	if err := s.st.DB.First(&after, "id = ?", nodeID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Name != before.Name {
		t.Fatalf("internal name changed: %q → %q", before.Name, after.Name)
	}
	if after.DisplayName != "爱丽丝的机器" {
		t.Fatalf("display name not saved: %q", after.DisplayName)
	}
	// An admin node's displayName field is inert: it has no display name
	// to edit and its real name keeps working as before.
	adminNode(t, s, "adm-n1", "adm-n1")
	if code, out := admin.do("PUT", "/nodes/adm-n1", map[string]any{"name": "adm-n1-renamed", "displayName": "ignored"}); code != http.StatusOK {
		t.Fatalf("admin node update: %d %v", code, out)
	}
	var an store.Node
	if err := s.st.DB.First(&an, "id = ?", "adm-n1").Error; err != nil {
		t.Fatal(err)
	}
	if an.Name != "adm-n1-renamed" || an.DisplayName != "" {
		t.Fatalf("admin node after update: %+v", an)
	}
}
