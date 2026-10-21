package panel

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func bootSample(boot, epoch string, seq, rx, tx uint64) *polarisv1.SystemStats {
	p := nicSample(epoch, seq, rx, tx)
	p.NetBootId = boot
	return p
}

func TestNodeNetSourceReplayAcrossInstances(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		if err := ss[0].st.DB.Create(&store.Node{ID: "net", Name: "net"}).Error; err != nil {
			t.Fatal(err)
		}
		steps := []struct {
			sample *polarisv1.SystemStats
			rx, tx int64
		}{
			{bootSample("boot-1", "interfaces-a", 100, 1000, 2000), 0, 0},
			{bootSample("boot-1", "interfaces-a", 200, 1010, 2020), 10, 20},
			{bootSample("boot-1", "interfaces-b", 400, 1<<40, 2<<40), 10, 20},
			// Older interface epochs cannot rewind this boot's high-water mark.
			{bootSample("boot-1", "interfaces-a", 250, 1015, 2030), 10, 20},
			{bootSample("boot-1", "interfaces-a", 200, 1010, 2020), 10, 20},
			{bootSample("boot-1", "interfaces-b", 500, 1<<40+5, 2<<40+10), 15, 30},
			// Removing an extra NIC can legitimately restore a previous epoch.
			{bootSample("boot-1", "interfaces-a", 600, 1020, 2040), 15, 30},
			{bootSample("boot-1", "interfaces-b", 500, 1<<40+5, 2<<40+10), 15, 30},
			{bootSample("boot-1", "interfaces-a", 700, 1030, 2060), 25, 50},
			{bootSample("boot-2", "interfaces-c", 10, 3, 4), 25, 50},
			{bootSample("boot-2", "interfaces-c", 20, 5, 7), 27, 53},
			// Even a higher uptime from a retired boot cannot resurrect it.
			{bootSample("boot-1", "interfaces-a", 800, 1040, 2080), 27, 53},
			{bootSample("boot-1", "interfaces-a", 900, 1050, 2100), 27, 53},
			{bootSample("boot-2", "interfaces-c", 30, 8, 11), 30, 57},
		}
		for i, step := range steps {
			s := ss[i%len(ss)]
			if err := s.accountNodeNet("net", step.sample, time.Now()); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
			var n store.Node
			var d store.NodeTrafficDaily
			if err := s.st.DB.First(&n, "id = ?", "net").Error; err != nil {
				t.Fatal(err)
			}
			if err := s.st.DB.First(&d, "node_id = ?", "net").Error; err != nil {
				t.Fatal(err)
			}
			if n.NetRx != step.rx || n.NetTx != step.tx || d.Rx != step.rx || d.Tx != step.tx {
				t.Fatalf("step %d replayed or lost bytes: cycle=%d/%d daily=%d/%d want=%d/%d", i, n.NetRx, n.NetTx, d.Rx, d.Tx, step.rx, step.tx)
			}
		}
		var boots []store.NodeNetBoot
		ss[0].st.DB.Where("node_id = ?", "net").Order("boot_id").Find(&boots)
		if len(boots) != 2 || !boots[0].Retired || boots[0].LastUptime != 700 || boots[1].Retired || boots[1].LastUptime != 30 {
			t.Fatalf("boot progress not persisted: %+v", boots)
		}
	})
}

