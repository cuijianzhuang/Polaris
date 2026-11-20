package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Sync upgrade (design docs/design/ha-sync-upgrade.md): a leader-run
// state machine on top of cluster operations that upgrades the whole
// cluster to one target version in stages — standbys first as canaries,
// then the leader with the standby's failover watchdog suppressed —
// instead of every instance upgrading at once. The syncupdate operation
// carries its staged update deliveries directly; each instance executes
// its own delivery through the unchanged update-request path and writes
// its receipt back (recordUpdateReceipt), with member-version inference
// as the fallback. All orchestration state lives in the operation row,
// so a leader change mid-upgrade hands over cleanly.

const (
	syncPhaseStandby = "standby"
	syncPhaseLeader  = "leader"
	syncPhaseVerify  = "verify"
	syncPhaseDone    = "done"

	syncStateRunning   = "running"
	syncStateSucceeded = "succeeded"
	syncStatePartial   = "partial"
	syncStateFailed    = "failed"
	syncStateAborted   = "aborted"

	// One stage may take a while: a standby's panel can need ~5 minutes
	// to come back, and the updater itself allows 600s of health wait.
	syncPhaseTimeout = 30 * time.Minute
	syncTotalTimeout = 60 * time.Minute
)

// resolveSyncTarget turns "" / "latest" into the concrete newest release
// tag, so every instance of a sync upgrade aims at the same version.
func (s *Server) resolveSyncTarget(target string) (string, error) {
	if target != "" && target != "latest" {
		return target, nil
	}
	ctx, cancel := context.WithTimeout(s.ctxBG(), 30*time.Second)
	defer cancel()
	return s.latestPanelVersion(ctx, false)
}

// requestClusterUpdate is the cluster-mode path of requestPanelUpdate:
// a staged sync upgrade (standbys first, watchdog suppressed, leader
// last) instead of upgrading every instance at once. The target is
// resolved to a concrete tag here so every instance aims at the same
// release.
func (s *Server) requestClusterUpdate(target, who string, thenAgents bool) error {
	resolved, err := s.resolveSyncTarget(target)
	if err != nil {
		return fmt.Errorf("解析目标版本失败：%w", err)
	}
	if _, err := s.startSyncUpdate(resolved, thenAgents); err != nil {
		return err
	}
	s.audit(who, "panel.update_request", resolved, map[bool]string{true: "panel + agents", false: "panel"}[thenAgents], "")
	s.processSyncUpdate()
	return nil
}

// startSyncUpdate records a new sync update operation in the standby
// phase. Only one may run at a time; a run that outlived the total
// timeout without a leader to settle it is closed out first.
func (s *Server) startSyncUpdate(target string, thenAgents bool) (string, error) {
	now := time.Now().UTC()
	var actives []store.ClusterOperation
	s.st.DB.Where("kind = ? AND state = ?", "syncupdate", syncStateRunning).Order("created_at").Find(&actives)
	for _, a := range actives {
		if now.Sub(a.CreatedAt) < syncTotalTimeout+5*time.Minute {
			return "", fmt.Errorf("已有进行中的同步升级（目标 %s），请等它结束后再试", a.Target)
		}
		s.st.DB.Model(&store.ClusterOperation{}).Where("id = ? AND state = ?", a.ID, syncStateRunning).
			Updates(map[string]any{"state": syncStateFailed, "phase": syncPhaseDone, "detail": "编排超时未推进，已自动结束", "updated_at": now})
	}
	op := store.ClusterOperation{ID: store.NewID(), Kind: "syncupdate", Target: target,
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
		State: syncStateRunning, Phase: syncPhaseStandby, UpdatedAt: now}
	if err := s.st.DB.Create(&op).Error; err != nil {
		return "", err
	}
	s.st.SetSetting("sync_update_then_agents", strconv.FormatBool(thenAgents))
	if s.bus != nil {
		_ = s.bus.publish("cluster-ops", op.ID)
	}
	return op.ID, nil
}

