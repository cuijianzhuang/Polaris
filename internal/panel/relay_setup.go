package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

const relayPlanTTL = 10 * time.Minute

type relaySetupRequest struct {
	Name      string           `json:"name"`
	Mode      string           `json:"mode"`      // l4 (transparent) / tunnel (authenticated GOST)
	Transport string           `json:"transport"` // tcp / ws for tunnel
	Relays    []store.ChainHop `json:"relays"`
	Remark    string           `json:"remark"`
}

type relaySetupPlan struct {
	Version   int
	Network   string
	Owner     string
	Expires   time.Time
	Chain     store.Chain
	Auth      string // token is encrypted; Chain.TunnelAuth is deliberately write-only
	InboundID string
	Inputs    string
}

type relaySetupNode struct {
	NodeID      string `json:"nodeId"`
	Name        string `json:"name"`
	DesiredRev  int64  `json:"desiredRev"`
	AppliedRev  int64  `json:"appliedRev"`
	RequiredRev int64  `json:"requiredRev"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
}

type inboundRelayView struct {
	BusinessDiagnostic relayBusinessSupport `json:"businessDiagnostic"`
	store.Chain
	ShareAddress string           `json:"shareAddress,omitempty"`
	SharePort    int              `json:"sharePort,omitempty"`
	ShareName    string           `json:"shareName,omitempty"`
	Address      string           `json:"address"`
	Port         int              `json:"port"`
	State        string           `json:"state"`
	Nodes        []relaySetupNode `json:"nodes"`
}

func relayInputHash(in store.Inbound, nodes map[string]store.Node, ch *store.Chain) string {
	values := []any{in.ID, in.NodeID, in.Tag, in.Port, in.Listen, in.Enabled, in.ServiceOnly, in.Protocol, in.Transport, in.Security, in.Extra, in.RelayAccess}
	for _, h := range ch.Hops.V {
		n := nodes[h.NodeID]
		values = append(values, []any{n.ID, n.Revoked, nodeHost(&n), n.PublicIPv4, n.PublicIPv6, n.AgentFeatures})
	}
	return templateHash(values)
}

// Load the complete port allocation state through the same transaction. Ports
// are reserved conservatively across TCP and UDP, including disabled resources.
func relayUsedPorts(db *gorm.DB, nodeID string) (map[int]bool, error) {
	used := map[int]bool{}
	var ins []store.Inbound
	if err := db.Where("node_id = ?", nodeID).Find(&ins).Error; err != nil {
		return nil, err
	}
	for _, in := range ins {
		used[in.Port] = true
	}
	var fs []store.ForwardRule
	if err := db.Where("node_id = ?", nodeID).Find(&fs).Error; err != nil {
		return nil, err
	}
	for _, f := range fs {
		used[f.ListenPort] = true
	}
	var chains []store.Chain
	if err := db.Where("mode = ?", "l4").Find(&chains).Error; err != nil {
		return nil, err
	}
	for _, ch := range chains {
		for _, h := range ch.Hops.V {
			if h.NodeID == nodeID {
				used[h.Port] = true
				used[h.TunnelPort] = true
			}
		}
	}
	return used, nil
}

func relayPort(used map[int]bool, requested int) (int, error) {
	if requested < 0 || requested > 65535 {
		return 0, fmt.Errorf("端口应为 1–65535，留空自动分配")
	}
	if requested > 0 {
		if used[requested] {
			return 0, fmt.Errorf("端口 %d 已被使用，请重新预览", requested)
		}
		used[requested] = true
		return requested, nil
	}
	start := 20000 + rand.IntN(40001)
	for i := 0; i <= 40000; i++ {
		p := 20000 + (start-20000+i)%40001
		if !used[p] {
			used[p] = true
			return p, nil
		}
	}
	return 0, fmt.Errorf("没有可用的转发端口")
}

func relayLanding(db *gorm.DB, id string) (store.Inbound, error) {
	var in store.Inbound
	if err := db.First(&in, "id = ?", id).Error; err != nil {
		return in, err
	}
	if !in.Enabled || in.ServiceOnly {
		return in, fmt.Errorf("请选择已启用、面向用户的落地入站")
	}
	return in, nil
}

// errRelayNodeGone: a hop's node was revoked or has no address. Building
// specs skips such a chain (it stops forwarding) rather than failing every
// node on it.
var errRelayNodeGone = errors.New("链路节点不可用")

func relayNodes(db *gorm.DB, ch *store.Chain) (map[string]store.Node, error) {
	out := map[string]store.Node{}
	for _, h := range ch.Hops.V {
		if _, found := out[h.NodeID]; found {
			return nil, fmt.Errorf("链路不能重复使用同一个节点")
		}
		var n store.Node
		if err := db.First(&n, "id = ?", h.NodeID).Error; err != nil {
			return nil, err
		}
		if n.Revoked || nodeHost(&n) == "" {
			return nil, fmt.Errorf("%w：节点 %s 已撤销或没有可用地址", errRelayNodeGone, n.Name)
		}
		out[n.ID] = n
	}
	return out, nil
}

func (s *Server) planInboundRelay(c *gin.Context, req relaySetupRequest) (*relaySetupPlan, map[string]store.Node, []string, error) {
	return s.planInboundRelayWithPorts(c, req, nil)
}

func (s *Server) planInboundRelayWithPorts(c *gin.Context, req relaySetupRequest, reserved map[string]map[int]bool) (*relaySetupPlan, map[string]store.Node, []string, error) {
	if req.Mode == "" {
		req.Mode = "l4"
	}
	if req.Mode != "l4" && req.Mode != "tunnel" {
		return nil, nil, nil, fmt.Errorf("模式应为 l4 或 tunnel")
	}
	if len(req.Relays) < 1 || len(req.Relays) > maxChainHops {
		return nil, nil, nil, fmt.Errorf("请选择 1–%d 个中转节点", maxChainHops)
	}
	if req.Mode == "tunnel" {
		if req.Transport == "" {
			req.Transport = "tcp"
		}
		if req.Transport != "tcp" && req.Transport != "ws" {
			return nil, nil, nil, fmt.Errorf("一键隧道当前支持 tcp / ws")
		}
	} else if req.Transport != "" {
		return nil, nil, nil, fmt.Errorf("原样转发不需要隧道传输参数")
	}
	in, err := relayLanding(s.st.DB, c.Param("id"))
	if err != nil {
		return nil, nil, nil, err
	}
	if m := s.referencingMaster(in.ID); m != "" {
		// A fallback child is reached through its master's port and TLS; a
		// relay to its own port would not match what clients are given.
		return nil, nil, nil, fmt.Errorf("该入站是入站「%s」的回落目标，请对「%s」设置中转", m, m)
	}
	if req.Mode == "l4" && isLoopbackHost(in.Listen) {
		return nil, nil, nil, fmt.Errorf("仅本机监听的入站请使用 tunnel 模式")
	}
	// An accepting PROXY protocol inbound cannot receive a plain tunnel stream.
	if in.Transport.V.AcceptProxyProtocol && in.RelayAccess.V.ProxyProtocol == 0 {
		return nil, nil, nil, fmt.Errorf("该入站要求 PROXY protocol，请使用现有转发配置显式匹配发送端")
	}
	for _, h := range req.Relays {
		if h.InboundID != "" {
			return nil, nil, nil, fmt.Errorf("中转只需选择节点，无需配置代理入站")
		}
	}
	hops := append([]store.ChainHop(nil), req.Relays...)
	hops = append(hops, store.ChainHop{NodeID: in.NodeID, InboundID: in.ID})
	ch := store.Chain{ID: store.NewID(), Name: strings.TrimSpace(req.Name), Mode: "l4", Managed: true, Hops: store.NewJSON(hops), Remark: req.Remark, Probe: store.NewJSON([]store.HopProbe{})}
	if req.Mode == "tunnel" {
		ch.TunnelType = req.Transport
	}
	nodes, err := relayNodes(s.st.DB, &ch)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = validateRelayAccessInbound(s.st.DB, in); err != nil {
		return nil, nil, nil, err
	}
	if _, err = checkRelayAccessChain(s.st.DB, in, &ch); err != nil {
		return nil, nil, nil, err
	}
	if ch.Name == "" {
		n := nodes[hops[0].NodeID]
		ch.Name = truncate(n.Name+" → "+in.Tag+" · "+ch.ID[:6], 64)
	}
	if len([]rune(ch.Name)) > 64 || len([]rune(ch.Remark)) > 255 {
		return nil, nil, nil, fmt.Errorf("名称最多 64 字，备注最多 255 字")
	}
	warnings := []string{}
	for id, n := range nodes {
		if !inScope(c, id) {
			return nil, nil, nil, fmt.Errorf("无权访问节点 %s", id)
		}
		if n.Status != "online" && n.Status != "syncing" {
			warnings = append(warnings, "节点 "+n.Name+" 当前未在线；远端占用端口无法完全确认，创建后等待 Agent 下发")
		}
		if n.NoFirewall {
			warnings = append(warnings, "节点 "+n.Name+" 未开启自动防火墙，请手动放行预览中的公网端口")
		}
		used, e := relayUsedPorts(s.st.DB, id)
		if e != nil {
			return nil, nil, nil, e
		}
		for p := range reserved[id] {
			used[p] = true
		}
		for p := range s.nodeBusyPorts(id) {
			used[p] = true
		}
		for i := range ch.Hops.V {
			h := &ch.Hops.V[i]
			if h.NodeID != id {
				continue
			}
			if i < len(hops)-1 {
				h.Port, e = relayPort(used, h.Port)
				if e != nil {
					return nil, nil, nil, e
				}
			}
			if ch.TunnelType != "" && i > 0 {
				h.TunnelPort, e = relayPort(used, h.TunnelPort)
				if e != nil {
					return nil, nil, nil, e
				}
			} else if h.TunnelPort != 0 {
				return nil, nil, nil, fmt.Errorf("此跳不需要隧道接收端口")
			}
		}
	}
	auth := ""
	if ch.TunnelType != "" {
		auth = s.st.Box.Seal(pki.Password(32))
		warnings = append(warnings, "TCP / WS 隧道提供认证和转发，不增加 TLS 加密；客户端原有协议及加密保持不变")
	}
	m := s.toModelInbound(&in)
	m.Normalize()
	return &relaySetupPlan{Version: 1, Network: m.Network(), Owner: idempotencyIdentity(c), Expires: time.Now().Add(relayPlanTTL), Chain: ch, Auth: auth, InboundID: in.ID, Inputs: relayInputHash(in, nodes, &ch)}, nodes, warnings, nil
}

func (s *Server) previewInboundRelay(c *gin.Context) {
	var req relaySetupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	plan, nodes, warnings, err := s.planInboundRelay(c, req)
	if err != nil {
		badRequest(c, err)
		return
	}
	if plan.Owner == "" {
		badRequest(c, fmt.Errorf("无法确定操作账号"))
		return
	}
	b, _ := json.Marshal(plan)
	var inbound store.Inbound
	if err := s.st.DB.First(&inbound, "id = ?", plan.InboundID).Error; err != nil {
		dbError(c, err)
		return
	}
	view := relayPlanPreview(plan, nodes, inbound)
	view["planToken"], view["expiresAt"], view["warnings"] = s.st.Box.Seal(string(b)), plan.Expires, warnings
	c.JSON(http.StatusOK, view)
}

func (s *Server) createInboundRelay(c *gin.Context) {
	var req struct {
		PlanToken string `json:"planToken" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	raw, err := s.st.Box.Open(req.PlanToken)
	var plan relaySetupPlan
	if err != nil || !strings.HasPrefix(req.PlanToken, "enc:v1:") || json.Unmarshal([]byte(raw), &plan) != nil || plan.Version != 1 || plan.InboundID != c.Param("id") || plan.Owner == "" || plan.Owner != idempotencyIdentity(c) {
		badRequest(c, fmt.Errorf("无效的中转预览，请重新预览"))
		return
	}
	for _, h := range plan.Chain.Hops.V {
		if !inScope(c, h.NodeID) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
	}
	s.commitInboundRelayPlans(c, []relaySetupPlan{plan}, "")
}

