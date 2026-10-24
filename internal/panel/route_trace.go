package panel

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

func (s *Server) traceNodeRoute(c *gin.Context) {
	var req model.RouteTraceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := req.Validate(); err != nil {
		badRequest(c, err)
		return
	}
	if req.IPv4 != "" {
		req.IPv4 = netip.MustParseAddr(req.IPv4).String()
	}
	if req.IPv6 != "" {
		req.IPv6 = netip.MustParseAddr(req.IPv6).String()
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if !contains(n.AgentFeatures.V, model.RouteTraceFeature) {
		c.JSON(http.StatusConflict, gin.H{"error": "节点 Agent 不支持回程检测，请先升级"})
		return
	}
	if !s.hub.Online(n.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "节点离线"})
		return
	}
	rows, err := s.beginRouteTrace(n.ID, req)
	if err != nil {
		dbError(c, err)
		return
	}
	results, saved, err := s.executeRouteTrace(c.Request.Context(), n.ID, req, rows)
	if err != nil {
		if errors.Is(err, errRouteTraceCommand) || errors.Is(err, errRouteTraceResult) {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		} else {
			serverError(c, err)
		}
		return
	}
	history := make([]gin.H, 0, len(saved))
	for _, row := range saved {
		history = append(history, routeTraceView(row, false))
	}
	c.JSON(http.StatusOK, gin.H{"nodeId": n.ID, "results": results, "history": history})
}

var errRouteTraceCommand = errors.New("回程检测未完成，请检查节点状态或稍后重试")
var errRouteTraceResult = errors.New("节点返回了无效的回程检测结果")

func (s *Server) executeRouteTrace(parent context.Context, nodeID string, req model.RouteTraceRequest, rows []store.NodeRouteTrace) ([]model.RouteTraceResult, []store.NodeRouteTrace, error) {
	finished := false
	defer func() {
		if !finished {
			if _, err := s.finishRouteTrace(rows, nil, "interrupted"); err != nil {
				slog.Warn("finish interrupted route trace", "node", nodeID, "error", err)
			}
		}
	}()
	if parent.Err() != nil {
		if _, err := s.finishRouteTrace(rows, nil, "interrupted"); err == nil {
			finished = true
		}
		return nil, nil, errRouteTraceCommand
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	raw, _ := json.Marshal(req)
	res, err := s.hub.Command(ctx, nodeID, model.RouteTraceFeature, map[string]string{"request": string(raw)}, 14*time.Second)
	if err != nil || !res.Ok {
		state := "failed"
		if parent.Err() != nil {
			state = "interrupted"
		}
		if _, saveErr := s.finishRouteTrace(rows, nil, state); saveErr == nil {
			finished = true
		}
		return nil, nil, errRouteTraceCommand
	}
	var results []model.RouteTraceResult
	if len(res.Output) > 96*1024 || json.Unmarshal([]byte(res.Output), &results) != nil || !validRouteTraceResults(req, results) {
		if _, saveErr := s.finishRouteTrace(rows, nil, "failed"); saveErr == nil {
			finished = true
		}
		return nil, nil, errRouteTraceResult
	}
	saved, err := s.finishRouteTrace(rows, results, "completed")
	if err != nil {
		return nil, nil, err
	}
	finished = true
	return results, saved, nil
}

func validRouteTraceResults(req model.RouteTraceRequest, results []model.RouteTraceResult) bool {
	targets := map[string]string{}
	if req.IPv4 != "" {
		targets["ipv4"] = req.IPv4
	}
	if req.IPv6 != "" {
		targets["ipv6"] = req.IPv6
	}
	if len(results) != len(targets) {
		return false
	}
	for _, r := range results {
		if targets[r.Family] == "" || targets[r.Family] != r.Target || r.CheckedAt.IsZero() || len(r.Raw) > 32*1024 || len(r.Hops) > 20 {
			return false
		}
		switch r.State {
		case "reached", "partial", "timeout", "unavailable", "failed":
		default:
			return false
		}
		delete(targets, r.Family)
	}
	return len(targets) == 0
}