func TestNodeNetSourceCheckpointRollsBack(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "net", Name: "net"})
	if err := s.accountNodeNet("net", bootSample("boot-1", "nic-a", 100, 100, 200), time.Now()); err != nil {
		t.Fatal(err)
	}
	fail := func() {
		s.st.DB.Callback().Update().Before("gorm:update").Register("test:source-fail", func(tx *gorm.DB) {
			if tx.Statement.Table == "nodes" {
				tx.AddError(errors.New("node write failed"))
			}
		})
	}
	fail()
	if err := s.accountNodeNet("net", bootSample("boot-1", "nic-a", 200, 110, 220), time.Now()); err == nil {
		t.Fatal("failed baseline write acknowledged")
	}
	s.st.DB.Callback().Update().Remove("test:source-fail")
	var boot store.NodeNetBoot
	s.st.DB.First(&boot, "node_id = ? AND boot_id = ?", "net", "boot-1")
	if boot.LastUptime != 100 || boot.Retired {
		t.Fatalf("failed sample advanced checkpoint: %+v", boot)
	}
	if err := s.accountNodeNet("net", bootSample("boot-1", "nic-a", 200, 110, 220), time.Now()); err != nil {
		t.Fatal(err)
	}
	fail()
	if err := s.accountNodeNet("net", bootSample("boot-2", "nic-b", 10, 1, 2), time.Now()); err == nil {
		t.Fatal("failed reboot write acknowledged")
	}
	s.st.DB.Callback().Update().Remove("test:source-fail")
	var count int64
	s.st.DB.Model(&store.NodeNetBoot{}).Where("node_id = ?", "net").Count(&count)
	s.st.DB.First(&boot, "node_id = ? AND boot_id = ?", "net", "boot-1")
	if count != 1 || boot.Retired || boot.LastUptime != 200 {
		t.Fatalf("failed reboot retired active boot: %+v count=%d", boot, count)
	}
	if err := s.accountNodeNet("net", bootSample("boot-1", "nic-a", 300, 115, 230), time.Now()); err != nil {
		t.Fatal(err)
	}
	var n store.Node
	s.st.DB.First(&n, "id = ?", "net")
	if n.NetRx != 15 || n.NetTx != 30 {
		t.Fatalf("retry lost traffic: %+v", n)
	}
}

func TestNodeNetConcurrentInterfaceSources(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		ss[0].st.DB.Create(&store.Node{ID: "net", Name: "net"})
		if err := ss[0].accountNodeNet("net", bootSample("boot", "nic-a", 100, 1000, 2000), time.Now()); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i, p := range []*polarisv1.SystemStats{
			bootSample("boot", "nic-a", 200, 1010, 2020),
			bootSample("boot", "nic-b", 300, 1<<40, 2<<40),
		} {
			wg.Add(1)
			go func(s *Server, p *polarisv1.SystemStats) {
				defer wg.Done()
				errs <- s.accountNodeNet("net", p, time.Now())
			}(ss[i], p)
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
		if n.NetEpoch != "nic-b" || n.NetSampleUptime != 300 || (n.NetRx != 0 && n.NetRx != 10) {
			t.Fatalf("concurrent old source rewound baseline: %+v", n)
		}
		beforeRx, beforeTx := n.NetRx, n.NetTx
		if err := ss[1].accountNodeNet("net", bootSample("boot", "nic-b", 400, 1<<40+5, 2<<40+10), time.Now()); err != nil {
			t.Fatal(err)
		}
		ss[0].st.DB.First(&n, "id = ?", "net")
		if n.NetRx != beforeRx+5 || n.NetTx != beforeTx+10 {
			t.Fatal("new source interval lost")
		}
	})
}

func TestNodeNetSourceRejectsInvalidMetadata(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "net", Name: "net"})
	for _, p := range []*polarisv1.SystemStats{
		bootSample(strings.Repeat("b", 65), "nic", 100, 10, 20),
		bootSample("boot", "nic", math.MaxUint64, 10, 20),
		{NetBootId: "boot", RxBytes: 10},
	} {
		if err := s.accountNodeNet("net", p, time.Now()); err == nil {
			t.Fatal("invalid source accepted", p)
		}
	}
	var count int64
	s.st.DB.Model(&store.NodeNetBoot{}).Count(&count)
	if count != 0 {
		t.Fatal("invalid source created checkpoint")
	}
}

func TestNodeNetDeleteRemovesBootProgress(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "net", Name: "net"})
	if err := s.accountNodeNet("net", bootSample("boot", "nic", 100, 10, 20), time.Now()); err != nil {
		t.Fatal(err)
	}
	if status, out := c.do("DELETE", "/nodes/net", nil); status != 204 {
		t.Fatalf("delete node: %d %v", status, out)
	}
	var count int64
	s.st.DB.Model(&store.NodeNetBoot{}).Where("node_id = ?", "net").Count(&count)
	if count != 0 {
		t.Fatal("deleted node left replay checkpoints")
	}
}
