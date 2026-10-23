package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

const maxChainHops = 3

type chainReq struct {
	Name   string           `json:"name" binding:"required"`
	Mode   string           `json:"mode" binding:"required"` // l4 / proxy
	Hops   []store.ChainHop `json:"hops" binding:"required"`
	Remark string           `json:"remark"`
}

func (s *Server) listChains(c *gin.Context) {
	var cs []store.Chain
	s.st.DB.Order("name").Find(&cs)
	c.JSON(http.StatusOK, cs)
}

// outboundTypeFor maps an inbound protocol to the client outbound used to reach it.
func outboundTypeFor(p model.Protocol) (model.OutboundType, bool) {
	switch p {
	case model.ProtoVLESS:
		return model.OutVLESS, true
	case model.ProtoVMess:
		return model.OutVMess, true
	case model.ProtoTrojan:
		return model.OutTrojan, true
	case model.ProtoShadowsocks:
		return model.OutShadowsocks, true
	case model.ProtoHysteria2:
		return model.OutHysteria2, true
	case model.ProtoSOCKS:
		return model.OutSOCKS, true
	case model.ProtoHTTP:
		return model.OutHTTP, true
	}
	return "", false
}

// nodeSupportsOutbound checks every proxying core on the node can render t.
func (s *Server) nodeSupportsOutbound(nodeID string, t model.OutboundType) error {
	return s.nodeSupportsOutboundOn(s.st.DB, nodeID, t)
}

func (s *Server) nodeSupportsOutboundOn(db *gorm.DB, nodeID string, t model.OutboundType) error {
	var cores []store.Core
	if err := db.Where("node_id = ?", nodeID).Find(&cores).Error; err != nil {
		return err
	}
	ok := false
	for _, cr := range cores {
		caps, _ := core.Capabilities(cr.Type, cr.Version)
		switch cr.Type {
		case model.CoreXray, model.CoreSingbox, model.CoreMihomo:
			if !caps.SupportsOutbound(t) {
				return fmt.Errorf("%s %s 不支持 %s 出站", cr.Type, cr.Version, t)
			}
			ok = true
		case model.CoreHysteria:
			// Hysteria carries direct / SOCKS5 / HTTP; rules to other
			// egresses just don't apply to its inbounds.
			ok = ok || caps.SupportsOutbound(t)
		}
	}
	if !ok {
		return fmt.Errorf("节点没有可承载 %s 出站的内核（Xray / sing-box / mihomo；Hysteria2 仅支持 SOCKS5 / HTTP）", t)
	}
	return nil
}

// validateChain enforces acyclic paths, the hop limit and per-hop support.
func (s *Server) validateChain(req *chainReq, chainID string) error {
	if req.Mode != "l4" && req.Mode != "proxy" {
		return fmt.Errorf("mode must be l4 or proxy")
	}
	if len(req.Hops) < 2 {
		return fmt.Errorf("至少需要一个中转和一个落地")
	}
	if len(req.Hops)-1 > maxChainHops {
		return fmt.Errorf("最多 %d 跳", maxChainHops)
	}
	seen := map[string]bool{}
	for _, h := range req.Hops {
		if seen[h.NodeID] {
			return fmt.Errorf("链路成环：节点重复出现")
		}
		seen[h.NodeID] = true
		var n store.Node
		if err := s.st.DB.First(&n, "id = ?", h.NodeID).Error; err != nil {
			return fmt.Errorf("节点 %s 不存在", h.NodeID)
		}
	}
	last := req.Hops[len(req.Hops)-1]
	var landing store.Inbound
	if err := s.st.DB.First(&landing, "id = ? AND node_id = ?", last.InboundID, last.NodeID).Error; err != nil {
		return fmt.Errorf("落地入站不存在")
	}
	if landing.RelayAccess.V.Enabled {
		return fmt.Errorf("该落地已启用来源保护，请使用入站一键中转接口创建受管线路")
	}
	for i, h := range req.Hops[:len(req.Hops)-1] {
		switch req.Mode {
		case "l4":
			if h.Port <= 0 || h.Port > 65535 {
				return fmt.Errorf("第 %d 跳需要转发端口", i+1)
			}
			lm := s.toModelInbound(&landing)
			probe := model.Inbound{Port: h.Port, Protocol: lm.Protocol, Transport: lm.Transport}
			probe.Normalize()
			if err := s.portConflictExcept(h.NodeID, &probe, "", chainID); err != nil {
				return err
			}
		case "proxy":
			var relay store.Inbound
			if err := s.st.DB.First(&relay, "id = ? AND node_id = ?", h.InboundID, h.NodeID).Error; err != nil {
				return fmt.Errorf("第 %d 跳需要选择中转节点上的入站", i+1)
			}
			var next store.Inbound
			if err := s.st.DB.First(&next, "id = ?", req.Hops[i+1].InboundID).Error; err != nil {
				return fmt.Errorf("第 %d 跳的下一跳需要入站", i+1)
			}
			t, ok := outboundTypeFor(next.Protocol.V.Type)
			if !ok {
				return fmt.Errorf("%s 不能作为链路下一跳", next.Protocol.V.Type)
			}
			if err := s.nodeSupportsOutbound(h.NodeID, t); err != nil {
				return fmt.Errorf("第 %d 跳：%w", i+1, err)
			}
		}
	}
	return nil
}

