package panel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// fwdFixture: a pro panel with node n1 (public 203.0.113.9) that user u1
// reaches through plan p1, which grants two forwards.
func fwdFixture(t *testing.T) (*Server, *store.User) {
	t.Helper()
	s := newTestServer(t)
	s.lic.Store(&licenseState{st: license.State{Plan: "pro", Features: []string{license.FeaturePlanAddons}}})
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "hk", Address: "203.0.113.9", PublicIPv4: "203.0.113.9", TrafficRate: 1.5})
	db.Create(&store.Node{ID: "n2", Name: "jp", Address: "203.0.113.10"})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	db.Create(&store.Inbound{ID: "i1", NodeID: "n1", Tag: "in1", Port: 443, Enabled: true, Core: model.CoreXray,
		Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS})})
	ent := store.ForwardEntitlement{FwdMax: 2, FwdPortLo: 30000, FwdPortHi: 30100, FwdTraffic: 1000, FwdRate: 2, FwdSpeedMbps: 8}
	db.Create(&store.Group{ID: "p1", Name: "月付", Plan: true, ForwardEntitlement: ent})
	db.Create(&store.GroupInbound{GroupID: "p1", InboundID: "i1"})
	u := &store.User{ID: "u1", Username: "alice", UUID: "x", Password: "p", Status: "active", SubToken: "alice-token-000000000", PlanID: "p1", ForwardEntitlement: ent}
	db.Create(u)
	db.Create(&store.UserGroup{UserID: "u1", GroupID: "p1"})
	return s, u
}

