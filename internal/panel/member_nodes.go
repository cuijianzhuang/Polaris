package panel

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Batch 2 of the member principal (docs/design/panel-member-login.md
// §7/§9): the write surface. A member manages their own nodes and the
// inbounds on them through the panel's admin routes, but every handler
// below resolves its targets through the member ownership checks first
// (another user's resource is a 404, exactly like the portal's
// portalOwnedNode) and then runs the very same flow the portal runs —
// createOwnedNode / updateOwnedNode / deleteOwnedNode and the inbound
// trio from user_nodes.go / user_node_inbounds.go — so the panel result
// is the portal result, with the member actor in the audit log. The
// admin handlers branch here before touching anything when the caller
// is a member; admin and operator semantics are untouched.

// memberOwnedNode loads the node named by :id only when it belongs to
// the member. Anything else is a 404: another user's node must be
// indistinguishable from a nonexistent one (portalOwnedNode's rule).
func (s *Server) memberOwnedNode(c *gin.Context, u *store.User) *store.Node {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ? AND owner_id = ?", c.Param("id"), u.ID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		return nil
	}
	return &n
}

// memberApprovedNodeByID resolves a node for an inbound operation:
// owned by the member (else 404) and approved (else the portal's 403
// wording) — a pending or rejected node serves nothing, so its inbounds
// are frozen too.
func (s *Server) memberApprovedNodeByID(c *gin.Context, u *store.User, id string) *store.Node {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ? AND owner_id = ?", id, u.ID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		return nil
	}
	switch n.ReviewState {
	case "approved":
		return &n
	case "pending":
		c.JSON(http.StatusForbidden, gin.H{"error": "节点待管理员审批，通过后才能管理入站"})
	default: // rejected (user nodes always carry a review state)
		c.JSON(http.StatusForbidden, gin.H{"error": "节点已被管理员拒绝，无法管理入站，请删除后重新添加"})
	}
	return nil
}

// memberOwnedInbound resolves the inbound named by :id together with
// its node, under the same rules as memberApprovedNodeByID.
func (s *Server) memberOwnedInbound(c *gin.Context, u *store.User) (*store.Node, *store.Inbound) {
	var row store.Inbound
	if err := s.st.DB.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "入站不存在"})
		return nil, nil
	}
	n := s.memberApprovedNodeByID(c, u, row.NodeID)
	if n == nil {
		return nil, nil
	}
	return n, &row
}

// memberNodeView renders one of the member's nodes the way the panel's
// node list does (nodeViews carries cores, counts and review state).
func (s *Server) memberNodeView(n *store.Node) nodeView {
	for _, v := range s.nodeViews() {
		if v.ID == n.ID {
			return v
		}
	}
	return nodeView{Node: *n}
}

// memberInboundView renders an inbound on a member node the way the
// admin inbound list does, except the node name is the owner's display
// label — the internal un-<id> name never leaves through a member view.
func (s *Server) memberInboundView(row *store.Inbound, n *store.Node) inboundView {
	users, _ := s.inboundUsers([]string{row.ID})
	return s.inboundView(*row, map[string]string{n.ID: nodeDisplayLabel(n)}, users)
}

// memberCreateNode is POST /nodes for a member: the portal's creation
// flow (mode, per-user quota, safe fields, install command).
func (s *Server) memberCreateNode(c *gin.Context, u *store.User) {
	n, tok, cmd, ok := s.createOwnedNode(c, u, memberActor(u))
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"node": s.memberNodeView(n), "token": tok, "command": cmd})
}

// memberUpdateNode is PUT /nodes/:id for a member: the portal's
// display-field patch (displayName / address / region / flag / labels);
// the admin request's management fields are not part of the patch
// struct and are ignored by construction.
func (s *Server) memberUpdateNode(c *gin.Context, u *store.User) {
	n := s.memberOwnedNode(c, u)
	if n == nil {
		return
	}
	updated, ok := s.updateOwnedNode(c, memberActor(u), n)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, s.memberNodeView(updated))
}

// memberDeleteNode is DELETE /nodes/:id for a member: the portal's
// cascade, restricted to the member's own node.
func (s *Server) memberDeleteNode(c *gin.Context, u *store.User) {
	n := s.memberOwnedNode(c, u)
	if n == nil {
		return
	}
	if !s.deleteOwnedNode(c, memberActor(u), n) {
		return
	}
	c.Status(http.StatusNoContent)
}