// Both APIs reserve one canonical command before reading allocation state.
// A batch commits all chains, endpoints, audits and replay body together.
func (s *Server) commitInboundRelayPlans(c *gin.Context, plans []relaySetupPlan, batchID string) {
	key := "relay-plan:" + plans[0].Owner + ":" + plans[0].Chain.ID
	var hash any = plans[0]
	if batchID != "" {
		key = "relay-batch:" + plans[0].Owner + ":" + batchID
		hash = plans
	}
	canonical := &idempotencyRequest{Key: idemDigest(key), RequestHash: templateHash(hash)}
	infos := []*idempotencyRequest{canonical}
	if v, ok := c.Get(idemContextKey); ok {
		infos = append([]*idempotencyRequest{v.(*idempotencyRequest)}, infos...)
	}
	var cached *store.IdempotencyKey
	var body []byte
	s.relayMu.Lock()
	err := s.st.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		for _, info := range infos {
			prev, e := reserveUserCommand(tx, info)
			if e != nil {
				return e
			}
			if prev != nil {
				cached = prev
				break
			}
		}
		if cached != nil {
			for _, info := range infos {
				if e := tx.Model(&store.IdempotencyKey{}).Where("key = ? AND status = 0", info.Key).Updates(map[string]any{"status": cached.Status, "body": cached.Body}).Error; e != nil {
					return e
				}
			}
			return nil
		}
		if tx.Dialector.Name() == "postgres" {
			if e := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "polaris-relay-config").Error; e != nil {
				return e
			}
		}
		for i := range plans {
			if e := s.persistRelayPlan(tx, c, &plans[i]); e != nil {
				return e
			}
		}
		var e error
		if batchID == "" {
			body, e = json.Marshal(plans[0].Chain)
		} else {
			chains := make([]store.Chain, 0, len(plans))
			for _, p := range plans {
				chains = append(chains, p.Chain)
			}
			body, e = json.Marshal(gin.H{"batchId": batchID, "chains": chains})
		}
		if e != nil {
			return e
		}
		for _, info := range infos {
			if e := tx.Model(&store.IdempotencyKey{}).Where("key = ?", info.Key).Updates(map[string]any{"status": http.StatusCreated, "body": body}).Error; e != nil {
				return e
			}
		}
		return nil
	})
	s.relayMu.Unlock()
	if err != nil {
		relaySetupError(c, err)
		return
	}
	if cached != nil {
		for _, plan := range plans {
			var existing store.Chain
			if s.st.DB.First(&existing, "id = ?", plan.Chain.ID).Error == nil {
				s.MarkDirty("relay replay reconciliation", chainNodes(&existing)...)
			}
		}
		replayIdempotency(c, *cached)
		return
	}
	var nodes []string
	for _, p := range plans {
		nodes = append(nodes, chainNodes(&p.Chain)...)
	}
	s.MarkDirty("one-click relay", nodes...)
	c.Data(http.StatusCreated, "application/json; charset=utf-8", body)
}

