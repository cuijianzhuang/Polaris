package panel

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/realityscan"
)

type templateRealityCheck struct {
	OK        bool                 `json:"ok"`
	From      string               `json:"from"`
	CheckedAt time.Time            `json:"checkedAt"`
	Results   []realityscan.Result `json:"results"`
}

func templateRealityKey(runID, nodeID string) string {
	return "template-reality:" + runID + ":" + nodeID
}

func frozenNodeHasReality(node *frozenTemplateNode) bool {
	for _, in := range node.Models {
		if in != nil && in.Security.Type == model.SecReality {
			return true
		}
	}
	return false
}

func validRealityOverride(target templateRealityTarget) error {
	host, port, err := net.SplitHostPort(target.Dest)
	n, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 || strings.ContainsAny(host, " /?#\t\n") {
		return fmt.Errorf("REALITY 目标需填写主机:端口，例如 www.example.com:443")
	}
	if len(target.ServerNames) == 0 || len(target.ServerNames) > 32 {
		return fmt.Errorf("REALITY 需填写 1–32 个 SNI 域名")
	}
	for _, name := range target.ServerNames {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "*/: \t\n") {
			return fmt.Errorf("SNI 需填写具体域名")
		}
	}
	return nil
}

func (s *Server) checkTemplateReality(c *gin.Context) {
	var req struct {
		NodeID string `json:"nodeId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	run, snapshot, err := s.loadTemplateRun(c.Param("run"), c.Param("id"))
	if err != nil {
		dbError(c, err)
		return
	}
	if run.State != "preview" || time.Now().After(run.ExpiresAt) {
		c.JSON(http.StatusConflict, gin.H{"error": "预览已过期，请重新预览后检测"})
		return
	}
	var node *frozenTemplateNode
	for i := range snapshot.Nodes {
		if snapshot.Nodes[i].Plan.NodeID == req.NodeID {
			node = &snapshot.Nodes[i]
			break
		}
	}
	if node == nil {
		badRequest(c, fmt.Errorf("此节点不在当前计划中"))
		return
	}
	if !s.hub.Online(req.NodeID) {
		badRequest(c, fmt.Errorf("节点离线，无法从节点检测 REALITY 目标"))
		return
	}
	var candidates []realityscan.Candidate
	seen := map[string]bool{}
	for _, in := range node.Models {
		if in == nil || in.Security.Type != model.SecReality {
			continue
		}
		for _, sni := range in.Security.RealityServerNames {
			candidate := realityscan.Candidate{Address: strings.ToLower(in.Security.RealityDest), ServerName: strings.ToLower(sni)}
			key := candidate.Address + "|" + candidate.ServerName
			if !seen[key] {
				seen[key] = true
				candidates = append(candidates, candidate)
			}
		}
	}
	if len(candidates) == 0 || len(candidates) > 512 {
		badRequest(c, fmt.Errorf("此节点需有 1–512 个 REALITY 目标与 SNI 组合"))
		return
	}
	encoded, _ := json.Marshal(candidates)
	response, err := s.hub.Command(c.Request.Context(), req.NodeID, "reality_scan_v2", map[string]string{"candidates": string(encoded)}, 120*time.Second)
	if err != nil {
		badRequest(c, err)
		return
	}
	if !response.Ok {
		message := response.Error
		if strings.HasPrefix(message, "unknown command") {
			message = "请先升级节点 Agent，再从节点检测 REALITY 目标"
		}
		badRequest(c, fmt.Errorf("%s", orDefault(message, "节点检测失败")))
		return
	}
	check := templateRealityCheck{OK: true, From: "node", CheckedAt: time.Now()}
	if err := json.Unmarshal([]byte(response.Output), &check.Results); err != nil || check.Results == nil {
		badRequest(c, fmt.Errorf("节点返回无效扫描结果"))
		return
	}
	passed := map[string]bool{}
	for _, result := range check.Results {
		passed[strings.ToLower(result.Target)+"|"+strings.ToLower(result.ServerName)] = result.OK
	}
	for key := range seen {
		if !passed[key] {
			check.OK = false
		}
	}
	if err := s.eph.put(templateRealityKey(run.ID, req.NodeID), check, time.Until(run.ExpiresAt)); err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, check)
}
