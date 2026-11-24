package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Batch-2 acceptance tests (docs/design/panel-member-login.md §13):
// the member write surface. The semantics under test are the portal's —
// the panel handlers run the portal's own flows — so several tests run
// the same operation through both surfaces and compare the result.

// memberCreateNodeViaPanel creates a node as the member and returns its id.
func memberCreateNodeViaPanel(t *testing.T, mc *apiClient, name string) (int, map[string]any) {
	t.Helper()
	return mc.do("POST", "/nodes", map[string]any{"displayName": name, "address": "203.0.113.10"})
}

func TestMemberWriteNodes(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	licenseNone(s) // community: per-user cap is 3
	u := memberUser(t, s, "w-nodes", "w-nodes@example.com")
	mc := memberMailLogin(t, admin.base, m, u.Email, 1)

	// Create: 201 with the install command; approval mode ⇒ pending,
	// internal name un-<id>, display name as given.
	code, out := memberCreateNodeViaPanel(t, mc, "我的小鸡")
	if code != http.StatusCreated {
		t.Fatalf("member create node: %d %v", code, out)
	}
	if out["token"] == "" || out["command"] == "" {
		t.Fatalf("create response lacks token/command: %v", out)
	}
	id := portalNodeID(t, out)
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if n.OwnerID != u.ID || n.ReviewState != "pending" || n.Name != "un-"+id || n.DisplayName != "我的小鸡" {
		t.Fatalf("created node: %+v", n)
	}
	// The write is audited under the member actor.
	var al store.AuditLog
	if err := s.st.DB.Where("action = ?", "node.create").Order("created_at desc").First(&al).Error; err != nil {
		t.Fatal(err)
	}
	if al.Admin != memberActor(&u) {
		t.Fatalf("audit actor: %q (want %q)", al.Admin, memberActor(&u))
	}

	// Rename through the display-field patch.
	if code, out := mc.do("PUT", "/nodes/"+id, map[string]any{"displayName": "改名了", "region": "香港"}); code != http.StatusOK {
		t.Fatalf("member rename: %d %v", code, out)
	}
	if err := s.st.DB.First(&n, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if n.DisplayName != "改名了" || n.Region != "香港" {
		t.Fatalf("renamed node: %+v", n)
	}
	// Management fields in the patch body are not part of the patch
	// struct: a smuggled traffic rate / relay flag changes nothing.
	if code, _ := mc.do("PUT", "/nodes/"+id, map[string]any{"trafficRate": 99, "relayOnly": true}); code != http.StatusOK {
		t.Fatalf("member patch with junk fields: %d", code)
	}
	if err := s.st.DB.First(&n, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if n.TrafficRate == 99 {
		t.Fatalf("traffic rate smuggled through member patch")
	}

	// Re-issue the install token while unenrolled.
	if code, out := mc.do("POST", "/nodes/"+id+"/install-token", nil); code != http.StatusOK || out["token"] == "" {
		t.Fatalf("member install token: %d %v", code, out)
	}

	// Quota: two more fit under the community cap of 3, the fourth is 402.
	for _, name := range []string{"n2", "n3"} {
		if code, out := memberCreateNodeViaPanel(t, mc, name); code != http.StatusCreated {
			t.Fatalf("create %s: %d %v", name, code, out)
		}
	}
	if code, out := memberCreateNodeViaPanel(t, mc, "n4"); code != http.StatusPaymentRequired {
		t.Fatalf("create over quota: %d (want 402) %v", code, out)
	}

	// Mode off closes the member surface entirely (design D6): the
	// per-request gate in authMember refuses the session itself, so a
	// create attempt never even reaches the handler.
	s.st.SetSetting("user_nodes_mode", "off")
	if code, out := memberCreateNodeViaPanel(t, mc, "x"); code != http.StatusUnauthorized {
		t.Fatalf("create while off: %d (want 401, member gate shut) %v", code, out)
	}
}

func TestMemberWriteInboundLifecycle(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	u := memberUser(t, s, "w-inb", "w-inb@example.com")
	other := memberUser(t, s, "w-inb-other", "w-inb-other@example.com")
	mc := memberMailLogin(t, admin.base, m, u.Email, 1)

	_, out := memberCreateNodeViaPanel(t, mc, "inb-node")
	nodeID := portalNodeID(t, out)

	// Pending node: inbound create is the portal's 403.
	body := vlessInboundBody("early", 24443)
	body["nodeId"] = nodeID
	if code, out := mc.do("POST", "/inbounds", body); code != http.StatusForbidden {
		t.Fatalf("inbound on pending node: %d (want 403) %v", code, out)
	}

	approveUserNode(t, admin, nodeID)

	// Approved: create works, binds exactly the owner, no group rows.
	code, out := mc.do("POST", "/inbounds", body)
	if code != http.StatusCreated {
		t.Fatalf("member create inbound: %d %v", code, out)
	}
	inb, _ := out["inbound"].(map[string]any)
	inbID, _ := inb["id"].(string)
	if inbID == "" {
		t.Fatalf("no inbound id: %v", out)
	}
	if inb["nodeName"] != "inb-node" {
		t.Fatalf("inbound nodeName leaks internal name: %v", inb["nodeName"])
	}
	var grants int64
	s.st.DB.Model(&store.UserAccess{}).Where("kind = ? AND target_id = ?", "inbound", inbID).Count(&grants)
	if grants != 1 {
		t.Fatalf("owner grants on member inbound: %d (want 1)", grants)
	}
	var groupRows int64
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", inbID).Count(&groupRows)
	if groupRows != 0 {
		t.Fatalf("group rows on member inbound: %d (want 0)", groupRows)
	}
	if got := grantsFor(t, s, u.ID, inbID); got != 1 {
		t.Fatalf("GrantsSQL for owner: %d (want 1)", got)
	}
	if got := grantsFor(t, s, other.ID, inbID); got != 0 {
		t.Fatalf("GrantsSQL for other user: %d (want 0)", got)
	}

	// List and read show only the member's own inbounds.
	if code, raw := staffRaw(t, mc, "/inbounds"); code != http.StatusOK || !strings.Contains(raw, inbID) {
		t.Fatalf("member list inbounds: %d %s", code, raw)
	}
	if code, out := mc.do("GET", "/inbounds/"+inbID, nil); code != http.StatusOK {
		t.Fatalf("member get inbound: %d %v", code, out)
	}

	// Update the remark, then delete; the grant goes with the row.
	if code, out := mc.do("PUT", "/inbounds/"+inbID, vlessInboundBody("early", 24443)); code != http.StatusOK {
		t.Fatalf("member update inbound: %d %v", code, out)
	}
	if code, _ := mc.do("DELETE", "/inbounds/"+inbID, nil); code != http.StatusNoContent {
		t.Fatalf("member delete inbound: %d", code)
	}
	s.st.DB.Model(&store.UserAccess{}).Where("kind = ? AND target_id = ?", "inbound", inbID).Count(&grants)
	if grants != 0 {
		t.Fatalf("grant survived inbound delete: %d", grants)
	}
}

func TestMemberWriteForeignTargets(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	victim := memberUser(t, s, "w-victim", "w-victim@example.com")
	attacker := memberUser(t, s, "w-attacker", "w-attacker@example.com")
	vc := memberMailLogin(t, admin.base, m, victim.Email, 1)
	ac := memberMailLogin(t, admin.base, m, attacker.Email, 1)

	_, out := memberCreateNodeViaPanel(t, vc, "victim-node")
	victimNode := portalNodeID(t, out)
	approveUserNode(t, admin, victimNode)
	body := vlessInboundBody("victim-inb", 25443)
	body["nodeId"] = victimNode
	code, out := vc.do("POST", "/inbounds", body)
	if code != http.StatusCreated {
		t.Fatalf("victim create inbound: %d %v", code, out)
	}
	victimInb, _ := out["inbound"].(map[string]any)["id"].(string)

	// Every write/read class against someone else's node or inbound is
	// a 404 — existence must not leak (the inbound list's ?node= filter
	// keeps the batch-1 scope 403).
	for _, x := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/nodes/" + victimNode, map[string]any{"displayName": "pwned"}},
		{"DELETE", "/nodes/" + victimNode, nil},
		{"POST", "/nodes/" + victimNode + "/install-token", nil},
		{"GET", "/inbounds/" + victimInb, nil},
		{"PUT", "/inbounds/" + victimInb, vlessInboundBody("pwned", 25444)},
		{"DELETE", "/inbounds/" + victimInb, nil},
		{"POST", "/inbounds", func() map[string]any { b := vlessInboundBody("sneak", 25445); b["nodeId"] = victimNode; return b }()},
	} {
		if code, out := ac.do(x.method, x.path, x.body); code != http.StatusNotFound {
			t.Fatalf("attacker %s %s: %d (want 404) %v", x.method, x.path, code, out)
		}
	}
	if code, out := ac.do("GET", "/inbounds?node="+victimNode, nil); code != http.StatusForbidden {
		t.Fatalf("attacker list with foreign ?node=: %d (want 403) %v", code, out)
	}
	// The victim's resources are untouched.
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", victimNode).Error; err != nil || n.DisplayName != "victim-node" {
		t.Fatalf("victim node after attacks: %+v err=%v", n, err)
	}
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", victimInb).Error; err != nil {
		t.Fatalf("victim inbound deleted by attacker: %v", err)
	}
}

