package panel

import (
	"fmt"
	"sync"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func nicSample(epoch string, seq, rx, tx uint64) *polarisv1.SystemStats {
	return &polarisv1.SystemStats{NetCountersValid: true, NetEpoch: epoch, NetSampleUptimeMillis: seq, RxBytes: rx, TxBytes: tx}
}
func TestNodeNetSamplesNeverReplayLifetimeCounters(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "net", Name: "net"})
	beat := func(p *polarisv1.SystemStats) {
		t.Helper()
		if e := s.accountNodeNet("net", p, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	load := func() store.Node { var n store.Node; s.st.DB.First(&n, "id = ?", "net"); return n }
	beat(nicSample("boot-interfaces", 100, 0, 0))
	beat(nicSample("boot-interfaces", 200, 10, 20))
	beat(nicSample("boot-interfaces", 200, 10, 20)) // duplicate
	beat(nicSample("boot-interfaces", 150, 5, 10))  // late heartbeat
	beat(nicSample("boot-interfaces", 300, 15, 30))
	n := load()
	if n.NetRx != 15 || n.NetTx != 30 {
		t.Fatal("duplicate or zero-baseline error", n.NetRx, n.NetTx)
	}
	beat(nicSample("new-interfaces", 400, 1<<40, 2<<40)) // adding a NIC with old lifetime counters
	beat(nicSample("new-interfaces", 500, 1<<40+5, 2<<40+10))
	beat(nicSample("new-boot", 10, 1000, 2000))
	beat(nicSample("new-boot", 20, 1010, 2020))
	// A decrease without an identity change is also a baseline, not a charge.
	beat(nicSample("new-boot", 30, 500, 600))
	beat(nicSample("new-boot", 40, 505, 610))
	n = load()
	if n.NetRx != 35 || n.NetTx != 70 {
		t.Fatal("source reset replayed history", n.NetRx, n.NetTx)
	}
	var d store.NodeTrafficDaily
	s.st.DB.First(&d, "node_id = ?", "net")
	if d.Rx != n.NetRx || d.Tx != n.NetTx {
		t.Fatal("daily and cycle diverged", d, n.NetRx, n.NetTx)
	}
}
func TestNodeNetLedgerAndBaselineCommitTogether(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "net", Name: "net"})
	if e := s.accountNodeNet("net", nicSample("boot", 100, 100, 200), time.Now()); e != nil {
		t.Fatal(e)
	}
	s.st.DB.Callback().Update().Before("gorm:update").Register("test:node-net", func(tx *gorm.DB) {
		if tx.Statement.Table == "nodes" {
			tx.AddError(fmt.Errorf("node write failed"))
		}
	})
	if e := s.accountNodeNet("net", nicSample("boot", 200, 110, 220), time.Now()); e == nil {
		t.Fatal("failed baseline write acknowledged")
	}
	s.st.DB.Callback().Update().Remove("test:node-net")
	if e := s.accountNodeNet("net", nicSample("boot", 200, 110, 220), time.Now()); e != nil {
		t.Fatal(e)
	}
	var n store.Node
	var d store.NodeTrafficDaily
	s.st.DB.First(&n, "id = ?", "net")
	s.st.DB.First(&d, "node_id = ?", "net")
	if n.NetRx != 10 || n.NetTx != 20 || d.Rx != 10 || d.Tx != 20 {
		t.Fatal("retry duplicated daily ledger", n.NetRx, n.NetTx, d)
	}
}
func TestNodeNetConcurrentHeartbeats(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		ss[0].st.DB.Create(&store.Node{ID: "net", Name: "net"})
		if e := ss[0].accountNodeNet("net", nicSample("boot", 100, 100, 200), time.Now()); e != nil {
			t.Fatal(e)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, s := range ss {
			wg.Add(1)
			go func(s *Server) {
				defer wg.Done()
				errs <- s.accountNodeNet("net", nicSample("boot", 200, 110, 220), time.Now())
			}(s)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
		var n store.Node
		var d store.NodeTrafficDaily
		ss[0].st.DB.First(&n, "id = ?", "net")
		ss[0].st.DB.First(&d, "node_id = ?", "net")
		if n.NetRx != 10 || n.NetTx != 20 || d.Rx != 10 || d.Tx != 20 {
			t.Fatal("concurrent heartbeat charged twice", n.NetRx, n.NetTx, d)
		}
	})
}
