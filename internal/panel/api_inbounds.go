package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

type inboundReq struct {
	OutboundIPStrategy *model.OutboundIPStrategy `json:"outboundIPStrategy,omitempty"`
	NodeID             string                    `json:"nodeId" binding:"required"`
	Core               model.CoreType            `json:"core"`
	Tag                string                    `json:"tag" binding:"required"`
	Listen             string                    `json:"listen"`
	Port               int                       `json:"port" binding:"required"`
	Protocol           model.ProtocolSpec        `json:"protocol"`
	Transport          model.TransportSpec       `json:"transport"`
	Security           model.SecuritySpec        `json:"security"`
	Sniffing           model.SniffSpec           `json:"sniffing"`
	Extra              json.RawMessage           `json:"extra"`
	Enabled            *bool                     `json:"enabled"`
	RelayOnly          *bool                     `json:"relayOnly,omitempty"`
	RelayAccess        *store.RelayAccessPolicy  `json:"relayAccess,omitempty"`
	Remark             string                    `json:"remark"`
	GroupIDs           []string                  `json:"groupIds"`
	// OutboundTag sends all traffic of the inbound to this egress ("" = routing rules decide).
	OutboundTag string `json:"outboundTag"`
	// SpeedLimitMbps caps the port's bandwidth on the node (0 = none).
	SpeedLimitMbps int `json:"speedLimitMbps"`
	// Create only: also publish through this CDN profile; CDNOnly drops
	// the direct entry so the node's address stays out of subscriptions.
	CDNProfileID string `json:"cdnProfileId,omitempty"`
	CDNOnly      bool   `json:"cdnOnly,omitempty"`
}

// inboundMeta is what an inbound row carries besides the model.
type inboundMeta struct {
	Remark         string
	OutboundTag    string
	SpeedLimitMbps int
	RelayOnly      bool
	RelayAccess    store.RelayAccessPolicy
	CDNProfileID   string
	CDNOnly        bool
}

func (r *inboundReq) meta() inboundMeta {
	m := inboundMeta{Remark: r.Remark, OutboundTag: r.OutboundTag, SpeedLimitMbps: r.SpeedLimitMbps, RelayOnly: r.RelayOnly != nil && *r.RelayOnly,
		CDNProfileID: r.CDNProfileID, CDNOnly: r.CDNOnly && r.CDNProfileID != ""}
	if r.RelayAccess != nil {
		m.RelayAccess = *r.RelayAccess
	}
	return m
}

func checkSpeedLimit(mbps int) error {
	if mbps < 0 || mbps > 100000 {
		return fmt.Errorf("限速应在 0–100000 Mbps 之间（0 为不限速）")
	}
	return nil
}

type inboundView struct {
	store.Inbound
	NodeName  string               `json:"nodeName"`
	UserCount int                  `json:"userCount"`
	GroupIDs  []string             `json:"groupIds"`
	Endpoints int64                `json:"endpoints"`
	Network   string               `json:"network"`
	CDN       bool                 `json:"cdnCapable"`
	Health    *store.InboundHealth `json:"health,omitempty"`
}

func (s *Server) inboundView(in store.Inbound, nodes map[string]string, users map[string][]store.User) inboundView {
	m := s.toModelInbound(&in)
	v := inboundView{Inbound: in, NodeName: nodes[in.NodeID], UserCount: len(users[in.ID]), Network: m.Network(), CDN: m.CDNCapable()}
	v.Security.V.RealityPrivateKey = ""
	v.Protocol.V.ServerKey, v.Protocol.V.Decryption = "", ""
	v.Security.V.RealityMldsa65Seed = ""
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", in.ID).Pluck("group_id", &v.GroupIDs)
	if v.GroupIDs == nil {
		v.GroupIDs = []string{}
	}
	s.st.DB.Model(&store.Endpoint{}).Where("inbound_id = ?", in.ID).Count(&v.Endpoints)
	var h store.InboundHealth
	if !in.RelayOnly && s.st.DB.First(&h, "inbound_id = ?", in.ID).Error == nil {
		v.Health = &h
	}
	return v
}

