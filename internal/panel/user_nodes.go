package panel

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// User-owned nodes (design user-nodes): portal users bring their own VPS,
// enrol it with the usual one-time token and manage it themselves. A user
// node belongs to exactly one user (Node.OwnerID), serves only that user's
// accounts, and — in approval mode — serves nothing at all until an admin
// approves it (Node.ReviewState, enforced in BuildSpec).

// Enrolment modes for user-owned nodes (setting user_nodes_mode).
const (
	userNodesOff      = "off"
	userNodesApproval = "approval"
	userNodesOpen     = "open"
)

// effectiveUserNodesMode resolves the mode in effect: the admin's setting,
// intersected with the licence. Open enrolment (no review) is a paid
// feature; without license.FeatureUserNodesOpen an "open" setting degrades
// to approval. Unknown values degrade to approval too.
func (s *Server) effectiveUserNodesMode() string {
	mode := strings.TrimSpace(s.st.GetSetting("user_nodes_mode", userNodesApproval))
	switch mode {
	case userNodesOff, userNodesOpen:
	default:
		mode = userNodesApproval
	}
	if mode == userNodesOpen && !s.license().Allow(license.FeatureUserNodesOpen) {
		mode = userNodesApproval
	}
	return mode
}

// userNodeCap is how many own nodes a user may manage: the licence's
// per-user cap (community edition: CommunityUserNodes), tightened by the
// user's plan NodeLimit when the plan sets one. 0 = unlimited.
func (s *Server) userNodeCap(u *store.User) int {
	limit := s.license().Limit(license.LimitUserNodes)
	if u.PlanID != "" {
		if p, err := loadPlan(s.st.DB, u.PlanID); err == nil && p.NodeLimit > 0 {
			if limit <= 0 || p.NodeLimit < limit {
				limit = p.NodeLimit
			}
		}
	}
	return limit
}

// userNodeCount counts a user's own nodes against the cap. Pending nodes
// count (approval may land any moment; not counting them would invite
// create-and-abandon farming), rejected ones do not (they serve nothing
// and hold no quota hostage; deleting one frees the slot as usual).
func (s *Server) userNodeCount(userID string) int64 {
	var n int64
	s.st.DB.Model(&store.Node{}).Where("owner_id = ? AND review_state <> ?", userID, "rejected").Count(&n)
	return n
}

// withOwnNodeCounts fills OwnNodes / OwnPendingNodes on a page of users
// with one grouped query over their brought-in nodes (rejected nodes are
// dead weight and do not count, same as the quota's counting rule).
func (s *Server) withOwnNodeCounts(us []store.User) {
	if len(us) == 0 {
		return
	}
	ids := make([]string, len(us))
	for i := range us {
		ids[i] = us[i].ID
	}
	type cnt struct {
		OwnerID     string
		ReviewState string
		N           int64
	}
	var rows []cnt
	s.st.DB.Model(&store.Node{}).Select("owner_id, review_state, count(*) AS n").
		Where("owner_id IN ?", ids).Group("owner_id, review_state").Scan(&rows)
	total := map[string]int{}
	pending := map[string]int{}
	for _, r := range rows {
		if r.ReviewState != "rejected" {
			total[r.OwnerID] += int(r.N)
		}
		if r.ReviewState == "pending" {
			pending[r.OwnerID] += int(r.N)
		}
	}
	for i := range us {
		us[i].OwnNodes = total[us[i].ID]
		us[i].OwnPendingNodes = pending[us[i].ID]
	}
}

// nodeDisplayLabel is how a node is named in admin-facing messages: the
// owner's display name for a user node, the internal name otherwise.
func nodeDisplayLabel(n *store.Node) string {
	if n.OwnerID != "" && n.DisplayName != "" {
		return n.DisplayName
	}
	return n.Name
}