// memberInstallToken is POST /nodes/:id/install-token for a member. It
// follows the portal's two install endpoints through this one route: a
// node that has not enrolled yet simply gets a fresh token (the
// previous link dies, as with the admin endpoint); an enrolled node is
// reset instead — certificate revoked, fresh token — and a rejected
// node may not re-enrol its way around the rejection.
func (s *Server) memberInstallToken(c *gin.Context, u *store.User) {
	n := s.memberOwnedNode(c, u)
	if n == nil {
		return
	}
	actor := memberActor(u)
	reset := n.CertSerial != ""
	if reset {
		if n.ReviewState == "rejected" {
			badRequest(c, errors.New("节点已被管理员拒绝，请删除后重新添加"))
			return
		}
		if err := s.revokeNodeCert(n); err != nil {
			dbError(c, err)
			return
		}
	}
	tok, cmd, err := s.newInstallToken(n, c.Request)
	if err != nil {
		serverError(c, err)
		return
	}
	action := "node.install_token"
	if reset {
		action = "node.reset_access"
	}
	s.audit(actor, action, n.DisplayName, "", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"token": tok, "command": cmd, "url": s.joinURL(c.Request, tok),
		"wget": "wget -qO- " + s.joinURL(c.Request, tok) + " | bash", "expiresIn": 86400, "reset": reset})
}

// memberListInbounds is GET /inbounds for a member: only inbounds on
// their own approved nodes (the operation scope — the guards have
// already refused a ?node= outside it).
func (s *Server) memberListInbounds(c *gin.Context, u *store.User) {
	sc := nodeScope(c)
	q := s.st.DB.Order("node_id, port")
	if v := c.Query("node"); v != "" {
		q = q.Where("node_id = ?", v)
	} else {
		ids := make([]string, 0, len(sc))
		for id := range sc {
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			c.JSON(http.StatusOK, []inboundView{})
			return
		}
		q = q.Where("node_id IN ?", ids)
	}
	if v := c.Query("core"); v != "" {
		q = q.Where("core = ?", v)
	}
	var ins []store.Inbound
	if err := q.Find(&ins).Error; err != nil {
		serverError(c, err)
		return
	}
	names := map[string]string{}
	var ns []store.Node
	s.st.DB.Where("owner_id = ?", u.ID).Find(&ns)
	for i := range ns {
		names[ns[i].ID] = nodeDisplayLabel(&ns[i])
	}
	ids := make([]string, len(ins))
	for i := range ins {
		ids[i] = ins[i].ID
	}
	users, _ := s.inboundUsers(ids)
	out := make([]inboundView, 0, len(ins))
	for i := range ins {
		out = append(out, s.inboundView(ins[i], names, users))
	}
	c.JSON(http.StatusOK, out)
}

// memberInboundCreateReq is the panel create body: the portal's
// inbound form plus the node it targets. The dangerous knobs of the
// admin request are absent on purpose (see portalInboundReq).
type memberInboundCreateReq struct {
	NodeID string `json:"nodeId"`
	portalInboundReq
}

// memberCreateInbound is POST /inbounds for a member: the node comes
// from the body but is resolved through the ownership + approval gate
// before anything is created.
func (s *Server) memberCreateInbound(c *gin.Context, u *store.User) {
	var req memberInboundCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	n := s.memberApprovedNodeByID(c, u, req.NodeID)
	if n == nil {
		return
	}
	row, ok := s.createOwnedInbound(c, u, memberActor(u), n, req.portalInboundReq)
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"inbound": s.memberInboundView(row, n), "core": row.Core})
}

// memberGetInbound is GET /inbounds/:id for a member.
func (s *Server) memberGetInbound(c *gin.Context, u *store.User) {
	n, row := s.memberOwnedInbound(c, u)
	if row == nil {
		return
	}
	c.JSON(http.StatusOK, s.memberInboundView(row, n))
}

// memberUpdateInbound is PUT /inbounds/:id for a member: the portal's
// update flow (field set, preserve rules) under the relay lock.
func (s *Server) memberUpdateInbound(c *gin.Context, u *store.User) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	n, row := s.memberOwnedInbound(c, u)
	if row == nil {
		return
	}
	if !s.updateOwnedInbound(c, memberActor(u), n, row) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"inbound": s.memberInboundView(row, n), "core": row.Core})
}

// memberDeleteInbound is DELETE /inbounds/:id for a member: the
// portal's delete flow under the relay lock.
func (s *Server) memberDeleteInbound(c *gin.Context, u *store.User) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	n, row := s.memberOwnedInbound(c, u)
	if row == nil {
		return
	}
	if !s.deleteOwnedInbound(c, memberActor(u), n, row) {
		return
	}
	c.Status(http.StatusNoContent)
}
