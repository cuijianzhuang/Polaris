package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Agent upgrades used to run inside the HTTP handler: the request blocked
// until every node had downloaded the new binary and restarted, which takes
// minutes, so the reverse proxy killed the connection and the browser
// reported a network failure ("Load failed"). The handlers now start a
// rollout and return it at once; the rollout runs in the background.

func TestUpgradeAgentAPICreatesRollout(t *testing.T) {
	s, _, admin := newAdminClient(t)
	ids := rolloutNodesFixture(t, s, 2)
	(&fakeFleet{offline: map[string]bool{ids[1]: true}}).install(t, s)

	code, out := admin.do("POST", "/nodes/"+ids[0]+"/upgrade-agent", nil)
	if code != 200 {
		t.Fatalf("upgrade-agent %d %v", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" || out["kind"] != "agent" {
		t.Fatalf("expected a rollout, got %v", out)
	}
	var r store.UpgradeRollout
	if err := s.st.DB.First(&r, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if len(r.NodeIDs.V) != 1 || r.NodeIDs.V[0] != ids[0] {
		t.Fatalf("rollout nodes %v", r.NodeIDs.V)
	}
	// The rollout drives the upgrade to completion in the background.
	waitUntil(t, "rollout done", func() bool {
		stepRolloutT(t, s, id)
		s.st.DB.First(&r, "id = ?", id)
		return r.State == "done"
	})
	var n store.Node
	s.st.DB.First(&n, "id = ?", ids[0])
	if n.AgentVersion != "2.0.0" {
		t.Fatalf("node still on %s", n.AgentVersion)
	}
}

func TestUpgradeAllAgentsCreatesRollout(t *testing.T) {
	s, _, admin := newAdminClient(t)
	ids := rolloutNodesFixture(t, s, 3)
	(&fakeFleet{offline: map[string]bool{ids[2]: true}}).install(t, s)

	code, out := admin.do("POST", "/nodes/upgrade-agents", nil)
	if code != 200 {
		t.Fatalf("upgrade-agents %d %v", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" || out["kind"] != "agent" {
		t.Fatalf("expected a rollout, got %v", out)
	}
	if nodes, _ := out["nodeIds"].([]any); len(nodes) != 3 {
		t.Fatalf("expected 3 nodes in the rollout, got %v", out["nodeIds"])
	}
	var r store.UpgradeRollout
	waitUntil(t, "rollout done", func() bool {
		stepRolloutT(t, s, id)
		s.st.DB.First(&r, "id = ?", id)
		return r.State == "done"
	})
	if r.Results.V[ids[2]].State != "skipped" {
		t.Fatalf("offline node not skipped: %+v", r.Results.V)
	}
	for _, want := range ids[:2] {
		var n store.Node
		s.st.DB.First(&n, "id = ?", want)
		if n.AgentVersion != "2.0.0" {
			t.Fatalf("node %s still on %s", want, n.AgentVersion)
		}
	}
}

func TestUpgradeAllAgentsNoOutdated(t *testing.T) {
	s, _, admin := newAdminClient(t)
	(&fakeFleet{}).install(t, s)
	for _, id := range []string{"x-node", "y-node"} {
		s.st.DB.Create(&store.Node{ID: id, Name: id, AgentVersion: "2.0.0", Status: "online"})
	}
	code, out := admin.do("POST", "/nodes/upgrade-agents", nil)
	if code != 200 {
		t.Fatalf("upgrade-agents %d %v", code, out)
	}
	if _, hasID := out["id"]; hasID {
		t.Fatalf("no rollout expected, got %v", out)
	}
	if res, _ := out["results"].([]any); len(res) != 0 {
		t.Fatalf("expected empty results, got %v", out)
	}
	var count int64
	s.st.DB.Model(&store.UpgradeRollout{}).Count(&count)
	if count != 0 {
		t.Fatalf("no rollout expected, found %d", count)
	}
}