func (s *Server) persistRelayPlan(tx *gorm.DB, c *gin.Context, plan *relaySetupPlan) error {
	if time.Now().After(plan.Expires) {
		return badInput{fmt.Errorf("预览已过期，请重新预览")}
	}
	in, e := relayLanding(tx, plan.InboundID)
	if e != nil {
		return badInput{e}
	}
	nodes, e := relayNodes(tx, &plan.Chain)
	if e != nil {
		return badInput{e}
	}
	if relayInputHash(in, nodes, &plan.Chain) != plan.Inputs {
		return badInput{fmt.Errorf("入站或节点地址已变化，请重新预览")}
	}
	if e = validateRelayAccessInbound(tx, in); e != nil {
		return badInput{e}
	}
	if _, e = checkRelayAccessChain(tx, in, &plan.Chain); e != nil {
		return badInput{e}
	}
	for _, h := range plan.Chain.Hops.V {
		used, e := relayUsedPorts(tx, h.NodeID)
		if e != nil {
			return e
		}
		for _, p := range []int{h.Port, h.TunnelPort} {
			if p > 0 {
				if _, e = relayPort(used, p); e != nil {
					return badInput{e}
				}
			}
		}
	}
	plan.Chain.TunnelAuth = plan.Auth
	plan.Chain.TargetRevs = store.NewJSON(relayTargetRevs(&plan.Chain, nodes))
	if e = tx.Create(&plan.Chain).Error; e != nil {
		return e
	}
	// Preserve the implicit direct entry instead of silently replacing it.
	var count int64
	if e = tx.Model(&store.Endpoint{}).Where("inbound_id = ?", in.ID).Count(&count).Error; e != nil {
		return e
	}
	if count == 0 && !in.RelayOnly && !isLoopbackHost(in.Listen) {
		if e = tx.Create(&store.Endpoint{ID: store.NewID(), InboundID: in.ID, Kind: "direct"}).Error; e != nil {
			return e
		}
	}
	if e = s.materializeChain(tx, &plan.Chain); e != nil {
		return e
	}
	audit := store.AuditLog{Admin: c.GetString("admin"), Action: "relay.create", Target: plan.Chain.Name, Detail: "one-click " + plan.Chain.TunnelType, IP: clientIP(c.Request)}
	if e = tx.Create(&audit).Error; e != nil {
		return e
	}
	return nil
}

