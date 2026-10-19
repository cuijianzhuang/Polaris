package panel

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newTrafficServers opens two panels on one database: a restart, or a
// second instance.
func newTrafficServers(t *testing.T, pgDSN string) (*Server, *Server) {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "p.db")
	if pgDSN != "" {
		dsn = pgTestSchema(t, pgDSN)
	}
	open := func() *Server {
		cfg := &Config{DataDir: dir, SecretKey: "x", AdminPath: "/"}
		cfg.DB.DSN = dsn
		s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if db, err := s.st.DB.DB(); err == nil {
				db.Close()
			}
		})
		return s
	}
	a := open()
	if err := a.st.DB.Create(&store.User{ID: "u", Username: "alice", UUID: "u", SubToken: "u", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.st.DB.Create(&store.Node{ID: "n", Name: "HK", TrafficRate: 1}).Error; err != nil {
		t.Fatal(err)
	}
	return a, open()
}

// pgTestSchema returns pgDSN pointed at a new random schema, dropped when
// the test ends; the developer's database is never cleared.
func pgTestSchema(t *testing.T, pgDSN string) string {
	t.Helper()
	db, err := gorm.Open(postgres.Open(pgDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open PostgreSQL test database:", err)
	}
	schema := "test_traffic_" + store.NewID()
	if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP SCHEMA " + schema + " CASCADE"); sqlDB, _ := db.DB(); sqlDB.Close() })
	u, err := url.Parse(pgDSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// onEngines runs f on SQLite, and on PostgreSQL when
// POLARIS_TEST_POSTGRES_DSN is set.
func onEngines(t *testing.T, f func(t *testing.T, pgDSN string)) {
	t.Run("sqlite", func(t *testing.T) { f(t, "") })
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("POLARIS_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("POLARIS_TEST_POSTGRES_DSN not set")
		}
		f(t, dsn)
	})
}

var trafficAllowed = map[string]bool{"u": true}

func batch(id, epoch string, seq uint64, up int64) *polarisv1.TrafficBatch {
	return &polarisv1.TrafficBatch{BatchId: id, Epoch: epoch, Seq: seq, Users: []*polarisv1.UserTraffic{{UserId: "u", Up: up, Down: up}}}
}

func usedUp(t *testing.T, s *Server) int64 {
	t.Helper()
	if err := s.settleTraffic(); err != nil {
		t.Fatal(err)
	}
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", "u").Error; err != nil {
		t.Fatal(err)
	}
	return u.UsedUp
}

func ingest(t *testing.T, s *Server, bs ...*polarisv1.TrafficBatch) []string {
	t.Helper()
	ids, _, err := s.ingestTraffic("n", bs, trafficAllowed)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// A batch acknowledged before a restart, resent afterwards (the reply was
// lost), counts once.
func TestTrafficRepeatAfterRestart(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		a, b := newTrafficServers(t, pgDSN)
		ingest(t, a, batch("b1", "e1", 1, 100))
		if got := usedUp(t, a); got != 100 {
			t.Fatalf("first: %d", got)
		}
		ids := ingest(t, b, batch("b1", "e1", 1, 100), batch("b2", "e1", 2, 5))
		if len(ids) != 2 {
			t.Fatalf("repeats must still be acknowledged: %v", ids)
		}
		if got := usedUp(t, b); got != 105 {
			t.Fatalf("after restart: %d", got)
		}
	})
}

// Older agents send no sequence numbers; their batch IDs are remembered
// in the database.
func TestTrafficLegacyRepeatAfterRestart(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		a, b := newTrafficServers(t, pgDSN)
		ingest(t, a, batch("old1", "", 0, 10))
		ingest(t, b, batch("old1", "", 0, 10), batch("old2", "", 0, 1))
		if got := usedUp(t, b); got != 11 {
			t.Fatalf("legacy: %d", got)
		}
	})
}

// Out-of-order and overlapping requests, gaps and a new epoch.
func TestTrafficSeqOrdering(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		s, _ := newTrafficServers(t, pgDSN)
		ingest(t, s, batch("c", "e1", 3, 1), batch("a", "e1", 1, 1))
		ingest(t, s, batch("a", "e1", 1, 1), batch("c", "e1", 3, 1), batch("e", "e1", 5, 1)) // 1 and 3 repeat; the gap at 4 is fine
		ingest(t, s, batch("z", "e2", 1, 10))                                                // reinstalled agent: new epoch
		if got := usedUp(t, s); got != 13 {
			t.Fatalf("got %d, want 13", got)
		}
	})
}

// Two settlements working from the same snapshot charge it once; traffic
// arriving in between is kept for the next round.
func TestTrafficConcurrentSettle(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		a, b := newTrafficServers(t, pgDSN)
		ingest(t, a, batch("b1", "e1", 1, 100))
		snap, err := b.traffic.Drain()
		if err != nil {
			t.Fatal(err)
		}
		if got := usedUp(t, a); got != 100 {
			t.Fatalf("a: %d", got)
		}
		ingest(t, a, batch("b2", "e1", 2, 7))
		if err := b.st.DB.Transaction(func(tx *gorm.DB) error { return takeInbox(tx, snap) }); err != errInboxTaken {
			t.Fatalf("stale snapshot must be refused, got %v", err)
		}
		if got := usedUp(t, b); got != 107 {
			t.Fatalf("b: %d", got)
		}
	})
}

// A failed request counts nothing: no cursor move, no inbox row.
func TestTrafficIngestAtomic(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		s, _ := newTrafficServers(t, pgDSN)
		if err := s.st.DB.Migrator().DropTable(&store.TrafficInbox{}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ingestTraffic("n", []*polarisv1.TrafficBatch{batch("b1", "e1", 1, 100)}, trafficAllowed); err == nil {
			t.Fatal("expected failure")
		}
		if err := s.st.DB.AutoMigrate(&store.TrafficInbox{}); err != nil {
			t.Fatal(err)
		}
		ingest(t, s, batch("b1", "e1", 1, 100))
		if got := usedUp(t, s); got != 100 {
			t.Fatalf("retry after failure: %d", got)
		}
	})
}

// Two panels on one database receive overlapping retries and settle at the
// same time; every batch is charged exactly once.
func TestTrafficConcurrentInstances(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		a, b := newTrafficServers(t, pgDSN)
		const n = 40
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for _, s := range []*Server{a, b} {
			s := s
			wg.Add(1)
			go func() { // agent requests, each resending everything so far
				defer wg.Done()
				for i := 1; i <= n; i++ {
					var bs []*polarisv1.TrafficBatch
					for j := 1; j <= i; j++ {
						bs = append(bs, batch(fmt.Sprint("b", j), "e1", uint64(j), 1))
					}
					for {
						if _, _, err := s.ingestTraffic("n", bs, trafficAllowed); err == nil {
							break
						}
						time.Sleep(time.Millisecond) // conflict with the other instance: retry
					}
				}
			}()
		}
		var sg sync.WaitGroup
		for _, s := range []*Server{a, b} {
			s := s
			sg.Add(1)
			go func() {
				defer sg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						_ = s.settleTraffic()
					}
				}
			}()
		}
		wg.Wait()
		close(stop)
		sg.Wait()
		for i := 0; i < 5; i++ {
			_ = a.settleTraffic()
		}
		if got := usedUp(t, a); got != n {
			t.Fatalf("charged %d, want %d", got, n)
		}
	})
}