func (s *Server) nodeNames() map[string]string {
	var ns []store.Node
	s.st.DB.Select("id", "name").Find(&ns)
	m := map[string]string{}
	for _, n := range ns {
		m[n.ID] = n.Name
	}
	return m
}

func (s *Server) listInbounds(c *gin.Context) {
	q := s.st.DB.Order("node_id, port")
	if v := c.Query("node"); v != "" {
		q = q.Where("node_id = ?", v)
	}
	if v := c.Query("core"); v != "" {
		q = q.Where("core = ?", v)
	}
	if v := c.Query("template"); v != "" {
		q = q.Where("template_id = ?", v)
	}
	var ins []store.Inbound
	if err := q.Find(&ins).Error; err != nil {
		serverError(c, err)
		return
	}
	ids := make([]string, len(ins))
	for i, in := range ins {
		ids[i] = in.ID
	}
	users, _ := s.inboundUsers(ids)
	names := s.nodeNames()
	out := make([]inboundView, 0, len(ins))
	for _, in := range ins {
		out = append(out, s.inboundView(in, names, users))
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) getInbound(c *gin.Context) {
	var in store.Inbound
	if err := s.st.DB.First(&in, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	users, _ := s.inboundUsers([]string{in.ID})
	c.JSON(http.StatusOK, s.inboundView(in, s.nodeNames(), users))
}

// validateInbound runs preparation, certificate selection, core assignment and
// conflict checks; it is shared by the API and template engine.
func (s *Server) validateInbound(node *store.Node, m *model.Inbound, excludeID string) error {
	if err := s.prepareInbound(m); err != nil {
		return err
	}
	if err := s.ensureCert(node, m); err != nil {
		return err
	}
	if err := s.assignCore(node, m); err != nil {
		return err
	}
	if err := s.checkInboundOutboundIP(node, m); err != nil {
		return err
	}
	if err := s.validateFallbacks(m, excludeID); err != nil {
		return err
	}
	return s.portConflict(node.ID, m, excludeID)
}

func (r *inboundReq) toModel() model.Inbound {
	en := true
	if r.Enabled != nil {
		en = *r.Enabled
	}
	return model.Inbound{NodeID: r.NodeID, Core: r.Core, Tag: r.Tag, Listen: r.Listen, Port: r.Port, Protocol: r.Protocol,
		Transport: r.Transport, Security: r.Security, Sniffing: r.Sniffing, Extra: r.Extra, Enabled: en, OutboundIPStrategy: r.ipStrategy()}
}

func (r *inboundReq) ipStrategy() model.OutboundIPStrategy {
	if r.OutboundIPStrategy != nil {
		return *r.OutboundIPStrategy
	}
	return model.OutboundIPAuto
}

func fromModel(m *model.Inbound, row *store.Inbound) {
	row.OutboundIPStrategy = m.OutboundIPStrategy
	row.NodeID, row.Core, row.Tag, row.Listen, row.Port = m.NodeID, m.Core, m.Tag, m.Listen, m.Port
	row.Protocol, row.Transport = store.NewJSON(m.Protocol), store.NewJSON(m.Transport)
	row.Security, row.Sniffing = store.NewJSON(m.Security), store.NewJSON(m.Sniffing)
	row.Extra, row.Enabled = m.Extra, m.Enabled
}

func (s *Server) createInbound(c *gin.Context) {
	var req inboundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", req.NodeID).Error; err != nil {
		dbError(c, err)
		return
	}
	m := req.toModel()
	row, err := s.insertInbound(&node, &m, req.meta(), req.GroupIDs)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.MarkDirty("inbound "+row.Tag+" created", node.ID)
	s.auditCtx(c, "inbound.create", node.Name+"/"+row.Tag, fmt.Sprintf("%s %s+%s+%s :%d", row.Core, m.Protocol.Type, m.Transport.Type, m.Security.Type, m.Port))
	c.JSON(http.StatusCreated, gin.H{"inbound": row, "core": m.Core})
}

// insertInbound validates and stores a new inbound with its direct endpoint
// and group bindings (nil groups = the default groups).
func (s *Server) insertInbound(node *store.Node, m *model.Inbound, meta inboundMeta, groupIDs []string) (store.Inbound, error) {
	if err := s.validateInbound(node, m, ""); err != nil {
		return store.Inbound{}, err
	}
	if err := s.checkOutboundTag(node.ID, meta.OutboundTag); err != nil {
		return store.Inbound{}, err
	}
	if err := checkSpeedLimit(meta.SpeedLimitMbps); err != nil {
		return store.Inbound{}, err
	}
	row := store.Inbound{ID: store.NewID(), Remark: meta.Remark, OutboundTag: meta.OutboundTag, SpeedLimitMbps: meta.SpeedLimitMbps, RelayOnly: meta.RelayOnly,
		OverriddenFields: store.NewJSON([]string{})}
	fromModel(m, &row)
	row.RelayAccess = store.NewJSON(meta.RelayAccess)
	if row.RelayAccess.V.Enabled {
		row.RelayOnly = true
	}
	if err := validateRelayAccessInbound(s.st.DB, row); err != nil {
		return store.Inbound{}, err
	}
	endpoints := []store.Endpoint{}
	if !meta.CDNOnly {
		endpoints = append(endpoints, store.Endpoint{ID: store.NewID(), InboundID: row.ID, Kind: "direct"})
	}
	if meta.CDNProfileID != "" {
		if err := s.checkCDNEndpoint(m, row.RelayAccess.V.Enabled, meta.CDNProfileID, 0); err != nil {
			return store.Inbound{}, err
		}
		endpoints = append(endpoints, store.Endpoint{ID: store.NewID(), InboundID: row.ID, Kind: "cdn", CDNProfileID: meta.CDNProfileID})
	}
	s.sealInbound(&row)
	if groupIDs == nil {
		groupIDs = s.defaultGroupIDs() // looked up outside the transaction (single SQLite connection)
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		for i := range endpoints {
			if err := tx.Create(&endpoints[i]).Error; err != nil {
				return err
			}
		}
		for _, g := range groupIDs {
			if err := tx.Create(&store.GroupInbound{GroupID: g, InboundID: row.ID}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return row, err
}

func (s *Server) updateInbound(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req inboundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	req.NodeID = row.NodeID // moving inbounds between nodes is not supported
	if req.RelayAccess != nil && templateHash(*req.RelayAccess) != templateHash(row.RelayAccess.V) {
		badRequest(c, fmt.Errorf("请使用 relay-access 接口修改访问保护"))
		return
	}
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", row.NodeID).Error; err != nil {
		dbError(c, err)
		return
	}
	old := s.toModelInbound(&row)
	m := req.toModel()
	if req.OutboundIPStrategy == nil {
		m.OutboundIPStrategy = row.OutboundIPStrategy
	}
	if req.Enabled == nil {
		m.Enabled = row.Enabled
	}
	// Keep generated secrets unless explicitly replaced.
	if m.Security.Type == model.SecReality && m.Security.RealityPrivateKey == "" && old.Security.Type == model.SecReality {
		m.Security.RealityPrivateKey, m.Security.RealityPublicKey = old.Security.RealityPrivateKey, old.Security.RealityPublicKey
		if len(m.Security.RealityShortIDs) == 0 {
			m.Security.RealityShortIDs = old.Security.RealityShortIDs
		}
	}
	if m.Protocol.Type == model.ProtoShadowsocks && m.Protocol.ServerKey == "" && m.Protocol.Method == old.Protocol.Method {
		m.Protocol.ServerKey = old.Protocol.ServerKey
	}
	keepSecrets(&m, &old)
	if err := s.validateInbound(&node, &m, row.ID); err != nil {
		badRequest(c, err)
		return
	}
	if row.TemplateID != "" {
		row.OverriddenFields = store.NewJSON(mergeFields(row.OverriddenFields.V, diffFields(&old, &m)))
	}
	if err := s.checkOutboundTag(node.ID, req.OutboundTag); err != nil {
		badRequest(c, err)
		return
	}
	if err := checkSpeedLimit(req.SpeedLimitMbps); err != nil {
		badRequest(c, err)
		return
	}
	fromModel(&m, &row)
	if row.RelayAccess.V.Enabled && req.RelayOnly != nil && !*req.RelayOnly {
		badRequest(c, fmt.Errorf("来源限制开启时必须仅发布中转"))
		return
	}
	if err := validateRelayAccessInbound(s.st.DB, row); err != nil {
		badRequest(c, err)
		return
	}
	row.Remark, row.OutboundTag, row.SpeedLimitMbps = req.Remark, req.OutboundTag, req.SpeedLimitMbps
	s.sealInbound(&row)
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		// Publication is edited independently; an older editor must not
		// overwrite a relay-only policy selected after it loaded this row.
		write := tx.Omit("RelayOnly", "RelayAccess", "OutboundIPStrategy")
		if req.RelayOnly != nil {
			row.RelayOnly = *req.RelayOnly
			write = tx.Omit("RelayAccess", "OutboundIPStrategy")
		}
		if err := write.Save(&row).Error; err != nil {
			return err
		}
		if req.OutboundIPStrategy != nil {
			if err := tx.Model(&store.Inbound{}).Where("id = ?", row.ID).Update("outbound_ip_strategy", row.OutboundIPStrategy).Error; err != nil {
				return err
			}
		}
		if req.GroupIDs != nil {
			if err := tx.Where("inbound_id = ?", row.ID).Delete(&store.GroupInbound{}).Error; err != nil {
				return err
			}
			for _, g := range req.GroupIDs {
				if err := tx.Create(&store.GroupInbound{GroupID: g, InboundID: row.ID}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("inbound "+row.Tag+" updated", s.nodesForInbounds(row.ID)...)
	s.auditCtx(c, "inbound.update", node.Name+"/"+row.Tag, "")
	var current store.Inbound
	if s.st.DB.Select("relay_only").First(&current, "id = ?", row.ID).Error == nil {
		row.RelayOnly = current.RelayOnly
	}
	c.JSON(http.StatusOK, gin.H{"inbound": row, "core": m.Core})
}

func (s *Server) deleteInbound(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var refs int64
	s.st.DB.Model(&store.Outbound{}).Where("node_ref_inbound_id = ?", row.ID).Count(&refs)
	if refs > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("有 %d 个出站 / 链路引用该入站，请先删除", refs)})
		return
	}
	if m := s.referencingMaster(row.ID); m != "" {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("入站「%s」把它作为回落目标，请先在该入站中移除", m)})
		return
	}
	nodes := s.nodesForInbounds(row.ID)
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		extra, e := s.cleanupManagedRelays(tx, row.ID, "")
		if e != nil {
			return e
		}
		nodes = append(nodes, extra...)

		tx.Where("inbound_id = ?", row.ID).Delete(&store.GroupInbound{})
		tx.Where("kind = ? AND target_id = ?", "inbound", row.ID).Delete(&store.UserAccess{})
		tx.Where("inbound_id = ?", row.ID).Delete(&store.Endpoint{})
		return tx.Delete(&row).Error
	})
	if err != nil {
		serverError(c, err)
		return
	}
	s.MarkDirty("inbound "+row.Tag+" deleted", nodes...)
	s.auditCtx(c, "inbound.delete", row.Tag, "")
	c.Status(http.StatusNoContent)
}

