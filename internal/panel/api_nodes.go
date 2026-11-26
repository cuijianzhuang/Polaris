package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

type nodeView struct {
	store.Node
	NetHealth    nodeNetHealth `json:"netHealth"`
	Cores        []store.Core  `json:"cores"`
	InboundCount int64         `json:"inboundCount"`
	Connected    bool          `json:"connected"`
	Progress     *nodeProgress `json:"progress,omitempty"`
	// OwnerName is the display name of the portal user who brought this
	// node ("" for the admin's own nodes).
	OwnerName string `json:"ownerName,omitempty"`
}

func (s *Server) nodeViews() []nodeView {
	var ns []store.Node
	s.st.DB.Order("sort_order, name").Find(&ns)
	var cores []store.Core
	s.st.DB.Order("type").Find(&cores)
	type cnt struct {
		NodeID string
		N      int64
		Core   model.CoreType
	}
	var counts []cnt
	s.st.DB.Model(&store.Inbound{}).Select("node_id, core, count(*) AS n").Group("node_id, core").Scan(&counts)
	// Owner display names for user-brought nodes, one query for all owners.
	ownerNames := map[string]string{}
	{
		seen := map[string]bool{}
		var ids []string
		for _, n := range ns {
			if n.OwnerID != "" && !seen[n.OwnerID] {
				seen[n.OwnerID] = true
				ids = append(ids, n.OwnerID)
			}
		}
		if len(ids) > 0 {
			var us []store.User
			s.st.DB.Select("id, username, email").Where("id IN ?", ids).Find(&us)
			for _, u := range us {
				name := u.Username
				if name == "" {
					name = u.Email
				}
				ownerNames[u.ID] = name
			}
		}
	}
	out := make([]nodeView, 0, len(ns))
	now := time.Now()
	for _, n := range ns {
		v := nodeView{Node: n, NetHealth: nodeNetHealthOf(&n, now), Cores: []store.Core{}, Connected: s.hub.Online(n.ID), Progress: s.nodeProgressFor(n.ID), OwnerName: ownerNames[n.OwnerID]}
		for _, c := range cores {
			if c.NodeID == n.ID {
				for _, k := range counts {
					if k.NodeID == n.ID && k.Core == c.Type {
						c.Inbounds = int(k.N)
					}
				}
				v.Cores = append(v.Cores, c)
			}
		}
		for _, k := range counts {
			if k.NodeID == n.ID {
				v.InboundCount += k.N
			}
		}
		out = append(out, v)
	}
	return out
}

// listNodes returns node views, optionally narrowed by ?owner= (admin =
// the admin's own nodes, user = any user-brought node, anything else = that
// owner's user id) and ?reviewState= (pending / approved / rejected).
func (s *Server) listNodes(c *gin.Context) {
	var vs []nodeView
	if u := memberUserOf(c); u != nil {
		// Members list every node they own, pending and rejected included
		// (design D4's list exception): the list is metadata about their
		// own machines, while every operation path stays limited to the
		// approved scope the guards enforce.
		vs = make([]nodeView, 0)
		for _, v := range s.nodeViews() {
			if v.OwnerID == u.ID {
				vs = append(vs, v)
			}
		}
	} else {
		vs = scopedNodeViews(s.nodeViews(), nodeScope(c))
	}
	owner, review := c.Query("owner"), c.Query("reviewState")
	if owner != "" || review != "" {
		out := make([]nodeView, 0, len(vs))
		for _, v := range vs {
			switch owner {
			case "":
			case "admin":
				if v.OwnerID != "" {
					continue
				}
			case "user":
				if v.OwnerID == "" {
					continue
				}
			default:
				if v.OwnerID != owner {
					continue
				}
			}
			if review != "" && v.ReviewState != review {
				continue
			}
			out = append(out, v)
		}
		vs = out
	}
	c.JSON(http.StatusOK, vs)
}

func (s *Server) getNode(c *gin.Context) {
	for _, v := range scopedNodeViews(s.nodeViews(), nodeScope(c)) {
		if v.ID == c.Param("id") {
			c.JSON(http.StatusOK, v)
			return
		}
	}
	notFound(c)
}

