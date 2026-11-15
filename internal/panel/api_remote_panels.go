package panel

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

// maxCommunityPanels caps read-only panels when the multi_panel feature is
// not granted (community build or a license without it). Pro licenses lift
// the cap; write actions are compiled from polaris-pro.
const maxCommunityPanels = 3

// listRemotePanels returns the registry with health and snapshot freshness.
func (s *Server) listRemotePanels(c *gin.Context) {
	var panels []store.RemotePanel
	if err := s.st.DB.Order("name").Find(&panels).Error; err != nil {
		dbError(c, err)
		return
	}

	type panelView struct {
		store.RemotePanel
		Stale      bool   `json:"stale"`
		AgeSeconds int    `json:"ageSeconds"`
		Health     string `json:"health"` // ok / stale / failed / never
	}

	now := time.Now()
	out := make([]panelView, len(panels))
	for i, p := range panels {
		v := panelView{RemotePanel: p}
		if p.SnapshotAt != nil {
			age := now.Sub(*p.SnapshotAt)
			v.AgeSeconds = int(age.Seconds())
			v.Stale = age > 5*time.Minute
		}
		switch {
		case p.Failures > 0:
			v.Health = "failed"
		case p.LastOKAt == nil:
			v.Health = "never"
		case v.Stale:
			v.Health = "stale"
		default:
			v.Health = "ok"
		}
		out[i] = v
	}

	c.JSON(http.StatusOK, gin.H{"panels": out, "pro": s.license().Allow(license.FeatureMultiPanel), "cap": maxCommunityPanels})
}

// createRemotePanel registers a new member panel.
func (s *Server) createRemotePanel(c *gin.Context) {
	var req struct {
		Name     string   `json:"name" binding:"required"`
		BaseURL  string   `json:"baseUrl" binding:"required"`
		Token    string   `json:"token" binding:"required"`
		Scopes   []string `json:"scopes"`
		CAPin    string   `json:"caPin"`
		Insecure bool     `json:"insecure"`
		Labels   []string `json:"labels"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}

	if err := validateRemotePanelURL(req.BaseURL); err != nil {
		badRequest(c, errors.New("invalid baseUrl: "+err.Error()))
		return
	}

	// Without the multi_panel feature (community build or a license that
	// does not grant it), the registry is capped at read-only panels.
	if !s.license().Allow(license.FeatureMultiPanel) {
		var count int64
		if err := s.st.DB.Model(&store.RemotePanel{}).Count(&count).Error; err != nil {
			dbError(c, err)
			return
		}
		if count >= maxCommunityPanels {
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "社区版最多登记 3 台面板", "feature": license.FeatureMultiPanel, "plan": "pro"})
			return
		}
	}

	p := store.RemotePanel{
		ID:        store.NewID(),
		Name:      req.Name,
		BaseURL:   req.BaseURL,
		Token:     s.st.Box.Seal(req.Token),
		Scopes:    store.JSON[[]string]{V: req.Scopes},
		CAPin:     req.CAPin,
		Insecure:  req.Insecure,
		Labels:    store.JSON[[]string]{V: req.Labels},
		Enabled:   true,
		CreatedBy: c.GetString("admin"),
	}

	if err := s.st.DB.Create(&p).Error; err != nil {
		dbError(c, err)
		return
	}

	s.auditCtx(c, "panel.create", p.ID, p.Name)
	c.JSON(http.StatusCreated, gin.H{"id": p.ID, "token": req.Token}) // echo the token once
}

// updateRemotePanel changes name, URL, token, or cert pin.
func (s *Server) updateRemotePanel(c *gin.Context) {
	id := c.Param("id")
	var p store.RemotePanel
	if err := s.st.DB.First(&p, "id = ?", id).Error; err != nil {
		dbError(c, err)
		return
	}

	var req struct {
		Name     *string  `json:"name"`
		BaseURL  *string  `json:"baseUrl"`
		Token    *string  `json:"token"`
		Scopes   []string `json:"scopes"`
		CAPin    *string  `json:"caPin"`
		Insecure *bool    `json:"insecure"`
		Labels   []string `json:"labels"`
		Enabled  *bool    `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}

	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.BaseURL != nil {
		if err := validateRemotePanelURL(*req.BaseURL); err != nil {
			badRequest(c, errors.New("invalid baseUrl: "+err.Error()))
			return
		}
		p.BaseURL = *req.BaseURL
	}
	if req.Token != nil {
		p.Token = s.st.Box.Seal(*req.Token)
	}
	if req.Scopes != nil {
		p.Scopes = store.JSON[[]string]{V: req.Scopes}
	}
	if req.CAPin != nil {
		p.CAPin = *req.CAPin
	}
	if req.Insecure != nil {
		p.Insecure = *req.Insecure
	}
	if req.Labels != nil {
		p.Labels = store.JSON[[]string]{V: req.Labels}
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}

	if err := s.st.DB.Save(&p).Error; err != nil {
		dbError(c, err)
		return
	}

	s.auditCtx(c, "panel.update", p.ID, p.Name)
	c.JSON(http.StatusOK, gin.H{"id": p.ID})
}