// previewInbound renders the native config of the inbound's core.
func (s *Server) previewInbound(c *gin.Context) {
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	spec, err := s.BuildSpec(row.NodeID)
	if err != nil {
		serverError(c, err)
		return
	}
	version := ""
	for _, cs := range spec.Cores {
		if cs.Type == row.Core {
			version = cs.Version
		}
	}
	ad, err := core.New(row.Core, core.Options{Version: version, Limit: outboundIPPreviewWire()})
	if err != nil {
		badRequest(c, err)
		return
	}
	cfg, err := ad.Render(spec)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"core": row.Core, "version": version, "config": string(cfg)})
}

// previewDraftInbound renders the native config as it would look with an
// unsaved inbound added (or replacing ?id=).
func (s *Server) previewDraftInbound(c *gin.Context) {
	var req inboundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", req.NodeID).Error; err != nil {
		dbError(c, err)
		return
	}
	m := req.toModel()
	if m.Port == 0 {
		m.Port = 1
	}
	exclude := c.Query("id")
	if exclude != "" {
		var old store.Inbound
		if err := s.st.DB.First(&old, "id = ?", exclude).Error; err == nil {
			om := s.toModelInbound(&old)
			if m.Security.Type == model.SecReality && m.Security.RealityPrivateKey == "" {
				m.Security.RealityPrivateKey, m.Security.RealityPublicKey, m.Security.RealityShortIDs =
					om.Security.RealityPrivateKey, om.Security.RealityPublicKey, om.Security.RealityShortIDs
			}
			if m.Protocol.ServerKey == "" {
				m.Protocol.ServerKey = om.Protocol.ServerKey
			}
			keepSecrets(&m, &om)
		}
	}
	if err := s.validateInbound(&node, &m, exclude); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	spec, err := s.BuildSpec(node.ID)
	if err != nil {
		serverError(c, err)
		return
	}
	if m.Security.CertificateID != "" {
		var cert store.Certificate
		if s.st.DB.First(&cert, "id = ?", m.Security.CertificateID).Error == nil {
			m.Security.CertPEM, m.Security.KeyPEM = cert.Cert, "<private key>"
		}
	}
	var ins []model.Inbound
	for _, x := range spec.Inbounds {
		if x.ID != exclude {
			ins = append(ins, x)
		}
	}
	spec.Inbounds = append(ins, m)
	version := ""
	for _, cs := range spec.Cores {
		if cs.Type == m.Core {
			version = cs.Version
		}
	}
	ad, err := core.New(m.Core, core.Options{Version: version, Limit: outboundIPPreviewWire()})
	if err != nil {
		badRequest(c, err)
		return
	}
	cfg, err := ad.Render(spec)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"core": m.Core, "version": version, "config": string(cfg)})
}

