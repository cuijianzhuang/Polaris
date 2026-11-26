package panel

import (
	"context"
	"fmt"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Leader auto-return after a sync upgrade. During a sync upgrade the
// pre-upgrade leader restarts for its own stage, and the standby that
// orchestrates the rest of the run keeps the leader lease afterwards:
// leadership used to stay on the wrong instance until someone took it
// back by hand (haTakeLeader) or restarted the standby. Now, once a
// sync upgrade has fully settled, the pre-upgrade leader asks for the
// lease back exactly once, through the same stepdown channel as the
// manual takeover (requestLeaderTakeover / honorStepdown).
//
// The boundary is deliberate and narrow:
//   - Only the sync-upgrade path returns leadership. A manual takeover
//     (haTakeLeader) is never undone: the return is evaluated only
//     inside leaderReturnWindow after the upgrade settles, and each
//     upgrade gets a single attempt, so a takeover arranged later —
//     inside or outside the window — stands.
//   - Only the panel leader lease moves. The PostgreSQL primary role
//     and DNS are untouched; promotion and switchback stay manual.
const (
	// leaderReturnStableFor is how long the pre-upgrade leader's
	// current process must have been up continuously before it may ask
	// for the lease back. The upgraded panel has to finish starting,
	// reconnect its agents and settle into the member heartbeat rhythm
	// first; three minutes is a dozen member touches, long enough that
	// a crash-looping or still-starting instance never grabs the
	// lease, short enough that the return lands while the upgrade is
	// still the most recent event in the cluster.
	leaderReturnStableFor = 3 * time.Minute
	// leaderReturnWindow bounds how long after a sync upgrade settles
	// the return may be evaluated at all. Inside the window the only
	// way leadership moved is the upgrade itself; outside it, whatever
	// arrangement exists is treated as intentional and left alone.
	// This is what makes "a manual takeover is never undone" hold
	// without having to distinguish takeover causes.
	leaderReturnWindow = 30 * time.Minute
	// leaderReturnTick is the evaluation cadence: the stability window
	// is minutes-scale, so half a minute is plenty fine-grained.
	leaderReturnTick = 30 * time.Second
	// leaderAutoReturnOpSetting is the settings key holding the id of
	// the sync update whose single return attempt has been spent. The
	// settings table is shared by the cluster, so the marker survives
	// the acting instance's restarts and is visible to every instance.
	// Only the latest upgrade is ever evaluated, so one slot suffices.
	leaderAutoReturnOpSetting = "leader_auto_return_op"
)

// leaderReturnFacts is everything the return decision reads, gathered
// from the shared database and this process. Keeping it a plain struct
// makes the rule testable without a cluster (same pattern as
// shouldStepDown).
type leaderReturnFacts struct {
	opState        string        // latest sync update's state
	opPhase        string        // latest sync update's phase
	leaderDelivery string        // the pre-upgrade leader's delivery status ("" = none)
	preLeader      string        // member id that led before the upgrade (op.LeaderMember)
	selfMember     string        // this instance's member id
	currentLeader  string        // member id currently holding the lease ("" = unknown)
	selfIsLeader   bool          // this instance already leads
	selfUptime     time.Duration // this process has been up this long
	finishedAgo    time.Duration // the upgrade settled this long ago
	attempted      bool          // this upgrade's one attempt is spent
	anotherRunning bool          // a newer sync update is currently running
}

// syncUpdateSettledForReturn reports whether a sync update has finished
// in a state where returning leadership makes sense: the run is settled
// (phase done) and the pre-upgrade leader's own upgrade completed. A
// partial run still qualifies when the leader member itself finished —
// the return is about that member, not about the member that failed.
func syncUpdateSettledForReturn(state, phase, leaderDelivery string) bool {
	if phase != syncPhaseDone {
		return false
	}
	switch state {
	case syncStateSucceeded:
		return true
	case syncStatePartial:
		return leaderDelivery == "done"
	}
	return false
}

// shouldAutoReturnLeader is the pure return rule. Every condition in
// the design maps to one check here:
//   - the upgrade fully settled, with the pre-upgrade leader upgraded;
//   - the lease actually moved to another member;
//   - the actor is the pre-upgrade leader itself (nobody orchestrates
//     the return on its behalf, so there is exactly one candidate);
//   - it has been back up and stable for leaderReturnStableFor;
//   - the upgrade settled within leaderReturnWindow;
//   - no newer upgrade is running, and the one attempt is unspent.
func shouldAutoReturnLeader(f leaderReturnFacts) bool {
	if f.attempted || f.anotherRunning {
		return false
	}
	if !syncUpdateSettledForReturn(f.opState, f.opPhase, f.leaderDelivery) {
		return false
	}
	if f.preLeader == "" || f.preLeader != f.selfMember {
		return false
	}
	if f.selfIsLeader || f.currentLeader == "" || f.currentLeader == f.preLeader {
		return false
	}
	if f.selfUptime < leaderReturnStableFor {
		return false
	}
	// No lower bound on finishedAgo: the settle time was written by
	// whichever instance orchestrated the verify phase, and a small
	// clock skew against it must not block the return.
	return f.finishedAgo <= leaderReturnWindow
}

// leaderDeliveryStatus returns the delivery status of the operation's
// pinned leader member ("" when the run never staged one).
func (s *Server) leaderDeliveryStatus(op *store.ClusterOperation) string {
	for _, d := range s.syncDeliveries(op.ID) {
		if d.MemberID == op.LeaderMember {
			return d.Status
		}
	}
	return ""
}

// leaderReturnFacts gathers the decision inputs for the latest sync
// update from the shared database and this process.
func (s *Server) leaderReturnFacts(op *store.ClusterOperation) leaderReturnFacts {
	f := leaderReturnFacts{
		opState:        op.State,
		opPhase:        op.Phase,
		leaderDelivery: s.leaderDeliveryStatus(op),
		preLeader:      op.LeaderMember,
		selfMember:     s.memberID,
		selfIsLeader:   s.isLeader(),
		selfUptime:     time.Since(s.startedAt),
		finishedAgo:    time.Now().UTC().Sub(op.UpdatedAt),
		attempted:      s.st.GetSetting(leaderAutoReturnOpSetting, "") == op.ID,
		anotherRunning: s.activeSyncUpdate() != nil,
	}
	if f.selfIsLeader {
		f.currentLeader = s.memberID
	} else {
		f.currentLeader = s.leaderMemberID()
	}
	return f
}

// maybeAutoReturnLeader evaluates the return rule once and, when it
// fires, spends the upgrade's single attempt on a stepdown request
// through the manual takeover's channel. The current leader honors the
// request on its next lease round and this instance claims the freed
// lease by the normal race, so the handover reuses the tested path
// end to end. A failed attempt is logged and audited, never retried:
// the marker is written before the request, so neither this process
// nor a restarted one asks twice for the same upgrade.
func (s *Server) maybeAutoReturnLeader() {
	if s.bus == nil {
		return // single-instance panel: no lease ever moves
	}
	op := s.latestSyncUpdate()
	if op == nil || op.LeaderMember == "" {
		return
	}
	f := s.leaderReturnFacts(op)
	if !shouldAutoReturnLeader(f) {
		return
	}
	s.st.SetSetting(leaderAutoReturnOpSetting, op.ID)
	hosts := s.memberHosts()
	from := s.memberName(hosts, f.currentLeader)
	to := s.memberName(hosts, f.preLeader)
	if err := s.requestLeaderTakeover(); err != nil {
		s.log.Warn("leader auto-return: record the stepdown request", "op", op.ID, "err", err)
		s.audit("system", "ha.leader_auto_return", from,
			fmt.Sprintf("同步升级 %s 后 Leader 自动归位失败（%s → %s）：%v", op.Target, from, to, err), "")
		return
	}
	s.log.Info("leader auto-return: asked the current leader to step down", "op", op.ID, "from", f.currentLeader, "to", f.preLeader)
	s.audit("system", "ha.leader_auto_return", from,
		fmt.Sprintf("同步升级 %s 完成后，Leader 由 %s 自动归位到升级前的 %s（已发出 stepdown 请求）", op.Target, from, to), "")
}

// runLeaderAutoReturn ticks the return evaluation until ctx ends. It
// runs on every cluster instance; the rule itself singles out the
// pre-upgrade leader as the only actor.
func (s *Server) runLeaderAutoReturn(ctx context.Context) {
	t := time.NewTicker(leaderReturnTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.maybeAutoReturnLeader()
		}
	}
}

// leaderReturnStatus summarizes where the latest sync upgrade's leader
// return stands, for GET /ha/status: "returned" (the lease is with the
// pre-upgrade leader, whether it came back or never left), "pending"
// (settled, inside the window, attempt not yet spent), "attempted"
// (the one request was spent and the lease has not come back), or
// "skipped" (the window closed before any attempt). "" means the
// upgrade is not one a return applies to.
func (s *Server) leaderReturnStatus(op *store.ClusterOperation) string {
	if op == nil || op.LeaderMember == "" ||
		!syncUpdateSettledForReturn(op.State, op.Phase, s.leaderDeliveryStatus(op)) {
		return ""
	}
	if s.leaderMemberID() == op.LeaderMember {
		return "returned"
	}
	if s.st.GetSetting(leaderAutoReturnOpSetting, "") == op.ID {
		return "attempted"
	}
	if time.Now().UTC().Sub(op.UpdatedAt) <= leaderReturnWindow {
		return "pending"
	}
	return "skipped"
}