type nodeReq struct {
	Name        string           `json:"name" binding:"required"`
	Address     string           `json:"address"`
	Domain      string           `json:"domain"`
	Region      string           `json:"region"`
	Flag        string           `json:"flag"`
	Lat         *float64         `json:"lat"` // map position; both or neither, null clears
	Lng         *float64         `json:"lng"`
	Labels      []string         `json:"labels"`
	DisplayName *string          `json:"displayName"` // user-owned nodes only: the owner's display name (internal Name is untouched)
	PublicName  *string          `json:"publicName"`  // admin nodes only: name shown on the public service status page; nil = unchanged
	RelayFor    *[]string        `json:"relayFor"`    // absent preserves externally configured relay metadata
	DefaultCore model.CoreType   `json:"defaultCore"`
	TrafficRate float64          `json:"trafficRate"`
	SortOrder   int              `json:"sortOrder"`
	DDNS        bool             `json:"ddns"`
	NoFirewall  bool             `json:"noFirewall"`
	Level       *int             `json:"level"` // tier groups need; nil = unchanged
	Cores       []model.CoreSpec `json:"cores"`
}

// validNodePos checks the optional map coordinates: both or neither, in range.
func validNodePos(lat, lng *float64) error {
	if (lat == nil) != (lng == nil) {
		return fmt.Errorf("纬度和经度需同时填写或同时留空")
	}
	if lat == nil {
		return nil
	}
	if math.IsNaN(*lat) || *lat < -90 || *lat > 90 {
		return fmt.Errorf("纬度应在 -90 到 90 之间")
	}
	if math.IsNaN(*lng) || *lng < -180 || *lng > 180 {
		return fmt.Errorf("经度应在 -180 到 180 之间")
	}
	return nil
}

type nodeRelayValidationError struct{ error }

// Take the shared relay lock before reading nodes. PostgreSQL's advisory lock
// protects panel replicas; SQLite needs its writer reservation before reading
// targets so concurrent deletion cannot leave a dangling reference.
func lockNodeRelayConfiguration(tx *gorm.DB) error {
	if err := lockChainConfiguration(tx); err != nil {
		return err
	}
	if tx.Dialector.Name() == "sqlite" {
		return tx.Exec("UPDATE nodes SET relay_for = relay_for WHERE 1 = 0").Error
	}
	return nil
}

// validateNodeRelayFor only records declared, externally configured targets.
// It never creates forwarding rules or derives client-IP exemptions.
func validateNodeRelayFor(tx *gorm.DB, id string, targets []string) ([]string, error) {
	invalid := func(message string) ([]string, error) {
		return nil, nodeRelayValidationError{errors.New(message)}
	}
	if len(targets) > 64 {
		return invalid("relayFor supports at most 64 targets")
	}
	unique := make(map[string]bool, len(targets))
	normalized := make([]string, 0, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			return invalid("relayFor target must not be empty")
		}
		if target == id {
			return invalid("relayFor cannot target the node itself")
		}
		if !unique[target] {
			unique[target] = true
			normalized = append(normalized, target)
		}
	}
	sort.Strings(normalized)
	if len(normalized) > 0 {
		var ids []string
		if err := tx.Model(&store.Node{}).Where("id IN ?", normalized).Pluck("id", &ids).Error; err != nil {
			return nil, err
		}
		existing := make(map[string]bool, len(ids))
		for _, target := range ids {
			existing[target] = true
		}
		for _, target := range normalized {
			if !existing[target] {
				return invalid("relayFor target does not exist: " + target)
			}
		}
		// User-owned nodes never take part in relays, as either end: the
		// source may not declare relay targets, and no node may declare a
		// user-owned node as its target. (Clearing the list is always fine;
		// a not-yet-created source simply has no row to find.)
		var src store.Node
		if err := tx.First(&src, "id = ?", id).Error; err == nil && src.OwnerID != "" {
			return invalid("用户自带节点不能作为中转来源（relayFor）")
		}
		var owned []store.Node
		if err := tx.Where("id IN ? AND owner_id <> ''", normalized).Find(&owned).Error; err != nil {
			return nil, err
		}
		if len(owned) > 0 {
			return invalid("relayFor 目标不能是用户自带节点：" + nodeDisplayLabel(&owned[0]))
		}
	}
	// Opposing relationships can describe different external listeners; node
	// metadata alone does not establish a forwarding cycle.
	return normalized, nil
}

