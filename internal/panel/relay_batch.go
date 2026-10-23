package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

const maxRelayEntries = 16

type relayBatchRequest struct {
	relaySetupRequest
	Entries []store.ChainHop `json:"entries"`
}

type relayBatchPlan struct {
	Version   int
	ID        string
	Owner     string
	InboundID string
	Expires   time.Time
	Plans     []relaySetupPlan
}

// Entries are parallel alternatives, never consecutive hops. Relays is the
// optional common tail after each entry; each path owns its ports and secret.
func (s *Server) previewInboundRelayBatch(c *gin.Context) {
	var req relayBatchRequest
	if e := c.ShouldBindJSON(&req); e != nil {
		badRequest(c, e)
		return
	}
	if len(req.Entries) < 1 || len(req.Entries) > maxRelayEntries {
		badRequest(c, fmt.Errorf("请选择 1–%d 个并行入口节点", maxRelayEntries))
		return
	}
	if len([]rune(strings.TrimSpace(req.Name))) > 64 {
		badRequest(c, fmt.Errorf("名称最多 64 字"))
		return
	}
	if len(req.Relays) >= maxChainHops {
		badRequest(c, fmt.Errorf("每条线路含入口最多 %d 个中转节点", maxChainHops))
		return
	}
	seen := map[string]bool{}
	for _, entry := range req.Entries {
		if entry.NodeID == "" || seen[entry.NodeID] {
			badRequest(c, fmt.Errorf("入口节点不能为空或重复"))
			return
		}
		seen[entry.NodeID] = true
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	owner := idempotencyIdentity(c)
	if owner == "" {
		badRequest(c, fmt.Errorf("无法确定操作账号"))
		return
	}
	batch := relayBatchPlan{Version: 2, ID: store.NewID(), Owner: owner, InboundID: c.Param("id"), Expires: time.Now().Add(relayPlanTTL)}
	reserved := map[string]map[int]bool{}
	previews := []gin.H{}
	warnings := []string{}
	for _, entry := range req.Entries {
		path := req.relaySetupRequest
		path.Name = ""
		path.Relays = append([]store.ChainHop{entry}, req.Relays...)
		plan, nodes, notes, e := s.planInboundRelayWithPorts(c, path, reserved)
		if e != nil {
			badRequest(c, fmt.Errorf("入口 %s: %w", entry.NodeID, e))
			return
		}
		if prefix := strings.TrimSpace(req.Name); prefix != "" {
			plan.Chain.Name = truncate(prefix+" · "+nodes[entry.NodeID].Name, 55) + " · " + plan.Chain.ID[:6]
		}
		plan.Expires = batch.Expires
		plan.Chain.BatchID = batch.ID
		for _, hop := range plan.Chain.Hops.V {
			if reserved[hop.NodeID] == nil {
				reserved[hop.NodeID] = map[int]bool{}
			}
			for _, port := range []int{hop.Port, hop.TunnelPort} {
				if port > 0 {
					reserved[hop.NodeID][port] = true
				}
			}
		}
		var in store.Inbound
		if e = s.st.DB.First(&in, "id = ?", plan.InboundID).Error; e != nil {
			dbError(c, e)
			return
		}
		batch.Plans = append(batch.Plans, *plan)
		view := relayPlanPreview(plan, nodes, in)
		view["warnings"] = notes
		previews = append(previews, view)
		for _, note := range notes {
			if !contains(warnings, note) {
				warnings = append(warnings, note)
			}
		}
	}
	raw, e := json.Marshal(batch)
	if e != nil {
		serverError(c, e)
		return
	}
	c.JSON(http.StatusOK, gin.H{"batchId": batch.ID, "planToken": s.st.Box.Seal(string(raw)), "expiresAt": batch.Expires, "entries": previews, "warnings": warnings})
}

func relayPlanPreview(plan *relaySetupPlan, nodes map[string]store.Node, in store.Inbound) gin.H {
	ports := []map[string]any{}
	for nodeID, rules := range managedRelayForwards(&plan.Chain, nodes, in, "") {
		for _, r := range rules {
			ports = append(ports, map[string]any{"nodeId": nodeID, "port": r.Listen, "network": r.ListenNetwork(), "public": !isLoopbackHost(r.ListenIP), "role": r.TunnelRole, "engine": orDefault(r.Engine, "realm")})
		}
	}
	sort.Slice(ports, func(i, j int) bool {
		a, b := ports[i], ports[j]
		if a["nodeId"] != b["nodeId"] {
			return a["nodeId"].(string) < b["nodeId"].(string)
		}
		return a["port"].(int) < b["port"].(int)
	})
	entry := nodes[plan.Chain.Hops.V[0].NodeID]
	return gin.H{"chain": plan.Chain, "network": plan.Network, "address": nodeHost(&entry), "port": plan.Chain.Hops.V[0].Port, "ports": ports}
}

func (s *Server) createInboundRelayBatch(c *gin.Context) {
	var req struct {
		PlanToken string `json:"planToken" binding:"required"`
	}
	if e := c.ShouldBindJSON(&req); e != nil {
		badRequest(c, e)
		return
	}
	raw, e := s.st.Box.Open(req.PlanToken)
	var batch relayBatchPlan
	if e != nil || !strings.HasPrefix(req.PlanToken, "enc:v1:") || json.Unmarshal([]byte(raw), &batch) != nil || batch.Version != 2 || batch.ID == "" || batch.Owner == "" || batch.Owner != idempotencyIdentity(c) || batch.InboundID != c.Param("id") || len(batch.Plans) < 1 || len(batch.Plans) > maxRelayEntries {
		badRequest(c, fmt.Errorf("无效的多入口预览，请重新预览"))
		return
	}
	for _, plan := range batch.Plans {
		if plan.Chain.BatchID != batch.ID || plan.Version != 1 || plan.Owner != batch.Owner || plan.InboundID != batch.InboundID || !plan.Expires.Equal(batch.Expires) {
			badRequest(c, fmt.Errorf("无效的多入口预览，请重新预览"))
			return
		}
		for _, h := range plan.Chain.Hops.V {
			if !inScope(c, h.NodeID) {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
		}
	}
	s.commitInboundRelayPlans(c, batch.Plans, batch.ID)
}
