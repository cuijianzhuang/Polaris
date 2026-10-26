package panel

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Per-node traffic for a user (store.UserNodeQuota): a traffic pack usable
// only on that node, spent before the account quota (and still usable after
// the account quota runs out), and a cap per traffic period on that node.

// loadNodeQuotas returns the quotas touched by a settlement, by "user|node".
func (s *Server) loadNodeQuotas(data map[string]map[string][2]int64) map[string]*store.UserNodeQuota {
	out := map[string]*store.UserNodeQuota{}
	nodes := make([]string, 0, len(data))
	for n := range data {
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return out
	}
	var qs []store.UserNodeQuota
	s.st.DB.Where("node_id IN ?", nodes).Find(&qs)
	for i := range qs {
		out[qs[i].UserID+"|"+qs[i].NodeID] = &qs[i]
	}
	return out
}

// chargeNodeQuota counts up/down bytes on q and returns the part charged to
// the account quota (what the pack did not cover). flipped reports that the
// user's access to the node changed: the cap was reached or the pack ran out.
func chargeNodeQuota(q *store.UserNodeQuota, up, down int64) (cu, cd int64, flipped bool) {
	wasCapped, hadPack := q.Capped(), q.PackLeft() > 0
	total := up + down
	q.Used += total
	fromPack := min(q.PackLeft(), total)
	q.ExtraUsed += fromPack
	if fromPack > 0 && total > 0 {
		// float: fromPack*up overflows int64 for large counters.
		upPack := min(int64(float64(fromPack)*(float64(up)/float64(total))), up, fromPack)
		if fromPack-upPack > down {
			upPack = fromPack - down
		}
		cu, cd = up-upPack, down-(fromPack-upPack)
	} else {
		cu, cd = up, down
	}
	return cu, cd, q.Capped() != wasCapped || (q.PackLeft() > 0) != hadPack
}

// resetNodeUsage starts a new period for the users' per-node caps (packs are
// one-off and keep what they have left).
func (s *Server) resetNodeUsage(userIDs ...string) {
	if len(userIDs) > 0 {
		if err := s.st.DB.Model(&store.UserNodeQuota{}).Where("user_id IN ?", userIDs).Update("used", 0).Error; err != nil {
			s.log.Warn("reset per-node usage", "users", len(userIDs), "err", err)
		}
	}
}

// nodeAccess tells which nodes a user is cut off from (cap reached) and
// which still have a traffic pack.
func (s *Server) nodeAccess(userID string) (capped, pack map[string]bool) {
	capped, pack = map[string]bool{}, map[string]bool{}
	var qs []store.UserNodeQuota
	s.st.DB.Where("user_id = ?", userID).Find(&qs)
	for i := range qs {
		if qs[i].Capped() {
			capped[qs[i].NodeID] = true
		} else if qs[i].PackLeft() > 0 {
			pack[qs[i].NodeID] = true
		}
	}
	return capped, pack
}

// servedUser reports whether u gets nodes at all: active, or out of account
// quota but holding a node traffic pack.
func (s *Server) servedUser(u *store.User) bool {
	if u.Status == "active" {
		return true
	}
	if u.Status != "limited" {
		return false
	}
	_, pack := s.nodeAccess(u.ID)
	return len(pack) > 0
}

// nodeQuotaUsers lists, for one node, the users cut off by their cap and the
// limited users still served by a pack.
func (s *Server) nodeQuotaUsers(nodeID string) (capped map[string]bool, packIDs []string) {
	capped = map[string]bool{}
	var qs []store.UserNodeQuota
	s.st.DB.Where("node_id = ?", nodeID).Find(&qs)
	for i := range qs {
		switch {
		case qs[i].Capped():
			capped[qs[i].UserID] = true
		case qs[i].PackLeft() > 0:
			packIDs = append(packIDs, qs[i].UserID)
		}
	}
	return capped, packIDs
}