// activeSyncUpdate returns the running sync update, if any.
func (s *Server) activeSyncUpdate() *store.ClusterOperation {
	var op store.ClusterOperation
	if err := s.st.DB.Where("kind = ? AND state = ?", "syncupdate", syncStateRunning).Order("created_at desc").First(&op).Error; err != nil {
		return nil
	}
	return &op
}

// latestSyncUpdate returns the most recent sync update in any state.
func (s *Server) latestSyncUpdate() *store.ClusterOperation {
	var op store.ClusterOperation
	if err := s.st.DB.Where("kind = ?", "syncupdate").Order("created_at desc").First(&op).Error; err != nil {
		return nil
	}
	return &op
}

func (s *Server) syncDeliveries(opID string) []store.ClusterDelivery {
	var ds []store.ClusterDelivery
	s.st.DB.Where("operation_id = ?", opID).Order("next_at").Find(&ds)
	return ds
}

// onlineMemberRows returns the freshest member row per member id for
// members seen within the online window.
func (s *Server) onlineMemberRows() map[string]store.ClusterMember {
	var rows []store.ClusterMember
	s.st.DB.Where("seen_at > ?", time.Now().UTC().Add(-memberOnline)).Order("seen_at desc").Find(&rows)
	out := map[string]store.ClusterMember{}
	for _, r := range rows {
		if r.MemberID == "" {
			continue
		}
		if _, ok := out[r.MemberID]; !ok {
			out[r.MemberID] = r
		}
	}
	return out
}

// memberHosts maps member ids to hostnames for human-readable details.
func (s *Server) memberHosts() map[string]string {
	var rows []store.ClusterMember
	s.st.DB.Order("seen_at desc").Find(&rows)
	out := map[string]string{}
	for _, r := range rows {
		if r.MemberID == "" {
			continue
		}
		if _, ok := out[r.MemberID]; !ok {
			out[r.MemberID] = r.Host
		}
	}
	return out
}

func (s *Server) memberName(hosts map[string]string, memberID string) string {
	if h := hosts[memberID]; h != "" {
		return h
	}
	return memberID
}

// leaderMemberID resolves the member id of the instance currently
// holding the leader lease.
func (s *Server) leaderMemberID() string {
	holder := s.procID
	if s.st.DB.Dialector.Name() == "postgres" {
		var l store.ClusterLease
		if err := s.st.DB.Where("name = ? AND expires_at > now()", leaseName).First(&l).Error; err == nil {
			holder = l.Holder
		}
	}
	var m store.ClusterMember
	if err := s.st.DB.Where("instance_id = ?", holder).Order("seen_at desc").First(&m).Error; err == nil && m.MemberID != "" {
		return m.MemberID
	}
	if holder == s.procID {
		return s.memberID
	}
	return ""
}

// runSyncUpdateOrchestrator ticks the sync update state machine.
func (s *Server) runSyncUpdateOrchestrator(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.processSyncUpdate()
		}
	}
}

// processSyncUpdate advances the active sync update by one step. Only
// the leader orchestrates; every transition is a compare-and-swap on
// the operation row, so a lease change mid-step cannot double-advance.
func (s *Server) processSyncUpdate() {
	if !s.isLeader() {
		return
	}
	op := s.activeSyncUpdate()
	if op == nil {
		return
	}
	now := time.Now().UTC()
	if op.Phase != syncPhaseVerify && now.Sub(op.CreatedAt) > syncTotalTimeout {
		detail := "同步升级总时长超过 60 分钟，已停止推进"
		if s.finishSyncUpdate(op, syncStateFailed, detail) {
			s.releaseSuppression(op)
			s.notifySyncResult(op, syncStateFailed, detail)
		}
		return
	}
	switch op.Phase {
	case syncPhaseStandby:
		s.syncStandbyPhase(op, now)
	case syncPhaseLeader:
		s.syncLeaderPhase(op, now)
	case syncPhaseVerify:
		s.syncVerifyPhase(op, now)
	}
}