func nodeRelayChangeError(c *gin.Context, err error) {
	var invalid nodeRelayValidationError
	if errors.As(err, &invalid) {
		badRequest(c, err)
		return
	}
	dbError(c, err)
}

func (s *Server) createNode(c *gin.Context) {
	if u := memberUserOf(c); u != nil {
		s.memberCreateNode(c, u)
		return
	}
	var req nodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := validNodePos(req.Lat, req.Lng); err != nil {
		badRequest(c, err)
		return
	}
	if req.DDNS && !s.requireFeature(c, license.FeatureDDNS) {
		return
	}
	if !s.limitOK(c, license.LimitNodes, 1) {
		return
	}
	if req.TrafficRate <= 0 {
		req.TrafficRate = 1
	}
	if len(req.Cores) == 0 {
		req.Cores = []model.CoreSpec{{Type: model.CoreXray}}
	}
	for i := range req.Cores {
		if req.Cores[i].Version == "" || req.Cores[i].Version == "latest" {
			req.Cores[i].Version = s.defaultCoreVersion(c.Request.Context(), req.Cores[i].Type)
		}
	}
	if req.DefaultCore == "" {
		req.DefaultCore = req.Cores[0].Type
	}
	n := store.Node{ID: store.NewID(), Name: req.Name, Address: req.Address, Domain: req.Domain, Region: req.Region,
		Flag: strings.ToUpper(req.Flag), Lat: req.Lat, Lng: req.Lng, Labels: store.NewJSON(nonNilStrs(req.Labels)), DefaultCore: req.DefaultCore,
		TrafficRate: req.TrafficRate, SortOrder: req.SortOrder, Status: "pending", DDNS: req.DDNS, NoFirewall: req.NoFirewall}
	if req.PublicName != nil {
		n.PublicName = strings.TrimSpace(*req.PublicName)
	}
	if req.Level != nil && *req.Level > 0 && *req.Level <= 99 {
		n.Level = *req.Level
	}
	s.relayMu.Lock()
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockNodeRelayConfiguration(tx); err != nil {
			return err
		}
		targets := []string{}
		if req.RelayFor != nil {
			targets = *req.RelayFor
		}
		targets, err := validateNodeRelayFor(tx, n.ID, targets)
		if err != nil {
			return err
		}
		n.RelayFor = store.NewJSON(targets)
		if err := tx.Create(&n).Error; err != nil {
			return err
		}
		for _, cs := range req.Cores {
			if _, ok := core.Capabilities(cs.Type, cs.Version); !ok {
				return fmt.Errorf("unknown core %s", cs.Type)
			}
			if err := tx.Create(&store.Core{ID: store.NewID(), NodeID: n.ID, Type: cs.Type, Version: cs.Version,
				Status: "installing", IsDefault: cs.Type == req.DefaultCore, InstalledVersions: store.NewJSON([]string{})}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	s.relayMu.Unlock()
	if err != nil {
		nodeRelayChangeError(c, err)
		return
	}
	tok, cmd, err := s.newInstallToken(&n, c.Request)
	if err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "node.create", n.Name, n.Address)
	c.JSON(http.StatusCreated, gin.H{"node": n, "token": tok, "command": cmd})
}

func (s *Server) updateNode(c *gin.Context) {
	if u := memberUserOf(c); u != nil {
		s.memberUpdateNode(c, u)
		return
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req nodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := validNodePos(req.Lat, req.Lng); err != nil {
		badRequest(c, err)
		return
	}
	if req.DDNS && !n.DDNS && !s.requireFeature(c, license.FeatureDDNS) {
		return // turning DDNS on is a paid feature (keeping it on is not)
	}
	addrChanged := n.Address != req.Address || n.Domain != req.Domain
	upd := map[string]any{"name": req.Name, "address": req.Address, "domain": req.Domain, "region": req.Region,
		"flag": strings.ToUpper(req.Flag), "lat": req.Lat, "lng": req.Lng, "labels": store.NewJSON(nonNilStrs(req.Labels)), "sort_order": req.SortOrder, "d_dns": req.DDNS, "no_firewall": req.NoFirewall} // GORM names the DDNS field d_dns
	if req.TrafficRate > 0 {
		upd["traffic_rate"] = req.TrafficRate
	}
	levelChanged := false
	if req.Level != nil {
		if *req.Level < 0 || *req.Level > 99 {
			badRequest(c, fmt.Errorf("等级应在 0–99 之间"))
			return
		}
		levelChanged = *req.Level != n.Level
		upd["level"] = *req.Level
	}
	if req.DefaultCore != "" {
		upd["default_core"] = req.DefaultCore
	}
	firewallChanged := false
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockNodeRelayConfiguration(tx); err != nil {
			return err
		}
		if err := tx.First(&n, "id = ?", c.Param("id")).Error; err != nil {
			return err
		}
		addrChanged = n.Address != req.Address || n.Domain != req.Domain
		firewallChanged = n.NoFirewall != req.NoFirewall
		levelChanged = req.Level != nil && *req.Level != n.Level
		if req.RelayFor != nil {
			targets, err := validateNodeRelayFor(tx, n.ID, *req.RelayFor)
			if err != nil {
				return err
			}
			upd["relay_for"] = store.NewJSON(targets)
		}
		if req.DisplayName != nil && n.OwnerID != "" {
			// A user node's visible name lives in DisplayName; its internal
			// Name (un-<id>) is an implementation detail and never edited.
			upd["display_name"] = strings.TrimSpace(*req.DisplayName)
		}
		if req.PublicName != nil && n.OwnerID == "" {
			// Only admin nodes appear on the public service status page,
			// so only they carry a public alias.
			upd["public_name"] = strings.TrimSpace(*req.PublicName)
		}
		if req.DefaultCore != "" {
			if err := tx.Model(&store.Core{}).Where("node_id = ?", n.ID).Update("is_default", gorm.Expr("type = ?", req.DefaultCore)).Error; err != nil {
				return err
			}
		}
		return tx.Model(&n).Updates(upd).Error
	})
	if err != nil {
		nodeRelayChangeError(c, err)
		return
	}
	if firewallChanged {
		s.MarkDirty("firewall management changed", n.ID)
	}
	if addrChanged {
		s.dirtyRelayPaths(n.ID)
		// Upstream nodes reference this node's address in their outbounds/forwards.
		var ids []string
		s.st.DB.Model(&store.Inbound{}).Where("node_id = ?", n.ID).Pluck("id", &ids)
		s.MarkDirty("node address changed", s.nodesForInbounds(ids...)...)
	}
	if levelChanged {
		// Who may use this node changed: its users and its relays' upstreams.
		var ids []string
		s.st.DB.Model(&store.Inbound{}).Where("node_id = ?", n.ID).Pluck("id", &ids)
		s.MarkDirty("node level changed", s.nodesForInbounds(ids...)...)
	}
	s.MarkDirty("node updated", n.ID)
	s.svcCache.Store(nil) // a rename or public alias applies to the service page at once
	s.auditCtx(c, "node.update", req.Name, "")
	c.JSON(http.StatusOK, n)
}

