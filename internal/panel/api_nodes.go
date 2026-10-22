package panel

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
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
	out := make([]nodeView, 0, len(ns))
	now := time.Now()
	for _, n := range ns {
		v := nodeView{Node: n, NetHealth: nodeNetHealthOf(&n, now), Cores: []store.Core{}, Connected: s.hub.Online(n.ID), Progress: s.nodeProgressFor(n.ID)}
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

func (s *Server) listNodes(c *gin.Context) {
	c.JSON(http.StatusOK, scopedNodeViews(s.nodeViews(), nodeScope(c)))
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

func (s *Server) createNode(c *gin.Context) {
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
	if req.Level != nil && *req.Level > 0 && *req.Level <= 99 {
		n.Level = *req.Level
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
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
	if err != nil {
		dbError(c, err)
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
		s.st.DB.Model(&store.Core{}).Where("node_id = ?", n.ID).Update("is_default", gorm.Expr("type = ?", req.DefaultCore))
	}
	if err := s.st.DB.Model(&n).Updates(upd).Error; err != nil {
		dbError(c, err)
		return
	}
	if n.NoFirewall != req.NoFirewall {
		s.MarkDirty("firewall management changed", n.ID)
	}
	if addrChanged {
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
	s.auditCtx(c, "node.update", req.Name, "")
	c.JSON(http.StatusOK, n)
}

func (s *Server) deleteNode(c *gin.Context) {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	id := c.Param("id")
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", id).Error; err != nil {
		dbError(c, err)
		return
	}
	var inIDs []string
	s.st.DB.Model(&store.Inbound{}).Where("node_id = ?", id).Pluck("id", &inIDs)
	upstream := s.nodesForInbounds(inIDs...)
	err := s.withNodeNet(id, func(tx *gorm.DB, _ *store.Node) error {
		extra, e := s.cleanupManagedRelays(tx, "", id)
		if e != nil {
			return e
		}
		upstream = append(upstream, extra...)

		for _, m := range []any{&store.Core{}, &store.Inbound{}, &store.Outbound{}, &store.RoutingRule{}, &store.ConfigRevision{}, &store.ForwardRule{}, &store.NodeNetBoot{}} {
			if err := tx.Where("node_id = ?", id).Delete(m).Error; err != nil {
				return err
			}
		}
		tx.Where("kind = ? AND target_id = ?", "node", id).Delete(&store.UserAccess{})
		tx.Where("node_id = ?", id).Delete(&store.GroupNode{})
		tx.Where("node_id = ?", id).Delete(&store.NodeCost{})
		tx.Where("node_id = ?", id).Delete(&store.NodeProvider{})
		tx.Where("node_id = ?", id).Delete(&store.NodeLatency{})
		tx.Where("node_id = ?", id).Delete(&store.UserNodeQuota{})
		tx.Where("node_id = ?", id).Delete(&store.TrafficCursor{})
		tx.Where("node_id = ?", id).Delete(&store.TrafficBatchSeen{})
		if len(inIDs) > 0 {
			tx.Where("kind = ? AND target_id IN ?", "inbound", inIDs).Delete(&store.UserAccess{})
			tx.Where("inbound_id IN ?", inIDs).Delete(&store.GroupInbound{})
			tx.Where("inbound_id IN ?", inIDs).Delete(&store.Endpoint{})
		}
		return tx.Delete(&store.Node{}, "id = ?", id).Error
	})
	if err != nil {
		serverError(c, err)
		return
	}
	s.hub.Kick(id)
	s.MarkDirty("upstream node deleted", upstream...)
	s.auditCtx(c, "node.delete", n.Name, "")
	c.Status(http.StatusNoContent)
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
	exp := time.Now().Add(24 * time.Hour)
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
	s.st.DB.Model(&n).Updates(map[string]any{"revoked": true, "cert_serial": "", "status": "offline"})
	s.hub.Kick(n.ID)
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
	ad, err := core.New(t, core.Options{Version: version})
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
