package panel

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Portal inbound management for user-owned nodes (design user-nodes §7/§8,
// batch 3). Every handler takes its node through portalOwnedNode (another
// user's node is a 404) and additionally requires the node to be approved:
// a pending or rejected node serves nothing, so its inbounds are frozen
// too. What a user may create on their own node is deliberately narrower
// than the admin surface — see portalInboundReq.

// portalApprovedNode resolves the node for an inbound handler: owned by
// the signed-in user (else 404) and approved (else a 403 that says why).
func (s *Server) portalApprovedNode(c *gin.Context, u *store.User) *store.Node {
	n := s.portalOwnedNode(c, u)
	if n == nil {
		return nil
	}
	switch n.ReviewState {
	case "approved":
		return n
	case "pending":
		c.JSON(http.StatusForbidden, gin.H{"error": "节点待管理员审批，通过后才能管理入站"})
	default: // rejected (user nodes always carry a review state)
		c.JSON(http.StatusForbidden, gin.H{"error": "节点已被管理员拒绝，无法管理入站，请删除后重新添加"})
	}
	return nil
}

// portalInboundReq is the portal's inbound form. The dangerous knobs of
// the admin request are absent on purpose and therefore stripped by
// construction when a client sends them anyway: no nodeId (forced to the
// owned node), no groupIds (a portal inbound is never group-bound), no
// relayOnly / relayAccess, no template fields, no CDN profile, no
// outboundTag, no speed limit, no raw Extra escape hatch.
type portalInboundReq struct {
	Tag       string              `json:"tag" binding:"required"`
	Listen    string              `json:"listen"`
	Port      int                 `json:"port" binding:"required"`
	Protocol  model.ProtocolSpec  `json:"protocol"`
	Transport model.TransportSpec `json:"transport"`
	Security  model.SecuritySpec  `json:"security"`
	Sniffing  model.SniffSpec     `json:"sniffing"`
	Enabled   *bool               `json:"enabled"`
	Remark    string              `json:"remark"`
}

// sanitizePortalInbound strips the spec-level fields that would reach
// beyond the owner's own node even though the request struct never names
// them at the top level:
//   - CertificateID: naming an existing certificate would hand that
//     certificate's private key to the user's own agent at spec build
//     (ensureCert mints a self-signed one instead when the ID is empty).
//   - CertPEM/KeyPEM are build-time resolutions, never stored input.
//   - Protocol.Fallbacks reference other inbounds by ID (a relay-shaped
//     reference); SnellUserID dedicates the inbound to one chosen user.
//   - PROXY-protocol acceptance and forwarded-for trust belong to relay
//     landings, not to a user's own listener.
func sanitizePortalInbound(m *model.Inbound) {
	m.Security.CertificateID = ""
	m.Security.CertPEM, m.Security.KeyPEM = "", ""
	m.Protocol.Fallbacks = nil
	m.Protocol.SnellUserID = ""
	m.Transport.AcceptProxyProtocol = false
	m.Transport.TrustedForwardedFor = nil
}

func (r *portalInboundReq) toAdmin() inboundReq {
	return inboundReq{Tag: r.Tag, Listen: r.Listen, Port: r.Port, Protocol: r.Protocol,
		Transport: r.Transport, Security: r.Security, Sniffing: r.Sniffing, Enabled: r.Enabled, Remark: r.Remark}
}

// portalInboundView is the portal-safe shape of an inbound: everything the
// owner needs to manage their listener, none of the server secrets the
// store row carries (SS/Snell server keys, VLESS decryption, REALITY
// private key — the admin view blanks the same fields).
type portalInboundView struct {
	ID        string `json:"id"`
	Tag       string `json:"tag"`
	Core      string `json:"core"`
	Listen    string `json:"listen"`
	Port      int    `json:"port"`
	Enabled   bool   `json:"enabled"`
	Remark    string `json:"remark"`
	Protocol  any    `json:"protocol"`
	Transport any    `json:"transport"`
	Security  any    `json:"security"`
	Sniffing  any    `json:"sniffing"`
	CreatedAt string `json:"createdAt"`
}

func (s *Server) portalInboundViewOf(row *store.Inbound) portalInboundView {
	m := s.toModelInbound(row)
	return portalInboundView{
		ID: row.ID, Tag: row.Tag, Core: string(row.Core), Listen: row.Listen, Port: row.Port,
		Enabled: row.Enabled, Remark: row.Remark,
		Protocol: map[string]any{
			"type": string(m.Protocol.Type), "flow": m.Protocol.Flow, "method": m.Protocol.Method,
			"congestionControl": m.Protocol.CongestionControl, "upMbps": m.Protocol.UpMbps,
			"downMbps": m.Protocol.DownMbps, "obfs": m.Protocol.Obfs, "obfsPassword": m.Protocol.ObfsPassword,
			"snellVersion": m.Protocol.SnellVersion,
		},
		Transport: map[string]any{
			"type": string(m.Transport.Type), "path": m.Transport.Path, "host": m.Transport.Host,
			"serviceName": m.Transport.ServiceName, "mode": m.Transport.Mode,
		},
		Security: map[string]any{
			"type": string(m.Security.Type), "serverName": m.Security.ServerName,
			"alpn": m.Security.ALPN, "fingerprint": m.Security.Fingerprint,
			"realityDest": m.Security.RealityDest, "realityServerNames": m.Security.RealityServerNames,
			"realityPublicKey": m.Security.RealityPublicKey, "realityShortIds": m.Security.RealityShortIDs,
		},
		Sniffing: map[string]any{
			"enabled": m.Sniffing.Enabled, "destOverride": m.Sniffing.DestOverride,
			"routeOnly": m.Sniffing.RouteOnly, "metadataOnly": m.Sniffing.MetadataOnly,
		},
		CreatedAt: row.CreatedAt.Format("2006-01-02 15:04"),
	}
}

// portalListNodeInbounds is GET /u/my-nodes/:id/inbounds.
func (s *Server) portalListNodeInbounds(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalNodeUser(c)
	if u == nil {
		return
	}
	n := s.portalApprovedNode(c, u)
	if n == nil {
		return
	}
	var ins []store.Inbound
	if err := s.st.DB.Where("node_id = ?", n.ID).Order("port, tag").Find(&ins).Error; err != nil {
		serverError(c, err)
		return
	}
	views := make([]portalInboundView, 0, len(ins))
	for i := range ins {
		views = append(views, s.portalInboundViewOf(&ins[i]))
	}
	c.JSON(http.StatusOK, gin.H{"inbounds": views})
}

// portalCreateNodeInbound is POST /u/my-nodes/:id/inbounds. The inbound is
// created through the same core the admin API and the template engine use
// (insertInbound), but bound to nobody except its owner: an explicit empty
// group set (nil would mean "the default groups") plus one direct
// UserAccess grant (kind=inbound) for the owner alone.
func (s *Server) portalCreateNodeInbound(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalNodeUser(c)
	if u == nil {
		return
	}
	n := s.portalApprovedNode(c, u)
	if n == nil {
		return
	}
	var req portalInboundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	row, ok := s.createOwnedInbound(c, u, portalActor(u, s.portalViaMail(c)), n, req)
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"inbound": s.portalInboundViewOf(row)})
}

