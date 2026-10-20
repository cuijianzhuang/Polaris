package panel

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// clusterServers opens n panels on one database, each with its own data
// directory: separate machines.
func clusterServers(t *testing.T, pgDSN string, n int) []*Server {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "p.db")
	if pgDSN != "" {
		dsn = pgTestSchema(t, pgDSN)
	}
	var out []*Server
	for i := 0; i < n; i++ {
		cfg := &Config{DataDir: t.TempDir(), SecretKey: "cluster-secret-key", AdminPath: "/"}
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
		out = append(out, s)
	}
	return out
}

func pgOnly(t *testing.T) string {
	dsn := os.Getenv("POLARIS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POLARIS_TEST_POSTGRES_DSN not set")
	}
	return dsn
}

// One instance holds the lease; when it stops, another takes over.
func TestLeaderLease(t *testing.T) {
	ss := clusterServers(t, pgOnly(t), 2)
	a, b := ss[0], ss[1]
	if !a.isLeader() || b.isLeader() {
		t.Fatalf("leaders: a=%v b=%v", a.isLeader(), b.isLeader())
	}
	b.updateLease()
	if b.isLeader() {
		t.Fatal("a live lease was taken over")
	}
	// The leader shuts down: it releases the lease.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.runLease(ctx); close(done) }()
	cancel()
	<-done
	b.updateLease()
	if !b.isLeader() || a.isLeader() {
		t.Fatalf("after release: a=%v b=%v", a.isLeader(), b.isLeader())
	}
	// A leader that stops renewing (crash) loses the lease once it expires.
	b.st.DB.Exec("UPDATE cluster_leases SET expires_at = now() - interval '1 second'")
	a.updateLease()
	b.updateLease()
	if !a.isLeader() || b.isLeader() {
		t.Fatalf("after expiry: a=%v b=%v", a.isLeader(), b.isLeader())
	}
}

// A SQLite panel is always the leader.
func TestLeaderSQLite(t *testing.T) {
	if s := newTestServer(t); !s.isLeader() {
		t.Fatal("SQLite panel is not the leader")
	}
}

// Instances publishing the same node at once get consecutive revisions.
func TestConcurrentPublish(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		ss := clusterServers(t, pgDSN, 2)
		n := store.Node{ID: "n1", Name: "hk"}
		if err := ss[0].st.DB.Create(&n).Error; err != nil {
			t.Fatal(err)
		}
		const each = 8
		var wg sync.WaitGroup
		errs := make(chan error, 2*each)
		for _, s := range ss {
			for i := 0; i < each; i++ {
				wg.Add(1)
				go func(s *Server) {
					defer wg.Done()
					if _, _, err := s.Publish("n1", "test", true); err != nil {
						errs <- err
					}
				}(s)
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		var got store.Node
		ss[0].st.DB.First(&got, "id = ?", "n1")
		var revs int64
		ss[0].st.DB.Model(&store.ConfigRevision{}).Where("node_id = ?", "n1").Count(&revs)
		if got.DesiredRev != 2*each || revs != 2*each {
			t.Fatalf("desired_rev %d, revisions %d, want %d", got.DesiredRev, revs, 2*each)
		}
	})
}

// Every instance uses the CA stored in the database; an older install's CA
// file is imported.
func TestSharedCA(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		ss := clusterServers(t, pgDSN, 2)
		if ss[0].ca.Fingerprint() != ss[1].ca.Fingerprint() {
			t.Fatal("instances use different CAs")
		}
		b, err := os.ReadFile(filepath.Join(ss[1].cfg.DataDir, "ca.crt"))
		if err != nil || string(b) != string(ss[0].ca.CertPEM) {
			t.Fatal("CA file not written for the backup format")
		}
	})
	t.Run("import", func(t *testing.T) {
		s := newTestServer(t)
		want := s.ca.Fingerprint()
		s.st.DB.Where("1 = 1").Delete(&store.ClusterSecret{}) // as before the upgrade
		ca, err := loadOrCreateCA(s.st, s.cfg.DataDir, s.st.Box)
		if err != nil || ca.Fingerprint() != want {
			t.Fatalf("file CA not imported: %v", err)
		}
		var n int64
		s.st.DB.Model(&store.ClusterSecret{}).Count(&n)
		if n != 2 {
			t.Fatalf("CA rows %d", n)
		}
	})
}
