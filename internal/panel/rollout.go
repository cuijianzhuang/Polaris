package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Staged upgrades: a canary batch first, then the rest a batch at a time.
// Each batch must stay healthy for a soak period before the next starts; a
// node that does not come back healthy in time stops the rollout, and core
// upgrades in the failed batch are rolled back. The leader drives rollouts
// under a lease (like refund jobs), so one survives restarts and failover.

const (
	rolloutLease       = 6 * time.Minute // longer than one batch of agent upgrades
	rolloutNodeTimeout = 10 * time.Minute
)

// Hooks for tests: talking to real agents is not possible there.
var (
	rolloutOnline       = func(s *Server, nodeID string) bool { return s.hub.Online(nodeID) }
	rolloutUpgradeAgent = func(s *Server, ctx context.Context, n *store.Node) error { return s.upgradeAgent(ctx, n) }
	rolloutAgentVersion = func() string { return Version }
)

type rolloutReq struct {
	Kind          string   `json:"kind"`
	CoreType      string   `json:"coreType"`
	Version       string   `json:"version"`
	NodeIDs       []string `json:"nodeIds"` // empty = every outdated node
	Canary        int      `json:"canary"`
	BatchSize     int      `json:"batchSize"`
	SoakSeconds   int      `json:"soakSeconds"`
	ProbeInbounds bool     `json:"probeInbounds"`
}

// newRollout validates req and stores a running rollout.
func (s *Server) newRollout(req rolloutReq, by string) (*store.UpgradeRollout, error) {
	switch req.Kind {
	case "agent":
		req.Version, req.CoreType = rolloutAgentVersion(), ""
	case "core":
		req.Version = strings.TrimPrefix(strings.TrimSpace(req.Version), "v")
		if req.CoreType == "" || req.Version == "" {
			return nil, errors.New("内核升级需要指定内核类型和版本")
		}
	default:
		return nil, errors.New("kind 应为 agent 或 core")
	}
	if req.Canary <= 0 {
		req.Canary = 1
	}
	if req.BatchSize <= 0 {
		req.BatchSize = 4
	}
	if req.SoakSeconds <= 0 {
		req.SoakSeconds = 120
	}
	req.Canary, req.BatchSize = min(req.Canary, 20), min(req.BatchSize, 20)
	req.SoakSeconds = min(req.SoakSeconds, 3600)
	ids, err := s.rolloutNodes(req)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, errors.New("没有需要升级的节点")
	}
	var running int64
	s.st.DB.Model(&store.UpgradeRollout{}).Where("state IN ?", []string{"running", "paused"}).Count(&running)
	if running > 0 {
		return nil, errors.New("已有一个升级任务在进行，请等它完成或取消")
	}
	now := time.Now()
	r := &store.UpgradeRollout{ID: store.NewID(), Kind: req.Kind, CoreType: req.CoreType, Version: req.Version,
		NodeIDs: store.NewJSON(ids), Canary: req.Canary, BatchSize: req.BatchSize, SoakSeconds: req.SoakSeconds,
		ProbeInbounds: req.ProbeInbounds, State: "running", Batch: store.NewJSON([]string{}),
		Results: store.NewJSON(map[string]store.RolloutNode{}), CreatedBy: by, CreatedAt: now, UpdatedAt: now}
	if err := s.st.DB.Create(r).Error; err != nil {
		return nil, err
	}
	return r, nil
}

// rolloutNodes is the ordered node list: the requested ones that still need
// the upgrade, or every outdated node.
func (s *Server) rolloutNodes(req rolloutReq) ([]string, error) {
	var nodes []store.Node
	q := s.st.DB.Where("revoked = ?", false).Order("name")
	if len(req.NodeIDs) > 0 {
		q = q.Where("id IN ?", req.NodeIDs)
	}
	if err := q.Find(&nodes).Error; err != nil {
		return nil, err
	}
	var ids []string
	for _, n := range nodes {
		switch req.Kind {
		case "agent":
			if n.AgentVersion != req.Version {
				ids = append(ids, n.ID)
			}
		case "core":
			var c store.Core
			if s.st.DB.First(&c, "node_id = ? AND type = ?", n.ID, req.CoreType).Error == nil && c.Version != req.Version {
				ids = append(ids, n.ID)
			}
		}
	}
	return ids, nil
}