func (s *Server) listEndpoints(c *gin.Context) {
	var es []store.Endpoint
	s.st.DB.Where("inbound_id = ?", c.Param("id")).Order("sort_order, created_at").Find(&es)
	c.JSON(http.StatusOK, es)
}

func (s *Server) createEndpoint(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var e store.Endpoint
	if err := c.ShouldBindJSON(&e); err != nil {
		badRequest(c, err)
		return
	}
	m := s.toModelInbound(&row)
	switch e.Kind {
	case "direct":
	case "cdn":
		if err := s.checkCDNEndpoint(&m, row.RelayAccess.V.Enabled, e.CDNProfileID, e.Port); err != nil {
			badRequest(c, err)
			return
		}
	case "relay":
		var ch store.Chain
		if err := s.st.DB.First(&ch, "id = ?", e.RelayChainID).Error; err != nil {
			badRequest(c, fmt.Errorf("relay chain: %w", err))
			return
		}
		if ch.Mode != "l4" || len(ch.Hops.V) < 2 || ch.Hops.V[len(ch.Hops.V)-1].InboundID != row.ID {
			badRequest(c, fmt.Errorf("该中转线路的落地与当前入站不匹配"))
			return
		}
	default:
		badRequest(c, fmt.Errorf("kind must be direct, cdn or relay"))
		return
	}
	e.ID, e.InboundID = store.NewID(), row.ID
	if err := s.st.DB.Create(&e).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "endpoint.create", row.Tag, e.Kind)
	c.JSON(http.StatusCreated, e)
}