// createOwnedInbound is the shared creation flow for an inbound on a
// user-owned node: the portal's POST and the panel member's POST run
// the same sanitising, the same insertInbound core and the same
// owner-only binding — only the actor and the rendered view differ.
// Failures are answered here; ok=false means the response is written.
func (s *Server) createOwnedInbound(c *gin.Context, u *store.User, actor string, n *store.Node, req portalInboundReq) (*store.Inbound, bool) {
	adminReq := req.toAdmin()
	adminReq.NodeID = n.ID // the node is never the client's choice
	m := adminReq.toModel()
	sanitizePortalInbound(&m)
	row, err := s.insertInbound(n, &m, adminReq.meta(), []string{})
	if err != nil {
		badRequest(c, err)
		return nil, false
	}
	if err := s.st.DB.Create(&store.UserAccess{UserID: u.ID, Kind: "inbound", TargetID: row.ID}).Error; err != nil {
		// The grant is what makes the inbound the owner's; without it the
		// row would be an orphan on their node, so roll the create back.
		s.st.DB.Transaction(func(tx *gorm.DB) error {
			tx.Where("inbound_id = ?", row.ID).Delete(&store.GroupInbound{})
			tx.Where("inbound_id = ?", row.ID).Delete(&store.Endpoint{})
			return tx.Delete(&store.Inbound{}, "id = ?", row.ID).Error
		})
		serverError(c, err)
		return nil, false
	}
	s.MarkDirty("inbound "+row.Tag+" created", n.ID)
	s.audit(actor, "inbound.create", n.DisplayName+"/"+row.Tag,
		fmt.Sprintf("%s %s+%s+%s :%d", row.Core, m.Protocol.Type, m.Transport.Type, m.Security.Type, m.Port), clientIP(c.Request))
	return &row, true
}

// portalOwnedInbound loads an inbound of the node by :inboundId; anything
// else is a 404, same as the node itself.
func (s *Server) portalOwnedInbound(c *gin.Context, n *store.Node) *store.Inbound {
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ? AND node_id = ?", c.Param("inboundId"), n.ID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "入站不存在"})
		return nil
	}
	return &row
}

