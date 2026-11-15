package panel

import (
	"net/http"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

// panelSelf returns a snapshot of this panel for multi-panel hubs.
//
// User counts and month traffic come from the same cached aggregates the
// dashboard uses (30s TTL), so hub polling does not re-run the heavy
// queries on every request. Node counts are cheap indexed counts.
func (s *Server) panelSelf(c *gin.Context) {
	now := time.Now().In(s.cfg.Location())
	agg := s.statsOverviewAggregates(now, 7)

	users, _ := agg.users["total"].(int64)
	onlineUsers, _ := agg.users["online"].(int64)

	var nodes, nodesOnline int64
	db := s.st.DB
	if err := db.Model(&store.Node{}).Count(&nodes).Error; err != nil {
		dbError(c, err)
		return
	}
	if err := db.Model(&store.Node{}).Where("status IN ?", []string{"online", "syncing"}).Count(&nodesOnline).Error; err != nil {
		dbError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"version":      Version,
		"capabilities": []string{"v1"},
		"users":        users,
		"onlineUsers":  onlineUsers,
		"nodes":        nodes,
		"nodesOnline":  nodesOnline,
		"trafficUp":    agg.month.Up,
		"trafficDown":  agg.month.Down,
	})
}
