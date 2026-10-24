package panel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type routeScheduleRequest struct {
	Enabled          bool                    `json:"enabled"`
	Targets          model.RouteTraceRequest `json:"targets"`
	IntervalMinutes  int                     `json:"intervalMinutes"`
	ExpectedRevision *int64                  `json:"expectedRevision,omitempty"`
}

func (r *routeScheduleRequest) validate() error {
	if r.IntervalMinutes == 0 {
		r.IntervalMinutes = 60
	}
	if r.IntervalMinutes < 5 || r.IntervalMinutes > 1440 {
		return errors.New("检测间隔必须为 5–1440 分钟")
	}
	if r.Enabled || r.Targets.IPv4 != "" || r.Targets.IPv6 != "" {
		if err := r.Targets.Validate(); err != nil {
			return err
		}
	}
	if r.ExpectedRevision != nil && *r.ExpectedRevision < 0 {
		return errors.New("expectedRevision 无效")
	}
	if r.Targets.IPv4 != "" {
		r.Targets.IPv4 = netip.MustParseAddr(r.Targets.IPv4).String()
	}
	if r.Targets.IPv6 != "" {
		r.Targets.IPv6 = netip.MustParseAddr(r.Targets.IPv6).String()
	}
	return nil
}

func (s *Server) getNodeRouteSchedule(c *gin.Context) {
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	s.routeScheduleJSON(c, node)
}

func (s *Server) routeScheduleJSON(c *gin.Context, node store.Node) {
	row := store.NodeRouteSchedule{NodeID: node.ID, IntervalMinutes: 60}
	if err := s.st.DB.First(&row, "node_id = ?", node.ID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		serverError(c, err)
		return
	}
	var state store.NodeRouteScheduleState
	if err := s.st.DB.First(&state, "node_id = ? AND revision = ?", node.ID, row.Revision).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		serverError(c, err)
		return
	}
	var runtime *store.NodeRouteScheduleState
	if state.NodeID != "" {
		runtime = &state
	}
	c.JSON(http.StatusOK, gin.H{"policy": row, "runtime": runtime, "pausedByGlobalProbeSwitch": !s.probeEnabled(), "agentSupported": contains(node.AgentFeatures.V, model.RouteTraceFeature), "online": s.hub.Online(node.ID)})
}

var errRouteScheduleConflict = errors.New("回程定时配置已变化，请刷新后重试")
var errRouteScheduleUnsupported = errors.New("节点 Agent 不支持回程检测，请先升级")

