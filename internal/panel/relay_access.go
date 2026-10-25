package panel

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func relaySourceIPs(n store.Node, p store.RelayAccessPolicy) ([]string, error) {
	if sources, ok := p.SourceOverrides[n.ID]; ok {
		if len(sources) == 0 {
			return nil, fmt.Errorf("节点 %s 的来源地址不能为空", n.Name)
		}
		rules, e := model.NormalizeAccessRules([]model.AccessRule{{Protocol: "tcp", Port: 1, Sources: sources}})
		if e != nil {
			return nil, e
		}
		return rules[0].Sources, nil
	}
	var sources []string
	for _, s := range []string{n.PublicIPv4, n.PublicIPv6} {
		if ip, e := netip.ParseAddr(s); e == nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			ip = ip.Unmap()
			sources = append(sources, netip.PrefixFrom(ip, ip.BitLen()).String())
		}
	}
	if len(sources) == 0 {
		if ip, e := netip.ParseAddr(n.Address); e == nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			ip = ip.Unmap()
			sources = append(sources, netip.PrefixFrom(ip, ip.BitLen()).String())
		}
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("节点 %s 未上报来源公网 IP，请填写 sourceOverrides（实际 NAT 出口的 CIDR）", n.Name)
	}
	return sources, nil
}

func (s *Server) dirtyRelayPaths(nodeID string) {
	var chains []store.Chain
	if s.st.DB.Find(&chains).Error != nil {
		return
	}
	for _, ch := range chains {
		for _, h := range ch.Hops.V {
			if h.NodeID == nodeID {
				s.MarkDirty("relay source IP changed", chainNodes(&ch)...)
				break
			}
		}
	}
}

func checkRelayAccessChain(db *gorm.DB, in store.Inbound, ch *store.Chain) (map[string][]model.AccessRule, error) {
	out := map[string][]model.AccessRule{}
	p := in.RelayAccess.V
	if !p.Enabled || ch.Disabled {
		return out, nil
	}
	if !ch.Managed {
		return nil, fmt.Errorf("来源保护需要受管线路，请先迁移手工链路 %s", ch.Name)
	}
	if p.ProxyProtocol > 0 && ch.TunnelType != "" {
		return nil, fmt.Errorf("真实客户端 IP 当前支持原样 TCP 转发；请先迁移认证隧道 %s", ch.Name)
	}
	nodes, e := relayNodes(db, ch)
	if e != nil {
		return nil, e
	}
	for i := 1; i < len(ch.Hops.V); i++ {
		h := ch.Hops.V[i]
		n := nodes[h.NodeID]
		if !contains(n.AgentFeatures.V, model.RelayAccessFeature) {
			return nil, fmt.Errorf("节点 %s 需要先升级 Agent 并完成一次配置拉取", n.Name)
		}
		sources, e := relaySourceIPs(nodes[ch.Hops.V[i-1].NodeID], p)
		if e != nil {
			return nil, e
		}
		port := h.Port
		network := "tcp"
		if ch.TunnelType != "" {
			port = h.TunnelPort
		} else {
			m := model.Inbound{Protocol: in.Protocol.V, Transport: in.Transport.V}
			m.Normalize()
			network = m.Network()
			if i == len(ch.Hops.V)-1 {
				port = in.Port
			}
		}
		for _, nw := range strings.Split(network, ",") {
			out[n.ID] = append(out[n.ID], model.AccessRule{Protocol: nw, Port: port, Sources: sources})
		}
	}
	return out, nil
}