// checkGroupTargets refuses to place user-owned nodes — or inbounds on
// them — into a group or plan. A user node may only ever serve its owner
// (through the owner's own direct bindings); group membership would hand
// it to every member of the group. Called on every group/plan save path
// before any binding row is written.
func (s *Server) checkGroupTargets(nodeIDs, inboundIDs []string) error {
	if len(nodeIDs) > 0 {
		var ns []store.Node
		if err := s.st.DB.Where("id IN ?", nodeIDs).Find(&ns).Error; err != nil {
			return err
		}
		for i := range ns {
			if ns[i].OwnerID != "" {
				return commandInputError{fmt.Sprintf("节点「%s」是用户自带节点，只能服务其主人，不能加入分组或套餐", nodeDisplayLabel(&ns[i]))}
			}
		}
	}
	if len(inboundIDs) > 0 {
		var ins []store.Inbound
		if err := s.st.DB.Where("id IN ?", inboundIDs).Find(&ins).Error; err != nil {
			return err
		}
		owners, err := s.nodeOwnersOf(ins)
		if err != nil {
			return err
		}
		for i := range ins {
			if owners[ins[i].NodeID] != "" {
				return commandInputError{fmt.Sprintf("入站「%s」位于用户自带节点上，只能服务其主人，不能加入分组或套餐", ins[i].Tag)}
			}
		}
	}
	return nil
}

// nodeOwnersOf maps node ID → OwnerID for the nodes hosting ins.
func (s *Server) nodeOwnersOf(ins []store.Inbound) (map[string]string, error) {
	owners := map[string]string{}
	if len(ins) == 0 {
		return owners, nil
	}
	ids := make([]string, 0, len(ins))
	seen := map[string]bool{}
	for i := range ins {
		if !seen[ins[i].NodeID] {
			seen[ins[i].NodeID] = true
			ids = append(ids, ins[i].NodeID)
		}
	}
	var ns []store.Node
	if err := s.st.DB.Where("id IN ?", ids).Find(&ns).Error; err != nil {
		return nil, err
	}
	for i := range ns {
		owners[ns[i].ID] = ns[i].OwnerID
	}
	return owners, nil
}

// checkAccessTargets refuses a direct binding (UserAccess) that would hand
// a user-owned node — or an inbound on one — to anyone but its owner.
// Bindings for the owner themself pass. Called from setAccess/addAccess so
// every admin path that writes user_accesses is covered.
func checkAccessTargets(tx *gorm.DB, userID, kind string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	switch kind {
	case "node":
		var ns []store.Node
		if err := tx.Where("id IN ?", ids).Find(&ns).Error; err != nil {
			return err
		}
		for i := range ns {
			if ns[i].OwnerID != "" && ns[i].OwnerID != userID {
				return commandInputError{fmt.Sprintf("节点「%s」是用户自带节点，只能绑定给其主人", nodeDisplayLabel(&ns[i]))}
			}
		}
	case "inbound":
		var ins []store.Inbound
		if err := tx.Where("id IN ?", ids).Find(&ins).Error; err != nil {
			return err
		}
		nodeIDs := make([]string, 0, len(ins))
		seen := map[string]bool{}
		for i := range ins {
			if !seen[ins[i].NodeID] {
				seen[ins[i].NodeID] = true
				nodeIDs = append(nodeIDs, ins[i].NodeID)
			}
		}
		if len(nodeIDs) > 0 {
			var ns []store.Node
			if err := tx.Where("id IN ?", nodeIDs).Find(&ns).Error; err != nil {
				return err
			}
			byID := map[string]*store.Node{}
			for i := range ns {
				byID[ns[i].ID] = &ns[i]
			}
			for i := range ins {
				if n := byID[ins[i].NodeID]; n != nil && n.OwnerID != "" && n.OwnerID != userID {
					return commandInputError{fmt.Sprintf("入站「%s」位于用户自带节点上，只能绑定给其主人", ins[i].Tag)}
				}
			}
		}
	}
	return nil
}

// portalNodeUser answers 401 and returns nil when the portal session is
// gone; the JSON node APIs use it instead of the page handlers' redirect.
func (s *Server) portalNodeUser(c *gin.Context) *store.User {
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
	}
	return u
}

