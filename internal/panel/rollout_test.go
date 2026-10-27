package panel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// fakeFleet stands in for agents: who is online, and what an agent upgrade
// does to the node's reported version.
type fakeFleet struct {
	offline  map[string]bool
	failNode string // agent upgrade of this node errors
	stuck    string // this node never reports the new version
	mu       sync.Mutex
	upgraded []string
}

func (f *fakeFleet) install(t *testing.T, s *Server) {
	t.Helper()
	oldOnline, oldUp, oldVer := rolloutOnline, rolloutUpgradeAgent, rolloutAgentVersion
	rolloutOnline = func(_ *Server, id string) bool { return !f.offline[id] }
	rolloutAgentVersion = func() string { return "2.0.0" }
	rolloutUpgradeAgent = func(s *Server, _ context.Context, n *store.Node) error {
		f.mu.Lock()
		f.upgraded = append(f.upgraded, n.ID)
		f.mu.Unlock()
		if n.ID == f.failNode {
			return errors.New("checksum mismatch")
		}
		if n.ID != f.stuck {
			s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Update("agent_version", "2.0.0")
		}
		return nil
	}
	t.Cleanup(func() { rolloutOnline, rolloutUpgradeAgent, rolloutAgentVersion = oldOnline, oldUp, oldVer })
}

func rolloutNodesFixture(t *testing.T, s *Server, n int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		id := string(rune('a'+i)) + "-node"
		s.st.DB.Create(&store.Node{ID: id, Name: id, AgentVersion: "1.0.0", Status: "online"})
		ids = append(ids, id)
	}
	return ids
}

// step advances the rollout as the leader's tick would, skipping the soak
// wait by moving the timestamps back.
func stepRolloutT(t *testing.T, s *Server, id string) store.UpgradeRollout {
	t.Helper()
	s.runRollouts()
	var r store.UpgradeRollout
	s.st.DB.First(&r, "id = ?", id)
	if r.HealthySince != nil {
		past := r.HealthySince.Add(-time.Hour)
		s.st.DB.Model(&store.UpgradeRollout{}).Where("id = ?", id).Update("healthy_since", past)
	}
	return r
}

