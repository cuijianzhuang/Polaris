package panel

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
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

// startBus runs a server's cluster bus for the test and waits until it
// listens.
func startBus(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.bus.run(ctx)
	select {
	case <-s.bus.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("bus not listening")
	}
}

func nextMsg(t *testing.T, c *agentConn) *polarisv1.PanelMessage {
	t.Helper()
	select {
	case m := <-c.out:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message reached the agent")
		return nil
	}
}

// An agent connected to instance A is reachable from instance B: online
// state, config notices, commands (large results too) and disconnects.
func TestClusterAgentRouting(t *testing.T) {
	ss := clusterServers(t, pgOnly(t), 2)
	a, b := ss[0], ss[1]
	startBus(t, a)
	startBus(t, b)
	a.st.DB.Create(&store.Node{ID: "n1", Name: "hk", Status: "online", LastSeen: ptrTime(time.Now().Add(-10 * time.Minute))})
	conn := a.hub.register("n1")
	a.presenceUp("n1")

	if !b.hub.Online("n1") || b.hub.Local("n1") {
		t.Fatal("B does not see the node on A")
	}
	// The offline sweep on B must not take the node down.
	b.markOfflineNodes()
	var n store.Node
	b.st.DB.First(&n, "id = ?", "n1")
	if n.Status != "online" {
		t.Fatalf("node marked %s by another instance", n.Status)
	}

	rev, _, err := b.Publish("n1", "test", true)
	if err != nil {
		t.Fatal(err)
	}
	if m := nextMsg(t, conn).GetSpecChanged(); m == nil || m.Rev != rev {
		t.Fatalf("spec notice: %v", m)
	}

	big := strings.Repeat("x", 20000) // beyond a NOTIFY payload
	go func() {
		cmd := nextMsg(t, conn).GetCommand()
		a.hub.resolve("n1", &polarisv1.CommandResult{Id: cmd.Id, Ok: true, Output: cmd.Kind + big})
	}()
	res, err := b.hub.Command(context.Background(), "n1", "core_logs", nil, 5*time.Second)
	if err != nil || !res.Ok || res.Output != "core_logs"+big {
		t.Fatalf("forwarded command: %v %v", err, res != nil && res.Ok)
	}

	b.hub.Kick("n1")
	select {
	case <-conn.done:
	case <-time.After(5 * time.Second):
		t.Fatal("kick did not reach A")
	}
	a.presenceDown("n1")
	time.Sleep(2100 * time.Millisecond) // presence cache
	if b.hub.Online("n1") {
		t.Fatal("node still online after disconnect")
	}
	if _, err := b.hub.Command(context.Background(), "n1", "x", nil, time.Second); err == nil {
		t.Fatal("command to an offline node succeeded")
	}
}

// A heartbeat behind the published revision gets the notice again, at most
// every 30 seconds.
func TestResendSpecOnLaggingHeartbeat(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "hk", DesiredRev: 3})
	conn := s.hub.register("n1")
	s.handleHeartbeat("n1", &polarisv1.Heartbeat{AppliedRev: 2})
	if m := nextMsg(t, conn).GetSpecChanged(); m == nil || m.Rev != 3 {
		t.Fatalf("resend: %v", m)
	}
	s.handleHeartbeat("n1", &polarisv1.Heartbeat{AppliedRev: 2})
	select {
	case m := <-conn.out:
		t.Fatalf("resent again within 30s: %v", m)
	default:
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// One-time codes issued on one instance are redeemed on another, once.
func TestClusterOneTimeCodes(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		ss := clusterServers(t, pgDSN, 2)
		a, b := ss[0], ss[1]

		code := a.newTGLoginCode("adm1")
		var wg sync.WaitGroup
		var wins atomic.Int32
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if id, ok := b.takeTGLoginCode(code); ok && id == "adm1" {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("login code redeemed %d times", wins.Load())
		}

		st := a.ghAuth.put(oauthState{Mode: "login", Provider: "p1", Verifier: "v"})
		if got, ok := b.ghAuth.take(st); !ok || got.Provider != "p1" || got.Verifier != "v" {
			t.Fatalf("oauth state: %+v %v", got, ok)
		}
		if _, ok := b.ghAuth.take(st); ok {
			t.Fatal("oauth state reused")
		}
		tk := a.ghAuth.ticket("adm1")
		if id, ok := b.ghAuth.takeTicket(tk); !ok || id != "adm1" {
			t.Fatal("ticket")
		}

		cb := a.callbacks.issue("/sub/tok")
		for i := 0; i < callbackUses; i++ {
			if p, ok := b.callbacks.take(cb); !ok || p != "/sub/tok" {
				t.Fatalf("callback use %d: %q %v", i, p, ok)
			}
		}
		if _, ok := b.callbacks.take(cb); ok {
			t.Fatal("callback used beyond its limit")
		}

		a.tg.putBindCode("abc123", bindCode{Role: "owner", Expires: time.Now().Add(time.Minute)})
		var bc bindCode
		if !b.eph.take("tgbind:ABC123", &bc) || bc.Role != "owner" {
			t.Fatal("bind code")
		}

		// Telegram sign-in from a page: started on A, claimed and approved
		// by the bot on B (the leader), polled again on A.
		id := "req1"
		if err := a.eph.put(a.tgWeb.key(id), &tgWebLogin{Secret: "s", Code: 42}, time.Minute); err != nil {
			t.Fatal(err)
		}
		var claimed tgWebLogin
		b.tgWeb.eph.update(b.tgWeb.key(id), &claimed, func() ephAction { claimed.Chat = 7; return ephSave })
		if res, _ := b.tg.loginPick(&b.tgWeb, 7, id, "42", "adm1"); res != "ok" {
			t.Fatalf("pick: %s", res)
		}
		var req tgWebLogin
		if !a.eph.get(a.tgWeb.key(id), &req) || req.Subject != "adm1" {
			t.Fatalf("approval not visible on A: %+v", req)
		}
	})
}

// Settings changed on one instance are seen at once on another; node list
// changes refresh the other instance's admin pages; install progress of a
// node connected elsewhere is visible.
func TestClusterSharedState(t *testing.T) {
	ss := clusterServers(t, pgOnly(t), 2)
	a, b := ss[0], ss[1]
	startBus(t, a)
	startBus(t, b)

	if b.st.GetSetting("site_name", "old") != "old" { // cached on B
		t.Fatal("unexpected setting")
	}
	if err := a.st.SetSetting("site_name", "new"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for b.st.GetSetting("site_name", "old") != "new" {
		if time.Now().After(deadline) {
			t.Fatal("B kept the stale setting")
		}
		time.Sleep(50 * time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.shareNodeChanges(ctx)
	b.rt.dirty.Store(false)
	a.broadcastNodes()
	deadline = time.Now().Add(5 * time.Second)
	for !b.rt.dirty.Load() {
		if time.Now().After(deadline) {
			t.Fatal("B's admin pages were not told to refresh")
		}
		time.Sleep(50 * time.Millisecond)
	}

	a.st.DB.Create(&store.Node{ID: "n1", Name: "hk"})
	a.hub.register("n1")
	a.presenceUp("n1")
	a.handleProgress("n1", &polarisv1.Progress{Active: true, Stage: "下载 xray", Done: 5, Total: 10})
	if p := b.nodeProgressFor("n1"); p == nil || p.Stage != "下载 xray" || p.Done != 5 {
		t.Fatalf("progress on B: %+v", p)
	}
}
