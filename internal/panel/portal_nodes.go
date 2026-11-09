package panel

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"github.com/gin-gonic/gin"
)

// Build user-visible status only for nodes already produced by the user's
// authorized subscription. Network success alone never becomes business success.
func (s *Server) portalNodeDetails(info *sub.Info, clientNodes []sub.Node) error {
	info.Links = nil
	info.TrafficMeterNote = ""
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
	for _, client := range clientNodes {
		if in, ok := inbounds[client.PanelInboundID]; ok && in.Core == "mihomo" {
			cumulative := false
			for _, cr := range nodeCores[in.NodeID] {
				if cr.Type == in.Core && cr.TrafficMeter.V.Mode == "kernel_counter" {
					cumulative = true
					break
				}
			}
			if cumulative {
				continue
			}
			info.TrafficMeterNote = "部分节点使用活动连接采样，可能漏掉短连接和连接关闭前的尾部流量；展示用量不等同于精确网络账单。"
			break
		}
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
		link := sub.PageLink{Name: n.Name, Protocol: sub.ProtocolLabel(n), Link: sub.ShareLink(n), Region: region, Rate: "×" + strconv.FormatFloat(rate, 'f', -1, 64), State: "unknown", StateLabel: "待检查", Online: "unknown", Network: "unknown", Business: "unknown", CheckScope: "暂无近期检查", Reason: "尚未执行端到端业务验证，建议在客户端测试连接。"}
		link.Key = portalNodeKey(n)
		node, knownNode := nodes[n.PanelNodeID]
		if knownNode {
			if portalNodeUnavailable(node) {
				link.Online = "offline"
			} else if node.Status == "online" {
				link.Online = "online"
			}
		}
		coreUnavailable := false
		if in, ok := inbounds[n.PanelInboundID]; ok {
			for _, core := range nodeCores[n.PanelNodeID] {
				if core.Type == in.Core && (core.Status == "stopped" || core.Status == "error" || core.Status == "failed" || core.Status == "uninstalled") {
					coreUnavailable = true
				}
			}
		}
		if knownNode && portalNodeUnavailable(node) {
			link.State, link.StateLabel, link.Reason = "unavailable", "暂不可用", "所属节点当前离线，请选择其他节点。"
		} else if coreUnavailable {
			link.State, link.StateLabel, link.Reason = "unavailable", "暂不可用", "该线路服务当前未运行，请选择其他节点或联系支持。"
		} else if knownNode && node.DesiredRev > node.AppliedRev {
			link.StateLabel, link.Reason = "配置更新中", "线路配置正在更新，等待同步完成后复查。"
		} else if ch, ok := chainMap[n.PanelChainID]; ok {
			if ch.Managed && ch.Mode == "l4" {
				state, code, checked, _ := relayOverviewState(ch, nodes, inbounds, endpoints[n.PanelEndpointID], proofs[ch.ID], now, cores...)
				if checked != nil {
					link.CheckedAt = checked.UTC().Format(time.RFC3339)
					link.CheckScope = "实际入口业务"
				}
				if state == "passed" {
					link.Business, link.Network, link.CheckScope = "passed", "passed", "实际入口业务"
					link.State, link.StateLabel, link.Reason = "verified", "业务已验证", "近期端到端业务验证通过，实际连接体验以客户端测试为准。"
				} else if state == "failed" || code == "deployment_not_ready" {
					if state == "failed" {
						link.Business = "failed"
					}
					link.State, link.StateLabel, link.Reason = "unavailable", "暂不可用", "线路近期检查异常，请先选择其他节点或联系支持。"
				} else if state == "stale" {
					link.Business, link.StateLabel = "stale", "待复检"
					link.Reason = "检测结果已过期或配置已变化，请在客户端重新测试。"
				}
			} else if ch.Mode == "proxy" {
				if ch.ProbedAt != nil && !ch.ProbedAt.After(now) && now.Sub(*ch.ProbedAt) < 5*time.Minute && len(ch.Hops.V) > 0 && len(ch.Probe.V) == len(ch.Hops.V) {
					complete := true
					for i, probe := range ch.Probe.V {
						if probe.NodeID != ch.Hops.V[i].NodeID || !probe.OK || probe.Skipped {
							complete = false
						}
					}
					if complete {
						link.Network, link.StateLabel, link.CheckScope = "passed", "连接通过", "节点间网络"
						link.CheckedAt = ch.ProbedAt.UTC().Format(time.RFC3339)
						link.Reason = "节点间连接检查通过，尚未完成客户端入口、代理认证与数据传输验证。"
					}
				}
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
		} else if h, ok := health[n.PanelInboundID]; ok && (n.PanelKind == "direct" || n.PanelKind == "") && !h.CheckedAt.IsZero() && !h.CheckedAt.After(now) && h.Error != probeUDPReason {
			link.CheckedAt = h.CheckedAt.UTC().Format(time.RFC3339)
			link.CheckScope = "服务端连接"
			if now.Sub(h.CheckedAt) >= 5*time.Minute {
				link.Network, link.StateLabel, link.Reason = "stale", "待复检", "连接检查结果已过期，尚未执行端到端业务复检。"
			} else if !h.OK {
				link.Network = "failed"
				link.State, link.StateLabel, link.Reason = "unavailable", "暂不可用", "近期连接检查失败，请先选择其他节点。"
			} else {
				link.Network, link.StateLabel = "passed", "连接通过"
				link.Reason = "连接检查通过，尚未完成代理认证与数据传输验证。"
			}
		}
		info.Links = append(info.Links, link)
	}
	return nil
}

func portalLinkKey(name, link string) string {
	identity := sha256.Sum256([]byte(name + "\x00" + link))
	return fmt.Sprintf("%x", identity[:16])
}

func portalNodeUnavailable(n store.Node) bool {
	return n.Revoked || n.Status == "offline" || n.Status == "pending" || n.Status == "error" || n.LastError != ""
}

// Status refresh repeats authorization and publication checks, without starting
// diagnostics or returning node credentials, raw kernel errors or private IDs.
func (s *Server) portalNodeStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var info sub.Info
	if s.servedUser(u) {
		nodes, err := s.subNodesFor(u, s.subCarrier(c.Request, u), s.subRegion(c.Request, u))
		if err != nil {
			serverError(c, err)
			return
		}
		if err := s.portalNodeDetails(&info, nodes); err != nil {
			serverError(c, err)
			return
		}
	}
	s.applyPortalEntryChecks(&info, u.ID)
	items := make([]gin.H, 0, len(info.Links))
	for _, link := range info.Links {
		state := link.State
		if state == "unknown" && link.Network == "passed" {
			state = "reachable"
		}
		items = append(items, gin.H{"key": link.Key, "state": state, "label": link.StateLabel, "reason": link.Reason, "checkedAt": link.CheckedAt, "online": link.Online, "network": link.Network, "business": link.Business, "scope": link.CheckScope})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "at": time.Now().UTC()})
}
