package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/provider"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Hosting provider APIs: a node can carry its BandwagonHost (KiwiVM) VEID and
// API key. The leader reads the plan and the month's traffic every 15
// minutes; with Sync on, the node's traffic allowance, reset day and cycle
// total follow the provider's figures (which is what the provider bills),
// and the usual traffic alerts fire on them. Only read-only API calls are
// made.

const providerBandwagon = "bandwagon"

type providerView struct {
	store.NodeProvider
	KeySet bool           `json:"keySet"`
	Info   *provider.Info `json:"info,omitempty"`
}

func (s *Server) providerClient(p *store.NodeProvider) *provider.BWH {
	return &provider.BWH{VEID: p.VEID, APIKey: s.st.Box.MustOpen(p.APIKey)}
}

func providerViewOf(p *store.NodeProvider) providerView {
	v := providerView{NodeProvider: *p, KeySet: p.APIKey != ""}
	if p.Info != "" {
		var in provider.Info
		if json.Unmarshal([]byte(p.Info), &in) == nil {
			v.Info = &in
		}
	}
	return v
}

// syncProvider reads one node's provider and applies it.
func (s *Server) syncProvider(ctx context.Context, p *store.NodeProvider, live bool) (*provider.Info, error) {
	cl := s.providerClient(p)
	var in *provider.Info
	var err error
	if live {
		in, err = cl.LiveInfo(ctx)
	} else {
		in, err = cl.ServiceInfo(ctx)
	}
	now := time.Now()
	if err != nil {
		s.st.DB.Model(&store.NodeProvider{}).Where("node_id = ?", p.NodeID).Updates(map[string]any{"last_error": truncate(err.Error(), 255), "last_sync": now})
		return nil, err
	}
	raw, _ := json.Marshal(in)
	s.st.DB.Model(&store.NodeProvider{}).Where("node_id = ?", p.NodeID).Updates(map[string]any{"info": string(raw), "last_error": "", "last_sync": now})
	if p.Sync {
		s.calibrateNodeNet(p.NodeID, in, now)
	}
	return in, nil
}

// calibrateNodeNet makes the node's traffic cycle match the provider's.
func (s *Server) calibrateNodeNet(nodeID string, in *provider.Info, now time.Time) {
	var n store.Node
	if s.st.DB.First(&n, "id = ?", nodeID).Error != nil {
		return
	}
	upd := map[string]any{}
	if in.DataLimit > 0 && in.DataLimit != n.NetLimit {
		n.NetLimit = in.DataLimit
		upd["net_limit"] = n.NetLimit
	}
	if !in.NextReset.IsZero() {
		day := in.NextReset.In(now.Location()).Day()
		if day != n.NetResetDay {
			n.NetResetDay = day
			upd["net_reset_day"] = day
			upd["net_cycle_start"] = cycleStart(day, now)
		}
	}
	n.NetOffset = 0
	n.NetOffset = in.DataUsed - netUsed(&n)
	upd["net_offset"] = n.NetOffset
	lvl := netAlertLevel(&n)
	switch {
	case lvl > n.NetWarned:
		upd["net_warned"] = lvl
		s.notify(Event{Kind: "node_traffic", NodeID: n.ID, Text: netAlertText(&n, lvl, now)})
	case lvl < n.NetWarned:
		upd["net_warned"] = lvl // a new cycle at the provider
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(upd)
}

// syncProviders runs for every configured node (leader, every 15 minutes).
func (s *Server) syncProviders() {
	var ps []store.NodeProvider
	s.st.DB.Find(&ps)
	for i := range ps {
		ctx, cancel := context.WithTimeout(s.ctxBG(), 40*time.Second)
		if _, err := s.syncProvider(ctx, &ps[i], false); err != nil {
			s.log.Warn("provider sync", "node", ps[i].NodeID, "err", err)
		}
		cancel()
	}
}

// --- API ---

// getNodeProvider is GET /nodes/:id/provider.
func (s *Server) getNodeProvider(c *gin.Context) {
	var p store.NodeProvider
	if s.st.DB.First(&p, "node_id = ?", c.Param("id")).Error != nil {
		c.JSON(http.StatusOK, gin.H{"configured": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"configured": true, "provider": providerViewOf(&p)})
}

// putNodeProvider is PUT /nodes/:id/provider {kind, veid, apiKey, sync}; an
// empty apiKey keeps the saved one. The settings are checked against the
// API before they are saved.
func (s *Server) putNodeProvider(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req struct {
		Kind   string `json:"kind"`
		VEID   string `json:"veid"`
		APIKey string `json:"apiKey"`
		Sync   bool   `json:"sync"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Kind == "" {
		req.Kind = providerBandwagon
	}
	if req.Kind != providerBandwagon {
		badRequest(c, errors.New("目前只支持搬瓦工（KiwiVM）"))
		return
	}
	var p store.NodeProvider
	s.st.DB.First(&p, "node_id = ?", n.ID)
	p.NodeID, p.Kind, p.VEID, p.Sync = n.ID, req.Kind, strings.TrimSpace(req.VEID), req.Sync
	if k := strings.TrimSpace(req.APIKey); k != "" {
		p.APIKey = s.st.Box.Seal(k)
	}
	if p.VEID == "" || p.APIKey == "" {
		badRequest(c, errors.New("请填写 VEID 和 API Key"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if _, err := s.providerClient(&p).ServiceInfo(ctx); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.st.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&p).Error; err != nil {
		dbError(c, err)
		return
	}
	if _, err := s.syncProvider(ctx, &p, false); err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "node.provider", n.Name, fmt.Sprintf("%s VEID %s, sync %v", p.Kind, p.VEID, p.Sync))
	s.st.DB.First(&p, "node_id = ?", n.ID)
	c.JSON(http.StatusOK, gin.H{"configured": true, "provider": providerViewOf(&p)})
}

// refreshNodeProvider is POST /nodes/:id/provider/refresh: reads now,
// including the VPS's live state.
func (s *Server) refreshNodeProvider(c *gin.Context) {
	var p store.NodeProvider
	if s.st.DB.First(&p, "node_id = ?", c.Param("id")).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未配置服务商 API"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 40*time.Second)
	defer cancel()
	if _, err := s.syncProvider(ctx, &p, true); err != nil {
		badRequest(c, err)
		return
	}
	s.st.DB.First(&p, "node_id = ?", p.NodeID)
	c.JSON(http.StatusOK, gin.H{"configured": true, "provider": providerViewOf(&p)})
}

// deleteNodeProvider is DELETE /nodes/:id/provider.
func (s *Server) deleteNodeProvider(c *gin.Context) {
	s.st.DB.Delete(&store.NodeProvider{}, "node_id = ?", c.Param("id"))
	s.auditCtx(c, "node.provider.delete", c.Param("id"), "")
	c.Status(http.StatusNoContent)
}
