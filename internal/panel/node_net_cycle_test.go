package panel

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/provider"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestNodeNetRolloverBeforeFirstHeartbeat(t *testing.T) {
	s := newTestServer(t)
	loc := time.FixedZone("test", 8*60*60)
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	now := time.Date(2026, 11, 1, 0, 0, 5, 0, loc)
	n := store.Node{ID: "net", Name: "net", NetResetDay: 1, NetCycleStart: &old,
		NetRx: 500, NetTx: 600, NetOffset: 30, NetWarned: 2,
		NetBaseline: true, NetEpoch: "boot", NetSampleUptime: 100, NetLastRx: 1000, NetLastTx: 2000}
	if err := s.st.DB.Create(&n).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.accountNodeNet(n.ID, nicSample("boot", 200, 1010, 2020), now); err != nil {
		t.Fatal(err)
	}
	// An hourly sweep, including a second leader's sweep, must not clear bytes
	// that were already assigned to the new cycle by the first heartbeat.
	s.resetNodeCyclesAt(now.Add(30 * time.Minute))
	s.resetNodeCyclesAt(now.Add(45 * time.Minute))
	if err := s.st.DB.First(&n, "id = ?", n.ID).Error; err != nil {
		t.Fatal(err)
	}
	if n.NetRx != 10 || n.NetTx != 20 || n.NetOffset != 0 || n.NetWarned != 0 || !n.NetCycleStart.Equal(cycleStart(1, now)) {
		t.Fatalf("new cycle lost or retained old bytes: %+v", n)
	}
	var d store.NodeTrafficDaily
	if err := s.st.DB.First(&d, "node_id = ? AND day = ?", n.ID, "2026-11-01").Error; err != nil {
		t.Fatal(err)
	}
	if d.Rx != 10 || d.Tx != 20 {
		t.Fatalf("daily ledger includes old cycle: %+v", d)
	}
}

func TestNodeNetRolloverAndSampleRollbackTogether(t *testing.T) {
	s := newTestServer(t)
	now := time.Date(2026, 11, 1, 0, 0, 5, 0, time.Local)
	old := cycleStart(1, now.AddDate(0, -1, 0))
	n := store.Node{ID: "net", Name: "net", NetResetDay: 1, NetCycleStart: &old,
		NetRx: 500, NetTx: 600, NetOffset: 30, NetWarned: 2,
		NetBaseline: true, NetEpoch: "boot", NetSampleUptime: 100, NetLastRx: 1000, NetLastTx: 2000}
	s.st.DB.Create(&n)
	s.st.DB.Callback().Update().Before("gorm:update").Register("test:sample-fail", func(tx *gorm.DB) {
		if m, ok := tx.Statement.Dest.(map[string]any); ok && m["net_last_rx"] != nil {
			tx.AddError(errors.New("sample write failed"))
		}
	})
	if err := s.accountNodeNet(n.ID, nicSample("boot", 200, 1010, 2020), now); err == nil {
		t.Fatal("failed sample acknowledged")
	}
	s.st.DB.Callback().Update().Remove("test:sample-fail")
	s.st.DB.First(&n, "id = ?", n.ID)
	var count int64
	s.st.DB.Model(&store.NodeTrafficDaily{}).Where("node_id = ?", n.ID).Count(&count)
	if count != 0 || n.NetRx != 500 || n.NetTx != 600 || n.NetOffset != 30 || n.NetWarned != 2 || !n.NetCycleStart.Equal(old) || n.NetSampleUptime != 100 {
		t.Fatalf("partial rollover escaped rollback: %+v daily=%d", n, count)
	}
	if err := s.accountNodeNet(n.ID, nicSample("boot", 200, 1010, 2020), now); err != nil {
		t.Fatal(err)
	}
	s.st.DB.First(&n, "id = ?", n.ID)
	if n.NetRx != 10 || n.NetTx != 20 {
		t.Fatalf("retry lost new cycle interval: %+v", n)
	}
}

