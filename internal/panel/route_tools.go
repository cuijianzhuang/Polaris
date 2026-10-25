package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

func (s *Server) getNodeRouteTool(c *gin.Context)     { s.nodeRouteTool(c, "status") }
func (s *Server) installNodeRouteTool(c *gin.Context) { s.nodeRouteTool(c, "install") }

func (s *Server) nodeRouteTool(c *gin.Context, operation string) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if n.Revoked || !s.hub.Online(n.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "节点离线或已撤销，无法检查回程环境"})
		return
	}
	respond := func(result model.RouteTraceToolStatus) {
		c.JSON(http.StatusOK, struct {
			model.RouteTraceToolStatus
			AgentVersion string `json:"agentVersion"`
			PanelVersion string `json:"panelVersion"`
		}{result, n.AgentVersion, Version})
	}
	// Capability headers may be stale after upgrading the panel or Agent.
	// A read-only command is evidence; a missing cached flag is not evidence
	// that traceroute is missing or that the running Agent needs an upgrade.
	query := func(op string) (model.RouteTraceToolStatus, bool) {
		res, err := s.hub.Command(c.Request.Context(), n.ID, model.RouteTraceToolFeature, map[string]string{"operation": op}, 8*time.Second)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "回程环境检查未完成，请检查 Agent 连接后重试"})
			return model.RouteTraceToolStatus{}, false
		}
		if !res.Ok {
			if res.Error == fmt.Sprintf("unknown command %q", model.RouteTraceToolFeature) {
				respond(model.RouteTraceToolStatus{State: "unsupported", Issue: "agent_upgrade_required", CheckedAt: time.Now().UTC()})
			} else {
				c.JSON(http.StatusBadGateway, gin.H{"error": "回程环境检查未完成，请检查 Agent 连接后重试"})
			}
			return model.RouteTraceToolStatus{}, false
		}
		var result model.RouteTraceToolStatus
		if len(res.Output) > 128*1024 || json.Unmarshal([]byte(res.Output), &result) != nil || !validRouteToolStatus(result) {
			c.JSON(http.StatusBadGateway, gin.H{"error": "Agent 返回了无效的回程环境状态"})
			return model.RouteTraceToolStatus{}, false
		}
		return result, true
	}
	if operation == "install" && !contains(n.AgentFeatures.V, model.RouteTraceToolFeature) {
		// Before sending a mutation to an unadvertised implementation, verify
		// its live status. Permission checks remain on the HTTP route and Agent.
		status, ok := query("status")
		if !ok {
			return
		}
		if !status.CanInstall {
			respond(status)
			return
		}
	}
	result, ok := query(operation)
	if !ok {
		return
	}
	if operation == "install" {
		s.auditCtx(c, "node.route-tool.install", n.ID, result.InstallState)
	}
	respond(result)
}

func validRouteToolStatus(s model.RouteTraceToolStatus) bool {
	if s.CheckedAt.IsZero() || len(s.CheckOutput) > 4096 || len(s.Path) > 1024 || len(s.Manager) > 1024 || len(s.ManualCommand) > 1024 || len(s.InstallOutput) > 16*1024 || len(s.Issue) > 64 || len(s.InstallIssue) > 64 {
		return false
	}
	switch s.State {
	case "ready", "missing", "unusable", "unsupported", "checking":
	default:
		return false
	}
	switch s.InstallState {
	case "", "installing", "succeeded", "failed":
	default:
		return false
	}
	return true
}
