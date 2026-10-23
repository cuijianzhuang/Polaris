package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// checkOutboundTag validates an inbound's bound egress.
func (s *Server) checkOutboundTag(nodeID, tag string) error {
	if tag == "" || tag == "direct" || tag == "block" {
		return nil
	}
	var n int64
	s.st.DB.Model(&store.Outbound{}).Where("node_id = ? AND tag = ?", nodeID, tag).Count(&n)
	if n == 0 {
		return fmt.Errorf("出站 %q 不在该节点上", tag)
	}
	return nil
}

// RulePreset is a ready-made set of routing rules.
type RulePreset struct {
	Key         string             `json:"key"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Rules       []model.RouteMatch `json:"rules"`
	Outbound    string             `json:"outbound"` // suggested egress; "" = chosen when applying
	Priority    int                `json:"priority"`
}

var rulePresets = []RulePreset{
	{Key: "block-private", Name: "拦截私有地址", Description: "防止经代理访问内网（geoip:private）", Outbound: "block", Priority: 5,
		Rules: []model.RouteMatch{{GeoIP: []string{"private"}}}},
	{Key: "block-ads", Name: "广告拦截", Description: "geosite:category-ads-all", Outbound: "block", Priority: 10,
		Rules: []model.RouteMatch{{Geosite: []string{"category-ads-all"}}}},
	{Key: "block-bt", Name: "拦截 BT 下载", Description: "geosite:category-public-tracker", Outbound: "block", Priority: 15,
		Rules: []model.RouteMatch{{Geosite: []string{"category-public-tracker"}}}},
	{Key: "cn-direct", Name: "国内直连", Description: "落地在国内时：geosite:cn + geoip:cn 直连", Outbound: "direct", Priority: 20,
		Rules: []model.RouteMatch{{Geosite: []string{"cn"}}, {GeoIP: []string{"cn"}}}},
	{Key: "streaming", Name: "流媒体分流", Description: "Netflix / Disney+ / HBO / Hulu / Prime Video", Priority: 30,
		Rules: []model.RouteMatch{{Geosite: []string{"netflix", "disney", "hbo", "hulu", "primevideo"}}}},
	{Key: "ai", Name: "AI 服务分流", Description: "OpenAI / Anthropic / Gemini 等", Priority: 30,
		Rules: []model.RouteMatch{{Geosite: []string{"openai", "anthropic", "google-gemini"}}}},
	{Key: "telegram", Name: "Telegram 分流", Description: "geosite:telegram + geoip:telegram", Priority: 40,
		Rules: []model.RouteMatch{{Geosite: []string{"telegram"}}, {GeoIP: []string{"telegram"}}}},
}

func (s *Server) listRulePresets(c *gin.Context) { c.JSON(http.StatusOK, rulePresets) }

// applyRulePreset appends a preset's rules to nodes (or the global rule set).
func (s *Server) applyRulePreset(c *gin.Context) {
	var req struct {
		Key      string   `json:"key" binding:"required"`
		NodeIDs  []string `json:"nodeIds"` // ["*"] = global
		Outbound string   `json:"outbound"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var preset *RulePreset
	for i := range rulePresets {
		if rulePresets[i].Key == req.Key {
			preset = &rulePresets[i]
		}
	}
	if preset == nil {
		notFound(c)
		return
	}
	out := req.Outbound
	if out == "" {
		out = preset.Outbound
	}
	if out == "" {
		badRequest(c, fmt.Errorf("该预设需要选择出口"))
		return
	}
	if len(req.NodeIDs) == 0 {
		req.NodeIDs = []string{model.GlobalNodeID}
	}
	for _, id := range req.NodeIDs {
		if id != model.GlobalNodeID {
			if err := s.checkOutboundTag(id, out); err != nil {
				badRequest(c, err)
				return
			}
		}
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		for _, id := range req.NodeIDs {
			for _, m := range preset.Rules {
				if err := tx.Create(&store.RoutingRule{ID: store.NewID(), NodeID: id, Match: store.NewJSON(m), OutboundTag: out,
					Priority: preset.Priority, Remark: "预设：" + preset.Name}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		serverError(c, err)
		return
	}
	s.markRoutingDirty(req.NodeIDs...)
	s.auditCtx(c, "routing.preset", preset.Name, fmt.Sprintf("%v → %s", req.NodeIDs, out))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) markRoutingDirty(nodeIDs ...string) {
	for _, id := range nodeIDs {
		if id == model.GlobalNodeID {
			s.MarkAllDirty("global routing changed")
			return
		}
	}
	s.MarkDirty("routing changed", nodeIDs...)
}

// validateBalancer checks members and that every proxying core on the node
// supports the strategy.
func (s *Server) validateBalancer(nodeID, selfTag string, set model.OutboundSettings) error {
	return s.validateBalancerOn(s.st.DB, nodeID, selfTag, set)
}

func (s *Server) validateBalancerOn(db *gorm.DB, nodeID, selfTag string, set model.OutboundSettings) error {
	if len(set.Members) == 0 {
		return fmt.Errorf("负载均衡组至少需要一个成员")
	}
	var tags []string
	db.Model(&store.Outbound{}).Where("node_id = ?", nodeID).Pluck("tag", &tags)
	have := map[string]bool{"direct": true, "block": true}
	for _, t := range tags {
		have[t] = true
	}
	for _, m := range append(append([]string{}, set.Members...), set.Fallback) {
		if m == "" {
			continue
		}
		if m == selfTag {
			return fmt.Errorf("组不能包含自身")
		}
		if !have[m] {
			return fmt.Errorf("成员出站 %q 不存在", m)
		}
	}
	if set.Interval != "" {
		if d, err := time.ParseDuration(set.Interval); err != nil || d < time.Second {
			return fmt.Errorf("探测间隔至少为 1s，例如 1m")
		}
	}
	if set.TestURL != "" {
		u, err := url.Parse(set.TestURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("探测地址必须是 HTTP / HTTPS URL")
		}
	}
	strategy := set.Strategy
	if strategy == "" {
		strategy = model.LBLeastPing
	}
	var cores []store.Core
	if err := db.Where("node_id = ?", nodeID).Find(&cores).Error; err != nil {
		return err
	}
	for _, cr := range cores {
		if cr.Type != model.CoreXray && cr.Type != model.CoreSingbox && cr.Type != model.CoreMihomo {
			continue
		}
		if err := validateGroupFallbackCore(cr.Type, set); err != nil {
			return err
		}
		caps, _ := core.Capabilities(cr.Type, cr.Version)
		if !caps.SupportsBalancer(strategy) {
			return fmt.Errorf("%s 不支持 %s 负载均衡策略", cr.Type, strategy)
		}
	}
	return nil
}

func validateGroupFallbackCore(typ model.CoreType, set model.OutboundSettings) error {
	if set.Fallback != "" && typ == model.CoreSingbox {
		return fmt.Errorf("sing-box 不支持均衡组兜底，请清除兜底或使用 Xray / mihomo")
	}
	if set.Fallback == "block" && typ == model.CoreMihomo {
		return fmt.Errorf("mihomo 无法对 block 兜底执行健康检查，请选择备用出站或均衡组")
	}
	return nil
}

// --- health: drop members that point at unreachable landing nodes ---

type healthState struct {
	mu   sync.Mutex
	down map[string]bool // node id -> unreachable
}

func (h *healthState) set(nodeID string, down bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down == nil {
		h.down = map[string]bool{}
	}
	if h.down[nodeID] == down {
		return false
	}
	if down {
		h.down[nodeID] = true
	} else {
		delete(h.down, nodeID)
	}
	return true
}

func (h *healthState) isDown(nodeID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.down[nodeID]
}

// nodesReferencing returns nodes with outbounds targeting inbounds on nodeID.
func (s *Server) nodesReferencing(nodeID string) []string {
	var ids []string
	s.st.DB.Table("outbounds").Joins("JOIN inbounds ON inbounds.id = outbounds.node_ref_inbound_id").
		Where("inbounds.node_id = ?", nodeID).Distinct().Pluck("outbounds.node_id", &ids)
	return ids
}

// setNodeHealth records reachability and republishes dependants on change.
func (s *Server) setNodeHealth(nodeID string, down bool, why string) {
	if !s.health.set(nodeID, down) {
		return
	}
	var n store.Node
	s.st.DB.Select("name").First(&n, "id = ?", nodeID)
	state := "恢复，已加回负载均衡组"
	if down {
		state = "不可达，已从负载均衡组摘除"
	}
	s.audit("system", "health.change", n.Name, why, "")
	if refs := s.nodesReferencing(nodeID); len(refs) > 0 {
		s.notify(Event{Kind: "health", NodeID: nodeID, Data: map[string]any{"down": down}, Text: "🩺 落地 " + n.Name + " " + state + "（" + why + "）"})
		s.MarkDirty("landing health changed", refs...)
	}
}

// filterGroupMembers removes unhealthy node_ref members from groups; a group
// whose members are all unhealthy keeps them (better degraded than empty).
func (s *Server) filterGroupMembers(obs []store.Outbound, out []model.Outbound) []model.Outbound {
	unhealthy := map[string]bool{}
	for _, o := range obs {
		if o.Type != model.OutNodeRef {
			continue
		}
		var in store.Inbound
		if s.st.DB.Select("node_id").First(&in, "id = ?", o.NodeRefInboundID).Error == nil && s.health.isDown(in.NodeID) {
			unhealthy[o.Tag] = true
		}
	}
	if len(unhealthy) == 0 {
		return out
	}
	for i := range out {
		if !out[i].Type.IsGroup() {
			continue
		}
		set, err := out[i].ParseSettings()
		if err != nil {
			continue
		}
		var keep []string
		for _, m := range set.Members {
			if !unhealthy[m] {
				keep = append(keep, m)
			}
		}
		if len(keep) > 0 && len(keep) < len(set.Members) {
			set.Members = keep
			out[i].Settings = mustJSON(set)
		}
	}
	return out
}

// probeChains runs every minute: relay→landing reachability per chain plus
// landing heartbeat state decide whether a landing node is healthy.
func (s *Server) probeChains() {
	var chains []store.Chain
	s.st.DB.Find(&chains)
	landing := map[string]bool{} // node -> any probe ok
	tested := map[string]bool{}
	for i := range chains {
		if chains[i].Disabled {
			continue
		}
		hops := chains[i].Hops.V
		if len(hops) < 2 {
			continue
		}
		probes := s.runChainProbe(&chains[i])
		last := hops[len(hops)-1].NodeID
		if len(probes) < 2 || probes[len(probes)-2].Skipped {
			continue // unsupported UDP check: retain heartbeat-based health
		}
		tested[last] = true
		if len(probes) >= 2 && probes[len(probes)-2].OK {
			landing[last] = true
		}
	}
	// node_ref targets without chains: rely on heartbeat state.
	var ns []store.Node
	s.st.DB.Select("id", "status").Find(&ns)
	for _, n := range ns {
		if len(s.nodesReferencing(n.ID)) == 0 {
			continue
		}
		down := n.Status == "offline"
		why := "心跳"
		if tested[n.ID] {
			down = down || !landing[n.ID]
			why = "链路探测"
		}
		s.setNodeHealth(n.ID, down, why)
	}
}

// sortRules orders rules by priority, stable.
func sortRules(rs []model.RouteRule) {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Priority < rs[j].Priority })
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