// deleteRemotePanel removes the hub-side registration only; the member panel
// and its data are untouched.
func (s *Server) deleteRemotePanel(c *gin.Context) {
	id := c.Param("id")
	var p store.RemotePanel
	if err := s.st.DB.First(&p, "id = ?", id).Error; err != nil {
		dbError(c, err)
		return
	}

	if err := s.st.DB.Delete(&p).Error; err != nil {
		dbError(c, err)
		return
	}

	s.auditCtx(c, "panel.delete", p.ID, p.Name)
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// probeRemotePanel triggers an immediate fetch and reports the result. It
// does not change the background backoff state.
func (s *Server) probeRemotePanel(c *gin.Context) {
	id := c.Param("id")
	var p store.RemotePanel
	if err := s.st.DB.First(&p, "id = ?", id).Error; err != nil {
		dbError(c, err)
		return
	}

	token, err := s.st.Box.Open(p.Token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt token"})
		return
	}

	cli := &remotePanelClient{
		baseURL:  p.BaseURL,
		token:    token,
		caPin:    p.CAPin,
		insecure: p.Insecure,
		timeout:  remotePanelTimeout,
	}

	snap, err := cli.fetchSnapshot(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": classifyRemotePanelError(err)})
		return
	}

	c.JSON(http.StatusOK, snap)
}

// aggregateRemotePanels returns cross-panel sums and per-panel rows. Traffic
// is always the calendar month, so summing across members is consistent.
func (s *Server) aggregateRemotePanels(c *gin.Context) {
	var panels []store.RemotePanel
	if err := s.st.DB.Where("enabled = ?", true).Order("name").Find(&panels).Error; err != nil {
		dbError(c, err)
		return
	}

	type row struct {
		Name        string `json:"name"`
		Users       int    `json:"users"`
		OnlineUsers int    `json:"onlineUsers"`
		Nodes       int    `json:"nodes"`
		NodesOnline int    `json:"nodesOnline"`
		TrafficUp   int64  `json:"trafficUp"`
		TrafficDown int64  `json:"trafficDown"`
		Stale       bool   `json:"stale"`
		Failed      bool   `json:"failed"`
		AgeSeconds  int    `json:"ageSeconds"`
	}

	var totalUsers, totalOnline, totalNodes, totalNodesOnline int
	var totalUp, totalDown int64

	now := time.Now()
	rows := make([]row, 0, len(panels))

	for _, p := range panels {
		if p.SnapshotAt == nil || p.Failures > 0 {
			rows = append(rows, row{Name: p.Name, Failed: true})
			continue
		}

		age := now.Sub(*p.SnapshotAt)
		snap := p.Snapshot.V
		r := row{
			Name:        p.Name,
			Users:       snap.Users,
			OnlineUsers: snap.OnlineUsers,
			Nodes:       snap.Nodes,
			NodesOnline: snap.NodesOnline,
			TrafficUp:   snap.TrafficUp,
			TrafficDown: snap.TrafficDown,
			Stale:       age > 5*time.Minute,
			AgeSeconds:  int(age.Seconds()),
		}
		rows = append(rows, r)

		totalUsers += snap.Users
		totalOnline += snap.OnlineUsers
		totalNodes += snap.Nodes
		totalNodesOnline += snap.NodesOnline
		totalUp += snap.TrafficUp
		totalDown += snap.TrafficDown
	}

	c.JSON(http.StatusOK, gin.H{
		"totalUsers":       totalUsers,
		"totalOnline":      totalOnline,
		"totalNodes":       totalNodes,
		"totalNodesOnline": totalNodesOnline,
		"totalTrafficUp":   totalUp,
		"totalTrafficDown": totalDown,
		"panels":           rows,
	})
}

// validateRemotePanelURL checks the scheme and host statically. Reaching
// link-local or metadata addresses is blocked again at dial time (see
// ssrfDialer), where DNS rebinding cannot bypass it.
func validateRemotePanelURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("only https and http are allowed")
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}