func relayTargetRevs(ch *store.Chain, nodes map[string]store.Node) map[string]int64 {
	out := map[string]int64{}
	for _, h := range ch.Hops.V {
		if n, ok := nodes[h.NodeID]; ok {
			// Raw landing ports can change their source guard without changing
			// the core configuration. relayView handles truly unchanged specs.
			out[n.ID] = n.DesiredRev + 1
		}
	}
	return out
}

func relaySetupError(c *gin.Context, err error) {
	var bad badInput
	if errors.As(err, &bad) {
		badRequest(c, bad.error)
	} else if errors.Is(err, gorm.ErrRecordNotFound) {
		notFound(c)
	} else {
		idempotencyError(c, err)
	}
}

func (s *Server) listInboundRelays(c *gin.Context) {
	var in store.Inbound
	if err := s.st.DB.First(&in, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var rows []store.Chain
	if err := s.st.DB.Where("managed = ?", true).Order("created_at").Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	out := []inboundRelayView{}
	for _, ch := range rows {
		hops := ch.Hops.V
		if len(hops) < 2 || hops[len(hops)-1].InboundID != c.Param("id") {
			continue
		}
		out = append(out, s.relayView(ch, in))
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) relayView(ch store.Chain, in store.Inbound) inboundRelayView {
	hops := ch.Hops.V
	view := inboundRelayView{Chain: ch, Port: hops[0].Port, State: "applied", Nodes: []relaySetupNode{}, BusinessDiagnostic: relayBusinessAvailability(in, store.Node{})}
	var endpoint store.Endpoint
	if s.st.DB.Where("relay_chain_id = ? AND inbound_id = ? AND kind = ?", ch.ID, in.ID, "relay").Order("created_at, id").First(&endpoint).Error == nil {
		view.ShareAddress, view.SharePort, view.ShareName = endpoint.Address, endpoint.Port, endpoint.ShareName
	}
	for _, h := range hops {
		var n store.Node
		if err := s.st.DB.First(&n, "id = ?", h.NodeID).Error; err != nil {
			view.State = "failed"
			continue
		}
		if h.NodeID == in.NodeID {
			view.BusinessDiagnostic = s.relayBusinessSupport(in, n)
		}
		if h.NodeID == hops[0].NodeID {
			view.Address = nodeHost(&n)
		}
		status := "applied"
		required := ch.TargetRevs.V[n.ID]
		// The reconciler may coalesce disable/enable into no change. In
		// that case there will be no extra revision to reach the estimate.
		if n.DesiredRev < required && n.DesiredRev > 0 {
			if published, err := s.loadRevision(n.ID, n.DesiredRev); err == nil {
				if desired, err := s.BuildSpec(n.ID); err == nil && sameSpec(published, desired) {
					required = n.DesiredRev
				}
			}
		}
		switch {
		case n.Status == "offline" || n.Status == "pending":
			status = "offline"
		case n.DesiredRev < required:
			status = "pending"
		case n.LastError != "":
			status = "failed"
		case n.AppliedRev < n.DesiredRev:
			status = "syncing"
		}
		rank := map[string]int{"applied": 0, "pending": 1, "syncing": 2, "offline": 3, "failed": 4}
		if rank[status] > rank[view.State] {
			view.State = status
		}
		view.Nodes = append(view.Nodes, relaySetupNode{NodeID: n.ID, Name: n.Name, DesiredRev: n.DesiredRev, AppliedRev: n.AppliedRev, RequiredRev: required, State: status, Error: n.LastError})
	}
	if !in.Enabled {
		view.State = "blocked"
	}
	if ch.Disabled {
		view.State = "disabled"
	}
	return view
}

func loadInboundRelay(db *gorm.DB, id, inboundID string) (store.Chain, error) {
	var ch store.Chain
	if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ch, "id = ? AND managed = ?", id, true).Error; err != nil {
		return ch, err
	}
	hops := ch.Hops.V
	if len(hops) < 2 || hops[len(hops)-1].InboundID != inboundID {
		return ch, gorm.ErrRecordNotFound
	}
	return ch, nil
}

func (s *Server) setInboundRelayState(c *gin.Context) {
	var req struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var ch store.Chain
	changed := false
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var e error
		ch, e = loadInboundRelay(tx, c.Param("rid"), c.Param("id"))
		if e != nil {
			return e
		}
		action := "disable"
		if *req.Enabled {
			action = "enable"
		}
		if e = checkRelayChange(tx, c, "relay", action, []store.Chain{ch}); e != nil {
			return e
		}
		if *req.Enabled {
			if _, err := relayLanding(tx, c.Param("id")); err != nil {
				return badInput{err}
			}
		}
		if ch.Disabled == !*req.Enabled {
			return nil
		}
		nodes, e := relayNodes(tx, &ch)
		if e != nil {
			return e
		}
		ch.Disabled = !*req.Enabled
		if *req.Enabled {
			var in store.Inbound
			if e := tx.First(&in, "id = ?", c.Param("id")).Error; e != nil {
				return e
			}
			if e := validateRelayAccessInbound(tx, in); e != nil {
				return badInput{e}
			}
			if _, e := checkRelayAccessChain(tx, in, &ch); e != nil {
				return badInput{e}
			}
		}
		ch.TargetRevs = store.NewJSON(relayTargetRevs(&ch, nodes))
		if e := tx.Save(&ch).Error; e != nil {
			return e
		}
		changed = true
		return nil
	})
	if err != nil {
		relayChangeError(c, err)
		return
	}
	if changed {
		s.MarkDirty("relay state changed", chainNodes(&ch)...)
		s.auditCtx(c, "relay.state", ch.Name, strconv.FormatBool(*req.Enabled))
	}
	c.JSON(http.StatusOK, ch)
}