// portalOwnedNode loads the node named by :id only when it belongs to the
// signed-in user. Anything else is a 404: another user's node must be
// indistinguishable from a nonexistent one. Every portal node handler
// takes its node through here.
func (s *Server) portalOwnedNode(c *gin.Context, u *store.User) *store.Node {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ? AND owner_id = ?", c.Param("id"), u.ID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		return nil
	}
	return &n
}

// portalNodeView is what the user center sees of a node: the internal
// Name ("un-<id>") never leaves the panel.
type portalNodeView struct {
	ID           string           `json:"id"`
	DisplayName  string           `json:"displayName"`
	Address      string           `json:"address"`
	Region       string           `json:"region"`
	Flag         string           `json:"flag"`
	Labels       []string         `json:"labels"`
	Status       string           `json:"status"`
	ReviewState  string           `json:"reviewState"`
	AgentVersion string           `json:"agentVersion"`
	Connected    bool             `json:"connected"`
	Enrolled     bool             `json:"enrolled"`
	System       store.SystemInfo `json:"system"`
	LastSeen     *time.Time       `json:"lastSeen"`
	CreatedAt    time.Time        `json:"createdAt"`
}

func (s *Server) portalNodeViewOf(n *store.Node) portalNodeView {
	labels := n.Labels.V
	if labels == nil {
		labels = []string{}
	}
	return portalNodeView{
		ID: n.ID, DisplayName: n.DisplayName, Address: n.Address, Region: n.Region,
		Flag: n.Flag, Labels: labels, Status: n.Status, ReviewState: n.ReviewState,
		AgentVersion: n.AgentVersion, Connected: s.hub.Online(n.ID), Enrolled: n.CertSerial != "",
		System: n.System.V, LastSeen: n.LastSeen, CreatedAt: n.CreatedAt,
	}
}

// portalMyNodes is GET /u/my-nodes: the signed-in user's own nodes, plus
// the mode and quota the page needs to render its limits.
func (s *Server) portalMyNodes(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalNodeUser(c)
	if u == nil {
		return
	}
	var ns []store.Node
	s.st.DB.Where("owner_id = ?", u.ID).Order("created_at, id").Find(&ns)
	views := make([]portalNodeView, 0, len(ns))
	for i := range ns {
		views = append(views, s.portalNodeViewOf(&ns[i]))
	}
	c.JSON(http.StatusOK, gin.H{"nodes": views, "mode": s.effectiveUserNodesMode(),
		"limit": s.userNodeCap(u), "count": s.userNodeCount(u.ID)})
}

type portalNodeReq struct {
	DisplayName string   `json:"displayName"`
	Address     string   `json:"address"`
	Region      string   `json:"region"`
	Flag        string   `json:"flag"`
	Labels      []string `json:"labels"`
}

// portalCreateNode is POST /u/my-nodes: create a node record for the
// user's own VPS and hand back the one-time install command. Only safe
// fields are accepted; management fields (relay, traffic rate, probes…)
// stay admin-only, and the traffic rate is fixed at 1.
func (s *Server) portalCreateNode(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalNodeUser(c)
	if u == nil {
		return
	}
	n, tok, cmd, ok := s.createOwnedNode(c, u, portalActor(u, s.portalViaMail(c)))
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"node": s.portalNodeViewOf(n), "token": tok, "command": cmd})
}