// portalUpdateNodeInbound is PATCH /u/my-nodes/:id/inbounds/:inboundId.
// It follows updateInbound's structure (secret carry-over, validation,
// transactional save) with the portal's field set: publication policy,
// relay access, outbound routing and group bindings are not the owner's
// to change and are preserved as-is.
func (s *Server) portalUpdateNodeInbound(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalNodeUser(c)
	if u == nil {
		return
	}
	n := s.portalApprovedNode(c, u)
	if n == nil {
		return
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	row := s.portalOwnedInbound(c, n)
	if row == nil {
		return
	}
	if !s.updateOwnedInbound(c, portalActor(u, s.portalViaMail(c)), n, row) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"inbound": s.portalInboundViewOf(row)})
}

// updateOwnedInbound is the shared update flow for an inbound on a
// user-owned node (portal PATCH and panel member PUT): the portal's
// field set and preserve rules, run under the caller's relayMu with the
// node and row already resolved as the caller's own. Failures are
// answered here; false means the response is already written.
func (s *Server) updateOwnedInbound(c *gin.Context, actor string, n *store.Node, row *store.Inbound) bool {
	if row.TemplateID != "" {
		badRequest(c, errors.New("该入站由模板管理，请联系管理员"))
		return false
	}
	if row.RelayAccess.V.Enabled {
		badRequest(c, errors.New("该入站开启了来源限制，请联系管理员"))
		return false
	}
	var req portalInboundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return false
	}
	adminReq := req.toAdmin()
	adminReq.NodeID = n.ID
	old := s.toModelInbound(row)
	m := adminReq.toModel()
	sanitizePortalInbound(&m)
	m.OutboundIPStrategy = row.OutboundIPStrategy // preserved, as on save below
	// The certificate the create minted (or an admin chose) stays: the
	// sanitiser cleared the ID, so put the existing one back — a TLS
	// inbound must not silently re-mint its certificate on every edit.
	m.Security.CertificateID = old.Security.CertificateID
	if req.Enabled == nil {
		m.Enabled = row.Enabled
	}
	// Keep generated secrets unless explicitly replaced (updateInbound's
	// carry-over, verbatim).
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
	m.Extra = old.Extra // the raw escape hatch is admin-only; preserve, never accept
	if err := s.validateInbound(n, &m, row.ID); err != nil {
		badRequest(c, err)
		return false
	}
	fromModel(&m, row)
	row.Remark = req.Remark
	s.sealInbound(row)
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockCDNReports(tx); err != nil {
			return err
		}
		// Publication policy, relay access and the outbound IP strategy
		// are preserved (see the handler comment), so they are omitted
		// from the write rather than overwritten with zero values.
		if err := tx.Omit("RelayOnly", "RelayAccess", "OutboundIPStrategy", "OutboundTag", "SpeedLimitMbps").Save(row).Error; err != nil {
			return err
		}
		return validateAttachedTunnels(tx, row.ID)
	})
	if err != nil {
		dbError(c, err)
		return false
	}
	s.MarkDirty("inbound "+row.Tag+" updated", s.nodesForInbounds(row.ID)...)
	s.audit(actor, "inbound.update", n.DisplayName+"/"+row.Tag, "", clientIP(c.Request))
	return true
}

// portalDeleteNodeInbound is DELETE /u/my-nodes/:id/inbounds/:inboundId:
// the same reference checks and cleanup cascade deleteInbound runs.
func (s *Server) portalDeleteNodeInbound(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalNodeUser(c)
	if u == nil {
		return
	}
	n := s.portalApprovedNode(c, u)
	if n == nil {
		return
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	row := s.portalOwnedInbound(c, n)
	if row == nil {
		return
	}
	if !s.deleteOwnedInbound(c, portalActor(u, s.portalViaMail(c)), n, row) {
		return
	}
	c.Status(http.StatusNoContent)
}

// deleteOwnedInbound is the shared delete flow for an inbound on a
// user-owned node (portal DELETE and panel member DELETE): the same
// reference checks and cleanup cascade deleteInbound runs, under the
// caller's relayMu with node and row already resolved as the caller's
// own. Failures are answered here; false means the response is written.
func (s *Server) deleteOwnedInbound(c *gin.Context, actor string, n *store.Node, row *store.Inbound) bool {
	var refs int64
	s.st.DB.Model(&store.Outbound{}).Where("node_ref_inbound_id = ?", row.ID).Count(&refs)
	if refs > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("有 %d 个出站 / 链路引用该入站，请先删除", refs)})
		return false
	}
	if master := s.referencingMaster(row.ID); master != "" {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("入站「%s」把它作为回落目标，请先在该入站中移除", master)})
		return false
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
		return tx.Delete(row).Error
	})
	if err != nil {
		serverError(c, err)
		return false
	}
	s.MarkDirty("inbound "+row.Tag+" deleted", nodes...)
	s.audit(actor, "inbound.delete", n.DisplayName+"/"+row.Tag, "", clientIP(c.Request))
	return true
}