type nodeInUseError struct{ refs int64 }

func (e *nodeInUseError) Error() string { return "node inbounds still referenced" }

type nodeRelayInUseError struct{ names []string }

func (e *nodeRelayInUseError) Error() string {
	return "node is still referenced by relayFor on: " + strings.Join(e.names, ", ") + "; remove those relay targets before deleting it"
}

func (s *Server) deleteNode(c *gin.Context) {
	if u := memberUserOf(c); u != nil {
		s.memberDeleteNode(c, u)
		return
	}
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	id := c.Param("id")
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", id).Error; err != nil {
		dbError(c, err)
		return
	}
	upstream, err := s.deleteNodeCascade(id)
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
	if err != nil {
		serverError(c, err)
		return
	}
	s.hub.Kick(id)
	s.MarkDirty("upstream node deleted", upstream...)
	s.auditCtx(c, "node.delete", n.Name, "")
	c.Status(http.StatusNoContent)
}

// deleteNodeCascade removes a node and everything that hangs off it
// (cores, inbounds, bindings, metrics, revisions…). It refuses while
// other nodes still reference the node — relay targets or outbound
// refs — with the typed errors the HTTP handlers map to 409s, and returns
// the other nodes whose specs the deletion affects. Callers hold relayMu
// and do the post-commit work (hub kick, MarkDirty, audit) themselves.
//
// The affected-node lookup runs before the transaction on purpose:
// nodesForInbounds reads through the root DB handle, which would
// deadlock against the transaction's connection on SQLite.
func (s *Server) deleteNodeCascade(id string) ([]string, error) {
	var inIDs []string
	s.st.DB.Model(&store.Inbound{}).Where("node_id = ?", id).Pluck("id", &inIDs)
	upstream := s.nodesForInbounds(inIDs...)
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		// Lock relay configuration before the node row, matching node updates
		// across replicas. Retain withNodeNet's gate for concurrent NIC writes.
		if err := lockNodeRelayConfiguration(tx); err != nil {
			return err
		}
		if err := tx.Exec("UPDATE nodes SET net_last_rx=net_last_rx WHERE id = ?", id).Error; err != nil {
			return err
		}
		var n store.Node
		if err := tx.First(&n, "id = ?", id).Error; err != nil {
			return err
		}
		var nodes []store.Node
		if err := tx.Select("id", "name", "relay_for").Where("id <> ?", id).Order("name").Find(&nodes).Error; err != nil {
			return err
		}
		var names []string
		for _, node := range nodes {
			for _, target := range node.RelayFor.V {
				if target == id {
					names = append(names, node.Name)
					break
				}
			}
		}
		if len(names) > 0 {
			return &nodeRelayInUseError{names}
		}
		extra, e := s.cleanupManagedRelays(tx, "", id)
		if e != nil {
			return e
		}
		upstream = append(upstream, extra...)
		if len(inIDs) > 0 {
			// Outbounds elsewhere pointing at this node's inbounds would leave
			// those nodes unable to render (as deleting an inbound refuses).
			var refs int64
			if err := tx.Model(&store.Outbound{}).Where("node_ref_inbound_id IN ? AND node_id <> ?", inIDs, id).Count(&refs).Error; err != nil {
				return err
			}
			if refs > 0 {
				return &nodeInUseError{refs}
			}
		}

		for _, m := range []any{&store.Website{}, &store.Core{}, &store.Inbound{}, &store.Outbound{}, &store.RoutingRule{}, &store.ConfigRevision{}, &store.ForwardRule{}, &store.NodeNetBoot{}, &store.NotificationState{}, &store.NodeResourceAlert{}, &store.NodeRouteTrace{}, &store.NodeRouteSchedule{}, &store.NodeRouteScheduleState{}} {
			if err := tx.Where("node_id = ?", id).Delete(m).Error; err != nil {
				return err
			}
		}
		tx.Where("kind = ? AND target_id = ?", "node", id).Delete(&store.UserAccess{})
		tx.Where("node_id = ?", id).Delete(&store.GroupNode{})
		tx.Where("node_id = ?", id).Delete(&store.NodeCost{})
		tx.Where("node_id = ?", id).Delete(&store.NodeProvider{})
		tx.Where("node_id = ?", id).Delete(&store.NodeLatency{})
		tx.Where("node_id = ?", id).Delete(&store.NodeExternalMetric{})
		tx.Where("node_id = ?", id).Delete(&store.UserNodeQuota{})
		tx.Where("node_id = ?", id).Delete(&store.TrafficCursor{})
		tx.Where("node_id = ?", id).Delete(&store.TrafficBatchSeen{})
		if err := tx.Where("key = ?", "agent-traffic:"+id).Delete(&store.OperationalStatus{}).Error; err != nil {
			return err
		}
		if len(inIDs) > 0 {
			tx.Where("kind = ? AND target_id IN ?", "inbound", inIDs).Delete(&store.UserAccess{})
			tx.Where("inbound_id IN ?", inIDs).Delete(&store.GroupInbound{})
			tx.Where("inbound_id IN ?", inIDs).Delete(&store.Endpoint{})
		}
		return tx.Delete(&store.Node{}, "id = ?", id).Error
	})
	if err != nil {
		return nil, err
	}
	return upstream, nil
}