// createOwnedNode is the shared creation flow for a user-owned node:
// the portal's POST /u/my-nodes and the panel member's POST /nodes run
// the same mode, quota and field rules — only the actor and the view
// the caller renders differ. Failures are answered here; ok=false means
// the response is already written.
func (s *Server) createOwnedNode(c *gin.Context, u *store.User, actor string) (*store.Node, string, string, bool) {
	mode := s.effectiveUserNodesMode()
	if mode == userNodesOff {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理员已关闭用户自带节点功能"})
		return nil, "", "", false
	}
	var req portalNodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return nil, "", "", false
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.DisplayName == "" {
		badRequest(c, errors.New("请填写节点名称"))
		return nil, "", "", false
	}
	if len([]rune(req.DisplayName)) > 64 {
		badRequest(c, errors.New("节点名称最长 64 个字符"))
		return nil, "", "", false
	}
	if !s.limitOK(c, license.LimitNodes, 1) {
		return nil, "", "", false // user nodes also count toward the panel-wide node cap
	}
	if limit := s.userNodeCap(u); limit > 0 {
		if count := s.userNodeCount(u.ID); count >= int64(limit) {
			c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
				"error": fmt.Sprintf("每个用户最多管理 %d 台自有服务器（已有 %d 台），请升级许可证或套餐", limit, count),
				"limit": license.LimitUserNodes, "plan": "pro"})
			return nil, "", "", false
		}
	}
	id := store.NewID()
	review := "pending"
	if mode == userNodesOpen {
		review = "approved"
	}
	version := s.defaultCoreVersion(c.Request.Context(), model.CoreXray)
	n := store.Node{ID: id, Name: "un-" + id, DisplayName: req.DisplayName, OwnerID: u.ID, ReviewState: review,
		Address: req.Address, Region: req.Region, Flag: strings.ToUpper(req.Flag),
		Labels: store.NewJSON(nonNilStrs(req.Labels)), DefaultCore: model.CoreXray,
		TrafficRate: 1, Status: "pending", StatusHidden: true}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&n).Error; err != nil {
			return err
		}
		return tx.Create(&store.Core{ID: store.NewID(), NodeID: n.ID, Type: model.CoreXray, Version: version,
			Status: "installing", IsDefault: true, InstalledVersions: store.NewJSON([]string{})}).Error
	})
	if err != nil {
		dbError(c, err)
		return nil, "", "", false
	}
	tok, cmd, err := s.newInstallToken(&n, c.Request)
	if err != nil {
		serverError(c, err)
		return nil, "", "", false
	}
	s.audit(actor, "node.create", n.DisplayName, n.Address, clientIP(c.Request))
	return &n, tok, cmd, true
}

// portalNodeInstall is GET /u/my-nodes/:id/install: the install command
// while the node has not enrolled yet. Only the token's hash is stored,
// so re-viewing mints a fresh token and the previous link dies — the same
// trade-off as the admin install-token endpoint.
func (s *Server) portalNodeInstall(c *gin.Context) {
	u := s.portalNodeUser(c)
	if u == nil {
		return
	}
	n := s.portalOwnedNode(c, u)
	if n == nil {
		return
	}
	if n.CertSerial != "" {
		c.JSON(http.StatusOK, gin.H{"enrolled": true, "command": ""})
		return
	}
	tok, cmd, err := s.newInstallToken(n, c.Request)
	if err != nil {
		serverError(c, err)
		return
	}
	s.audit(portalActor(u, s.portalViaMail(c)), "node.install_token", n.DisplayName, "", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"enrolled": false, "token": tok, "command": cmd, "url": s.joinURL(c.Request, tok),
		"wget": "wget -qO- " + s.joinURL(c.Request, tok) + " | bash", "expiresIn": 86400})
}

// revokeNodeCert invalidates a node's agent certificate and drops its
// connection (the field updates revokeNode makes).
func (s *Server) revokeNodeCert(n *store.Node) error {
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{
		"revoked": true, "cert_serial": "", "status": "offline"}).Error; err != nil {
		return err
	}
	s.hub.Kick(n.ID)
	return nil
}

// portalNodeResetAccess is POST /u/my-nodes/:id/reset-access: revoke the
// current agent certificate and issue a fresh install token, so a rebuilt
// or replaced VPS can enrol again.
func (s *Server) portalNodeResetAccess(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalNodeUser(c)
	if u == nil {
		return
	}
	n := s.portalOwnedNode(c, u)
	if n == nil {
		return
	}
	if n.ReviewState == "rejected" {
		// A rejected node must not re-enrol its way around the rejection
		// (Enroll clears the revoked flag); delete and re-add instead.
		badRequest(c, errors.New("节点已被管理员拒绝，请删除后重新添加"))
		return
	}
	if err := s.revokeNodeCert(n); err != nil {
		dbError(c, err)
		return
	}
	tok, cmd, err := s.newInstallToken(n, c.Request)
	if err != nil {
		serverError(c, err)
		return
	}
	s.audit(portalActor(u, s.portalViaMail(c)), "node.reset_access", n.DisplayName, "", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"token": tok, "command": cmd, "url": s.joinURL(c.Request, tok), "expiresIn": 86400})
}