// runRollouts is the leader's tick: claim each running rollout and advance it.
func (s *Server) runRollouts() {
	var rs []store.UpgradeRollout
	now := time.Now()
	if s.st.DB.Where("state = ? AND lease_until <= ?", "running", now).Order("created_at").Find(&rs).Error != nil {
		return
	}
	for _, r := range rs {
		if !s.isLeader() {
			return
		}
		token := store.NewID()
		claim := s.st.DB.Model(&store.UpgradeRollout{}).Where("id = ? AND state = ? AND lease_until <= ?", r.ID, "running", now).
			Updates(map[string]any{"lease_token": token, "lease_until": now.Add(rolloutLease)})
		if claim.Error != nil || claim.RowsAffected != 1 {
			continue
		}
		r.LeaseToken = token
		s.stepRollout(&r)
	}
}

// stepRollout advances r by one step and saves it (releasing the lease).
func (s *Server) stepRollout(r *store.UpgradeRollout) {
	now := time.Now()
	if len(r.Batch.V) == 0 {
		s.startBatch(r, now)
	} else {
		s.watchBatch(r, now)
	}
	r.UpdatedAt = time.Now()
	upd := map[string]any{"cursor": r.Cursor, "batch": r.Batch, "batch_started": r.BatchStarted, "healthy_since": r.HealthySince,
		"results": r.Results, "last_error": truncate(r.LastError, 500), "updated_at": r.UpdatedAt, "lease_until": time.Time{}, "lease_token": ""}
	// The state moves only from running: a pause or cancel made meanwhile wins.
	if r.State != "running" {
		upd["state"] = r.State
	}
	s.st.DB.Model(&store.UpgradeRollout{}).Where("id = ? AND lease_token = ?", r.ID, r.LeaseToken).Updates(upd)
}

func (s *Server) rolloutResult(r *store.UpgradeRollout, nodeID, state, msg, prev string) {
	if r.Results.V == nil {
		r.Results.V = map[string]store.RolloutNode{}
	}
	cur := r.Results.V[nodeID]
	if prev == "" {
		prev = cur.Prev
	}
	r.Results.V[nodeID] = store.RolloutNode{State: state, Error: truncate(msg, 300), Prev: prev, At: time.Now()}
}

// startBatch upgrades the next batch (the canary first).
func (s *Server) startBatch(r *store.UpgradeRollout, now time.Time) {
	size := r.BatchSize
	if r.Cursor == 0 {
		size = r.Canary
	}
	ids := r.NodeIDs.V
	if r.Cursor >= len(ids) {
		s.finishRollout(r, "done", "")
		return
	}
	end := min(r.Cursor+size, len(ids))
	batch := ids[r.Cursor:end]
	r.Cursor = end
	ctx, cancel := context.WithTimeout(s.ctxBG(), 4*time.Minute)
	defer cancel()
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		started []string
	)
	for _, id := range batch {
		var n store.Node
		if s.st.DB.First(&n, "id = ?", id).Error != nil || n.Revoked {
			s.rolloutResult(r, id, "skipped", "节点已删除或撤销", "")
			continue
		}
		if !rolloutOnline(s, id) {
			// An offline node cannot be upgraded; it is not a failed upgrade.
			s.rolloutResult(r, id, "skipped", "节点不在线", "")
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			prev, err := s.upgradeOne(ctx, r, &n)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				s.rolloutResult(r, n.ID, "failed", err.Error(), prev)
				return
			}
			s.rolloutResult(r, n.ID, "upgrading", "", prev)
			started = append(started, n.ID)
		}()
	}
	wg.Wait()
	for _, id := range batch {
		if r.Results.V[id].State == "failed" {
			r.Batch = store.NewJSON(started)
			s.failRollout(r, fmt.Sprintf("节点 %s 升级失败：%s", s.nodeName(id), r.Results.V[id].Error))
			return
		}
	}
	if len(started) == 0 {
		// Everything in the batch was skipped: go on with the next one.
		r.Batch, r.BatchStarted, r.HealthySince = store.NewJSON([]string{}), nil, nil
		return
	}
	r.Batch, r.BatchStarted, r.HealthySince = store.NewJSON(started), &now, nil
}