func (s *Server) deleteEndpoint(c *gin.Context) {
	var endpoint store.Endpoint
	if err := s.st.DB.First(&endpoint, "id = ? AND inbound_id = ?", c.Param("eid"), c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if endpoint.RelayChainID != "" {
		var chain store.Chain
		if s.st.DB.First(&chain, "id = ?", endpoint.RelayChainID).Error == nil && chain.Managed {
			c.JSON(http.StatusConflict, gin.H{"error": "该入口由一键中转管理，请停用或删除关联中转线路"})
			return
		}
	}

	res := s.st.DB.Where("id = ? AND inbound_id = ?", c.Param("eid"), c.Param("id")).Delete(&store.Endpoint{})
	if res.RowsAffected == 0 {
		notFound(c)
		return
	}
	s.auditCtx(c, "endpoint.delete", c.Param("id"), c.Param("eid"))
	c.Status(http.StatusNoContent)
}

// diffFields lists top-level inbound fields that differ (for "overridden" tracking).
func diffFields(a, b *model.Inbound) []string {
	var out []string
	eq := func(x, y any) bool {
		bx, _ := json.Marshal(x)
		by, _ := json.Marshal(y)
		return string(bx) == string(by)
	}
	if a.OutboundIPStrategy != b.OutboundIPStrategy {
		out = append(out, "outboundIPStrategy")
	}
	if a.Port != b.Port {
		out = append(out, "port")
	}
	if a.Tag != b.Tag {
		out = append(out, "tag")
	}
	if !eq(a.Protocol, b.Protocol) {
		for _, change := range configChanges("protocol", configObject(a.Protocol), configObject(b.Protocol)) {
			out = append(out, change.Field)
		}
	}
	if !eq(a.Transport, b.Transport) {
		for _, change := range configChanges("transport", configObject(a.Transport), configObject(b.Transport)) {
			out = append(out, change.Field)
		}
	}
	sa, sb := a.Security, b.Security
	sa.CertPEM, sa.KeyPEM, sb.CertPEM, sb.KeyPEM = "", "", "", ""
	if !eq(sa, sb) {
		for _, change := range configChanges("security", configObject(sa), configObject(sb)) {
			out = append(out, change.Field)
		}
	}
	if !eq(a.Sniffing, b.Sniffing) {
		for _, change := range configChanges("sniffing", configObject(a.Sniffing), configObject(b.Sniffing)) {
			out = append(out, change.Field)
		}
	}
	if string(a.Extra) != string(b.Extra) {
		out = append(out, "extra")
	}
	return out
}

func mergeFields(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range append(a, b...) {
		set[x] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkCDNEndpoint reports why the inbound cannot be published through the
// CDN profile (nil = it can).
func (s *Server) checkCDNEndpoint(m *model.Inbound, relayAccess bool, profileID string, port int) error {
	if !m.CDNCapable() {
		return fmt.Errorf("%s+%s 不能走 CDN（仅 WS / gRPC / HTTPUpgrade / XHTTP 且非 REALITY）", m.Transport.Type, m.Security.Type)
	}
	if profileID == "" {
		return fmt.Errorf("cdnProfileId is required")
	}
	var p store.CDNProfile
	if err := s.st.DB.First(&p, "id = ?", profileID).Error; err != nil {
		return fmt.Errorf("CDN 配置档不存在")
	}
	if relayAccess && p.CaddyFronting {
		return fmt.Errorf("来源限制不能与 Caddy 共用回源入口同时启用")
	}
	if problem := cdnPortProblem(m, &p, port); problem != "" {
		return errors.New(problem)
	}
	return nil
}