// portalUpdateNode is PATCH /u/my-nodes/:id: display fields only
// (displayName / address / region / flag / labels).
func (s *Server) portalUpdateNode(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalNodeUser(c)
	if u == nil {
		return
	}
	n := s.portalOwnedNode(c, u)
	if n == nil {
		return
	}
	updated, ok := s.updateOwnedNode(c, portalActor(u, s.portalViaMail(c)), n)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, s.portalNodeViewOf(updated))
}

// ownedNodePatch is the display-field patch a node owner may apply;
// the portal's PATCH and the panel member's PUT share it verbatim.
type ownedNodePatch struct {
	DisplayName *string   `json:"displayName"`
	Address     *string   `json:"address"`
	Region      *string   `json:"region"`
	Flag        *string   `json:"flag"`
	Labels      *[]string `json:"labels"`
}

// updateOwnedNode applies an ownedNodePatch to n (already resolved as
// the caller's own node). Failures are answered here; ok=false means
// the response is already written.
func (s *Server) updateOwnedNode(c *gin.Context, actor string, n *store.Node) (*store.Node, bool) {
	var req ownedNodePatch
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return nil, false
	}
	upd := map[string]any{}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if name == "" {
			badRequest(c, errors.New("节点名称不能为空"))
			return nil, false
		}
		if len([]rune(name)) > 64 {
			badRequest(c, errors.New("节点名称最长 64 个字符"))
			return nil, false
		}
		upd["display_name"] = name
	}
	if req.Address != nil {
		upd["address"] = *req.Address
	}
	if req.Region != nil {
		upd["region"] = *req.Region
	}
	if req.Flag != nil {
		upd["flag"] = strings.ToUpper(*req.Flag)
	}
	if req.Labels != nil {
		upd["labels"] = store.NewJSON(nonNilStrs(*req.Labels))
	}
	if len(upd) > 0 {
		if err := s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(upd).Error; err != nil {
			dbError(c, err)
			return nil, false
		}
		s.MarkDirty("user node updated", n.ID)
		s.audit(actor, "node.update", n.DisplayName, "", clientIP(c.Request))
		if err := s.st.DB.First(n, "id = ?", n.ID).Error; err != nil {
			dbError(c, err)
			return nil, false
		}
	}
	return n, true
}

// nodeDeleteError maps a failed node cascade to its HTTP answer (the
// mapping deleteNode makes inline).
func nodeDeleteError(c *gin.Context, err error) {
	var relayInUse *nodeRelayInUseError
	if errors.As(err, &relayInUse) {
		c.JSON(http.StatusConflict, gin.H{"error": relayInUse.Error()})
		return
	}
	var inUse *nodeInUseError
	if errors.As(err, &inUse) {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("其他节点有 %d 个出站 / 链路引用该节点的入站，请先删除", inUse.refs)})
		return
	}
	serverError(c, err)
}

// portalDeleteNode is DELETE /u/my-nodes/:id: the same cascade the admin
// node delete runs.
func (s *Server) portalDeleteNode(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalNodeUser(c)
	if u == nil {
		return
	}
	n := s.portalOwnedNode(c, u)
	if n == nil {
		return
	}
	if !s.deleteOwnedNode(c, portalActor(u, s.portalViaMail(c)), n) {
		return
	}
	c.Status(http.StatusNoContent)
}