// materializeChain creates the objects a chain needs: service account,
// node_ref outbounds and routing rules (proxy) or a relay endpoint (l4).
func (s *Server) materializeChain(tx *gorm.DB, ch *store.Chain) error {
	hops := ch.Hops.V
	landing := hops[len(hops)-1]
	if ch.Mode == "l4" {
		return tx.Create(&store.Endpoint{ID: store.NewID(), InboundID: landing.InboundID, Kind: "relay", RelayChainID: ch.ID}).Error
	}
	svc := store.User{ID: store.NewID(), Username: "svc-" + ch.ID, UUID: pki.UUID(), Password: pki.Password(24),
		SSSeed: pki.Token(24), Status: "active", SubToken: pki.Token(32), IsService: true, Remark: "chain " + ch.Name,
		OnlineIPs: store.NewJSON([]string{})}
	if err := tx.Create(&svc).Error; err != nil {
		return err
	}
	ch.ServiceUserID = svc.ID
	if err := tx.Model(ch).Update("service_user_id", svc.ID).Error; err != nil {
		return err
	}
	for i := 0; i < len(hops)-1; i++ {
		var relay store.Inbound
		if err := tx.First(&relay, "id = ?", hops[i].InboundID).Error; err != nil {
			return err
		}
		tag := fmt.Sprintf("chain-%s-%d", ch.ID[:8], i)
		if err := tx.Create(&store.Outbound{ID: store.NewID(), NodeID: hops[i].NodeID, Tag: tag, Type: model.OutNodeRef,
			NodeRefInboundID: hops[i+1].InboundID, ChainID: ch.ID, ServiceUserID: svc.ID, Remark: "chain " + ch.Name}).Error; err != nil {
			return err
		}
		if err := tx.Create(&store.RoutingRule{ID: store.NewID(), NodeID: hops[i].NodeID, ChainID: ch.ID, Priority: 1,
			Match: store.NewJSON(model.RouteMatch{Inbound: []string{relay.Tag}}), OutboundTag: tag, Remark: "chain " + ch.Name}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) dematerializeChain(tx *gorm.DB, ch *store.Chain) error {
	if err := tx.Where("chain_id = ?", ch.ID).Delete(&store.Outbound{}).Error; err != nil {
		return err
	}
	if err := tx.Where("chain_id = ?", ch.ID).Delete(&store.RoutingRule{}).Error; err != nil {
		return err
	}
	if err := tx.Where("relay_chain_id = ?", ch.ID).Delete(&store.Endpoint{}).Error; err != nil {
		return err
	}
	if ch.ServiceUserID != "" {
		if err := tx.Where("id = ? AND is_service = ?", ch.ServiceUserID, true).Delete(&store.User{}).Error; err != nil {
			return err
		}
	}
	return nil
}

func chainNodes(ch *store.Chain) []string {
	var ids []string
	for _, h := range ch.Hops.V {
		ids = append(ids, h.NodeID)
	}
	return ids
}

func (s *Server) createChain(c *gin.Context) {
	var req chainReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.validateChain(&req, ""); err != nil {
		badRequest(c, err)
		return
	}
	ch := store.Chain{ID: store.NewID(), Name: req.Name, Mode: req.Mode, Hops: store.NewJSON(req.Hops), Remark: req.Remark,
		Probe: store.NewJSON([]store.HopProbe{})}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&ch).Error; err != nil {
			return err
		}
		return s.materializeChain(tx, &ch)
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("chain "+ch.Name, chainNodes(&ch)...)
	s.auditCtx(c, "chain.create", ch.Name, ch.Mode)
	c.JSON(http.StatusCreated, ch)
}

func (s *Server) updateChain(c *gin.Context) {
	var ch store.Chain
	if err := s.st.DB.First(&ch, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if ch.Managed {
		c.JSON(http.StatusConflict, gin.H{"error": "该线路由一键中转管理，请使用入站中转接口；更换路径需重新预览"})
		return
	}
	var req chainReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.validateChain(&req, ch.ID); err != nil {
		badRequest(c, err)
		return
	}
	before := chainNodes(&ch)
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := s.dematerializeChain(tx, &ch); err != nil {
			return err
		}
		ch.Name, ch.Mode, ch.Hops, ch.Remark, ch.ServiceUserID = req.Name, req.Mode, store.NewJSON(req.Hops), req.Remark, ""
		if err := tx.Save(&ch).Error; err != nil {
			return err
		}
		return s.materializeChain(tx, &ch)
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("chain "+ch.Name, append(before, chainNodes(&ch)...)...)
	s.auditCtx(c, "chain.update", ch.Name, ch.Mode)
	c.JSON(http.StatusOK, ch)
}

func (s *Server) deleteChain(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var ch store.Chain
	if err := s.st.DB.First(&ch, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := s.dematerializeChain(tx, &ch); err != nil {
			return err
		}
		return tx.Delete(&ch).Error
	})
	if err != nil {
		serverError(c, err)
		return
	}
	s.MarkDirty("chain deleted", chainNodes(&ch)...)
	s.auditCtx(c, "chain.delete", ch.Name, "")
	c.Status(http.StatusNoContent)
}

// probeChain asks each hop's agent to measure latency to the next hop; the
// landing node probes an HTTP URL to check its egress.
func (s *Server) probeChain(c *gin.Context) {
	var ch store.Chain
	if err := s.st.DB.First(&ch, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if ch.Disabled {
		badRequest(c, fmt.Errorf("线路已停用"))
		return
	}
	probes := s.runChainProbe(&ch)
	c.JSON(http.StatusOK, gin.H{"probe": probes, "probedAt": ch.ProbedAt})
}

func (s *Server) runChainProbe(ch *store.Chain) []store.HopProbe {
	hops := ch.Hops.V
	udpOnly := false
	if ch.Mode == "l4" && ch.TunnelType == "" && len(hops) > 0 {
		var in store.Inbound
		if s.st.DB.First(&in, "id = ?", hops[len(hops)-1].InboundID).Error == nil {
			m := s.toModelInbound(&in)
			m.Normalize()
			udpOnly = !m.UsesTCP()
		}
	}
	probes := make([]store.HopProbe, len(hops))
	var wg sync.WaitGroup
	for i := range hops {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := hops[i]
			args := map[string]string{}
			if i < len(hops)-1 {
				if udpOnly {
					probes[i] = store.HopProbe{NodeID: h.NodeID, Skipped: true, Error: "原样 UDP 转发不使用 TCP 端口探测"}
					return
				}
				var next store.Node
				if s.st.DB.First(&next, "id = ?", hops[i+1].NodeID).Error != nil {
					probes[i] = store.HopProbe{NodeID: h.NodeID, Error: "next node missing"}
					return
				}
				port := hops[i+1].Port
				if i+1 == len(hops)-1 || ch.Mode == "proxy" {
					var in store.Inbound
					if s.st.DB.First(&in, "id = ?", hops[i+1].InboundID).Error == nil {
						port = in.Port
					}
				}
				if ch.TunnelType != "" {
					port = hops[i+1].TunnelPort
				}
				args["tcp"] = net.JoinHostPort(nodeHost(&next), strconv.Itoa(port))
			} else {
				args["http"] = "https://www.gstatic.com/generate_204"
			}
			res, err := s.hub.Command(s.ctxBG(), h.NodeID, "probe", args, 15*time.Second)
			p := store.HopProbe{NodeID: h.NodeID}
			switch {
			case err != nil:
				p.Error = err.Error()
			case !res.Ok:
				p.Error = res.Error
			default:
				p.OK = true
				p.LatencyMs, _ = strconv.Atoi(res.Output)
			}
			probes[i] = p
		}(i)
	}
	wg.Wait()
	now := time.Now()
	ch.Probe, ch.ProbedAt = store.NewJSON(probes), &now
	s.st.DB.Model(ch).Updates(map[string]any{"probe": ch.Probe, "probed_at": now})
	return probes
}

// --- outbounds & routing ---

func (s *Server) listOutbounds(c *gin.Context) {
	var obs []store.Outbound
	s.st.DB.Where("node_id = ?", c.Param("id")).Order("created_at").Find(&obs)
	c.JSON(http.StatusOK, obs)
}

type outboundReq struct {
	Tag              string             `json:"tag" binding:"required"`
	Type             model.OutboundType `json:"type" binding:"required"`
	Settings         json.RawMessage    `json:"settings"`
	NodeRefInboundID string             `json:"nodeRefInboundId"`
	Detour           string             `json:"detour"`
	Remark           string             `json:"remark"`
}

func (s *Server) saveOutbound(c *gin.Context, o *store.Outbound, req *outboundReq) {
	if err := s.storeOutbound(c.Param("id"), o, req); err != nil {
		var bad badInput
		if errors.As(err, &bad) {
			badRequest(c, bad.error)
		} else {
			dbError(c, err)
		}
		return
	}
	s.auditCtx(c, "outbound.save", o.Tag, string(o.Type))
	c.JSON(http.StatusOK, o)
}

// badInput marks a storeOutbound error caused by the request (400), not the
// database.
type badInput struct{ error }

// storeOutbound validates and saves an outbound on a node and schedules the
// affected nodes.
func (s *Server) storeOutbound(nodeID string, o *store.Outbound, req *outboundReq) error {
	if err := s.mutateOutbounds(nodeID, func(tx *gorm.DB) error { return s.storeOutboundTx(tx, nodeID, o, req) }); err != nil {
		return err
	}

	nodes := []string{nodeID}
	if o.NodeRefInboundID != "" {
		var in store.Inbound
		if s.st.DB.First(&in, "id = ?", o.NodeRefInboundID).Error == nil {
			nodes = append(nodes, in.NodeID)
		}
	}
	s.MarkDirty("outbound "+o.Tag, nodes...)
	return nil
}

func (s *Server) storeOutboundTx(tx *gorm.DB, nodeID string, o *store.Outbound, req *outboundReq) error {
	if req.Tag == "direct" || req.Tag == "block" || req.Tag == "api" {
		return badInput{fmt.Errorf("tag %q is reserved", req.Tag)}
	}
	t := req.Type
	if t == model.OutNodeRef {
		var in store.Inbound
		if err := tx.First(&in, "id = ?", req.NodeRefInboundID).Error; err != nil {
			return badInput{fmt.Errorf("referenced inbound: %w", err)}
		}
		if in.NodeID == nodeID {
			return badInput{fmt.Errorf("不能引用本节点的入站")}
		}
		var ok bool
		if t, ok = outboundTypeFor(in.Protocol.V.Type); !ok {
			return badInput{fmt.Errorf("%s 不能作为出站目标", in.Protocol.V.Type)}
		}
	}
	if err := s.nodeSupportsOutboundOn(tx, nodeID, t); err != nil {
		return badInput{err}
	}
	if req.Detour != "" {
		if err := s.checkDetourOn(tx, nodeID, o.ID, req.Tag, req.Detour); err != nil {
			return badInput{err}
		}
	}
	var tmp model.OutboundSettings
	if len(req.Settings) > 0 {
		if err := json.Unmarshal(req.Settings, &tmp); err != nil {
			return badInput{fmt.Errorf("settings: %w", err)}
		}
	}
	if req.Type.IsGroup() {
		if err := s.validateBalancerOn(tx, nodeID, req.Tag, tmp); err != nil {
			return badInput{err}
		}
	} else if tmp.Fallback != "" {
		return badInput{fmt.Errorf("仅均衡组支持兜底出站")}
	}
	if err := s.validateOutboundChange(tx, nodeID, o.ID, req); err != nil {
		return badInput{err}
	}

	o.NodeID, o.Tag, o.Type, o.Settings, o.NodeRefInboundID, o.Detour, o.Remark =
		nodeID, req.Tag, req.Type, req.Settings, req.NodeRefInboundID, req.Detour, req.Remark
	err := func() error {
		if o.Type == model.OutNodeRef && o.ServiceUserID == "" {
			svc := store.User{ID: store.NewID(), Username: "svc-out-" + o.ID, UUID: pki.UUID(), Password: pki.Password(24),
				SSSeed: pki.Token(24), Status: "active", SubToken: pki.Token(32), IsService: true, Remark: "outbound " + o.Tag,
				OnlineIPs: store.NewJSON([]string{})}
			if err := tx.Create(&svc).Error; err != nil {
				return err
			}
			o.ServiceUserID = svc.ID
		}
		return tx.Save(o).Error
	}()
	if err != nil {
		return err
	}
	return nil
}

// checkDetour rejects detour loops within the node's outbounds.
func (s *Server) checkDetour(nodeID, selfID, selfTag, detour string) error {
	return s.checkDetourOn(s.st.DB, nodeID, selfID, selfTag, detour)
}

func (s *Server) checkDetourOn(db *gorm.DB, nodeID, selfID, selfTag, detour string) error {
	var obs []store.Outbound
	db.Where("node_id = ? AND id <> ?", nodeID, selfID).Find(&obs)
	next := map[string]string{selfTag: detour}
	exists := map[string]bool{}
	for _, o := range obs {
		next[o.Tag] = o.Detour
		exists[o.Tag] = true
	}
	if !exists[detour] {
		return fmt.Errorf("detour %q 不存在", detour)
	}
	cur, steps := selfTag, 0
	for cur != "" {
		cur = next[cur]
		steps++
		if cur == selfTag {
			return fmt.Errorf("出站链成环")
		}
		if steps > maxChainHops+1 {
			return fmt.Errorf("出站链超过 %d 跳", maxChainHops)
		}
	}
	return nil
}

func (s *Server) createOutbound(c *gin.Context) {
	var req outboundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.saveOutbound(c, &store.Outbound{ID: store.NewID()}, &req)
}

func (s *Server) updateOutbound(c *gin.Context) {
	var o store.Outbound
	if err := s.st.DB.First(&o, "id = ? AND node_id = ?", c.Param("oid"), c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if o.ChainID != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "该出站由链路管理，请编辑链路"})
		return
	}
	var req outboundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.saveOutbound(c, &o, &req)
}

