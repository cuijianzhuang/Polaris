package panel

import (
	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"strings"
)

type snellReadiness struct {
	NodeID           string   `json:"nodeId"`
	Ready            bool     `json:"ready"`
	MinVersion       string   `json:"minVersion"`
	AgentReady       bool     `json:"agentReady"`
	CoreReady        bool     `json:"coreReady"`
	TrafficReady     bool     `json:"trafficReady"`
	InstalledVersion string   `json:"installedVersion"`
	Reasons          []string `json:"reasons"`
}

func (s *Server) getSnellReadiness(c *gin.Context) {
	if !inScope(c, c.Param("id")) {
		c.Status(403)
		return
	}
	var n store.Node
	if e := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; e != nil {
		dbError(c, e)
		return
	}
	v := snellReadiness{NodeID: n.ID, MinVersion: model.SnellMinVersion, Reasons: []string{}}
	v.AgentReady = contains(n.AgentFeatures.V, model.SnellUsersFeature) && contains(n.AgentFeatures.V, model.SnellPSKFeature)
	if !v.AgentReady {
		v.Reasons = append(v.Reasons, "先升级节点 Agent，并等待重新连接上报 Snell 用户授权能力。")
	}
	var cr store.Core
	if e := s.st.DB.Where("node_id = ? AND type = ?", n.ID, model.CoreSingbox).First(&cr).Error; e != nil {
		if !store.IsNotFound(e) {
			dbError(c, e)
			return
		}
		v.Reasons = append(v.Reasons, "先在内核管理安装 sing-box 1.14+（建议使用已验证的 1.14.4 统计版本）。")
	} else {
		v.InstalledVersion = cr.RunningVersion
		v.CoreReady = cr.Status == "running" && core.VersionAtLeast(cr.RunningVersion, model.SnellMinVersion) && !strings.Contains(cr.RunningVersion, "-")
		if !v.CoreReady {
			v.Reasons = append(v.Reasons, "需要运行 sing-box 1.14+ 稳定版，并等待 Agent 上报实际运行版本。")
		}
		v.TrafficReady = v.CoreReady && !cr.NoUserTraffic
		if cr.NoUserTraffic {
			v.Reasons = append(v.Reasons, "当前 sing-box 没有用户流量统计能力，请安装带 with_v2ray_api 的统计版本。")
		}
	}
	if n.Revoked {
		v.Reasons = append(v.Reasons, "节点已撤销，需要重新注册。")
	}
	v.Ready = v.AgentReady && v.CoreReady && v.TrafficReady && !n.Revoked
	c.JSON(200, v)
}
