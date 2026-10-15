package panel

import (
	"encoding/json"
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
	NodeID    string              `json:"nodeId" binding:"required"`
	Core      model.CoreType      `json:"core"`
	Tag       string              `json:"tag" binding:"required"`
	Listen    string              `json:"listen"`
	Port      int                 `json:"port" binding:"required"`
	Protocol  model.ProtocolSpec  `json:"protocol"`
	Transport model.TransportSpec `json:"transport"`
	Security  model.SecuritySpec  `json:"security"`
	Sniffing  model.SniffSpec     `json:"sniffing"`
	Extra     json.RawMessage     `json:"extra"`
	Enabled   *bool               `json:"enabled"`
	Remark    string              `json:"remark"`
	GroupIDs  []string            `json:"groupIds"`
	// OutboundTag sends all traffic of the inbound to this egress ("" = routing rules decide).
	OutboundTag string `json:"outboundTag"`
}

type inboundView struct {
	store.Inbound
	NodeName  string   `json:"nodeName"`
	UserCount int      `json:"userCount"`
	GroupIDs  []string `json:"groupIds"`
	Endpoints int64    `json:"endpoints"`
	Network   string   `json:"network"`
	CDN       bool     `json:"cdnCapable"`
}

func (s *Server) inboundView(in store.Inbound, nodes map[string]string, users map[string][]store.User) inboundView {
	m := s.toModelInbound(&in)
	v := inboundView{Inbound: in, NodeName: nodes[in.NodeID], UserCount: len(users[in.ID]), Network: m.Network(), CDN: m.CDNCapable()}
	v.Security.V.RealityPrivateKey = ""
	v.Protocol.V.ServerKey = ""
	s.st.DB.Model(&store.GroupInbound{}).Where("inbound_id = ?", in.ID).Pluck("group_id", &v.GroupIDs)
	if v.GroupIDs == nil {
		v.GroupIDs = []string{}
	}
	s.st.DB.Model(&store.Endpoint{}).Where("inbound_id = ?", in.ID).Count(&v.Endpoints)
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
		Transport: r.Transport, Security: r.Security, Sniffing: r.Sniffing, Extra: r.Extra, Enabled: en}
}

func fromModel(m *model.Inbound, row *store.Inbound) {
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
	if err := s.validateInbound(&node, &m, ""); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.checkOutboundTag(node.ID, req.OutboundTag); err != nil {
		badRequest(c, err)
		return
	}
	row := store.Inbound{ID: store.NewID(), Remark: req.Remark, OutboundTag: req.OutboundTag, OverriddenFields: store.NewJSON([]string{})}
	fromModel(&m, &row)
	s.sealInbound(&row)
	if req.GroupIDs == nil {
		req.GroupIDs = s.defaultGroupIDs() // looked up outside the transaction (single SQLite connection)
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		if err := tx.Create(&store.Endpoint{ID: store.NewID(), InboundID: row.ID, Kind: "direct"}).Error; err != nil {
			return err
		}
		for _, g := range req.GroupIDs {
			if err := tx.Create(&store.GroupInbound{GroupID: g, InboundID: row.ID}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("inbound "+row.Tag+" created", node.ID)
	s.auditCtx(c, "inbound.create", node.Name+"/"+row.Tag, fmt.Sprintf("%s %s+%s+%s :%d", row.Core, m.Protocol.Type, m.Transport.Type, m.Security.Type, m.Port))
	c.JSON(http.StatusCreated, gin.H{"inbound": row, "core": m.Core})
}

func (s *Server) updateInbound(c *gin.Context) {
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
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", row.NodeID).Error; err != nil {
		dbError(c, err)
		return
	}
	old := s.toModelInbound(&row)
	m := req.toModel()
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
	fromModel(&m, &row)
	row.Remark, row.OutboundTag = req.Remark, req.OutboundTag
	s.sealInbound(&row)
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&row).Error; err != nil {
			return err
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
	c.JSON(http.StatusOK, gin.H{"inbound": row, "core": m.Core})
}

func (s *Server) deleteInbound(c *gin.Context) {
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
	ad, err := core.New(row.Core, core.Options{Version: version})
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
	ad, err := core.New(m.Core, core.Options{Version: version})
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
		if !m.CDNCapable() {
			badRequest(c, fmt.Errorf("%s+%s 不能走 CDN（仅 WS / gRPC / HTTPUpgrade / XHTTP 且非 REALITY）", m.Transport.Type, m.Security.Type))
			return
		}
		if e.CDNProfileID == "" {
			badRequest(c, fmt.Errorf("cdnProfileId is required"))
			return
		}
		var p store.CDNProfile
		if err := s.st.DB.First(&p, "id = ?", e.CDNProfileID).Error; err != nil {
			dbError(c, err)
			return
		}
	case "relay":
		var ch store.Chain
		if err := s.st.DB.First(&ch, "id = ?", e.RelayChainID).Error; err != nil {
			badRequest(c, fmt.Errorf("relay chain: %w", err))
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
	if a.Port != b.Port {
		out = append(out, "port")
	}
	if a.Tag != b.Tag {
		out = append(out, "tag")
	}
	if !eq(a.Protocol, b.Protocol) {
		out = append(out, "protocol")
	}
	if !eq(a.Transport, b.Transport) {
		out = append(out, "transport")
	}
	sa, sb := a.Security, b.Security
	sa.CertPEM, sa.KeyPEM, sb.CertPEM, sb.KeyPEM = "", "", "", ""
	if !eq(sa, sb) {
		out = append(out, "security")
	}
	if !eq(a.Sniffing, b.Sniffing) {
		out = append(out, "sniffing")
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