func (s *Server) deleteOutbound(c *gin.Context) {
	var o store.Outbound
	err := s.mutateOutbounds(c.Param("id"), func(tx *gorm.DB) error {
		if err := tx.First(&o, "id = ? AND node_id = ?", c.Param("oid"), c.Param("id")).Error; err != nil {
			return err
		}
		if o.ChainID != "" {
			return badInput{fmt.Errorf("该出站由链路管理，请删除链路")}
		}
		if err := s.checkOutboundReferences(tx, o.NodeID, o.ID, o.Tag); err != nil {
			return badInput{err}
		}
		if err := tx.Delete(&o).Error; err != nil {
			return err
		}
		if o.ServiceUserID != "" {
			return tx.Where("id = ? AND is_service = ?", o.ServiceUserID, true).Delete(&store.User{}).Error
		}
		return nil
	})
	if err != nil {
		var bad badInput
		if errors.As(err, &bad) {
			c.JSON(http.StatusConflict, gin.H{"error": bad.Error()})
		} else {
			dbError(c, err)
		}
		return
	}

	nodes := []string{o.NodeID}
	if o.NodeRefInboundID != "" {
		nodes = append(nodes, s.nodesForInbounds(o.NodeRefInboundID)...)
	}
	s.MarkDirty("outbound deleted", nodes...)
	s.auditCtx(c, "outbound.delete", o.Tag, "")
	c.Status(http.StatusNoContent)
}

