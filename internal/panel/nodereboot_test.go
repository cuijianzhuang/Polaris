package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestNodeRebootEndpointGates: whole-host reboot is a full-administrator
// action. Anonymous callers get 401, a member (registered user) token
// gets 403 even for a node the member owns (the route is not in
// memberRoutes), and an admin asking for an offline node's reboot gets
// a readable 502 rather than the hub's raw timeout text.
func TestNodeRebootEndpointGates(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	u := memberUser(t, s, "reboot-user", "reboot@example.com")
	memberNode(t, s, "own-node", u.ID, "approved") // the member's own approved node
	memberNode(t, s, "admin-node", "", "")         // an administrator node, offline

	anon := &apiClient{t: t, base: admin.base}
	if code, out := anon.do("POST", "/nodes/admin-node/reboot", nil); code != 401 {
		t.Fatalf("anonymous reboot: %d %v, want 401", code, out)
	}

	mc := memberMailLogin(t, admin.base, m, u.Email, 1)
	if code, out := mc.do("POST", "/nodes/own-node/reboot", nil); code != 403 {
		t.Fatalf("member reboot of own node: %d %v, want 403", code, out)
	}
	if code, out := mc.do("POST", "/nodes/admin-node/reboot", nil); code != 403 {
		t.Fatalf("member reboot of admin node: %d %v, want 403", code, out)
	}

	code, out := admin.do("POST", "/nodes/admin-node/reboot", nil)
	if code != 502 {
		t.Fatalf("admin reboot of offline node: %d %v, want 502", code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "节点离线或未响应") {
		t.Fatalf("offline error = %q, want the readable offline message", msg)
	}
	if strings.Contains(msg, "did not answer") || strings.Contains(msg, "deadline") || strings.Contains(msg, "not connected") {
		t.Fatalf("offline error leaks hub internals: %q", msg)
	}
	// A refused reboot must not be audited as a scheduled one.
	var audits int64
	s.st.DB.Model(&store.AuditLog{}).Where("action = ?", "node.reboot").Count(&audits)
	if audits != 0 {
		t.Fatalf("node.reboot audit rows after refused reboots: %d, want 0", audits)
	}

	if code, out := admin.do("POST", "/nodes/no-such-node/reboot", nil); code != 404 {
		t.Fatalf("admin reboot of missing node: %d %v, want 404", code, out)
	}
}