// upgradeOne starts the upgrade of one node; prev is the core version it
// ran before (for rollback).
func (s *Server) upgradeOne(ctx context.Context, r *store.UpgradeRollout, n *store.Node) (prev string, err error) {
	if r.Kind == "agent" {
		return "", rolloutUpgradeAgent(s, ctx, n)
	}
	var c store.Core
	if err := s.st.DB.First(&c, "node_id = ? AND type = ?", n.ID, r.CoreType).Error; err != nil {
		return "", errors.New("节点上没有这个内核")
	}
	prev = orDefault(c.RunningVersion, c.Version)
	if _, _, err := s.setCoreVersion(n.ID, model.CoreType(r.CoreType), r.Version, false); err != nil {
		return prev, err
	}
	return prev, nil
}

// watchBatch waits for the batch to be healthy for the soak period.
func (s *Server) watchBatch(r *store.UpgradeRollout, now time.Time) {
	healthy := true
	for _, id := range r.Batch.V {
		ok, reason, fatal := s.rolloutNodeHealthy(r, id)
		if fatal {
			s.rolloutResult(r, id, "failed", reason, "")
			s.failRollout(r, fmt.Sprintf("节点 %s 升级后异常：%s", s.nodeName(id), reason))
			return
		}
		if !ok {
			healthy = false
			if r.BatchStarted != nil && now.Sub(*r.BatchStarted) > rolloutNodeTimeout {
				s.rolloutResult(r, id, "failed", "超时未恢复正常："+reason, "")
				s.failRollout(r, fmt.Sprintf("节点 %s 在 %d 分钟内没有恢复正常：%s", s.nodeName(id), int(rolloutNodeTimeout.Minutes()), reason))
				return
			}
		}
	}
	if !healthy {
		r.HealthySince = nil
		return
	}
	if r.HealthySince == nil {
		r.HealthySince = &now
		return
	}
	if now.Sub(*r.HealthySince) < time.Duration(r.SoakSeconds)*time.Second {
		return
	}
	for _, id := range r.Batch.V {
		s.rolloutResult(r, id, "ok", "", "")
	}
	r.Batch, r.BatchStarted, r.HealthySince = store.NewJSON([]string{}), nil, nil
	if r.Cursor >= len(r.NodeIDs.V) {
		s.finishRollout(r, "done", "")
	}
}

// rolloutNodeHealthy reports whether a node runs the target version and
// serves; fatal means it will not get there by waiting (the agent reverted
// a core that failed to start).
func (s *Server) rolloutNodeHealthy(r *store.UpgradeRollout, nodeID string) (ok bool, reason string, fatal bool) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", nodeID).Error; err != nil {
		return false, "节点已删除", true
	}
	if !rolloutOnline(s, nodeID) {
		return false, "节点离线", false
	}
	switch r.Kind {
	case "agent":
		if n.AgentVersion != r.Version {
			return false, "Agent 仍是 " + orDefault(n.AgentVersion, "旧版本"), false
		}
	case "core":
		var c store.Core
		if err := s.st.DB.First(&c, "node_id = ? AND type = ?", nodeID, r.CoreType).Error; err != nil {
			return false, "内核已删除", true
		}
		if c.Version != r.Version {
			// revertFailedUpgrades set it back: the new version did not start.
			return false, orDefault(c.Error, "新版本启动失败，节点已退回 "+c.Version), true
		}
		if c.RunningVersion != r.Version || c.Status != "running" {
			return false, fmt.Sprintf("内核 %s（%s）", orDefault(c.RunningVersion, "未运行"), orDefault(c.Status, "?")), false
		}
	}
	if n.AppliedRev < n.DesiredRev {
		return false, "配置尚未下发完成", false
	}
	if n.Status == "error" {
		return false, orDefault(n.LastError, "配置下发失败"), false
	}
	if r.ProbeInbounds {
		var ins []store.Inbound
		s.st.DB.Where("node_id = ? AND enabled = ?", nodeID, true).Find(&ins)
		for _, in := range ins {
			for _, t := range s.probeTargets(in.ID) {
				if h := s.probeAndRecord(t); !h.OK {
					return false, "入站 " + in.Tag + " 探测失败：" + h.Error, false
				}
			}
		}
	}
	return true, "", false
}