func (s *Server) getRouting(c *gin.Context) {
	var rs []store.RoutingRule
	s.st.DB.Where("node_id = ?", c.Param("id")).Order("priority").Find(&rs)
	c.JSON(http.StatusOK, rs)
}

// putRouting replaces the node's manual rules (chain-managed rules are kept).
func (s *Server) putRouting(c *gin.Context) {
	nodeID := c.Param("id")
	var req []struct {
		Match       model.RouteMatch `json:"match"`
		OutboundTag string           `json:"outboundTag"`
		Priority    int              `json:"priority"`
		Remark      string           `json:"remark"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var tags []string
	q := s.st.DB.Model(&store.Outbound{})
	if nodeID != model.GlobalNodeID {
		q = q.Where("node_id = ?", nodeID) // global rules may name any node's egress
	}
	q.Pluck("tag", &tags)
	valid := map[string]bool{"direct": true, "block": true}
	for _, t := range tags {
		valid[t] = true
	}
	sets := s.ruleSetsByTag()
	for _, r := range req {
		if !valid[r.OutboundTag] {
			badRequest(c, fmt.Errorf("出站 %q 不存在", r.OutboundTag))
			return
		}
		for _, t := range r.Match.RuleSets {
			if sets[t] == nil {
				badRequest(c, fmt.Errorf("规则集 %q 不存在", t))
				return
			}
		}
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("node_id = ? AND chain_id = ''", nodeID).Delete(&store.RoutingRule{}).Error; err != nil {
			return err
		}
		for _, r := range req {
			if err := tx.Create(&store.RoutingRule{ID: store.NewID(), NodeID: nodeID, Match: store.NewJSON(r.Match),
				OutboundTag: r.OutboundTag, Priority: r.Priority, Remark: r.Remark}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		serverError(c, err)
		return
	}
	s.markRoutingDirty(nodeID)
	s.auditCtx(c, "routing.update", nodeID, fmt.Sprintf("%d rules", len(req)))
	s.getRouting(c)
}
