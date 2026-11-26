package panel

import (
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// settledReturnFacts is the one constellation that must trigger the
// auto-return: a sync upgrade settled successfully, the lease moved to
// the standby, the pre-upgrade leader is back and stable, and its one
// attempt is unspent. Each table case below flips exactly one input.
func settledReturnFacts() leaderReturnFacts {
	return leaderReturnFacts{
		opState:        syncStateSucceeded,
		opPhase:        syncPhaseDone,
		leaderDelivery: "done",
		preLeader:      "member-a",
		selfMember:     "member-a",
		currentLeader:  "member-b",
		selfIsLeader:   false,
		selfUptime:     10 * time.Minute,
		finishedAgo:    5 * time.Minute,
		attempted:      false,
		anotherRunning: false,
	}
}

func TestShouldAutoReturnLeader(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*leaderReturnFacts)
		want   bool
	}{
		{"settled upgrade, lease moved, leader stable", func(f *leaderReturnFacts) {}, true},
		{"partial run, leader member itself finished", func(f *leaderReturnFacts) { f.opState = syncStatePartial }, true},
		{"partial run, leader member failed", func(f *leaderReturnFacts) {
			f.opState, f.leaderDelivery = syncStatePartial, "failed"
		}, false},
		{"upgrade still running", func(f *leaderReturnFacts) {
			f.opState, f.opPhase = syncStateRunning, syncPhaseVerify
		}, false},
		{"upgrade failed", func(f *leaderReturnFacts) { f.opState = syncStateFailed }, false},
		{"upgrade aborted in the standby phase", func(f *leaderReturnFacts) {
			f.opState, f.preLeader = syncStateAborted, ""
		}, false},
		{"leader never pinned", func(f *leaderReturnFacts) { f.preLeader = "" }, false},
		{"lease never moved", func(f *leaderReturnFacts) { f.currentLeader = "member-a" }, false},
		{"already leading again", func(f *leaderReturnFacts) {
			f.selfIsLeader, f.currentLeader = true, "member-a"
		}, false},
		{"current leader unknown", func(f *leaderReturnFacts) { f.currentLeader = "" }, false},
		{"standby evaluates: not the pre-upgrade leader", func(f *leaderReturnFacts) {
			f.selfMember = "member-b"
		}, false},
		{"leader back but not stable yet", func(f *leaderReturnFacts) {
			f.selfUptime = leaderReturnStableFor - time.Second
		}, false},
		{"leader exactly at the stability bound", func(f *leaderReturnFacts) {
			f.selfUptime = leaderReturnStableFor
		}, true},
		{"window just closed", func(f *leaderReturnFacts) {
			f.finishedAgo = leaderReturnWindow + time.Second
		}, false},
		{"attempt already spent", func(f *leaderReturnFacts) { f.attempted = true }, false},
		{"a newer upgrade is running", func(f *leaderReturnFacts) { f.anotherRunning = true }, false},
	}
	for _, tc := range cases {
		f := settledReturnFacts()
		tc.mutate(&f)
		if got := shouldAutoReturnLeader(f); got != tc.want {
			t.Errorf("%s: shouldAutoReturnLeader = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLeaderAutoReturn walks the whole flow on PostgreSQL: a settled
// sync upgrade whose lease landed on the standby, the pre-upgrade
// leader asking it back through the stepdown channel exactly once, and
// a later manual takeover that the auto-return must not undo.
func TestLeaderAutoReturn(t *testing.T) {
	ss := clusterServers(t, pgOnly(t), 2)
	a, b := ss[0], ss[1]
	if !a.isLeader() || b.isLeader() {
		t.Fatalf("leaders: a=%v b=%v", a.isLeader(), b.isLeader())
	}
	now := time.Now().UTC()
	// A sync upgrade that settled successfully with a as the pinned
	// pre-upgrade leader, as the verify phase would have left it.
	op := store.ClusterOperation{ID: store.NewID(), Kind: "syncupdate", Target: "v0.1.20",
		CreatedAt: now.Add(-20 * time.Minute), ExpiresAt: now.Add(24 * time.Hour),
		State: syncStateSucceeded, Phase: syncPhaseDone, LeaderMember: a.memberID}
	if err := a.st.DB.Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.st.DB.Create(&store.ClusterDelivery{OperationID: op.ID, MemberID: a.memberID, Status: "done"}).Error; err != nil {
		t.Fatal(err)
	}
	// a has been back up and stable well past the stability window.
	a.startedAt = now.Add(-10 * time.Minute)

	stepdownRows := func() int64 {
		var n int64
		a.st.DB.Model(&store.ClusterLease{}).Where("name = ?", stepdownLeaseName).Count(&n)
		return n
	}
	// The standby has nothing to return: it is not the pre-upgrade leader.
	b.maybeAutoReturnLeader()
	if n := stepdownRows(); n != 0 {
		t.Fatalf("standby acted on the return: %d stepdown rows", n)
	}
	// While a still leads, there is nothing to return either.
	a.maybeAutoReturnLeader()
	if n := stepdownRows(); n != 0 {
		t.Fatalf("return fired while the lease never moved: %d stepdown rows", n)
	}

	// The upgrade restarts a; its lease lapses and b picks it up (what
	// the standby orchestrating the verify phase ends up holding).
	if err := a.st.DB.Exec("UPDATE cluster_leases SET expires_at = now() - interval '1 second' WHERE name = ?", leaseName).Error; err != nil {
		t.Fatal(err)
	}
	b.updateLease()
	if !b.isLeader() {
		t.Fatal("standby did not pick up the lapsed lease")
	}
	a.updateLease() // a observes the lease is b's now
	if a.isLeader() {
		t.Fatal("old leader still believes it leads")
	}

	// The return fires: one stepdown request naming a, the spent-attempt
	// marker on the operation, and one audit row.
	a.maybeAutoReturnLeader()
	var row store.ClusterLease
	if err := a.st.DB.Where("name = ?", stepdownLeaseName).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Holder != a.procID {
		t.Fatalf("stepdown holder: %q, want the pre-upgrade leader %q", row.Holder, a.procID)
	}
	if got := a.st.GetSetting(leaderAutoReturnOpSetting, ""); got != op.ID {
		t.Fatalf("attempt marker: %q, want the operation %q", got, op.ID)
	}
	var audits int64
	a.st.DB.Model(&store.AuditLog{}).Where("action = ?", "ha.leader_auto_return").Count(&audits)
	if audits != 1 {
		t.Fatalf("audit rows: %d, want 1", audits)
	}
	if st := a.leaderReturnStatus(&op); st != "attempted" {
		t.Fatalf("return status before handover: %q, want attempted", st)
	}

	// The handover completes over the ordinary lease rounds.
	b.updateLease()
	if b.isLeader() {
		t.Fatal("current leader did not yield to the return request")
	}
	a.updateLease()
	if !a.isLeader() {
		t.Fatal("pre-upgrade leader did not take the lease back")
	}
	if st := a.leaderReturnStatus(&op); st != "returned" {
		t.Fatalf("return status after handover: %q, want returned", st)
	}

	// One attempt per upgrade: further evaluations stay quiet even
	// though the settle window is still open.
	a.maybeAutoReturnLeader()
	if n := stepdownRows(); n != 0 {
		t.Fatalf("return fired twice for one upgrade: %d stepdown rows", n)
	}

	// A manual takeover afterwards stands: b asks, a yields, and the
	// auto-return does not undo it (the attempt is spent, and the
	// return only ever belonged to the upgrade).
	//
	// b yielded to the return above, so b is inside its stepdown
	// cooldown (one lease TTL) and tryLease would refuse the claim.
	// Live rounds tick 5s apart and the cooldown lapses long before a
	// human clicks take-leader; the test calls the rounds back to back,
	// so expire the cooldown to model that elapsed time.
	b.stepdownUntil.Store(0)
	if err := b.requestLeaderTakeover(); err != nil {
		t.Fatal(err)
	}
	a.updateLease()
	if a.isLeader() {
		t.Fatal("leader did not yield to the manual takeover")
	}
	b.updateLease()
	if !b.isLeader() {
		t.Fatal("manual requester did not take over")
	}
	a.maybeAutoReturnLeader()
	if n := stepdownRows(); n != 0 {
		t.Fatalf("auto-return undid a manual takeover: %d stepdown rows", n)
	}
}

// TestLeaderAutoReturnWaitsForStability: a pre-upgrade leader that has
// only just restarted must not ask for the lease back yet — and the
// unspent attempt must still be there once it has settled.
func TestLeaderAutoReturnWaitsForStability(t *testing.T) {
	ss := clusterServers(t, pgOnly(t), 2)
	a, b := ss[0], ss[1]
	now := time.Now().UTC()
	op := store.ClusterOperation{ID: store.NewID(), Kind: "syncupdate", Target: "v0.1.20",
		CreatedAt: now.Add(-10 * time.Minute), ExpiresAt: now.Add(24 * time.Hour),
		State: syncStateSucceeded, Phase: syncPhaseDone, LeaderMember: a.memberID}
	if err := a.st.DB.Create(&op).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.st.DB.Exec("UPDATE cluster_leases SET expires_at = now() - interval '1 second' WHERE name = ?", leaseName).Error; err != nil {
		t.Fatal(err)
	}
	b.updateLease()
	a.updateLease()
	if a.isLeader() || !b.isLeader() {
		t.Fatalf("leaders: a=%v b=%v", a.isLeader(), b.isLeader())
	}
	// a restarted a minute ago: inside the stability window.
	a.startedAt = now.Add(-time.Minute)
	a.maybeAutoReturnLeader()
	var n int64
	a.st.DB.Model(&store.ClusterLease{}).Where("name = ?", stepdownLeaseName).Count(&n)
	if n != 0 {
		t.Fatalf("return fired before the leader settled: %d stepdown rows", n)
	}
	if got := a.st.GetSetting(leaderAutoReturnOpSetting, ""); got != "" {
		t.Fatalf("attempt spent too early: %q", got)
	}
	// Once stable, the same evaluation fires.
	a.startedAt = now.Add(-leaderReturnStableFor - time.Minute)
	a.maybeAutoReturnLeader()
	a.st.DB.Model(&store.ClusterLease{}).Where("name = ?", stepdownLeaseName).Count(&n)
	if n != 1 {
		t.Fatalf("return did not fire once stable: %d stepdown rows", n)
	}
}
