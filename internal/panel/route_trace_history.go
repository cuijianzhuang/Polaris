package panel

import (
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	routeTraceKeep          = 7 * 24 * time.Hour
	routeTraceCompareWindow = 25 * time.Hour // allow daily schedules plus dispatch jitter
	routeTraceHistoryLimit  = 100
	routeTraceProfile       = "udp-traceroute-v1"
)

func lockRouteNode(tx *gorm.DB, nodeID string) error {
	result := tx.Model(&store.Node{}).Where("id = ?", nodeID).UpdateColumn("id", gorm.Expr("id"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func createRouteTraceRows(tx *gorm.DB, nodeID string, req model.RouteTraceRequest, now time.Time, source string, revision int64) ([]store.NodeRouteTrace, error) {
	rows := []store.NodeRouteTrace{}
	if err := tx.Model(&store.NodeRouteTrace{}).Where("node_id = ? AND state = ? AND started_at < ?", nodeID, "running", now.Add(-model.RouteTraceStaleAfter)).Updates(map[string]any{"state": "interrupted", "finished_at": now, "comparison": store.NewJSON(store.RouteTraceComparison{State: "not_comparable", Reason: "interrupted", ChangedTTLs: []int{}})}).Error; err != nil {
		return nil, err
	}
	runID := store.NewID()
	for _, t := range []struct{ family, target string }{{"ipv4", req.IPv4}, {"ipv6", req.IPv6}} {
		if t.target == "" {
			continue
		}
		row := store.NodeRouteTrace{NodeID: nodeID, Family: t.family, Target: t.target, RunID: runID, Profile: routeTraceProfile, Source: source, ScheduleRevision: revision, State: "running", StartedAt: now, Comparison: store.NewJSON(store.RouteTraceComparison{State: "not_comparable", Reason: "running", ChangedTTLs: []int{}})}
		if err := tx.Create(&row).Error; err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Server) beginRouteTrace(nodeID string, req model.RouteTraceRequest) ([]store.NodeRouteTrace, error) {
	var rows []store.NodeRouteTrace
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockRouteNode(tx, nodeID); err != nil {
			return err
		}
		var err error
		rows, err = createRouteTraceRows(tx, nodeID, req, time.Now().UTC(), "manual", 0)
		return err
	})
	return rows, err
}

// Completion updates only the pre-created attempt. A late result cannot
// re-create a deleted node or replace a more recently started observation.
func (s *Server) finishRouteTrace(rows []store.NodeRouteTrace, results []model.RouteTraceResult, state string) ([]store.NodeRouteTrace, error) {
	if len(rows) == 0 {
		return nil, errors.New("missing route trace attempt")
	}
	now := time.Now().UTC()
	events := []Event{}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockRouteNode(tx, rows[0].NodeID); err != nil {
			return err
		}
		for i := range rows {
			var row store.NodeRouteTrace
			if err := tx.First(&row, "id = ? AND node_id = ? AND run_id = ?", rows[i].ID, rows[i].NodeID, rows[i].RunID).Error; err != nil {
				return err
			}
			if row.State != "running" {
				rows[i] = row
				continue
			}
			row.State = state
			if now.Sub(row.StartedAt) > model.RouteTraceStaleAfter {
				row.State = "interrupted"
			}
			row.FinishedAt = &now
			for _, r := range results {
				if row.State == "completed" && r.Family == row.Family && r.Target == row.Target {
					row.Result = store.NewJSON(r)
				}
			}
			var previous store.NodeRouteTrace
			err := tx.Where("node_id = ? AND family = ? AND target = ? AND id < ?", row.NodeID, row.Family, row.Target, row.ID).Order("id DESC").First(&previous).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			row.Comparison = store.NewJSON(compareRouteTrace(row, previous))
			if err := tx.Save(&row).Error; err != nil {
				return err
			}
			rows[i] = row
			if row.Comparison.V.State == "changed" {
				var newer int64
				if err := tx.Model(&store.NodeRouteTrace{}).Where("node_id = ? AND family = ? AND target = ? AND id > ?", row.NodeID, row.Family, row.Target, row.ID).Count(&newer).Error; err != nil {
					return err
				}
				if newer == 0 && routeScheduleNoticeCurrent(tx, row) {
					var node store.Node
					if err := tx.Select("id", "name").First(&node, "id = ?", row.NodeID).Error; err != nil {
						return err
					}
					events = append(events, routeChangeEvent(node, row))
				}
			}
		}
		// Bound both age and per-node storage. Keep pending observations until the
		// request finishes; crashed attempts are removed by age cleanup.
		var stale []int64
		if err := tx.Model(&store.NodeRouteTrace{}).Where("node_id = ? AND state <> ?", rows[0].NodeID, "running").Order("id DESC").Offset(routeTraceHistoryLimit).Pluck("id", &stale).Error; err != nil {
			return err
		}
		if len(stale) > 0 {
			if err := tx.Where("id IN ?", stale).Delete(&store.NodeRouteTrace{}).Error; err != nil {
				return err
			}
		}
		return tx.Where("node_id = ? AND started_at < ?", rows[0].NodeID, now.Add(-routeTraceKeep)).Delete(&store.NodeRouteTrace{}).Error
	})
	if err == nil {
		for _, e := range events {
			s.notify(e)
		}
	}
	return rows, err
}