// agentURL is the address agents dial.
func (s *Server) agentURL() string {
	_, port, err := net.SplitHostPort(s.cfg.AgentListen)
	if err != nil {
		port = "9443"
	}
	host := ""
	if len(s.cfg.AgentHosts) > 0 {
		host = s.cfg.AgentHosts[0]
	} else if u, err := url.Parse(s.cfg.PublicURL); err == nil {
		host = u.Hostname()
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return "https://" + net.JoinHostPort(host, port)
}

func (s *Server) newInstallToken(n *store.Node, r *http.Request) (string, string, error) {
	// 16 alphanumerics (~95 bits, one-time, 24 h) keep the one-click link short.
	tok := pki.Password(16)
	exp := time.Now().UTC().Add(24 * time.Hour)
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{
		"token_hash": hashToken(tok), "token_expires": exp}).Error; err != nil {
		return "", "", err
	}
	return tok, s.joinCommand(r, tok), nil
}

// joinURL is the one-click link: /j/<token> serves install.sh with the token
// filled in, so a node needs nothing but `curl … | bash`.
func (s *Server) joinURL(r *http.Request, tok string) string { return s.requestBase(r) + "/j/" + tok }

func (s *Server) joinCommand(r *http.Request, tok string) string {
	return "curl -fsSL " + s.joinURL(r, tok) + " | bash"
}

