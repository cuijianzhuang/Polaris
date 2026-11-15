package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

// buildForwards used to run one First per chain for the landing inbound and
// one per hop for the next node. The batched preload must keep the emitted
// rules identical — including the skip semantics for missing rows — while
// keeping the query count flat in the number of chains.
func TestBuildForwardsBatchedEquivalence(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "n0", Name: "entry"})
	s.st.DB.Create(&store.Node{ID: "n1", Name: "landing", Address: "203.0.113.1"})
	s.st.DB.Create(&store.Node{ID: "n2", Name: "landing2", Address: "203.0.113.2"})
	mkIn := func(id, node string, port int) {
		s.st.DB.Create(&store.Inbound{ID: id, NodeID: node, Tag: id, Port: port, Enabled: true})
	}
	mkIn("i1", "n1", 9001)
	mkIn("i2", "n2", 9002)
	chain := func(id string, hops []store.ChainHop) {
		s.st.DB.Create(&store.Chain{ID: id, Name: id, Mode: "l4", Hops: store.NewJSON(hops)})
	}
	// Missing landing inbound: the whole chain is skipped.
	chain("c-gone-in", []store.ChainHop{{NodeID: "n0", Port: 10001}, {NodeID: "n1", InboundID: "i-missing"}})
	// Missing next node: that hop is skipped (same rules as the per-hop First).
	chain("c-gone-node", []store.ChainHop{{NodeID: "n0", Port: 10002}, {NodeID: "n-deleted", InboundID: "i1"}})
	chain("c1", []store.ChainHop{{NodeID: "n0", Port: 10003}, {NodeID: "n1", InboundID: "i1"}})
	chain("c2", []store.ChainHop{{NodeID: "n0", Port: 10004}, {NodeID: "n2", InboundID: "i2"}})
	// Disabled and non-l4 chains never participate.
	s.st.DB.Create(&store.Chain{ID: "c-off", Name: "c-off", Mode: "l4", Disabled: true,
		Hops: store.NewJSON([]store.ChainHop{{NodeID: "n0", Port: 10005}, {NodeID: "n1", InboundID: "i1"}})})
	s.st.DB.Create(&store.Chain{ID: "c-proxy", Name: "c-proxy", Mode: "proxy",
		Hops: store.NewJSON([]store.ChainHop{{NodeID: "n0"}, {NodeID: "n1", InboundID: "i1"}})})

	fw, err := s.buildForwards("n0")
	if err != nil {
		t.Fatal(err)
	}
	if len(fw) != 2 {
		t.Fatalf("rules: %d, want 2 (missing rows and non-participating chains skipped): %+v", len(fw), fw)
	}
	want := map[string]string{
		"chain-c1-0": "203.0.113.1:9001",
		"chain-c2-0": "203.0.113.2:9002",
	}
	for _, r := range fw {
		if want[r.Tag] != r.Target {
			t.Errorf("%s: target %s, want %s", r.Tag, r.Target, want[r.Tag])
		}
		if r.Listen != 10003 && r.Listen != 10004 {
			t.Errorf("%s: unexpected listen port %d", r.Tag, r.Listen)
		}
	}

	var inboundReads, nodeReads int
	const cb = "test:buildforwards-reads"
	if err := s.st.DB.Callback().Query().Before("gorm:query").Register(cb, func(tx *gorm.DB) {
		switch tx.Statement.Table {
		case "inbounds":
			inboundReads++
		case "nodes":
			nodeReads++
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Query().Remove(cb) })
	if _, err := s.buildForwards("n0"); err != nil {
		t.Fatal(err)
	}
	s.st.DB.Callback().Query().Remove(cb)
	// Two base queries plus at most one batched read per table, regardless of
	// the four participating chains.
	if inboundReads != 1 || nodeReads != 1 {
		t.Fatalf("inbound reads=%d node reads=%d, want 1/1 batched reads", inboundReads, nodeReads)
	}
}

// A chain that passes through the same entry node twice emits one rule per
// on-node hop: intermediate hops forward to the next hop's listen port, the
// final hop to the landing inbound's port.
func TestBuildForwardsMultiHopSameEntry(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "n0", Name: "entry", Address: "198.51.100.1"})
	s.st.DB.Create(&store.Node{ID: "n2", Name: "landing", Address: "198.51.100.10"})
	s.st.DB.Create(&store.Inbound{ID: "land", NodeID: "n2", Tag: "land", Port: 8443, Enabled: true})
	s.st.DB.Create(&store.Chain{ID: "multi", Name: "multi", Mode: "l4",
		Hops: store.NewJSON([]store.ChainHop{
			{NodeID: "n0", Port: 20001},
			{NodeID: "n0", Port: 20002},
			{NodeID: "n2", InboundID: "land"},
		})})
	fw, err := s.buildForwards("n0")
	if err != nil {
		t.Fatal(err)
	}
	if len(fw) != 2 {
		t.Fatalf("rules: %d, want 2: %+v", len(fw), fw)
	}
	type rule struct {
		listen int
		target string
	}
	got := map[string]rule{}
	for _, r := range fw {
		got[r.Tag] = rule{r.Listen, r.Target}
	}
	// Hop 0 forwards to hop 1's listen port on n0 itself.
	if r := got["chain-multi-0"]; r.listen != 20001 || r.target != "198.51.100.1:20002" {
		t.Fatalf("chain-multi-0: %+v", r)
	}
	// Hop 1 is the last on-node hop: it forwards to the landing inbound port.
	if r := got["chain-multi-1"]; r.listen != 20002 || r.target != "198.51.100.10:8443" {
		t.Fatalf("chain-multi-1: %+v", r)
	}
}
