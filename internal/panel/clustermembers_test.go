package panel

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Members are listed with the leader marked; a stopped instance leaves.
func TestClusterMembers(t *testing.T) {
	ss := clusterServers(t, pgOnly(t), 2)
	a, b := ss[0], ss[1]
	a.memberTouch()
	b.memberTouch()
	ms := b.clusterMembers()
	if len(ms) != 2 {
		t.Fatalf("members %+v", ms)
	}
	leaders, selves := 0, 0
	for _, m := range ms {
		if !m.Online {
			t.Fatalf("offline: %+v", m)
		}
		if m.Leader {
			leaders++
			if m.ID != a.procID {
				t.Fatalf("leader %s, want %s", m.ID, a.procID)
			}
		}
		if m.Self {
			selves++
		}
	}
	if leaders != 1 || selves != 1 {
		t.Fatalf("leaders %d selves %d", leaders, selves)
	}
	if b.hostTag() == "" {
		t.Fatal("no host tag with two instances")
	}
	// A crashed instance shows as offline, then is forgotten by the leader.
	b.st.DB.Model(&store.ClusterMember{}).Where("instance_id = ?", b.procID).Update("seen_at", time.Now().Add(-time.Minute))
	for _, m := range a.clusterMembers() {
		if m.ID == b.procID && m.Online {
			t.Fatal("stale member online")
		}
	}
	if a.hostTag() != "" {
		t.Fatal("host tag with one running instance")
	}
	b.st.DB.Model(&store.ClusterMember{}).Where("instance_id = ?", b.procID).Update("seen_at", time.Now().Add(-25*time.Hour))
	a.memberAt = time.Time{}
	a.memberTouch()
	if n := len(a.clusterMembers()); n != 1 {
		t.Fatalf("old member kept: %d", n)
	}
	// Shutdown removes the row.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.runLease(ctx); close(done) }()
	cancel()
	<-done
	var n int64
	a.st.DB.Model(&store.ClusterMember{}).Count(&n)
	if n != 0 {
		t.Fatalf("row left after shutdown: %d", n)
	}
}

// SQLite: one member, the leader, without any rows written yet.
func TestClusterMembersSQLite(t *testing.T) {
	s := newTestServer(t)
	ms := s.clusterMembers()
	if len(ms) != 1 || !ms[0].Leader || !ms[0].Self || !ms[0].Online {
		t.Fatalf("%+v", ms)
	}
}

// A restore on one instance restarts the others.
func TestClusterRestoreRestartsOthers(t *testing.T) {
	ss := clusterServers(t, pgOnly(t), 2)
	a, b := ss[0], ss[1]
	restartedA, restartedB := make(chan struct{}, 1), make(chan struct{}, 1)
	a.restartHook = func() { restartedA <- struct{}{} }
	b.restartHook = func() { restartedB <- struct{}{} }
	startBus(t, a)
	startBus(t, b)
	if err := a.bus.publish(topicRestoreDone, a.procID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restartedB:
	case <-time.After(5 * time.Second):
		t.Fatal("other instance did not restart")
	}
	select {
	case <-restartedA:
		t.Fatal("the restoring instance restarted again")
	case <-time.After(1500 * time.Millisecond):
	}
}

// "Update" in one instance's UI writes an update request on every machine.
func TestClusterUpdateAll(t *testing.T) {
	ss := clusterServers(t, pgOnly(t), 2)
	a, b := ss[0], ss[1]
	withVersion(t, "v0.1.15")
	withUpdater(t, true)
	startBus(t, a)
	startBus(t, b)
	if err := a.requestPanelUpdate("v0.1.20", "me", true); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Server{a, b} {
		waitFor(t, "update request", func() bool {
			raw, err := os.ReadFile(s.updateRequestPath())
			return err == nil && strings.TrimSpace(string(raw)) == "v0.1.20"
		})
	}
	// A second click while they run is refused.
	if err := b.requestPanelUpdate("", "me", false); err != errUpdatePending {
		t.Fatalf("pending: %v", err)
	}
}