func (s *Server) deleteInboundRelay(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var ch store.Chain
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var e error
		ch, e = loadInboundRelay(tx, c.Param("rid"), c.Param("id"))
		if e != nil {
			return e
		}
		if e = checkRelayChange(tx, c, "relay", "delete", []store.Chain{ch}); e != nil {
			return e
		}
		if e = s.dematerializeChain(tx, &ch); e != nil {
			return e
		}
		return tx.Delete(&ch).Error
	})
	if err != nil {
		relayChangeError(c, err)
		return
	}
	s.MarkDirty("relay deleted", chainNodes(&ch)...)
	s.auditCtx(c, "relay.delete", ch.Name, "")
	c.Status(http.StatusNoContent)
}

// Raw TCP/UDP is preserved end-to-end. A tunnel has a pinned receiver target,
// never an arbitrary-destination public relay. Intermediate local forwarders
// listen only on loopback; only the entry and authenticated receivers are public.
func managedRelayForwards(ch *store.Chain, nodes map[string]store.Node, in store.Inbound, auth string) map[string][]model.ForwardRule {
	out := map[string][]model.ForwardRule{}
	m := model.Inbound{Protocol: in.Protocol.V, Transport: in.Transport.V}
	m.Normalize()
	hops := ch.Hops.V
	for i := 0; i < len(hops)-1; i++ {
		h, next := hops[i], hops[i+1]
		n := nodes[next.NodeID]
		client := model.ForwardRule{Tag: fmt.Sprintf("chain-%s-%d", ch.ID, i), Listen: h.Port, Network: m.Network()}
		if ch.TunnelType == "" {
			p := next.Port
			if i == len(hops)-2 {
				p = in.Port
			}
			client.Target = net.JoinHostPort(nodeHost(&n), strconv.Itoa(p))
		} else {
			client.Engine = model.EngineGost
			client.TunnelType = ch.TunnelType
			client.TunnelRole = "client"
			client.TunnelAuth = auth
			client.Target = net.JoinHostPort(nodeHost(&n), strconv.Itoa(next.TunnelPort))
			client.TunnelPath = "/polaris/" + ch.ID
			if i > 0 {
				client.ListenIP = "127.0.0.1"
			}
			target := net.JoinHostPort("127.0.0.1", strconv.Itoa(next.Port))
			if i == len(hops)-2 {
				host := "127.0.0.1"
				if in.Listen != "" && in.Listen != "::" && in.Listen != "0.0.0.0" && in.Listen != "localhost" {
					host = in.Listen
				}
				target = net.JoinHostPort(host, strconv.Itoa(in.Port))
			}
			server := model.ForwardRule{Tag: fmt.Sprintf("chain-%s-%d-recv", ch.ID, i), Engine: model.EngineGost, Listen: next.TunnelPort, Network: m.Network(), Target: target, TunnelType: ch.TunnelType, TunnelRole: "server", TunnelAuth: auth, TunnelPath: client.TunnelPath}
			out[next.NodeID] = append(out[next.NodeID], server)
		}
		out[h.NodeID] = append(out[h.NodeID], client)
	}
	return out
}

// Cleanup owns only managed chains and their relay endpoints, leaving direct,
// CDN and manually-created forwarding resources untouched.
func (s *Server) cleanupManagedRelays(tx *gorm.DB, inboundID, nodeID string) ([]string, error) {
	if tx.Dialector.Name() == "postgres" {
		if e := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "polaris-relay-config").Error; e != nil {
			return nil, e
		}
	}
	var rows []store.Chain
	if err := tx.Where("managed = ?", true).Find(&rows).Error; err != nil {
		return nil, err
	}
	nodes := []string{}
	for _, ch := range rows {
		matches := false
		for _, h := range ch.Hops.V {
			matches = matches || (nodeID != "" && h.NodeID == nodeID) || (inboundID != "" && h.InboundID == inboundID)
		}
		if !matches {
			continue
		}
		if err := s.dematerializeChain(tx, &ch); err != nil {
			return nil, err
		}
		if err := tx.Delete(&ch).Error; err != nil {
			return nil, err
		}
		nodes = append(nodes, chainNodes(&ch)...)
	}
	return nodes, nil
}