func TestUserForwardRules(t *testing.T) {
	s, u := fwdFixture(t)
	ctx := context.Background()
	add := func(node, target string, port int) (*store.ForwardRule, error) {
		return s.createUserForward(ctx, u, userForwardReq{NodeID: node, Target: target, ListenPort: port, Network: "tcp"})
	}
	for _, bad := range []string{"10.0.0.1:80", "127.0.0.1:22", "169.254.169.254:80", "[::1]:80", "203.0.113.10:22", "0.0.0.0:1"} {
		if _, err := add("n1", bad, 0); !errors.Is(err, errFwdTarget) {
			t.Fatalf("target %s accepted: %v", bad, err)
		}
	}
	if _, err := add("n2", "1.1.1.1:443", 0); err == nil {
		t.Fatal("forward on a node outside the plan")
	}
	if _, err := add("n1", "1.1.1.1:443", 20000); err == nil || !strings.Contains(err.Error(), "30000") {
		t.Fatalf("port outside the range: %v", err)
	}
	r1, err := add("n1", "1.1.1.1:443", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r1.ListenPort < 30000 || r1.ListenPort > 30100 || r1.Engine != model.EngineGost || r1.UserID != "u1" {
		t.Fatalf("rule %+v", r1)
	}
	if _, err := add("n1", "8.8.8.8:53", r1.ListenPort); err == nil {
		t.Fatal("port taken twice")
	}
	if _, err := add("n1", "8.8.8.8:53", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := add("n1", "9.9.9.9:53", 0); err == nil || !strings.Contains(err.Error(), "最多 2") {
		t.Fatalf("third rule: %v", err)
	}

	// Delivered as GOST with the plan's speed cap.
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	var got *model.ForwardRule
	for i := range spec.Forwards {
		if spec.Forwards[i].Tag == "fwd-"+r1.ID {
			got = &spec.Forwards[i]
		}
	}
	if got == nil || got.Engine != model.EngineGost || got.RateIn != 1000 || got.RateOut != 1000 || got.Target != "1.1.1.1:443" {
		t.Fatalf("delivered %+v", got)
	}

	// Without the licence the rules are withheld, and none can be added.
	s.lic.Store(&licenseState{st: license.State{Plan: "community"}})
	if spec, _ := s.BuildSpec("n1"); len(spec.Forwards) != 0 {
		t.Fatalf("community edition still delivers %d user forwards", len(spec.Forwards))
	}
	if _, err := add("n1", "1.0.0.1:443", 0); err == nil {
		t.Fatal("added without the licence")
	}
}

// Forwarded traffic is charged to the owner's forwarding allowance (node
// rate x plan rate), not the proxy quota; using it up withholds the rules
// until the next period.
func TestUserForwardMetering(t *testing.T) {
	s, u := fwdFixture(t)
	r, err := s.createUserForward(context.Background(), u, userForwardReq{NodeID: "n1", Target: "1.1.1.1:443", Network: "tcp"})
	if err != nil {
		t.Fatal(err)
	}
	tag := "inbound>>>fwd-" + r.ID
	s.traffic.Add("n1", []model.TrafficDelta{{UserID: tag, Up: 100, Down: 100}})
	if err := s.settleTraffic(); err != nil {
		t.Fatal(err)
	}
	var cur store.User
	s.st.DB.First(&cur, "id = ?", "u1")
	if cur.FwdUsed != 600 || cur.UsedUp+cur.UsedDown != 0 || cur.FwdCapped { // 200 x 1.5 x 2
		t.Fatalf("after 200 B: fwd %d, proxy %d, capped %v", cur.FwdUsed, cur.UsedUp+cur.UsedDown, cur.FwdCapped)
	}
	s.traffic.Add("n1", []model.TrafficDelta{{UserID: tag, Up: 100, Down: 100}})
	s.settleTraffic()
	s.st.DB.First(&cur, "id = ?", "u1")
	if !cur.FwdCapped {
		t.Fatalf("allowance used (%d / %d) but not capped", cur.FwdUsed, cur.FwdTraffic)
	}
	if spec, _ := s.BuildSpec("n1"); len(spec.Forwards) != 0 {
		t.Fatal("capped user's forward still delivered")
	}
	// A new period brings it back.
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Updates(map[string]any{"reset_policy": "cycle", "reset_day": 1, "last_reset_at": cur.CreatedAt.AddDate(0, 0, -2)})
	s.resetTrafficCycles()
	s.st.DB.First(&cur, "id = ?", "u1")
	if cur.FwdUsed != 0 || cur.FwdCapped {
		t.Fatalf("after reset: %d %v", cur.FwdUsed, cur.FwdCapped)
	}
	if spec, _ := s.BuildSpec("n1"); len(spec.Forwards) != 1 {
		t.Fatal("forward not back after the reset")
	}
	// A disabled account takes its forwards down too.
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Update("status", "disabled")
	if spec, _ := s.BuildSpec("n1"); len(spec.Forwards) != 0 {
		t.Fatal("disabled user's forward delivered")
	}
}

// Buying the plan copies its forwarding terms; another user cannot touch
// the rules.
func TestUserForwardPlanAndIsolation(t *testing.T) {
	s, u := fwdFixture(t)
	var p store.Group
	s.st.DB.First(&p, "id = ?", "p1")
	p.FwdMax = 5
	other := &store.User{ID: "u2", Username: "bob", UUID: "y", Password: "q", Status: "active", SubToken: "bob-token-00000000000"}
	s.st.DB.Create(other)
	if _, err := applyPlan(s.st.DB, other, &p, false, other.CreatedAt); err != nil {
		t.Fatal(err)
	}
	var cur store.User
	s.st.DB.First(&cur, "id = ?", "u2")
	if cur.FwdMax != 5 || cur.FwdTraffic != 1000 || cur.FwdSpeedMbps != 8 {
		t.Fatalf("plan terms not copied: %+v", cur.ForwardEntitlement)
	}
	if v, err := readOrderTerms(&store.Order{Entitlements: freezePlan(&p)}); err != nil || v.Plan.FwdMax != 5 {
		t.Fatalf("order snapshot lost the forward terms: %+v %v", v.Plan, err)
	}
	r, err := s.createUserForward(context.Background(), u, userForwardReq{NodeID: "n1", Target: "1.1.1.1:443"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.deleteUserForward(&cur, r.ID); err == nil {
		t.Fatal("another user deleted the rule")
	}
	if err := s.deleteUserForward(u, r.ID); err != nil {
		t.Fatal(err)
	}
}

// The user-center page renders the list, the quota line and the form.
func TestPortalForwardsPage(t *testing.T) {
	var buf []byte
	err := portalFwdTpl.Execute(&bytesWriter{b: &buf}, map[string]any{"Title": "P", "Rows": []portalFwdRow{{ID: "r1", Name: "a-30001", Node: "hk", Entry: "203.0.113.9:30001", Target: "1.1.1.1:443", Network: "tcp"}},
		"Nodes": []portalFwdNode{{ID: "n1", Name: "hk"}}, "Max": 2, "Count": 1, "Lo": 30000, "Hi": 30100, "Used": "1.0 MB", "Quota": "10.00 GB", "State": "", "Speed": 8, "CanAdd": true})
	html := string(buf)
	for _, want := range []string{"203.0.113.9:30001", "1.1.1.1:443", "已用 1 / 2 条", "每条限速 8 Mbps", `id="add"`, `data-id="r1"`, "X-Polaris-Portal"} {
		if err != nil || !strings.Contains(html, want) {
			t.Fatalf("missing %q (%v)", want, err)
		}
	}
}