func TestRolloutAgentStages(t *testing.T) {
	s := newTestServer(t)
	ids := rolloutNodesFixture(t, s, 5)
	f := &fakeFleet{offline: map[string]bool{ids[3]: true}}
	f.install(t, s)
	r, err := s.newRollout(rolloutReq{Kind: "agent", Canary: 1, BatchSize: 2, SoakSeconds: 60}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// Canary: only the first node is touched until it has soaked.
	cur := stepRolloutT(t, s, r.ID)
	if len(f.upgraded) != 1 || cur.Cursor != 1 || len(cur.Batch.V) != 1 {
		t.Fatalf("canary: upgraded %v, cursor %d, batch %v", f.upgraded, cur.Cursor, cur.Batch.V)
	}
	stepRolloutT(t, s, r.ID) // healthy: soak starts
	if len(f.upgraded) != 1 {
		t.Fatalf("next batch started before the canary soaked: %v", f.upgraded)
	}
	for i := 0; i < 10; i++ {
		cur = stepRolloutT(t, s, r.ID)
		if cur.State != "running" {
			break
		}
	}
	if cur.State != "done" {
		t.Fatalf("rollout state %s: %s", cur.State, cur.LastError)
	}
	if cur.Results.V[ids[3]].State != "skipped" || cur.Results.V[ids[4]].State != "ok" || cur.Results.V[ids[0]].State != "ok" {
		t.Fatalf("results %+v", cur.Results.V)
	}
	if len(f.upgraded) != 4 {
		t.Fatalf("upgraded %v (the offline node must be skipped)", f.upgraded)
	}
}

func TestRolloutCanaryFailureStops(t *testing.T) {
	s := newTestServer(t)
	ids := rolloutNodesFixture(t, s, 4)
	f := &fakeFleet{stuck: ids[0]}
	f.install(t, s)
	r, _ := s.newRollout(rolloutReq{Kind: "agent", Canary: 1, BatchSize: 3}, "admin")
	stepRolloutT(t, s, r.ID)
	// The canary never comes back on the new version: past the timeout the
	// rollout stops and nobody else is touched.
	past := time.Now().Add(-rolloutNodeTimeout - time.Minute)
	s.st.DB.Model(&store.UpgradeRollout{}).Where("id = ?", r.ID).Update("batch_started", past)
	cur := stepRolloutT(t, s, r.ID)
	if cur.State != "failed" || !strings.Contains(cur.LastError, ids[0]) || len(f.upgraded) != 1 {
		t.Fatalf("state %s err %q upgraded %v", cur.State, cur.LastError, f.upgraded)
	}

	// An upgrade command that errors stops the rollout at once.
	s.st.DB.Model(&store.UpgradeRollout{}).Where("id = ?", r.ID).Update("state", "canceled")
	f2 := &fakeFleet{failNode: ids[1]}
	f2.install(t, s)
	r2, err := s.newRollout(rolloutReq{Kind: "agent", NodeIDs: []string{ids[1], ids[2]}, Canary: 1}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	cur = stepRolloutT(t, s, r2.ID)
	if cur.State != "failed" || !strings.Contains(cur.LastError, "checksum") || len(f2.upgraded) != 1 {
		t.Fatalf("errored canary: %s %q %v", cur.State, cur.LastError, f2.upgraded)
	}
}

func TestRolloutCanarySkipsUnavailableNodes(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(fmt.Sprintf("persisted_cursor_%t", resumed), func(t *testing.T) {
			s := newTestServer(t)
			ids := rolloutNodesFixture(t, s, 6)
			f := &fakeFleet{offline: map[string]bool{ids[0]: true, ids[2]: true}}
			f.install(t, s)
			r, err := s.newRollout(rolloutReq{Kind: "agent", Canary: 2, BatchSize: 4, SoakSeconds: 60}, "admin")
			if err != nil {
				t.Fatal(err)
			}
			if resumed {
				// A pre-fix worker persisted a skipped first canary, then stopped.
				r.Results.V[ids[0]] = store.RolloutNode{State: "skipped"}
				s.st.DB.Model(r).Updates(map[string]any{"cursor": 1, "results": r.Results})
			}
			cur := stepRolloutT(t, s, r.ID)
			if len(f.upgraded) != 2 || cur.Cursor != 4 || len(cur.Batch.V) != 2 || cur.Results.V[ids[1]].State != "upgrading" || cur.Results.V[ids[3]].State != "upgrading" {
				t.Fatalf("canary not filled with online nodes: %+v upgrades=%v", cur, f.upgraded)
			}
			// Reload on every tick: starting an upgrade is not proof of a soak.
			cur = stepRolloutT(t, s, r.ID)
			if len(f.upgraded) != 2 || cur.HealthySince == nil {
				t.Fatalf("advanced before soak: %+v upgrades=%v", cur, f.upgraded)
			}
			stepRolloutT(t, s, r.ID) // confirm the canary after its observation period
			stepRolloutT(t, s, r.ID) // now the ordinary batch may start
			if len(f.upgraded) != 4 {
				t.Fatalf("ordinary batch did not follow verified canary: %v", f.upgraded)
			}
		})
	}
}

func TestRolloutUnavailableCanaryPausesUntilResume(t *testing.T) {
	s := newTestServer(t)
	ids := rolloutNodesFixture(t, s, 3)
	f := &fakeFleet{offline: map[string]bool{ids[0]: true, ids[1]: true, ids[2]: true}}
	f.install(t, s)
	r, err := s.newRollout(rolloutReq{Kind: "agent", Canary: 1, BatchSize: 3}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	cur := stepRolloutT(t, s, r.ID)
	if cur.State != "paused" || cur.Cursor != 0 || cur.LastError == "" || len(f.upgraded) != 0 {
		t.Fatalf("unavailable canary should pause without consuming nodes: %+v", cur)
	}
	f.offline[ids[1]] = false
	stepRolloutT(t, s, r.ID)
	if len(f.upgraded) != 0 {
		t.Fatal("paused rollout resumed without an admin action")
	}
	s.st.DB.Model(r).Updates(map[string]any{"state": "running", "control_version": gorm.Expr("control_version + 1")})
	cur = stepRolloutT(t, s, r.ID)
	if len(f.upgraded) != 1 || f.upgraded[0] != ids[1] || len(cur.Batch.V) != 1 || cur.LastError != "" {
		t.Fatalf("resume did not retry canary selection: %+v upgrades=%v", cur, f.upgraded)
	}
}

// A core batch where one node's new version fails to start is stopped and
// the other nodes of the batch go back to their previous version.
func TestRolloutCoreRollback(t *testing.T) {
	s := newTestServer(t)
	ids := rolloutNodesFixture(t, s, 3)
	for _, id := range ids {
		s.st.DB.Create(&store.Core{ID: "c-" + id, NodeID: id, Type: model.CoreXray, Version: "26.1.1", RunningVersion: "26.1.1", Status: "running"})
	}
	(&fakeFleet{}).install(t, s)
	r, err := s.newRollout(rolloutReq{Kind: "core", CoreType: string(model.CoreXray), Version: "26.3.27", Canary: 2, BatchSize: 2}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	cur := stepRolloutT(t, s, r.ID)
	if len(cur.Batch.V) != 2 {
		t.Fatalf("batch %v", cur.Batch.V)
	}
	// Node 0 runs the new version; node 1's agent reverted it (as
	// revertFailedUpgrades records).
	s.st.DB.Model(&store.Core{}).Where("node_id = ?", ids[0]).Update("running_version", "26.3.27")
	s.st.DB.Model(&store.Core{}).Where("node_id = ?", ids[1]).Updates(map[string]any{"version": "26.1.1", "error": "升级到 26.3.27 失败，仍运行 26.1.1：boom"})
	cur = stepRolloutT(t, s, r.ID)
	if cur.State != "rolling_back" || !strings.Contains(cur.LastError, "boom") {
		t.Fatalf("state %s err %q", cur.State, cur.LastError)
	}
	var c0, c2 store.Core
	s.st.DB.First(&c0, "node_id = ?", ids[0])
	s.st.DB.First(&c2, "node_id = ?", ids[2])
	if c0.Version != "26.1.1" || cur.Results.V[ids[0]].State != "rolling_back" {
		t.Fatalf("healthy node of the failed batch not rolled back: %s %+v", c0.Version, cur.Results.V[ids[0]])
	}
	if c2.Version != "26.1.1" {
		t.Fatalf("node of a later batch was touched: %s", c2.Version)
	}

	// Desired version alone is insufficient: the observed core still runs new.
	cur = stepRolloutT(t, s, r.ID)
	if cur.State != "rolling_back" {
		t.Fatalf("unconfirmed rollback completed: %s", cur.State)
	}
	s.st.DB.Model(&store.Core{}).Where("node_id IN ?", ids[:2]).Updates(map[string]any{"running_version": "26.1.1", "status": "running"})
	for _, id := range ids[:2] {
		s.st.DB.Model(&store.Node{}).Where("id = ?", id).Update("applied_rev", 999)
	}
	cur = stepRolloutT(t, s, r.ID)
	if cur.State != "failed" || cur.Results.V[ids[0]].State != "rolled_back" {
		t.Fatalf("rollback not confirmed: %+v", cur)
	}
}

func TestRolloutPauseResumeAPI(t *testing.T) {
	s, _, admin := newAdminClient(t)
	ids := rolloutNodesFixture(t, s, 2)
	(&fakeFleet{}).install(t, s)
	code, out := admin.do("POST", "/rollouts", map[string]any{"kind": "agent", "canary": 1, "batchSize": 1})
	if code != 200 {
		t.Fatalf("create %d %v", code, out)
	}
	id := out["id"].(string)
	if code, _ := admin.do("POST", "/rollouts", map[string]any{"kind": "agent"}); code != 400 {
		t.Fatal("a second rollout started while one runs")
	}
	waitUntil(t, "canary started", func() bool {
		var r store.UpgradeRollout
		s.st.DB.First(&r, "id = ?", id)
		return r.Cursor == 1 && r.LeaseToken == ""
	})
	if code, _ := admin.do("POST", "/rollouts/"+id+"/pause", nil); code != 204 {
		t.Fatal("pause", code)
	}
	for i := 0; i < 3; i++ {
		stepRolloutT(t, s, id)
	}
	var r store.UpgradeRollout
	s.st.DB.First(&r, "id = ?", id)
	if r.State != "paused" || r.Cursor != 1 {
		t.Fatalf("paused rollout moved on: %s cursor %d", r.State, r.Cursor)
	}
	if code, _ := admin.do("POST", "/rollouts/"+id+"/resume", nil); code != 204 {
		t.Fatal("resume", code)
	}
	waitUntil(t, "rollout done", func() bool {
		stepRolloutT(t, s, id)
		s.st.DB.First(&r, "id = ?", id)
		return r.State == "done"
	})
	if r.Results.V[ids[1]].State != "ok" {
		t.Fatalf("results %+v", r.Results.V)
	}
	if code, _ := admin.do("POST", "/rollouts/"+id+"/cancel", nil); code != 400 {
		t.Fatal("finished rollout canceled")
	}
}

func TestRolloutRejectsUnconfirmedAgentTrial(t *testing.T) {
	s, _, _ := newAdminClient(t)
	fleet := &fakeFleet{offline: map[string]bool{}}
	fleet.install(t, s)
	ids := rolloutNodesFixture(t, s, 1)
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", ids[0]).Update("agent_version", "2.0.0-trial").Error; err != nil {
		t.Fatal(err)
	}
	r := &store.UpgradeRollout{Kind: "agent", Version: "2.0.0"}
	if ok, _, _ := s.rolloutNodeHealthy(r, ids[0]); ok {
		t.Fatal("candidate accepted before local startup confirmation")
	}
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", ids[0]).Update("agent_version", "2.0.0").Error; err != nil {
		t.Fatal(err)
	}
	if ok, reason, _ := s.rolloutNodeHealthy(r, ids[0]); !ok {
		t.Fatal("confirmed version rejected", reason)
	}
}