func (s *Server) installToken(c *gin.Context) {
	if u := memberUserOf(c); u != nil {
		s.memberInstallToken(c, u)
		return
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	tok, cmd, err := s.newInstallToken(&n, c.Request)
	if err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "node.install_token", n.Name, "")
	c.JSON(http.StatusOK, gin.H{"token": tok, "command": cmd, "url": s.joinURL(c.Request, tok),
		"wget":      "wget -qO- " + s.joinURL(c.Request, tok) + " | bash",
		"long":      fmt.Sprintf("curl -fsSL %s/install.sh | bash -s -- --token %s", s.requestBase(c.Request), tok),
		"expiresIn": 86400})
}

func (s *Server) syncNode(c *gin.Context) {
	rev, changed, err := s.Publish(c.Param("id"), "manual sync", true)
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "node.sync", c.Param("id"), "rev "+strconv.FormatInt(rev, 10))
	c.JSON(http.StatusOK, gin.H{"rev": rev, "changed": changed})
}

// rollbackNode republishes an earlier revision's spec as a new revision.
func (s *Server) rollbackNode(c *gin.Context) {
	var req struct {
		Rev int64 `json:"rev"`
	}
	_ = c.ShouldBindJSON(&req)
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	target := req.Rev
	rev, err := s.rollbackTo(&n, target)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	s.auditCtx(c, "node.rollback", n.Name, fmt.Sprintf("-> rev %d", rev))
	c.JSON(http.StatusOK, gin.H{"rev": rev})
}

// rollbackTo republishes revision target (0 = the previous applied one) as a new revision.
func (s *Server) rollbackTo(n *store.Node, target int64) (int64, error) {
	if target == 0 {
		var r store.ConfigRevision
		err := s.st.DB.Where("node_id = ? AND rev < ? AND status = ?", n.ID, n.AppliedRev, "applied").Order("rev desc").First(&r).Error
		if err != nil {
			return 0, fmt.Errorf("没有可回滚的已应用版本")
		}
		target = r.Rev
	}
	spec, err := s.loadRevision(n.ID, target)
	if err != nil {
		return 0, err
	}
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	rev, _, err := s.storeRevision(n, spec, fmt.Sprintf("rollback to rev %d", target))
	return rev, err
}