// deleteOwnedNode cascade-deletes a user-owned node (the portal's
// DELETE and the panel member's DELETE share it). Failures are answered
// here with nodeDeleteError's mapping; the caller writes the 204.
func (s *Server) deleteOwnedNode(c *gin.Context, actor string, n *store.Node) bool {
	s.relayMu.Lock()
	upstream, err := s.deleteNodeCascade(n.ID)
	s.relayMu.Unlock()
	if err != nil {
		nodeDeleteError(c, err)
		return false
	}
	s.hub.Kick(n.ID)
	s.MarkDirty("upstream node deleted", upstream...)
	s.audit(actor, "node.delete", n.DisplayName, "", clientIP(c.Request))
	return true
}

// reviewNode is POST /nodes/:id/review (admin): approve / reject / suspend
// a user-owned node. approve opens the configuration gate (BuildSpec),
// reject additionally revokes the agent certificate, suspend sends an
// approved node back to pending (which closes the gate again).
func (s *Server) reviewNode(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req struct {
		Action string `json:"action" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if n.OwnerID == "" {
		badRequest(c, errors.New("管理员节点不走用户节点审批"))
		return
	}
	switch req.Action {
	case "approve":
		if n.ReviewState != "pending" {
			badRequest(c, errors.New("只有待审批的节点可以批准"))
			return
		}
		if err := s.st.DB.Model(&n).Update("review_state", "approved").Error; err != nil {
			dbError(c, err)
			return
		}
		s.MarkDirty("user node approved", n.ID)
	case "reject":
		if n.ReviewState != "pending" && n.ReviewState != "approved" {
			badRequest(c, errors.New("该节点已是拒绝状态"))
			return
		}
		if err := s.st.DB.Model(&n).Update("review_state", "rejected").Error; err != nil {
			dbError(c, err)
			return
		}
		if err := s.revokeNodeCert(&n); err != nil {
			dbError(c, err)
			return
		}
		s.MarkDirty("user node rejected", n.ID)
	case "suspend":
		if n.ReviewState != "approved" {
			badRequest(c, errors.New("只有已批准的节点可以停用"))
			return
		}
		if err := s.st.DB.Model(&n).Update("review_state", "pending").Error; err != nil {
			dbError(c, err)
			return
		}
		s.MarkDirty("user node suspended", n.ID)
	default:
		badRequest(c, errors.New("action 应为 approve、reject 或 suspend"))
		return
	}
	s.auditCtx(c, "node.review", n.Name, req.Action)
	if err := s.st.DB.First(&n, "id = ?", n.ID).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(http.StatusOK, n)
}

// revokeOwnedNodes invalidates the agent certificates of every node the
// given users own (design D6: disabling an account stops its machines at
// once). Already-revoked nodes are skipped.
func (s *Server) revokeOwnedNodes(userIDs ...string) {
	if len(userIDs) == 0 {
		return
	}
	var ids []string
	s.st.DB.Model(&store.Node{}).Where("owner_id IN ? AND revoked = ?", userIDs, false).Pluck("id", &ids)
	if len(ids) == 0 {
		return
	}
	if err := s.st.DB.Model(&store.Node{}).Where("id IN ?", ids).Updates(map[string]any{
		"revoked": true, "cert_serial": "", "status": "offline"}).Error; err != nil {
		return
	}
	for _, id := range ids {
		s.hub.Kick(id)
	}
}

// deleteOwnedNodes cascade-deletes every node a user owns (design D6:
// deleting an account deletes its nodes) and returns the deleted node ids
// plus the other nodes whose specs the deletions affect. Callers hold
// relayMu and do the post-commit work (hub kicks, MarkDirty).
func (s *Server) deleteOwnedNodes(userID string) (deleted []string, upstream []string, err error) {
	var ns []store.Node
	s.st.DB.Where("owner_id = ?", userID).Find(&ns)
	for i := range ns {
		up, e := s.deleteNodeCascade(ns[i].ID)
		if e != nil {
			return deleted, upstream, e
		}
		deleted = append(deleted, ns[i].ID)
		upstream = append(upstream, up...)
	}
	return deleted, upstream, nil
}
