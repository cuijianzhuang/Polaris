package panel

import (
	"strconv"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Build user-visible status only for nodes already produced by the user's
// authorized subscription. Network success alone never becomes business success.
func (s *Server) portalNodeDetails(info *sub.Info, clientNodes []sub.Node) error {
	info.Links = nil
	if len(clientNodes) == 0 {
		return nil
	}
	var nodeIDs, inboundIDs, endpointIDs, chainIDs []string
	for _, n := range clientNodes {
		nodeIDs = append(nodeIDs, n.PanelNodeID)
		inboundIDs = append(inboundIDs, n.PanelInboundID)
		endpointIDs = append(endpointIDs, n.PanelEndpointID)
		chainIDs = append(chainIDs, n.PanelChainID)
	}
	var chains []store.Chain
	if err := s.st.DB.Where("id IN ?", chainIDs).Find(&chains).Error; err != nil {
		return err
	}
	chainMap := map[string]store.Chain{}
	for _, ch := range chains {
		chainMap[ch.ID] = ch
		for _, h := range ch.Hops.V {
			nodeIDs = append(nodeIDs, h.NodeID)
			inboundIDs = append(inboundIDs, h.InboundID)
		}
	}
	var ns []store.Node
	if err := s.st.DB.Where("id IN ?", nodeIDs).Find(&ns).Error; err != nil {
		return err
	}
	nodes := map[string]store.Node{}
	for _, n := range ns {
		nodes[n.ID] = n
	}
	var ins []store.Inbound
	if err := s.st.DB.Where("id IN ?", inboundIDs).Find(&ins).Error; err != nil {
		return err
	}
	inbounds := map[string]store.Inbound{}
	for _, in := range ins {
		inbounds[in.ID] = in
	}
	var es []store.Endpoint
	if err := s.st.DB.Where("id IN ?", endpointIDs).Find(&es).Error; err != nil {
		return err
	}
	endpoints := map[string]store.Endpoint{}
	for _, ep := range es {
		endpoints[ep.ID] = ep
	}
	var records []store.RelayBusinessStatus
	if err := s.st.DB.Where("chain_id IN ?", chainIDs).Find(&records).Error; err != nil {
		return err
	}
	proofs := map[string]store.RelayBusinessStatus{}
	for _, r := range records {
		proofs[r.ChainID] = r
	}
	var healthRows []store.InboundHealth
	if err := s.st.DB.Where("inbound_id IN ?", inboundIDs).Find(&healthRows).Error; err != nil {
		return err
	}
	health := map[string]store.InboundHealth{}
	for _, h := range healthRows {
		health[h.InboundID] = h
	}
	var cores []store.Core
	if err := s.st.DB.Where("node_id IN ?", nodeIDs).Find(&cores).Error; err != nil {
		return err
	}
	nodeCores := map[string][]store.Core{}
	for _, cr := range cores {
		nodeCores[cr.NodeID] = append(nodeCores[cr.NodeID], cr)
	}
	regions := map[string]string{}
	for _, r := range sub.Regions() {
		regions[r.Code] = r.Zh
	}
	info.Links = nil
	now := time.Now()
	for _, n := range clientNodes {
		if n.Source == "info" || sub.ShareLink(n) == "" {
			continue
		}
		rate := n.TrafficRate
		if rate <= 0 {
			rate = 1
		}
		region := regions[sub.NodeRegion(n)]
		if region == "" {
			region = "其他地区"
		}
		link := sub.PageLink{Name: n.Name, Protocol: sub.ProtocolLabel(n), Link: sub.ShareLink(n), Region: region, Rate: "×" + strconv.FormatFloat(rate, 'f', -1, 64), State: "unknown", StateLabel: "未验证", Reason: "尚未执行端到端业务验证，建议在客户端测试连接。"}
		if node, ok := nodes[n.PanelNodeID]; ok && portalNodeUnavailable(node) {
			link.State, link.StateLabel, link.Reason = "unavailable", "暂不可用", "所属节点当前离线，请选择其他节点。"
		} else if ch, ok := chainMap[n.PanelChainID]; ok {
			if ch.Managed && ch.Mode == "l4" {
				state, code, checked, _ := relayOverviewState(ch, nodes, inbounds, endpoints[n.PanelEndpointID], proofs[ch.ID], now, cores...)
				if checked != nil {
					link.CheckedAt = checked.Local().Format("2006-01-02 15:04")
				}
				if state == "passed" {
					link.State, link.StateLabel, link.Reason = "verified", "已验证可用", "近期端到端业务验证通过，实际连接体验以客户端测试为准。"
				} else if state == "failed" || code == "deployment_not_ready" {
					link.State, link.StateLabel, link.Reason = "unavailable", "暂不可用", "线路近期检查异常，请先选择其他节点或联系支持。"
				} else if state == "stale" {
					link.Reason = "检测结果已过期或配置已变化，请在客户端重新测试。"
				}
			} else if ch.Mode == "proxy" {
				for _, hop := range ch.Hops.V {
					if node, exists := nodes[hop.NodeID]; !exists || portalNodeUnavailable(node) {
						link.State, link.StateLabel, link.Reason = "unavailable", "暂不可用", "线路中的节点当前不可用，请选择其他节点。"
					}
				}
				for i, h := range ch.Hops.V[:max(0, len(ch.Hops.V)-1)] {
					if next, exists := inbounds[ch.Hops.V[i+1].InboundID]; exists {
						m := s.toModelInbound(&next)
						if nodeRefTransportError(nodeCores[h.NodeID], &m) != nil {
							link.State, link.StateLabel, link.Reason = "unavailable", "暂不可用", "线路当前配置不兼容，请选择其他节点或联系支持。"
						}
					}
				}
			}
		} else if h, ok := health[n.PanelInboundID]; ok && (n.PanelKind == "direct" || n.PanelKind == "") && now.Sub(h.CheckedAt) < 5*time.Minute && !h.CheckedAt.After(now) {
			link.CheckedAt = h.CheckedAt.Local().Format("2006-01-02 15:04")
			if !h.OK {
				link.State, link.StateLabel, link.Reason = "unavailable", "暂不可用", "近期连接检查失败，请先选择其他节点。"
			} else {
				link.Reason = "连接检查通过，尚未完成代理认证与数据传输验证。"
			}
		}
		info.Links = append(info.Links, link)
	}
	return nil
}

func portalNodeUnavailable(n store.Node) bool {
	return n.Revoked || n.Status == "offline" || n.Status == "pending" || n.Status == "error" || n.LastError != ""
}
