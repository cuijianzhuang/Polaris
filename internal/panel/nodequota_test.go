package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestNodeQuota: a node traffic pack is spent before the account quota and
// keeps that node usable after the account quota runs out; a node cap cuts
// the user off that node only, until the next traffic reset.
func TestNodeQuota(t *testing.T) {
	s, _, c := newAdminClient(t)
	db := s.st.DB
	for _, n := range []string{"n1", "n2"} {
		db.Create(&store.Node{ID: n, Name: n, Address: "203.0.113.9", TrafficRate: 1})
		db.Create(&store.Core{ID: "c" + n, NodeID: n, Type: model.CoreXray, Version: "26.3.27", InstalledVersions: store.NewJSON([]string{})})
		if code, out := c.do("POST", "/inbounds", map[string]any{"nodeId": n, "core": "xray", "tag": "vless", "port": 2443,
			"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "tcp"}, "security": map[string]any{"type": "none"}}); code != 201 {
			t.Fatalf("inbound %d %v", code, out)
		}
	}
	_, u := c.do("POST", "/users", map[string]any{"username": "alice", "trafficLimit": 100})
	uid := u["id"].(string)
	settle := func(node string, up, down int64) {
		t.Helper()
		s.traffic.Add(node, []model.TrafficDelta{{UserID: uid, Up: up, Down: down}})
		if err := s.settleTraffic(); err != nil {
			t.Fatal(err)
		}
	}
	user := func() store.User {
		var x store.User
		db.First(&x, "id = ?", uid)
		return x
	}
	quota := func(node string) store.UserNodeQuota {
		var q store.UserNodeQuota
		db.First(&q, "user_id = ? AND node_id = ?", uid, node)
		return q
	}
	served := func(node string) bool {
		t.Helper()
		spec, err := s.BuildSpec(node)
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range spec.Inbounds {
			for _, x := range in.Users {
				if x.ID == uid {
					return true
				}
			}
		}
		return false
	}

	// Pack on n1: spent first, the overflow goes to the account.
	if code, out := c.do("PUT", "/users/"+uid+"/node-quotas/n1", map[string]any{"extra": 50}); code != 200 {
		t.Fatalf("put %d %v", code, out)
	}
	settle("n1", 10, 20)
	if q, x := quota("n1"), user(); q.ExtraUsed != 30 || x.Used() != 0 {
		t.Fatalf("pack not used first: %+v used=%d", q, x.Used())
	}
	settle("n1", 0, 40)
	if q, x := quota("n1"), user(); q.ExtraUsed != 50 || x.Used() != 20 {
		t.Fatalf("overflow: %+v used=%d", q, x.Used())
	}

	// Account quota gone: only the node with a pack left still works.
	settle("n2", 0, 100)
	if user().Status != "limited" {
		t.Fatalf("status %s", user().Status)
	}
	if served("n1") || served("n2") {
		t.Fatal("limited user served without a pack")
	}
	c.do("PUT", "/users/"+uid+"/node-quotas/n1", map[string]any{"addExtra": 100})
	if !served("n1") || served("n2") {
		t.Fatal("pack should keep n1 (only) usable")
	}
	x := user()
	if nodes, _ := s.subNodes(&x); len(nodes) != 1 || !s.servedUser(&x) {
		t.Fatalf("subscription for a pack-only user: %d nodes", len(nodes))
	}

	// Cap on n2: past it, n2 drops out; a traffic reset lifts it.
	c.do("POST", "/users/"+uid+"/reset-traffic", nil)
	c.do("PUT", "/users/"+uid+"/node-quotas/n2", map[string]any{"limit": 10})
	if !served("n2") {
		t.Fatal("active user not served")
	}
	settle("n2", 5, 10)
	if !quota("n2").Capped() || served("n2") || !served("n1") {
		t.Fatalf("cap: %+v", quota("n2"))
	}
	x = user()
	if nodes, _ := s.subNodes(&x); len(nodes) != 1 {
		t.Fatalf("capped node still in the subscription: %d", len(nodes))
	}
	c.do("POST", "/users/"+uid+"/reset-traffic", nil)
	if quota("n2").Used != 0 || !served("n2") || quota("n1").ExtraUsed != 50 {
		t.Fatalf("reset: n2 %+v n1 %+v", quota("n2"), quota("n1"))
	}
	if code, _ := c.do("GET", "/users/"+uid+"/node-quotas", nil); code != 200 {
		t.Fatalf("list %d", code)
	}
	if code, _ := c.do("DELETE", "/users/"+uid+"/node-quotas/n2", nil); code != 204 {
		t.Fatalf("delete %d", code)
	}
}

// Large counters must split without int64 overflow, and the split must add up.
func TestChargeNodeQuotaLarge(t *testing.T) {
	const tb = int64(1) << 40
	q := &store.UserNodeQuota{Extra: 8 * tb}
	up, down := 5*tb, 4*tb
	cu, cd, _ := chargeNodeQuota(q, up, down)
	if cu < 0 || cd < 0 || cu > up || cd > down || (up-cu)+(down-cd) != 8*tb {
		t.Fatalf("split: cu=%d cd=%d", cu, cd)
	}
}
