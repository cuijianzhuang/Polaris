package panel

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Node ↔ user binding as seen from a node: who may use it, how they got
// access, and their traffic and online IPs on this node.

type nodeUserRow struct {
	ID       string     `json:"id"`
	Username string     `json:"username"`
	Status   string     `json:"status"`
	Via      []string   `json:"via"`      // node / inbound / group:<name>
	Inbounds int        `json:"inbounds"` // inbounds on this node the user can use
	Up       int64      `json:"up"`       // traffic on this node, last 30 days
	Down     int64      `json:"down"`
	OnlineIP []string   `json:"onlineIps"` // currently seen on this node
	OnlineAt *time.Time `json:"onlineAt"`
}

func (s *Server) nodeUsersAPI(c *gin.Context) {
	nodeID := c.Param("id")
	var inIDs []string
	s.st.DB.Model(&store.Inbound{}).Where("node_id = ? AND service_only = ?", nodeID, false).Pluck("id", &inIDs)

	rows := map[string]*nodeUserRow{}
	via := func(uid, v string) {
		r := rows[uid]
		if r == nil {
			r = &nodeUserRow{ID: uid, Via: []string{}, OnlineIP: []string{}}
			rows[uid] = r
		}
		for _, x := range r.Via {
			if x == v {
				return
			}
		}
		r.Via = append(r.Via, v)
	}
	// Direct node bindings.
	var acc []store.UserAccess
	s.st.DB.Where("kind = ? AND target_id = ?", "node", nodeID).Find(&acc)
	for _, a := range acc {
		via(a.UserID, "node")
	}
	if len(inIDs) > 0 {
		s.st.DB.Where("kind = ? AND target_id IN ?", "inbound", inIDs).Find(&acc)
		for _, a := range acc {
			via(a.UserID, "inbound")
		}
		type gv struct{ UserID, Name string }
		var gs []gv
		s.st.DB.Table("user_groups").Select(`DISTINCT user_groups.user_id AS user_id, "groups".name AS name`).
			Joins("JOIN group_inbounds ON group_inbounds.group_id = user_groups.group_id").
			Joins(`JOIN "groups" ON "groups".id = user_groups.group_id`).
			Where("group_inbounds.inbound_id IN ?", inIDs).Scan(&gs)
		for _, g := range gs {
			via(g.UserID, "group:"+g.Name)
		}
		type cnt struct {
			UserID string
			N      int
		}
		var cs []cnt
		s.st.DB.Raw("SELECT user_id, COUNT(DISTINCT inbound_id) AS n FROM ("+store.GrantsSQL+") g WHERE inbound_id IN ? GROUP BY user_id", inIDs).Scan(&cs)
		for _, x := range cs {
			if r := rows[x.UserID]; r != nil {
				r.Inbounds = x.N
			}
		}
	}
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		var us []store.User
		s.st.DB.Where("id IN ? AND is_service = ?", ids, false).Find(&us)
		for _, u := range us {
			r := rows[u.ID]
			r.Username, r.Status, r.OnlineAt = u.Username, u.Status, u.OnlineAt
		}
		type tr struct {
			UserID   string
			Up, Down int64
		}
		var ts []tr
		s.st.DB.Model(&store.TrafficHourly{}).Select("user_id, SUM(up) AS up, SUM(down) AS down").
			Where("node_id = ? AND user_id IN ? AND hour > ?", nodeID, ids, time.Now().AddDate(0, 0, -30)).
			Group("user_id").Scan(&ts)
		for _, t := range ts {
			rows[t.UserID].Up, rows[t.UserID].Down = t.Up, t.Down
		}
		snap := s.shared.onlineIPs(ids)
		for _, id := range ids {
			for ip, seen := range snap[id][nodeID] {
				if time.Since(seen) < 3*time.Minute {
					rows[id].OnlineIP = append(rows[id].OnlineIP, ip)
				}
			}
		}
	}
	out := make([]*nodeUserRow, 0, len(rows))
	for _, r := range rows {
		if r.Username != "" { // skip service accounts and dangling rows
			sort.Strings(r.OnlineIP)
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if (len(out[i].OnlineIP) > 0) != (len(out[j].OnlineIP) > 0) {
			return len(out[i].OnlineIP) > 0
		}
		return out[i].Username < out[j].Username
	})
	c.JSON(http.StatusOK, out)
}

// bindNodeUsers attaches or detaches users to the whole node.
func (s *Server) bindNodeUsers(c *gin.Context) {
	var req struct {
		UserIDs []string `json:"userIds" binding:"required"`
		Detach  bool     `json:"detach"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if req.Detach {
			return tx.Where("kind = ? AND target_id = ? AND user_id IN ?", "node", n.ID, req.UserIDs).Delete(&store.UserAccess{}).Error
		}
		return addAccess(tx, req.UserIDs, "node", []string{n.ID})
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("node users changed", n.ID)
	action := "node.bind_users"
	if req.Detach {
		action = "node.unbind_users"
	}
	s.auditCtx(c, action, n.Name, fmt.Sprintf("%d users", len(req.UserIDs)))
	c.Status(http.StatusNoContent)
}