// advanceSyncPhase moves the operation to the next phase if it is still
// in the phase the caller read.
func (s *Server) advanceSyncPhase(op *store.ClusterOperation, phase string) bool {
	r := s.st.DB.Model(&store.ClusterOperation{}).
		Where("id = ? AND state = ? AND phase = ?", op.ID, syncStateRunning, op.Phase).
		Updates(map[string]any{"phase": phase, "updated_at": time.Now().UTC()})
	return r.Error == nil && r.RowsAffected == 1
}

// finishSyncUpdate settles the operation in a terminal state (same CAS
// discipline as advancing) and audits the outcome. Only the caller that
// wins the CAS may notify.
func (s *Server) finishSyncUpdate(op *store.ClusterOperation, state, detail string) bool {
	r := s.st.DB.Model(&store.ClusterOperation{}).
		Where("id = ? AND state = ? AND phase = ?", op.ID, syncStateRunning, op.Phase).
		Updates(map[string]any{"state": state, "phase": syncPhaseDone, "detail": truncate(detail, 500), "updated_at": time.Now().UTC()})
	if r.Error != nil || r.RowsAffected != 1 {
		return false
	}
	op.State, op.Phase, op.Detail = state, syncPhaseDone, detail
	s.audit("system", "panel.sync_update", op.Target, detail, "")
	return true
}

func (s *Server) notifySyncResult(op *store.ClusterOperation, state, detail string) {
	var b strings.Builder
	switch state {
	case syncStateSucceeded:
		b.WriteString("✅ 同步升级完成：" + op.Target)
	case syncStatePartial:
		b.WriteString("⚠️ 同步升级部分完成：" + op.Target)
	case syncStateAborted:
		b.WriteString("⛔ 同步升级已中止：" + op.Target)
	default:
		b.WriteString("❌ 同步升级失败：" + op.Target)
	}
	if detail != "" {
		b.WriteString("\n" + detail)
	}
	hosts := s.memberHosts()
	for _, d := range s.syncDeliveries(op.ID) {
		line := "\n" + s.memberName(hosts, d.MemberID) + "：" + orDefault(d.BeforeVersion, "?") + " → "
		switch d.Status {
		case "done":
			line += orDefault(d.AfterVersion, op.Target)
		case "failed":
			line += "失败"
			if d.LastError != "" {
				line += "（" + truncate(d.LastError, 80) + "）"
			}
		default:
			line += d.Status
		}
		b.WriteString(line)
	}
	s.notify(Event{Kind: "panel_update", Text: b.String()})
}

// inferSyncDeliveries folds member-table inference into this operation's
// accepted deliveries that never produced an authoritative receipt.
func (s *Server) inferSyncDeliveries(op *store.ClusterOperation, since time.Time) {
	for _, d := range s.syncDeliveries(op.ID) {
		if d.Status != "accepted" {
			continue
		}
		outcome, ok := inferUpdateOutcome(s.st.DB, d, op.Target, since)
		if !ok {
			continue
		}
		updates := map[string]any{"status": outcome, "finished_at": time.Now().UTC()}
		if outcome == "done" {
			updates["after_version"] = op.Target
			updates["last_error"] = ""
		} else {
			updates["last_error"] = "面板以旧版本重启，升级已被本机回滚（按成员版本推断）"
		}
		s.st.DB.Model(&store.ClusterDelivery{}).
			Where("operation_id = ? AND member_id = ? AND status = ?", op.ID, d.MemberID, "accepted").
			Updates(updates)
	}
}

// failOpenDeliveries settles every still-open delivery of the operation
// as failed with the given reason (phase timeouts).
func (s *Server) failOpenDeliveries(op *store.ClusterOperation, reason string) {
	s.st.DB.Model(&store.ClusterDelivery{}).
		Where("operation_id = ? AND status IN ?", op.ID, []string{"pending", "accepted"}).
		Updates(map[string]any{"status": "failed", "last_error": reason, "finished_at": time.Now().UTC()})
}