func (s *Server) putNodeRouteSchedule(c *gin.Context) {
	var req routeScheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := req.validate(); err != nil {
		badRequest(c, err)
		return
	}
	var node store.Node
	changed := false
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockRouteNode(tx, c.Param("id")); err != nil {
			return err
		}
		if err := tx.First(&node, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		if req.Enabled && !contains(node.AgentFeatures.V, model.RouteTraceFeature) {
			return errRouteScheduleUnsupported
		}
		current := store.NodeRouteSchedule{NodeID: node.ID, IntervalMinutes: 60}
		err := tx.First(&current, "node_id = ?", node.ID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if req.ExpectedRevision != nil && *req.ExpectedRevision != current.Revision {
			return errRouteScheduleConflict
		}
		if err == nil && current.Enabled == req.Enabled && current.Targets.V == req.Targets && current.IntervalMinutes == req.IntervalMinutes {
			return nil
		}
		current.Enabled, current.Targets, current.IntervalMinutes = req.Enabled, store.NewJSON(req.Targets), req.IntervalMinutes
		current.Revision++
		if err := tx.Save(&current).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", node.ID).Delete(&store.NodeRouteScheduleState{}).Error; err != nil {
			return err
		}
		if current.Enabled {
			state := store.NodeRouteScheduleState{NodeID: node.ID, Revision: current.Revision, NextAt: time.Now().UTC().Add(time.Duration(current.IntervalMinutes) * time.Minute), LastState: "waiting"}
			if err := tx.Create(&state).Error; err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	if errors.Is(err, errRouteScheduleConflict) || errors.Is(err, errRouteScheduleUnsupported) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		dbError(c, err)
		return
	}
	if changed {
		s.auditCtx(c, "node.route-schedule.update", node.Name, "")
	}
	s.routeScheduleJSON(c, node)
}

// Only current scheduled observations may generate notifications. Old manual
// records have no source value and retain their prior behavior.
func routeScheduleNoticeCurrent(tx *gorm.DB, row store.NodeRouteTrace) bool {
	if row.Source != "scheduled" {
		return true
	}
	if enabled, err := routeScheduleProbeEnabled(tx); err != nil || !enabled {
		return false
	}
	var p store.NodeRouteSchedule
	if tx.First(&p, "node_id = ? AND enabled = ? AND revision = ?", row.NodeID, true, row.ScheduleRevision).Error != nil {
		return false
	}
	return (row.Family == "ipv4" && p.Targets.V.IPv4 == row.Target) || (row.Family == "ipv6" && p.Targets.V.IPv6 == row.Target)
}

func (s *Server) runScheduledRouteTraces() {
	if !s.isLeader() || !s.probeEnabled() || !s.routeScheduleBusy.CompareAndSwap(false, true) {
		return
	}
	defer s.routeScheduleBusy.Store(false)
	ctx, cancel := context.WithTimeout(s.ctxBG(), 25*time.Second)
	defer cancel()
	s.runDueRouteTraces(ctx, time.Now().UTC())
}

func (s *Server) runDueRouteTraces(ctx context.Context, now time.Time) {
	if !s.probeEnabled() || ctx.Err() != nil {
		return
	}
	var ids []string
	err := s.st.DB.Table("node_route_schedules AS p").Select("p.node_id").Joins("LEFT JOIN node_route_schedule_states AS r ON r.node_id = p.node_id").Where("p.enabled = ? AND (r.node_id IS NULL OR r.revision <> p.revision OR r.next_at <= ?)", true, now).Order("r.next_at ASC, p.node_id ASC").Limit(16).Scan(&ids).Error
	if err != nil {
		slog.Warn("select due route schedules", "error", err)
		return
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, id := range ids {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		if ctx.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func(id string) {
			defer func() { <-sem; wg.Done() }()
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 15*time.Second {
				return
			}
			req, rows, err := s.claimScheduledRouteContext(ctx, id, now)
			if err != nil {
				slog.Warn("claim route schedule", "node", id, "error", err)
				return
			}
			if len(rows) == 0 {
				return
			}
			_, saved, runErr := s.executeRouteTrace(ctx, id, req, rows)
			state := "completed"
			if runErr != nil {
				state = "failed"
				if ctx.Err() != nil {
					state = "interrupted"
				}
			}
			for _, row := range saved {
				if row.State == "interrupted" {
					state = "interrupted"
				}
			}
			// A newer configuration or claim cannot be overwritten by an old result.
			if err := s.st.DB.Model(&store.NodeRouteScheduleState{}).Where("node_id = ? AND revision = ? AND last_run_id = ? AND last_state = ?", id, rows[0].ScheduleRevision, rows[0].RunID, "running").Update("last_state", state).Error; err != nil {
				slog.Warn("finish route schedule", "node", id, "error", err)
			}
		}(id)
	}
	wg.Wait()
}

func (s *Server) claimScheduledRoute(nodeID string, now time.Time) (model.RouteTraceRequest, []store.NodeRouteTrace, error) {
	return s.claimScheduledRouteContext(context.Background(), nodeID, now)
}

func (s *Server) claimScheduledRouteContext(ctx context.Context, nodeID string, now time.Time) (model.RouteTraceRequest, []store.NodeRouteTrace, error) {
	// Presence may query shared storage; never re-enter the pool while holding
	// a node transaction. Dispatch still handles disconnects after this snapshot.
	online := s.hub.Online(nodeID)
	var req model.RouteTraceRequest
	var rows []store.NodeRouteTrace
	err := s.st.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockRouteNode(tx, nodeID); err != nil {
			return err
		}
		if enabled, err := routeScheduleProbeEnabled(tx); err != nil {
			return err
		} else if !enabled {
			return nil
		}
		var policy store.NodeRouteSchedule
		if err := tx.First(&policy, "node_id = ? AND enabled = ?", nodeID, true).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		var node store.Node
		if err := tx.First(&node, "id = ?", nodeID).Error; err != nil {
			return err
		}
		state := store.NodeRouteScheduleState{NodeID: nodeID}
		err := tx.First(&state, "node_id = ?", nodeID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		req = policy.Targets.V
		interval := 60 * time.Minute
		intervalValid := policy.IntervalMinutes >= 5 && policy.IntervalMinutes <= 1440
		if intervalValid {
			interval = time.Duration(policy.IntervalMinutes) * time.Minute
		}
		valid := intervalValid && req.Validate() == nil
		if state.Revision != policy.Revision || errors.Is(err, gorm.ErrRecordNotFound) {
			state = store.NodeRouteScheduleState{NodeID: nodeID, Revision: policy.Revision, NextAt: now.Add(max(interval, 5*time.Minute)), LastState: "waiting"}
			return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&state).Error
		}
		if state.NextAt.After(now) {
			return nil
		}
		// Advance before dispatch, including skipped nodes: one interval produces
		// at most one attempt across replicas and offline nodes cannot starve others.
		state.NextAt = now.Add(max(interval, 5*time.Minute))
		state.LastState = "running"
		switch {
		case !valid:
			state.LastState = "invalid_policy"
		case !contains(node.AgentFeatures.V, model.RouteTraceFeature):
			state.LastState = "unsupported"
		case !online:
			state.LastState = "offline"
		default:
			var active int64
			if err := tx.Model(&store.NodeRouteTrace{}).Where("node_id = ? AND state = ? AND started_at >= ?", nodeID, "running", now.Add(-30*time.Second)).Count(&active).Error; err != nil {
				return err
			}
			if active > 0 {
				state.LastState = "busy"
			}
		}
		if state.LastState == "running" {
			var err error
			rows, err = createRouteTraceRows(tx, nodeID, req, now, "scheduled", policy.Revision)
			if err != nil {
				return err
			}
			state.LastStartedAt = &now
			state.LastRunID = rows[0].RunID
		}
		return tx.Save(&state).Error
	})
	return req, rows, err
}

// Read through the transaction; using Store.GetSetting here can re-enter the
// single SQLite connection while the node's writer reservation is held.
func routeScheduleProbeEnabled(tx *gorm.DB) (bool, error) {
	var row store.Setting
	err := tx.First(&row, "key = ?", "probe_enabled").Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return row.Value == "true", nil
}