func TestMemberInboundSanitisation(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true") // the same user also drives the portal surface below
	u := memberUser(t, s, "w-san", "w-san@example.com")
	mc := memberMailLogin(t, admin.base, m, u.Email, 1)
	token, err := s.issuePortalMailToken(&u)
	if err != nil {
		t.Fatal(err)
	}
	pc := portalCookieClient(t, strings.TrimSuffix(admin.base, s.cfg.APIBase()), token)

	_, out := memberCreateNodeViaPanel(t, mc, "san-node")
	nodeID := portalNodeID(t, out)
	approveUserNode(t, admin, nodeID)

	// An existing certificate (as an admin's inbound would hold) and
	// another inbound to point fallbacks at.
	cert := store.Certificate{ID: "cert-foreign", Domain: "foreign.example.com", Provider: "manual"}
	if err := s.st.DB.Create(&cert).Error; err != nil {
		t.Fatal(err)
	}
	decoy := vlessInboundBody("decoy", 26443)
	decoy["nodeId"] = nodeID
	code, out := mc.do("POST", "/inbounds", decoy)
	if code != http.StatusCreated {
		t.Fatalf("decoy inbound: %d %v", code, out)
	}
	decoyID, _ := out["inbound"].(map[string]any)["id"].(string)

	inject := func(tag string, port int) map[string]any {
		b := vlessInboundBody(tag, port)
		b["nodeId"] = nodeID
		b["groupIds"] = []string{"g1"}
		sec := b["security"].(map[string]any)
		sec["type"] = "tls"
		sec["certificateId"] = "cert-foreign"
		b["protocol"] = map[string]any{"type": "vless", "fallbacks": []map[string]any{{"inboundId": decoyID}}}
		return b
	}
	load := func(id string) store.Inbound {
		t.Helper()
		var row store.Inbound
		if err := s.st.DB.First(&row, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		return row
	}

	// Through the panel…
	code, out = mc.do("POST", "/inbounds", inject("injected", 26444))
	if code != http.StatusCreated {
		t.Fatalf("member injected create: %d %v", code, out)
	}
	panelRow := load(out["inbound"].(map[string]any)["id"].(string))
	// …and through the portal, the same injections must land identically.
	code, body := pc.req("POST", "/u/my-nodes/"+nodeID+"/inbounds", inject("injected-p", 26445), true)
	if code != http.StatusCreated {
		t.Fatalf("portal injected create: %d %s", code, body)
	}
	var pout map[string]any
	if err := json.Unmarshal([]byte(body), &pout); err != nil {
		t.Fatal(err)
	}
	portalRow := load(pout["inbound"].(map[string]any)["id"].(string))

	for name, row := range map[string]store.Inbound{"panel": panelRow, "portal": portalRow} {
		if row.Security.V.CertificateID == "cert-foreign" {
			t.Fatalf("%s: foreign certificate reference survived sanitising", name)
		}
		if len(row.Protocol.V.Fallbacks) != 0 {
			t.Fatalf("%s: fallbacks survived sanitising: %+v", name, row.Protocol.V.Fallbacks)
		}
		var groupRows int64
		s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", row.ID).Count(&groupRows)
		if groupRows != 0 {
			t.Fatalf("%s: groupIds produced group rows", name)
		}
	}
	// Both surfaces minted their own self-signed certificate (the IDs
	// necessarily differ); what must match is that neither kept the
	// injected reference — asserted in the loop above.
	_ = panelRow
	_ = portalRow
}

func TestMemberDeleteNodeCascade(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	u := memberUser(t, s, "w-del", "w-del@example.com")
	mc := memberMailLogin(t, admin.base, m, u.Email, 1)

	_, out := memberCreateNodeViaPanel(t, mc, "del-node")
	nodeID := portalNodeID(t, out)
	approveUserNode(t, admin, nodeID)
	body := vlessInboundBody("del-inb", 27443)
	body["nodeId"] = nodeID
	code, out := mc.do("POST", "/inbounds", body)
	if code != http.StatusCreated {
		t.Fatalf("create inbound: %d %v", code, out)
	}
	inbID, _ := out["inbound"].(map[string]any)["id"].(string)

	if code, _ := mc.do("DELETE", "/nodes/"+nodeID, nil); code != http.StatusNoContent {
		t.Fatalf("member delete node: %d", code)
	}
	counts := map[string]int64{}
	for table, model := range map[string]any{
		"nodes": &store.Node{}, "inbounds": &store.Inbound{}, "cores": &store.Core{},
	} {
		var n int64
		q := s.st.DB.Model(model)
		if table == "nodes" {
			q = q.Where("id = ?", nodeID)
		} else {
			q = q.Where("node_id = ?", nodeID)
		}
		q.Count(&n)
		counts[table] = n
	}
	var grants int64
	s.st.DB.Model(&store.UserAccess{}).Where("kind = ? AND target_id = ?", "inbound", inbID).Count(&grants)
	counts["grants"] = grants
	for k, n := range counts {
		if n != 0 {
			t.Fatalf("cascade left %s rows: %d", k, n)
		}
	}
}