func validateRelayAccessInbound(db *gorm.DB, in store.Inbound) error {
	p := in.RelayAccess.V
	if p.ProxyProtocol != 0 && p.ProxyProtocol != 1 && p.ProxyProtocol != 2 {
		return fmt.Errorf("proxyProtocol 应为 0、1 或 2")
	}
	if !p.Enabled {
		if p.ProxyProtocol != 0 {
			return fmt.Errorf("传递真实客户端 IP 必须同时开启来源限制")
		}
		return nil
	}
	var node store.Node
	if e := db.First(&node, "id = ?", in.NodeID).Error; e != nil {
		return e
	}
	if !contains(node.AgentFeatures.V, model.RelayAccessFeature) {
		return fmt.Errorf("落地节点需要先升级 Agent 并完成配置拉取")
	}
	var count int64
	if e := db.Model(&store.Inbound{}).Where("node_id = ? AND port = ? AND id <> ?", in.NodeID, in.Port, in.ID).Count(&count).Error; e != nil {
		return e
	}
	if count > 0 {
		return fmt.Errorf("该端口还有其他入站，不能应用按端口的来源限制")
	}
	if e := db.Table("endpoints ep").Joins("JOIN cdn_profiles p ON p.id = ep.cdn_profile_id").Where("ep.inbound_id = ? AND p.caddy_fronting = ?", in.ID, true).Count(&count).Error; e != nil {
		return e
	}
	if count > 0 {
		return fmt.Errorf("请先移除 Caddy 共用回源入口，再启用来源限制")
	}
	// Local fallback traffic bypasses the external-port source guard. Keep
	// this policy off shared fallback masters/children until both are modeled.
	if len(in.Protocol.V.Fallbacks) != 0 {
		return fmt.Errorf("来源限制暂不支持共享回落主入站，请使用独立落地入站")
	}
	var siblings []store.Inbound
	if e := db.Where("node_id = ?", in.NodeID).Find(&siblings).Error; e != nil {
		return e
	}
	for _, sibling := range siblings {
		for _, f := range sibling.Protocol.V.Fallbacks {
			if f.InboundID == in.ID {
				return fmt.Errorf("来源限制暂不支持回落子入站，请先解除共享回落")
			}
		}
	}
	if p.ProxyProtocol > 0 {
		m := model.Inbound{Protocol: in.Protocol.V, Transport: in.Transport.V, Security: in.Security.V}
		m.Normalize()
		if m.Network() != "tcp" || m.Transport.Type != model.TransTCP || in.Core != model.CoreXray {
			return fmt.Errorf("真实客户端 IP 当前要求 Xray TCP 入站（可配 TLS / REALITY）")
		}
		m.Transport.AcceptProxyProtocol = true
		var cr store.Core
		if e := db.First(&cr, "node_id = ? AND type = ?", in.NodeID, in.Core).Error; e != nil {
			return e
		}
		caps, ok := core.Capabilities(in.Core, cr.Version)
		if !ok {
			return fmt.Errorf("未知内核")
		}
		if e := caps.SupportsInbound(&m); e != nil {
			return e
		}
	}
	return nil
}