// failRollout stops r; core upgrades of the current batch are rolled back.
func (s *Server) failRollout(r *store.UpgradeRollout, msg string) {
	rolled := 0
	if r.Kind == "core" {
		for _, id := range r.Batch.V {
			res := r.Results.V[id]
			if res.Prev == "" || res.Prev == r.Version {
				continue
			}
			var c store.Core
			if s.st.DB.First(&c, "node_id = ? AND type = ?", id, r.CoreType).Error != nil || c.Version == res.Prev {
				continue // already back (the agent reverted it)
			}
			if _, _, err := s.setCoreVersion(id, model.CoreType(r.CoreType), res.Prev, false); err != nil {
				s.log.Warn("rollout rollback", "node", id, "err", err)
				continue
			}
			rolled++
			if res.State != "failed" {
				s.rolloutResult(r, id, "rolled_back", "", "")
			}
		}
	}
	if rolled > 0 {
		msg += fmt.Sprintf("；已把本批 %d 个节点退回原版本", rolled)
	}
	s.finishRollout(r, "failed", msg)
}

func (s *Server) finishRollout(r *store.UpgradeRollout, state, msg string) {
	r.State, r.LastError = state, msg
	r.Batch, r.BatchStarted, r.HealthySince = store.NewJSON([]string{}), nil, nil
	what := "Agent"
	if r.Kind == "core" {
		what = r.CoreType
	}
	ok, skipped := 0, 0
	for _, res := range r.Results.V {
		switch res.State {
		case "ok":
			ok++
		case "skipped":
			skipped++
		}
	}
	switch state {
	case "done":
		text := fmt.Sprintf("✅ 小批升级完成：%s → %s，成功 %d 个节点", what, r.Version, ok)
		if skipped > 0 {
			text += fmt.Sprintf("，跳过 %d 个（离线或已删除）", skipped)
		}
		s.notify(Event{Kind: "rollout", Text: text})
	case "failed":
		s.notify(Event{Kind: "rollout", Text: fmt.Sprintf("⛔ 小批升级已停止：%s → %s（已成功 %d 个）\n%s", what, r.Version, ok, msg)})
	}
	s.audit("system", "rollout."+state, what+" "+r.Version, msg, "")
}

func (s *Server) nodeName(id string) string {
	var n store.Node
	if s.st.DB.Select("name").First(&n, "id = ?", id).Error != nil {
		return id
	}
	return n.Name
}

// --- API ---

func (s *Server) listRollouts(c *gin.Context) {
	var rs []store.UpgradeRollout
	s.st.DB.Order("created_at desc").Limit(20).Find(&rs)
	c.JSON(http.StatusOK, gin.H{"items": rs})
}

func (s *Server) getRollout(c *gin.Context) {
	var r store.UpgradeRollout
	if err := s.st.DB.First(&r, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(http.StatusOK, r)
}

func (s *Server) createRollout(c *gin.Context) {
	var req rolloutReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	r, err := s.newRollout(req, c.GetString("admin"))
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "rollout.create", r.Kind+" "+r.CoreType+" "+r.Version, fmt.Sprintf("%d 个节点，首批 %d，每批 %d", len(r.NodeIDs.V), r.Canary, r.BatchSize))
	go s.runRollouts() // start now rather than on the next tick
	c.JSON(http.StatusOK, r)
}

// rolloutAction: pause / resume / cancel.
func (s *Server) rolloutAction(c *gin.Context) {
	from, to := map[string][]string{"pause": {"running"}, "resume": {"paused"}, "cancel": {"running", "paused"}}, map[string]string{"pause": "paused", "resume": "running", "cancel": "canceled"}
	act := c.Param("action")
	if from[act] == nil {
		notFound(c)
		return
	}
	upd := map[string]any{"state": to[act], "updated_at": time.Now()}
	if act == "resume" {
		upd["healthy_since"] = nil // soak again from the start
	}
	res := s.st.DB.Model(&store.UpgradeRollout{}).Where("id = ? AND state IN ?", c.Param("id"), from[act]).Updates(upd)
	if res.Error != nil {
		serverError(c, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		badRequest(c, errors.New("任务当前状态不允许这个操作"))
		return
	}
	s.auditCtx(c, "rollout."+act, c.Param("id"), "")
	if act == "resume" {
		go s.runRollouts()
	}
	c.Status(http.StatusNoContent)
}

// startAgentRollout is used after a panel self-update: agents follow in
// stages instead of all at once.
func (s *Server) startAgentRollout(by string) (*store.UpgradeRollout, error) {
	return s.newRollout(rolloutReq{Kind: "agent", Canary: atoiDefault(s.st.GetSetting("update_canary", "1"), 1)}, by)
}