// nodeQuotaLines describes a user's per-node traffic for the info page.
func (s *Server) nodeQuotaLines(userID string) []sub.NodeQuotaLine {
	var qs []store.UserNodeQuota
	s.st.DB.Where("user_id = ?", userID).Find(&qs)
	var out []sub.NodeQuotaLine
	for _, q := range qs {
		var n store.Node
		if s.st.DB.Select("name").First(&n, "id = ?", q.NodeID).Error != nil {
			continue
		}
		var parts []string
		if q.Extra > 0 {
			parts = append(parts, "流量包剩余 "+sub.HumanBytes(q.PackLeft()))
		}
		if q.Limit > 0 {
			parts = append(parts, "本期 "+sub.HumanBytes(q.Used)+" / "+sub.HumanBytes(q.Limit))
		}
		if len(parts) > 0 {
			out = append(out, sub.NodeQuotaLine{Node: n.Name, Text: strings.Join(parts, " · "), Bad: q.Capped()})
		}
	}
	return out
}

type nodeQuotaView struct {
	store.UserNodeQuota
	NodeName string `json:"nodeName"`
}

// listNodeQuotas: GET /users/:id/node-quotas.
func (s *Server) listNodeQuotas(c *gin.Context) {
	var qs []store.UserNodeQuota
	s.st.DB.Where("user_id = ?", c.Param("id")).Find(&qs)
	names := map[string]string{}
	var ns []store.Node
	s.st.DB.Select("id", "name").Find(&ns)
	for _, n := range ns {
		names[n.ID] = n.Name
	}
	out := make([]nodeQuotaView, 0, len(qs))
	for _, q := range qs {
		out = append(out, nodeQuotaView{UserNodeQuota: q, NodeName: names[q.NodeID]})
	}
	c.JSON(http.StatusOK, out)
}

// putNodeQuota: PUT /users/:id/node-quotas/:node {extra, limit, addExtra,
// resetUsed}. extra / limit set the pack size and cap (bytes); addExtra tops
// the pack up; resetUsed starts the cap's period over.
func (s *Server) putNodeQuota(c *gin.Context) {
	var req struct {
		Extra     *int64 `json:"extra"`
		Limit     *int64 `json:"limit"`
		AddExtra  int64  `json:"addExtra"`
		ResetUsed bool   `json:"resetUsed"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var u store.User
	var n store.Node
	if err := s.st.DB.First(&u, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if err := s.st.DB.First(&n, "id = ?", c.Param("node")).Error; err != nil {
		dbError(c, err)
		return
	}
	q := store.UserNodeQuota{UserID: u.ID, NodeID: n.ID}
	s.st.DB.Where("user_id = ? AND node_id = ?", u.ID, n.ID).First(&q)
	if req.Extra != nil {
		q.Extra = max(*req.Extra, 0)
	}
	if req.Limit != nil {
		q.Limit = max(*req.Limit, 0)
	}
	if req.AddExtra > 0 {
		q.Extra += req.AddExtra
	}
	if req.ResetUsed {
		q.Used = 0
	}
	q.UpdatedAt = time.Now()
	if err := s.st.DB.Save(&q).Error; err != nil {
		serverError(c, err)
		return
	}
	s.MarkDirty("node quota changed", n.ID)
	var parts []string
	if q.Extra > 0 {
		parts = append(parts, "pack "+sub.HumanBytes(q.Extra))
	}
	if q.Limit > 0 {
		parts = append(parts, "cap "+sub.HumanBytes(q.Limit))
	}
	s.auditCtx(c, "user.node_quota", u.Username+"@"+n.Name, strings.Join(parts, ", "))
	c.JSON(http.StatusOK, nodeQuotaView{UserNodeQuota: q, NodeName: n.Name})
}

// deleteNodeQuota: DELETE /users/:id/node-quotas/:node.
func (s *Server) deleteNodeQuota(c *gin.Context) {
	s.st.DB.Where("user_id = ? AND node_id = ?", c.Param("id"), c.Param("node")).Delete(&store.UserNodeQuota{})
	s.MarkDirty("node quota removed", c.Param("node"))
	s.auditCtx(c, "user.node_quota_delete", c.Param("id"), c.Param("node"))
	c.Status(http.StatusNoContent)
}