// revokeNode invalidates the node's client certificate.
func (s *Server) revokeNode(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if err := s.st.DB.Model(&n).Updates(map[string]any{"revoked": true, "cert_serial": "", "status": "offline"}).Error; err != nil {
		dbError(c, err) // not revoked: keep the agent connected rather than report a revocation that did not happen
		return
	}
	s.hub.Kick(n.ID)
	// Chains through this node stop forwarding: rebuild the other hops.
	var chains []store.Chain
	s.st.DB.Find(&chains)
	for i := range chains {
		for _, h := range chains[i].Hops.V {
			if h.NodeID == n.ID {
				s.MarkDirty("relay node revoked", chainNodes(&chains[i])...)
				break
			}
		}
	}
	s.auditCtx(c, "node.revoke", n.Name, "")
	c.Status(http.StatusNoContent)
}

func (s *Server) listRevisions(c *gin.Context) {
	var rs []store.ConfigRevision
	s.st.DB.Where("node_id = ?", c.Param("id")).Order("rev desc").Limit(50).Find(&rs)
	c.JSON(http.StatusOK, rs)
}

// nodeSpec returns the spec that would be published now, with secrets redacted.
func (s *Server) nodeSpec(c *gin.Context) {
	spec, err := s.BuildSpec(c.Param("id"))
	if err != nil {
		dbError(c, err)
		return
	}
	redactSpec(spec)
	c.JSON(http.StatusOK, spec)
}

// renderNodeCore returns the native config one core would run (?core=xray).
func (s *Server) renderNodeCore(c *gin.Context) {
	spec, err := s.BuildSpec(c.Param("id"))
	if err != nil {
		dbError(c, err)
		return
	}
	t := model.CoreType(c.Query("core"))
	version := ""
	for _, cs := range spec.Cores {
		if cs.Type == t {
			version = cs.Version
		}
	}
	ad, err := core.New(t, core.Options{Version: version, Limit: outboundIPPreviewWire()})
	if err != nil {
		badRequest(c, err)
		return
	}
	cfg, err := ad.Render(spec)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"core": t, "version": version, "config": string(cfg)})
}

func redactSpec(spec *model.NodeSpec) {
	for i := range spec.Outbounds {
		o := &spec.Outbounds[i]
		set, e := o.ParseSettings()
		if e != nil {
			continue
		}
		for _, v := range []*string{&set.Username, &set.Password, &set.UUID, &set.PrivateKey, &set.PreSharedKey, &set.ObfsPassword, &set.Encryption} {
			if *v != "" {
				*v = "<redacted>"
			}
		}
		if len(o.Settings) > 0 {
			o.Settings, _ = json.Marshal(set)
		}
	}

	for i := range spec.Inbounds {
		in := &spec.Inbounds[i]
		if in.Security.KeyPEM != "" {
			in.Security.KeyPEM = "<redacted>"
		}
		if in.Security.CertPEM != "" {
			in.Security.CertPEM = "<certificate>"
		}
		if in.Security.RealityPrivateKey != "" {
			in.Security.RealityPrivateKey = "<redacted>"
		}
		if in.Protocol.ServerKey != "" {
			in.Protocol.ServerKey = "<redacted>"
		}
		if in.Protocol.Decryption != "" {
			in.Protocol.Decryption = "<redacted>"
		}
		if in.Security.RealityMldsa65Seed != "" {
			in.Security.RealityMldsa65Seed = "<redacted>"
		}
		for j := range in.Users {
			in.Users[j].Password, in.Users[j].SSSeed = "<redacted>", ""
		}
	}
}

func (s *Server) listCores(c *gin.Context) {
	cs, err := s.nodeCores(c.Param("id"))
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, cs)
}

