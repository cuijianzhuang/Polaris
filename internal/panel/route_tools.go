package panel

import (
	"encoding/json"
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
	// Use an independent capability: older Agents can still run traces without
	// knowing the new readiness/install command.
	if !contains(n.AgentFeatures.V, model.RouteTraceToolFeature) {
		if operation == "install" {
			c.JSON(http.StatusConflict, gin.H{"error": "请升级 Linux Agent 以检查和安装 traceroute"})
			return
		}
		c.JSON(http.StatusOK, model.RouteTraceToolStatus{State: "unsupported", Issue: "agent_upgrade_required", CheckedAt: time.Now().UTC()})
		return
	}
	if n.Revoked || !s.hub.Online(n.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "节点离线或已撤销，无法检查回程环境"})
		return
	}
	res, err := s.hub.Command(c.Request.Context(), n.ID, model.RouteTraceToolFeature, map[string]string{"operation": operation}, 8*time.Second)
	if err != nil || !res.Ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "回程环境检查未完成，请检查 Agent 连接后重试"})
		return
	}
	var result model.RouteTraceToolStatus
	if len(res.Output) > 128*1024 || json.Unmarshal([]byte(res.Output), &result) != nil || !validRouteToolStatus(result) {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Agent 返回了无效的回程环境状态"})
		return
	}
	if operation == "install" {
		s.auditCtx(c, "node.route-tool.install", n.ID, result.InstallState)
	}
	c.JSON(http.StatusOK, result)
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
