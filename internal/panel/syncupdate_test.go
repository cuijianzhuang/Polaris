package panel

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// setMemberRow rewrites a server's cluster_members row as if its panel
// had (re)started at the given version and just checked in.
func setMemberRow(t *testing.T, s *Server, version string, startedAt time.Time) {
	t.Helper()
	err := s.st.DB.Model(&store.ClusterMember{}).Where("member_id = ?", s.memberID).
		Updates(map[string]any{"version": version, "started_at": startedAt, "seen_at": time.Now().UTC()}).Error
	if err != nil {
		t.Fatal(err)
	}
}

func syncOpOf(t *testing.T, s *Server, opID string) store.ClusterOperation {
	t.Helper()
	var op store.ClusterOperation
	if err := s.st.DB.First(&op, "id = ?", opID).Error; err != nil {
		t.Fatal(err)
	}
	return op
}

// syncPair returns a leader (a) and a standby (b) on one database, both
// on v0.1.15 with the updater installed.
func syncPair(t *testing.T) (a, b *Server) {
	t.Helper()
	ss := clusterServers(t, "", 2)
	a, b = ss[0], ss[1]
	b.leader.Store(false) // SQLite makes everyone leader; keep one driver
	withVersion(t, "v0.1.15")
	withUpdater(t, true)
	past := time.Now().Add(-time.Hour).UTC()
	setMemberRow(t, a, "v0.1.15", past)
	setMemberRow(t, b, "v0.1.15", past)
	return a, b
}

// driveStandbyDone runs the standby stage of a sync update to a receipt-
// confirmed completion for b.
func driveStandbyDone(t *testing.T, a, b *Server, opID string) {
	t.Helper()
	a.processSyncUpdate() // stage the standby delivery
	if d := deliveryOf(t, a, opID, b.memberID); d.Status != "pending" {
		t.Fatalf("standby delivery not staged: %+v", d)
	}
	if n := len(a.syncDeliveries(opID)); n != 1 {
		t.Fatalf("leader got a delivery during the standby stage: %d deliveries", n)
	}
	b.processClusterOperations()
	if d := deliveryOf(t, b, opID, b.memberID); d.Status != "accepted" {
		t.Fatalf("standby delivery not accepted: %+v", d)
	}
	writeUpdateResult(t, b, "ok=1\naction=update\narg=\nfrom=v0.1.15\nto=v0.1.20\nlog:\nok\n")
	b.checkUpdateResult()
	if d := deliveryOf(t, b, opID, b.memberID); d.Status != "done" || d.AfterVersion != "v0.1.20" {
		t.Fatalf("standby receipt not recorded: %+v", d)
	}
	setMemberRow(t, b, "v0.1.20", time.Now().UTC())
	a.processSyncUpdate() // standby settled -> leader phase
	if op := syncOpOf(t, a, opID); op.Phase != syncPhaseLeader {
		t.Fatalf("phase = %q, want leader", op.Phase)
	}
}

func TestSyncUpdateFullFlow(t *testing.T) {
	a, b := syncPair(t)
	opID, err := a.startSyncUpdate("v0.1.20", false)
	if err != nil {
		t.Fatal(err)
	}
	driveStandbyDone(t, a, b, opID)

	// Leader phase: suppression is queued for the standby and the leader
	// has no delivery until it is in effect.
	a.processSyncUpdate()
	sup := a.linkedOp(opID, "suppress")
	if sup == nil {
		t.Fatal("suppress operation not created")
	}
	if n := len(a.syncDeliveries(opID)); n != 1 {
		t.Fatalf("leader delivery created before suppression: %d deliveries", n)
	}
	b.processClusterOperations()
	if _, err := os.Stat(b.failoverSuppressPath()); err != nil {
		t.Fatalf("suppression file not written on the standby: %v", err)
	}
	a.processSyncUpdate() // suppression accepted -> leader delivery
	if d := deliveryOf(t, a, opID, a.memberID); d.Status != "pending" {
		t.Fatalf("leader delivery not staged: %+v", d)
	}
	a.processClusterOperations()
	writeUpdateResult(t, a, "ok=1\naction=update\narg=\nfrom=v0.1.15\nto=v0.1.20\nlog:\nok\n")
	a.checkUpdateResult()
	if d := deliveryOf(t, a, opID, a.memberID); d.Status != "done" {
		t.Fatalf("leader receipt not recorded: %+v", d)
	}
	setMemberRow(t, a, "v0.1.20", time.Now().UTC())
	a.processSyncUpdate() // leader done -> verify
	a.processSyncUpdate() // verify settles the operation
	op := syncOpOf(t, a, opID)
	if op.State != syncStateSucceeded || op.Phase != syncPhaseDone {
		t.Fatalf("operation not settled as succeeded: %+v", op)
	}
	if !strings.Contains(op.Detail, "全部 2 台") {
		t.Fatalf("detail = %q", op.Detail)
	}
	// Wrap-up released the suppression on the standby.
	if a.linkedOp(opID, "unsuppress") == nil {
		t.Fatal("unsuppress operation not created")
	}
	b.processClusterOperations()
	if _, err := os.Stat(b.failoverSuppressPath()); !os.IsNotExist(err) {
		t.Fatal("suppression file not removed")
	}
}