// upsertCore installs a core or switches its version. Installation itself is
// declarative: the new spec lists the core and the agent converges.
func (s *Server) upsertCore(c *gin.Context) {
	var req struct {
		Type      model.CoreType `json:"type" binding:"required"`
		Version   string         `json:"version" binding:"required"`
		IsDefault bool           `json:"isDefault"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	cur, code, err := s.setCoreVersion(c.Param("id"), req.Type, req.Version, req.IsDefault)
	if err != nil {
		c.JSON(code, gin.H{"error": err.Error()})
		return
	}
	s.auditCtx(c, "core.install", c.Param("id"), fmt.Sprintf("%s %s", req.Type, req.Version))
	c.JSON(http.StatusOK, cur)
}

func (s *Server) deleteCore(c *gin.Context) {
	var cur store.Core
	if err := s.st.DB.First(&cur, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var n int64
	if cur.Type == model.CoreCloudflared {
		if err := s.st.DB.Model(&store.CDNProfile{}).Where("tunnel_node_id = ? AND tunnel_managed = ?", cur.NodeID, true).Count(&n).Error; err != nil {
			dbError(c, err)
			return
		}
		if n > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "请先在 CDN 配置档停用 Agent 托管隧道"})
			return
		}
	}
	s.st.DB.Model(&store.Inbound{}).Where("node_id = ? AND core = ?", cur.NodeID, cur.Type).Count(&n)
	if n > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("该内核仍承载 %d 个入站", n)})
		return
	}
	s.st.DB.Delete(&cur)
	s.MarkDirty("core removed", cur.NodeID)
	s.auditCtx(c, "core.delete", cur.NodeID, string(cur.Type))
	c.Status(http.StatusNoContent)
}

func (s *Server) restartCore(c *gin.Context) {
	var cur store.Core
	if err := s.st.DB.First(&cur, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	res, err := s.hub.Command(c, cur.NodeID, "restart_core", map[string]string{"type": string(cur.Type)}, 30*time.Second)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	s.auditCtx(c, "core.restart", cur.NodeID, string(cur.Type))
	if !res.Ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": res.Error})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// rebootNode schedules a reboot of the node's host machine (not just a
// core). The agent acks as soon as the reboot is scheduled — it fires a
// few seconds later — so success here means "will reboot"; the node
// dropping offline and reconnecting minutes later is the completion
// signal. The route is deliberately absent from the operator and member
// whitelists (scopedRoutes / memberRoutes), matching upgrade-agent:
// whole-host disruption stays with full administrators. No sudo step:
// in this codebase sudo guards credential-level operations (admin
// accounts, passkeys, provider secrets), not node operations.
func (s *Server) rebootNode(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	res, err := s.hub.Command(c, n.ID, "reboot_host", nil, 15*time.Second)
	if err != nil {
		// An offline (or unresponsive) node never receives the command;
		// say that plainly instead of leaking the hub's timeout text.
		c.JSON(http.StatusBadGateway, gin.H{"error": "节点离线或未响应，无法下发重启命令"})
		return
	}
	s.auditCtx(c, "node.reboot", n.Name, "")
	if !res.Ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": res.Error})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) coreLogs(c *gin.Context) {
	var cur store.Core
	if err := s.st.DB.First(&cur, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	lines := c.DefaultQuery("lines", "200")
	res, err := s.hub.Command(c, cur.NodeID, "core_logs", map[string]string{"type": string(cur.Type), "lines": lines}, 10*time.Second)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": res.Ok, "error": res.Error, "logs": res.Output})
}

// capabilities returns the matrix for ?core=&version=, or for every core
// installed on ?node=. Without parameters it lists all registered cores.
func (s *Server) capabilities(c *gin.Context) {
	if nodeID := c.Query("node"); nodeID != "" {
		cs, err := s.nodeCores(nodeID)
		if err != nil {
			serverError(c, err)
			return
		}
		out := []model.CapSet{}
		for _, x := range cs {
			if caps, ok := core.Capabilities(x.Type, x.Version); ok {
				out = append(out, caps)
			}
		}
		c.JSON(http.StatusOK, out)
		return
	}
	if t := c.Query("core"); t != "" {
		caps, ok := core.Capabilities(model.CoreType(t), c.Query("version"))
		if !ok {
			notFound(c)
			return
		}
		c.JSON(http.StatusOK, caps)
		return
	}
	defaults := map[model.CoreType]string{}
	for t, v := range fallbackVersions {
		defaults[t] = v
	}
	for t, k := range coreDefaultKeys {
		if v := s.st.GetSetting(k, ""); v != "" && v != "latest" {
			defaults[t] = v
		} else {
			defaults[t] = "" // newest: every capability
		}
	}
	out := []model.CapSet{}
	for _, t := range core.Types() {
		caps, _ := core.Capabilities(t, defaults[t])
		out = append(out, caps)
	}
	c.JSON(http.StatusOK, out)
}

func nonNilStrs(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