func TestNodeNetConcurrentRolloverAndCalibration(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		now := time.Date(2026, 11, 1, 0, 0, 5, 0, time.Local)
		old := cycleStart(1, now.AddDate(0, -1, 0))
		n := store.Node{ID: "net", Name: "net", NetResetDay: 1, NetCycleStart: &old,
			NetRx: 500, NetTx: 600, NetOffset: 30, NetWarned: 2,
			NetBaseline: true, NetEpoch: "boot", NetSampleUptime: 100, NetLastRx: 1000, NetLastTx: 2000}
		if err := ss[0].st.DB.Create(&n).Error; err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 6)
		for _, s := range ss {
			wg.Add(2)
			go func(s *Server) {
				defer wg.Done()
				errs <- s.accountNodeNet(n.ID, nicSample("boot", 200, 1010, 2020), now)
			}(s)
			go func(s *Server) { defer wg.Done(); s.resetNodeCyclesAt(now) }(s)
		}
		wg.Wait()
		ss[0].st.DB.First(&n, "id = ?", n.ID)
		if n.NetRx != 10 || n.NetTx != 20 {
			t.Fatalf("concurrent sweep lost heartbeat: %+v", n)
		}

		// Calibration to 2 GiB can precede or follow the next 30-byte beat.
		// Both serial outcomes are valid; a stale offset would produce more.
		used := float64(2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := ss[0].updateNodeNet(n.ID, netReq{UsedGB: &used}, now)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			errs <- ss[1].accountNodeNet(n.ID, nicSample("boot", 300, 1020, 2040), now)
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		ss[0].st.DB.First(&n, "id = ?", n.ID)
		if got := netUsed(&n); got != 2*gib && got != 2*gib+30 {
			t.Fatalf("calibration used a stale total: %d", got)
		}
		var d store.NodeTrafficDaily
		ss[0].st.DB.First(&d, "node_id = ?", n.ID)
		if d.Rx != 20 || d.Tx != 40 {
			t.Fatalf("calibration rewrote daily history: %+v", d)
		}
	})
}

func TestNodeNetResetWriteFailure(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "net", Name: "net", NetRx: 100, NetTx: 200})
	s.st.DB.Callback().Update().Before("gorm:update").Register("test:reset-fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "nodes" {
			tx.AddError(errors.New("reset failed"))
		}
	})
	if status, _ := c.do("POST", "/nodes/net/net-reset", nil); status != 500 {
		t.Fatalf("failed reset returned %d", status)
	}
	s.st.DB.Callback().Update().Remove("test:reset-fail")
	var n store.Node
	s.st.DB.First(&n, "id = ?", "net")
	if n.NetRx != 100 || n.NetTx != 200 {
		t.Fatal("failed reset changed totals")
	}
}

func TestNodeNetCalibrationRejectsOverflow(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "net", Name: "net", NetRx: 100})
	for _, value := range []float64{-1, math.Inf(1), math.NaN(), float64(math.MaxInt64) / gib, 1e100} {
		if _, err := s.updateNodeNet("net", netReq{UsedGB: &value}, time.Now()); err == nil {
			t.Fatalf("accepted invalid calibration %v", value)
		}
		if _, err := s.updateNodeNet("net", netReq{LimitGB: &value}, time.Now()); err == nil {
			t.Fatalf("accepted invalid limit %v", value)
		}
	}
}

func TestNodeNetProviderCalibrationFailureDoesNotNotify(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "net", Name: "net", NetLimit: 100, NetRx: 10})
	ev := events(s)
	s.st.DB.Callback().Update().Before("gorm:update").Register("test:provider-fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "nodes" {
			tx.AddError(errors.New("calibration failed"))
		}
	})
	if err := s.calibrateNodeNet("net", &provider.Info{DataLimit: 100, DataUsed: 90}, time.Now()); err == nil {
		t.Fatal("failed calibration acknowledged")
	}
	s.st.DB.Callback().Update().Remove("test:provider-fail")
	select {
	case e := <-ev:
		t.Fatalf("failed transaction sent traffic alert: %+v", e)
	default:
	}
	var n store.Node
	s.st.DB.First(&n, "id = ?", "net")
	if n.NetOffset != 0 || n.NetWarned != 0 {
		t.Fatal("failed calibration escaped rollback")
	}
}

func TestNodeNetConcurrentProviderCalibration(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		if err := ss[0].st.DB.Create(&store.Node{ID: "net", Name: "net", NetLimit: 100, NetRx: 10}).Error; err != nil {
			t.Fatal(err)
		}
		watch := []chan Event{events(ss[0]), events(ss[1])}
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, s := range ss {
			wg.Add(1)
			go func(s *Server) {
				defer wg.Done()
				errs <- s.calibrateNodeNet("net", &provider.Info{DataLimit: 100, DataUsed: 90}, time.Now())
			}(s)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		var n store.Node
		ss[0].st.DB.First(&n, "id = ?", "net")
		if netUsed(&n) != 90 || n.NetWarned != 1 {
			t.Fatalf("concurrent provider calibration diverged: %+v", n)
		}
		alerts := 0
		for _, ch := range watch {
			for len(ch) > 0 {
				if e := <-ch; e.Kind == "node_traffic" {
					alerts++
				}
			}
		}
		if alerts != 1 {
			t.Fatalf("provider sent %d threshold alerts, want 1", alerts)
		}
	})
}