func (s *Server) setRelayAccess(c *gin.Context) {
	var req struct {
		Enabled         *bool               `json:"enabled" binding:"required"`
		ProxyProtocol   int                 `json:"proxyProtocol"`
		SourceOverrides map[string][]string `json:"sourceOverrides"`
	}
	if e := c.ShouldBindJSON(&req); e != nil {
		badRequest(c, e)
		return
	}
	policy := store.RelayAccessPolicy{Enabled: *req.Enabled, ProxyProtocol: req.ProxyProtocol, SourceOverrides: req.SourceOverrides}
	if len(policy.SourceOverrides) > 256 {
		badRequest(c, fmt.Errorf("来源覆盖节点过多"))
		return
	}
	for id, sources := range policy.SourceOverrides {
		if len(sources) == 0 {
			badRequest(c, fmt.Errorf("节点 %s 的来源地址不能为空", id))
			return
		}
		if _, e := model.NormalizeAccessRules([]model.AccessRule{{Protocol: "tcp", Port: 1, Sources: sources}}); e != nil {
			badRequest(c, e)
			return
		}
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var changed []string
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if e := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "polaris-relay-config").Error; e != nil {
				return e
			}
		}
		var in store.Inbound
		if e := tx.First(&in, "id = ?", c.Param("id")).Error; e != nil {
			return e
		}
		if in.ServiceOnly {
			return badInput{fmt.Errorf("请选择面向用户的落地入站")}
		}
		in.RelayAccess = store.NewJSON(policy)
		if e := validateRelayAccessInbound(tx, in); e != nil {
			return badInput{e}
		}
		var chains []store.Chain
		if e := tx.Find(&chains).Error; e != nil {
			return e
		}
		for _, ch := range chains {
			hops := ch.Hops.V
			if len(hops) < 2 || hops[len(hops)-1].InboundID != in.ID {
				continue
			}
			if _, e := checkRelayAccessChain(tx, in, &ch); e != nil {
				return badInput{e}
			}
			nodes := map[string]store.Node{}
			var rows []store.Node
			if e := tx.Where("id IN ?", chainNodes(&ch)).Find(&rows).Error; e != nil {
				return e
			}
			for _, n := range rows {
				nodes[n.ID] = n
			}
			ch.TargetRevs = store.NewJSON(relayTargetRevs(&ch, nodes))
			if e := tx.Save(&ch).Error; e != nil {
				return e
			}
			changed = append(changed, chainNodes(&ch)...)
		}
		updates := map[string]any{"relay_access": in.RelayAccess}
		if policy.Enabled {
			updates["relay_only"] = true
		}
		if e := tx.Model(&store.Inbound{}).Where("id = ?", in.ID).Updates(updates).Error; e != nil {
			return e
		}
		changed = append(changed, in.NodeID)
		return tx.Create(&store.AuditLog{Admin: c.GetString("admin"), Action: "inbound.relay_access", Target: in.Tag, Detail: fmt.Sprintf("enabled=%t proxy=%d", policy.Enabled, policy.ProxyProtocol), IP: clientIP(c.Request)}).Error
	})
	if err != nil {
		relaySetupError(c, err)
		return
	}
	s.MarkDirty("relay access policy", changed...)
	c.JSON(http.StatusOK, policy)
}

// Every protected inbound keeps a deny-all rule even with no active paths.
func (s *Server) buildAccessRules(nodeID string) ([]model.AccessRule, error) {
	var ins []store.Inbound
	if e := s.st.DB.Where("enabled = ?", true).Find(&ins).Error; e != nil {
		return nil, e
	}
	var chains []store.Chain
	if e := s.st.DB.Find(&chains).Error; e != nil {
		return nil, e
	}
	var rules []model.AccessRule
	for _, in := range ins {
		if !in.RelayAccess.V.Enabled {
			continue
		}
		participates := in.NodeID == nodeID
		for _, ch := range chains {
			hops := ch.Hops.V
			if len(hops) < 2 || hops[len(hops)-1].InboundID != in.ID {
				continue
			}
			for _, h := range hops {
				participates = participates || h.NodeID == nodeID
			}
		}
		if !participates {
			continue
		}
		if e := validateRelayAccessInbound(s.st.DB, in); e != nil {
			return nil, e
		}
		if in.NodeID == nodeID {
			m := s.toModelInbound(&in)
			m.Normalize()
			for _, nw := range strings.Split(m.Network(), ",") {
				rules = append(rules, model.AccessRule{Protocol: nw, Port: in.Port})
			}
		}
		for _, ch := range chains {
			hops := ch.Hops.V
			if len(hops) < 2 || hops[len(hops)-1].InboundID != in.ID {
				continue
			}
			perNode, e := checkRelayAccessChain(s.st.DB, in, &ch)
			if errors.Is(e, errRelayNodeGone) {
				continue // the chain no longer forwards: its sources are not allowed
			}
			if e != nil {
				return nil, e
			}
			rules = append(rules, perNode[nodeID]...)
		}
	}
	return model.NormalizeAccessRules(rules)
}
