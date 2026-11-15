package panel

import (
	"sync/atomic"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestProbeOverviewReusesNodeAndTargetSnapshot(t *testing.T) {
	s := newTestServer(t)
	s.seedProbeTargets()
	if err := s.st.DB.Create(&store.Node{ID: "query-node", Name: "query-node", Status: "offline"}).Error; err != nil {
		t.Fatal(err)
	}
	var nodeReads, targetReads atomic.Int32
	const callback = "test:probe-overview-query-count"
	if err := s.st.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		switch tx.Statement.Table {
		case "nodes":
			nodeReads.Add(1)
		case "probe_targets":
			targetReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Query().Remove(callback) })
	for _, public := range []bool{false, true} {
		nodeReads.Store(0)
		targetReads.Store(0)
		out := s.probeOverview(public)
		if out == nil {
			t.Fatal("overview unavailable")
		}
		if nodes := out["nodes"].([]probeNode); len(nodes) != 1 || nodes[0].ID != "query-node" {
			t.Fatalf("unexpected nodes: %+v", nodes)
		}
		if n, targets := nodeReads.Load(), targetReads.Load(); n != 1 || targets != 1 {
			t.Fatalf("public=%v: node reads=%d, target reads=%d; want one each", public, n, targets)
		}
	}
}