func TestSyncUpdateStandbyFailureAborts(t *testing.T) {
	a, b := syncPair(t)
	opID, err := a.startSyncUpdate("v0.1.20", false)
	if err != nil {
		t.Fatal(err)
	}
	a.processSyncUpdate()
	b.processClusterOperations()
	writeUpdateResult(t, b, "ok=0\naction=update\narg=\nfrom=v0.1.15\nto=v0.1.15\nlog:\n✗ 下载失败\n")
	b.checkUpdateResult()
	if d := deliveryOf(t, b, opID, b.memberID); d.Status != "failed" {
		t.Fatalf("standby failure not recorded: %+v", d)
	}
	a.processSyncUpdate()
	op := syncOpOf(t, a, opID)
	if op.State != syncStateAborted {
		t.Fatalf("operation state = %q, want aborted (%+v)", op.State, op)
	}
	if n := len(a.syncDeliveries(opID)); n != 1 {
		t.Fatalf("leader was given a delivery after a standby failure: %d", n)
	}
	if a.linkedOp(opID, "suppress") != nil {
		t.Fatal("suppression queued for an aborted upgrade")
	}
}

func TestSyncUpdateDuplicateRejectedAndStaleTakenOver(t *testing.T) {
	a, _ := syncPair(t)
	opID, err := a.startSyncUpdate("v0.1.20", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.startSyncUpdate("v0.1.20", false); err == nil {
		t.Fatal("second concurrent sync update accepted")
	}
	// A run abandoned past the total timeout no longer blocks new ones.
	a.st.DB.Model(&store.ClusterOperation{}).Where("id = ?", opID).
		Update("created_at", time.Now().Add(-2*time.Hour).UTC())
	if _, err := a.startSyncUpdate("v0.1.21", false); err != nil {
		t.Fatalf("stale run still blocks: %v", err)
	}
	if op := syncOpOf(t, a, opID); op.State != syncStateFailed {
		t.Fatalf("stale run state = %q, want failed", op.State)
	}
}

func TestSyncUpdateInferenceDone(t *testing.T) {
	a, b := syncPair(t)
	opID, err := a.startSyncUpdate("v0.1.20", false)
	if err != nil {
		t.Fatal(err)
	}
	a.processSyncUpdate()
	b.processClusterOperations() // accepted; no receipt ever arrives
	setMemberRow(t, b, "v0.1.20", time.Now().UTC())
	a.processSyncUpdate()
	if d := deliveryOf(t, a, opID, b.memberID); d.Status != "done" {
		t.Fatalf("outcome not inferred as done: %+v", d)
	}
	if op := syncOpOf(t, a, opID); op.Phase != syncPhaseLeader {
		t.Fatalf("phase = %q, want leader", op.Phase)
	}
}

func TestSyncUpdateInferenceRolledBack(t *testing.T) {
	a, b := syncPair(t)
	opID, err := a.startSyncUpdate("v0.1.20", false)
	if err != nil {
		t.Fatal(err)
	}
	a.processSyncUpdate()
	b.processClusterOperations()
	// The standby restarted during the phase but is back on the old
	// version: its updater rolled it back.
	setMemberRow(t, b, "v0.1.15", time.Now().UTC())
	a.processSyncUpdate()
	if d := deliveryOf(t, a, opID, b.memberID); d.Status != "failed" {
		t.Fatalf("rollback not inferred as failed: %+v", d)
	}
	if op := syncOpOf(t, a, opID); op.State != syncStateAborted {
		t.Fatalf("operation state = %q, want aborted", op.State)
	}
}

func TestSyncUpdateStandbyTimeout(t *testing.T) {
	a, b := syncPair(t)
	opID, err := a.startSyncUpdate("v0.1.20", false)
	if err != nil {
		t.Fatal(err)
	}
	a.processSyncUpdate() // staged; the standby never picks it up
	a.st.DB.Model(&store.ClusterOperation{}).Where("id = ?", opID).
		Update("created_at", time.Now().Add(-31*time.Minute).UTC())
	a.processSyncUpdate()
	if d := deliveryOf(t, a, opID, b.memberID); d.Status != "failed" {
		t.Fatalf("timed-out delivery not failed: %+v", d)
	}
	if op := syncOpOf(t, a, opID); op.State != syncStateAborted {
		t.Fatalf("operation state = %q, want aborted", op.State)
	}
}

func TestSyncUpdateSuppressRefusedForOldMember(t *testing.T) {
	a, b := syncPair(t)
	opID, err := a.startSyncUpdate("v0.1.20", false)
	if err != nil {
		t.Fatal(err)
	}
	driveStandbyDone(t, a, b, opID)
	a.processSyncUpdate() // queues the suppress operation
	sup := a.linkedOp(opID, "suppress")
	if sup == nil {
		t.Fatal("suppress operation not created")
	}
	// The standby runs a build that does not know the suppress kind: its
	// delivery keeps failing with an error.
	a.st.DB.Model(&store.ClusterDelivery{}).Where("operation_id = ?", sup.ID).
		Updates(map[string]any{"attempts": 3, "last_error": "未知集群操作"})
	a.processSyncUpdate()
	op := syncOpOf(t, a, opID)
	if op.State != syncStateFailed {
		t.Fatalf("operation state = %q, want failed (%+v)", op.State, op)
	}
	if !strings.Contains(op.Detail, "单独升级备机") {
		t.Fatalf("detail = %q", op.Detail)
	}
	if n := len(a.syncDeliveries(opID)); n != 1 {
		t.Fatalf("leader was given a delivery without suppression: %d", n)
	}
}

func TestSyncUpdateLeaderFailureReleasesSuppression(t *testing.T) {
	a, b := syncPair(t)
	opID, err := a.startSyncUpdate("v0.1.20", false)
	if err != nil {
		t.Fatal(err)
	}
	driveStandbyDone(t, a, b, opID)
	a.processSyncUpdate() // suppress queued
	b.processClusterOperations()
	a.processSyncUpdate() // leader delivery staged
	a.processClusterOperations()
	writeUpdateResult(t, a, "ok=0\naction=update\narg=\nfrom=v0.1.15\nto=v0.1.15\nlog:\n✗ 启动失败\n")
	a.checkUpdateResult()
	if d := deliveryOf(t, a, opID, a.memberID); d.Status != "failed" {
		t.Fatalf("leader failure not recorded: %+v", d)
	}
	a.processSyncUpdate()
	op := syncOpOf(t, a, opID)
	if op.State != syncStateFailed {
		t.Fatalf("operation state = %q, want failed (%+v)", op.State, op)
	}
	if a.linkedOp(opID, "unsuppress") == nil {
		t.Fatal("suppression not released after leader failure")
	}
	b.processClusterOperations()
	if _, err := os.Stat(b.failoverSuppressPath()); !os.IsNotExist(err) {
		t.Fatal("suppression file not removed after leader failure")
	}
}

func TestRequestClusterUpdateCreatesSyncOp(t *testing.T) {
	a, _ := syncPair(t)
	if err := a.requestClusterUpdate("v0.1.20", "tester", false); err != nil {
		t.Fatal(err)
	}
	op := a.activeSyncUpdate()
	if op == nil || op.Target != "v0.1.20" || op.Phase != syncPhaseStandby {
		t.Fatalf("sync update not started: %+v", op)
	}
}