// evalSyncDeliveries summarizes a delivery set: how many are still open
// and which have failed.
func evalSyncDeliveries(ds []store.ClusterDelivery) (open int, failed []store.ClusterDelivery) {
	for _, d := range ds {
		switch d.Status {
		case "failed":
			failed = append(failed, d)
		case "done":
		default:
			open++
		}
	}
	return open, failed
}

func (s *Server) syncStandbyPhase(op *store.ClusterOperation, now time.Time) {
	ds := s.syncDeliveries(op.ID)
	if len(ds) == 0 {
		// Stage entry: every online member except the leader upgrades
		// first, as the canary. Deliveries are created only now, so no
		// member can start before its stage.
		leaderID := s.leaderMemberID()
		var ids []string
		for id := range s.onlineMemberRows() {
			if id != leaderID {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		if len(ids) == 0 {
			s.advanceSyncPhase(op, syncPhaseLeader)
			return
		}
		if err := s.addSyncDeliveries(*op, ids); err != nil {
			s.log.Warn("sync update: stage standby deliveries", "op", op.ID, "err", err)
			return
		}
		if s.bus != nil {
			_ = s.bus.publish("cluster-ops", op.ID)
		}
		return
	}
	s.inferSyncDeliveries(op, op.CreatedAt)
	if now.Sub(op.CreatedAt) > syncPhaseTimeout {
		s.failOpenDeliveries(op, "阶段超时，未收到完成回执（按超时判失败）")
	}
	ds = s.syncDeliveries(op.ID)
	open, failed := evalSyncDeliveries(ds)
	if len(failed) > 0 {
		hosts := s.memberHosts()
		var names []string
		for _, d := range failed {
			names = append(names, s.memberName(hosts, d.MemberID))
		}
		detail := fmt.Sprintf("备机阶段失败：%s，主机未升级", strings.Join(names, "、"))
		if s.finishSyncUpdate(op, syncStateAborted, detail) {
			s.notifySyncResult(op, syncStateAborted, detail)
		}
		return
	}
	if open == 0 {
		s.advanceSyncPhase(op, syncPhaseLeader)
	}
}

// syncPhaseStart anchors the leader phase for timeouts and inference:
// the suppress operation is created as the phase begins.
func (s *Server) syncPhaseStart(op *store.ClusterOperation) time.Time {
	if sup := s.linkedOp(op.ID, "suppress"); sup != nil {
		return sup.CreatedAt
	}
	return op.UpdatedAt
}

func (s *Server) syncLeaderPhase(op *store.ClusterOperation, now time.Time) {
	// Pin the leader stage's target the first time this phase runs: the
	// lease may move while the upgrade runs, the target must not.
	target := op.LeaderMember
	if target == "" {
		target = s.leaderMemberID()
		if target == "" {
			return
		}
		r := s.st.DB.Model(&store.ClusterOperation{}).
			Where("id = ? AND state = ? AND leader_member = ?", op.ID, syncStateRunning, "").
			Updates(map[string]any{"leader_member": target, "updated_at": now})
		if r.Error != nil || r.RowsAffected != 1 {
			return
		}
		op.LeaderMember = target
	}
	phaseStart := s.syncPhaseStart(op)
	findLeader := func() *store.ClusterDelivery {
		for _, d := range s.syncDeliveries(op.ID) {
			if d.MemberID == target {
				dd := d
				return &dd
			}
		}
		return nil
	}
	leaderD := findLeader()
	if leaderD != nil {
		s.inferSyncDeliveries(op, phaseStart)
		leaderD = findLeader()
	}
	if leaderD != nil {
		switch leaderD.Status {
		case "done":
			s.advanceSyncPhase(op, syncPhaseVerify)
			return
		case "failed":
			detail := fmt.Sprintf("主机阶段失败：%s 升级未完成，备机已在 %s，请人工决定重跑同步升级或单独处理主机", s.memberName(s.memberHosts(), target), op.Target)
			if s.finishSyncUpdate(op, syncStateFailed, detail) {
				s.releaseSuppression(op)
				s.notifySyncResult(op, syncStateFailed, detail)
			}
			return
		}
	}
	// Still waiting — for the suppression, for the leader's own delivery
	// to be created, or for its outcome. The phase timeout bounds it all.
	if now.Sub(phaseStart) > syncPhaseTimeout {
		s.failOpenDeliveries(op, "阶段超时，未收到完成回执（按超时判失败）")
		detail := "主机阶段超时：看门狗抑制或主机升级未在 30 分钟内完成"
		if s.finishSyncUpdate(op, syncStateFailed, detail) {
			s.releaseSuppression(op)
			s.notifySyncResult(op, syncStateFailed, detail)
		}
		return
	}
	if leaderD == nil {
		// The watchdog must be suppressed before the leader restarts.
		// With no standby in this upgrade there is no watchdog to silence.
		if len(s.syncDeliveries(op.ID)) > 0 {
			ready, refuse := s.ensureSuppression(op)
			if refuse != "" {
				if s.finishSyncUpdate(op, syncStateFailed, refuse) {
					s.releaseSuppression(op)
					s.notifySyncResult(op, syncStateFailed, refuse)
				}
				return
			}
			if !ready {
				return
			}
		}
		// The phase has been in the database since the CAS into it; only
		// now, with suppression in effect, is the leader's own upgrade
		// triggered.
		if err := s.addSyncDeliveries(*op, []string{target}); err != nil {
			s.log.Warn("sync update: stage leader delivery", "op", op.ID, "err", err)
			return
		}
		if s.bus != nil {
			_ = s.bus.publish("cluster-ops", op.ID)
		}
	}
}

func (s *Server) syncVerifyPhase(op *store.ClusterOperation, now time.Time) {
	s.releaseSuppression(op)
	ds := s.syncDeliveries(op.ID)
	members := s.onlineMemberRows()
	hosts := s.memberHosts()
	inSet := map[string]bool{}
	done, bad := 0, 0
	var notes []string
	for _, d := range ds {
		inSet[d.MemberID] = true
		name := s.memberName(hosts, d.MemberID)
		switch d.Status {
		case "done":
			done++
			// A member that restarted during this upgrade on a version
			// other than the target contradicts its receipt; a stale
			// member row (not restarted since) does not.
			if m, ok := members[d.MemberID]; ok && m.Version != op.Target && !m.StartedAt.Before(op.CreatedAt) {
				bad++
				notes = append(notes, fmt.Sprintf("%s 当前版本为 %s", name, m.Version))
			}
		case "failed":
			bad++
			notes = append(notes, name+" 升级失败")
		default:
			bad++
			notes = append(notes, name+" 未完成")
		}
	}
	var left []string
	for id, m := range members {
		if !inSet[id] && m.Version != op.Target {
			left = append(left, s.memberName(hosts, id))
		}
	}
	state := syncStateSucceeded
	if bad > 0 {
		state = syncStatePartial
	}
	detail := fmt.Sprintf("全部 %d 台已升级到 %s", done, op.Target)
	if bad > 0 {
		detail = fmt.Sprintf("完成 %d 台，异常 %d 台：%s", done, bad, strings.Join(notes, "；"))
	}
	if len(left) > 0 {
		detail += fmt.Sprintf("；未包含在本次升级：%s", strings.Join(left, "、"))
	}
	if !s.finishSyncUpdate(op, state, detail) {
		return
	}
	s.notifySyncResult(op, state, detail)
	// The agent rollout follows a successful panel upgrade once, from
	// the orchestration wrap-up — not once per instance.
	if s.st.GetSetting("sync_update_then_agents", "false") == "true" {
		s.st.SetSetting("sync_update_then_agents", "false")
		go s.upgradeAgentsAfterUpdate()
	}
}

// linkedOp finds a staged child operation (suppress / unsuppress) of a
// sync update: the child carries the sync update's id as its target.
func (s *Server) linkedOp(syncOpID, kind string) *store.ClusterOperation {
	var o store.ClusterOperation
	if err := s.st.DB.Where("kind = ? AND target = ?", kind, syncOpID).Order("created_at").First(&o).Error; err != nil {
		return nil
	}
	return &o
}

// ensureSuppression makes sure the failover watchdog on the upgraded
// standbys is suppressed before the leader restarts. It reports whether
// suppression is in effect, or a refusal reason when a standby clearly
// cannot execute it (an old build retrying an unknown operation).
func (s *Server) ensureSuppression(op *store.ClusterOperation) (ready bool, refuse string) {
	sup := s.linkedOp(op.ID, "suppress")
	if sup == nil {
		// Only members whose upgrade completed can run the watchdog
		// suppression: they are on the target build, which understands
		// the suppress operation.
		var targets []string
		for _, d := range s.syncDeliveries(op.ID) {
			if d.Status == "done" && d.MemberID != op.LeaderMember {
				targets = append(targets, d.MemberID)
			}
		}
		if len(targets) == 0 {
			return false, "没有已升级完成的备机可落看门狗抑制，拒绝升级主机；请先单独升级备机"
		}
		if _, err := s.queueClusterOperationScoped("suppress", op.ID, targets); err != nil {
			s.log.Warn("sync update: queue suppression", "op", op.ID, "err", err)
			return false, ""
		}
		return false, ""
	}
	var sds []store.ClusterDelivery
	s.st.DB.Where("operation_id = ?", sup.ID).Find(&sds)
	if len(sds) == 0 {
		return false, ""
	}
	for _, d := range sds {
		if d.Status != "accepted" {
			if d.Attempts >= 3 && d.LastError != "" {
				return false, fmt.Sprintf("备机未能执行看门狗抑制（%s），疑似版本过旧，拒绝升级主机；请先单独升级备机", truncate(d.LastError, 80))
			}
			return false, ""
		}
	}
	return true, ""
}

// releaseSuppression lifts the watchdog suppression once the leader
// stage is settled. The suppression file's hard expiry covers a member
// that never processes this.
func (s *Server) releaseSuppression(op *store.ClusterOperation) {
	sup := s.linkedOp(op.ID, "suppress")
	if sup == nil || s.linkedOp(op.ID, "unsuppress") != nil {
		return
	}
	var sds []store.ClusterDelivery
	s.st.DB.Where("operation_id = ?", sup.ID).Find(&sds)
	var targets []string
	for _, d := range sds {
		targets = append(targets, d.MemberID)
	}
	if len(targets) == 0 {
		return
	}
	if _, err := s.queueClusterOperationScoped("unsuppress", op.ID, targets); err != nil {
		s.log.Warn("sync update: release suppression", "op", op.ID, "err", err)
	}
}

// haSyncUpdate is POST /ha/sync-update {target?, agents?} (sudo).
func (s *Server) haSyncUpdate(c *gin.Context) {
	var req struct {
		Target string `json:"target"`
		Agents bool   `json:"agents"`
	}
	_ = c.ShouldBindJSON(&req)
	if s.bus == nil {
		badRequest(c, errors.New("当前是单机模式，请在「设置 → 版本与更新」中升级本机"))
		return
	}
	if Version == "dev" {
		badRequest(c, errDevUpdate)
		return
	}
	if !s.updaterInstalled() {
		badRequest(c, errUpdaterMissing)
		return
	}
	if _, err := os.Stat(s.updateRequestPath()); err == nil {
		badRequest(c, errUpdatePending)
		return
	}
	target := strings.TrimSpace(req.Target)
	if target == "" || target == "latest" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
		defer cancel()
		v, err := s.latestPanelVersion(ctx, false)
		if err != nil {
			badRequest(c, err)
			return
		}
		target = v
	}
	id, err := s.startSyncUpdate(target, req.Agents)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "panel.sync_update", target, "发起同步升级")
	s.processSyncUpdate()
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "id": id, "target": target})
}