func completeRoutePath(row store.NodeRouteTrace) ([]string, bool) {
	r := row.Result.V
	if row.State != "completed" || r.State != "reached" || len(r.Hops) == 0 || len(r.Hops) > 20 {
		return nil, false
	}
	target, err := netip.ParseAddr(row.Target)
	if err != nil {
		return nil, false
	}
	path := make([]string, 0, len(r.Hops))
	for i, h := range r.Hops {
		if h.TTL != i+1 || len(h.Addresses) != 1 {
			return nil, false
		}
		addr, err := netip.ParseAddr(h.Addresses[0])
		if err != nil || addr.Zone() != "" || addr.Is4() != target.Is4() {
			return nil, false
		}
		path = append(path, addr.String())
	}
	return path, path[len(path)-1] == target.String()
}

func compareRouteTrace(current, previous store.NodeRouteTrace) store.RouteTraceComparison {
	c := store.RouteTraceComparison{State: "not_comparable", ChangedTTLs: []int{}}
	path, ok := completeRoutePath(current)
	if !ok {
		c.Reason = "incomplete_result"
		return c
	}
	if previous.ID == 0 {
		c.State = "baseline"
		return c
	}
	c.PreviousID = previous.ID
	if current.NodeID != previous.NodeID || current.Family != previous.Family || current.Target != previous.Target {
		c.Reason = "scope_changed"
		return c
	}
	if current.Profile != previous.Profile {
		c.Reason = "profile_changed"
		return c
	}
	if current.StartedAt.Before(previous.StartedAt) || current.StartedAt.Sub(previous.StartedAt) > routeTraceCompareWindow {
		c.Reason = "sample_window"
		return c
	}
	old, ok := completeRoutePath(previous)
	if !ok {
		c.Reason = "previous_incomplete"
		return c
	}
	if previous.FinishedAt == nil || previous.FinishedAt.After(current.StartedAt) {
		c.Reason = "overlapping_samples"
		return c
	}
	c.State = "same"
	for i := 0; i < max(len(path), len(old)); i++ {
		if i >= len(path) || i >= len(old) || path[i] != old[i] {
			c.ChangedTTLs = append(c.ChangedTTLs, i+1)
		}
	}
	if len(c.ChangedTTLs) > 0 {
		c.State = "changed"
	}
	return c
}

func routeTraceView(row store.NodeRouteTrace, detail bool) gin.H {
	// A process crash or lost HTTP handler cannot leave a forever-running UI.
	state := row.State
	comparison := row.Comparison.V
	if state == "running" && time.Since(row.StartedAt) > model.RouteTraceStaleAfter {
		state = "interrupted"
		comparison = store.RouteTraceComparison{State: "not_comparable", Reason: "interrupted", ChangedTTLs: []int{}}
	}
	result := row.Result.V
	if !detail {
		result.Raw = ""
	}
	return gin.H{"id": row.ID, "runId": row.RunID, "nodeId": row.NodeID, "family": row.Family, "target": row.Target, "profile": row.Profile, "source": orDefault(row.Source, "manual"), "scheduleRevision": row.ScheduleRevision, "state": state, "startedAt": row.StartedAt, "finishedAt": row.FinishedAt, "result": result, "comparison": comparison}
}

func (s *Server) nodeRouteHistory(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	limit := 20
	if v, exists := c.GetQuery("limit"); exists {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			badRequest(c, errors.New("limit 必须为 1–100"))
			return
		}
		limit = n
	}
	q := s.st.DB.Where("node_id = ? AND started_at >= ?", n.ID, time.Now().Add(-routeTraceKeep))
	if v, exists := c.GetQuery("before"); exists {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			badRequest(c, errors.New("before 必须为正整数"))
			return
		}
		q = q.Where("id < ?", id)
	}
	if v, exists := c.GetQuery("family"); exists {
		if v != "ipv4" && v != "ipv6" {
			badRequest(c, errors.New("family 必须为 ipv4 或 ipv6"))
			return
		}
		q = q.Where("family = ?", v)
	}
	if v, exists := c.GetQuery("target"); exists {
		addr, err := netip.ParseAddr(v)
		if err != nil || addr.Zone() != "" {
			badRequest(c, errors.New("target 必须为 IP 地址"))
			return
		}
		q = q.Where("target = ?", addr.String())
	}
	var rows []store.NodeRouteTrace
	if err := q.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	views := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		views = append(views, routeTraceView(row, false))
	}
	var next int64
	if hasMore {
		next = rows[len(rows)-1].ID
	}
	c.JSON(http.StatusOK, gin.H{"items": views, "nextBefore": next})
}

func (s *Server) nodeRouteDetail(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("traceId"), 10, 64)
	if err != nil || id < 1 {
		badRequest(c, errors.New("traceId 必须为正整数"))
		return
	}
	var row store.NodeRouteTrace
	if err := s.st.DB.First(&row, "id = ? AND node_id = ? AND started_at >= ?", id, c.Param("id"), time.Now().Add(-routeTraceKeep)).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(http.StatusOK, routeTraceView(row, true))
}
