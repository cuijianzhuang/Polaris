package panel

import (
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Atomic user operations for shops and payment plugins: each call adds to
// what the user has instead of replacing it, so it can be sent per order
// (with an Idempotency-Key against duplicate callbacks).

// renewUser is POST /users/:id/renew {days, resetTraffic}: expiry moves on
// by days, counted from now when the account has already expired. An
// account that starts on first use gets the days added to its period; one
// without expiry stays unlimited.
func (s *Server) renewUser(c *gin.Context) {
	var req struct {
		Days         int  `json:"days" binding:"required"`
		ResetTraffic bool `json:"resetTraffic"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Days < 1 || req.Days > 3650 {
		badRequest(c, fmt.Errorf("天数应为 1–3650"))
		return
	}
	var u store.User
	if err := s.st.DB.First(&u, "id = ? AND is_service = ?", c.Param("id"), false).Error; err != nil {
		dbError(c, err)
		return
	}
	now := time.Now()
	upd := map[string]any{"warned_expire": nil}
	detail := ""
	switch {
	case u.ExpireAt != nil:
		from := *u.ExpireAt
		if from.Before(now) {
			from = now
		}
		exp := from.AddDate(0, 0, req.Days)
		upd["expire_at"] = exp
		u.ExpireAt = &exp
		detail = fmt.Sprintf("+%d 天，至 %s", req.Days, exp.Format("2006-01-02"))
	case u.ExpireAfterDays > 0:
		upd["expire_after_days"] = u.ExpireAfterDays + req.Days
		detail = fmt.Sprintf("首次使用后有效期 +%d 天，共 %d 天", req.Days, u.ExpireAfterDays+req.Days)
	default:
		detail = fmt.Sprintf("+%d 天（账号不限期，未改动）", req.Days)
	}
	if req.ResetTraffic {
		upd["used_up"], upd["used_down"], upd["last_reset_at"], upd["warned_usage"] = 0, 0, now, false
		detail += "，流量已重置"
	}
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(upd)
	if req.ResetTraffic {
		s.resetNodeUsage(u.ID)
	}
	s.recomputeStatus(u.ID)
	s.MarkDirty("user renewed", s.nodesForUsers(u.ID)...)
	s.auditCtx(c, "user.renew", u.Username, detail)
	s.notify(Event{Kind: "user_renewed", UserID: u.ID, Text: "🔄 用户 " + u.Username + " 已续期：" + detail})
	s.userJSON(c, u.ID)
}

// addUserTraffic is POST /users/:id/add-traffic {gb}: raises the quota. An
// unlimited account stays unlimited.
func (s *Server) addUserTraffic(c *gin.Context) {
	var req struct {
		GB float64 `json:"gb" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.GB <= 0 || req.GB > 1<<20 || math.IsNaN(req.GB) {
		badRequest(c, fmt.Errorf("流量应为大于 0 的 GB 数"))
		return
	}
	var u store.User
	if err := s.st.DB.First(&u, "id = ? AND is_service = ?", c.Param("id"), false).Error; err != nil {
		dbError(c, err)
		return
	}
	detail := fmt.Sprintf("+%g GB（账号不限流量，未改动）", req.GB)
	if u.TrafficLimit > 0 {
		add := int64(req.GB * (1 << 30))
		s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"traffic_limit": u.TrafficLimit + add, "warned_usage": false})
		detail = fmt.Sprintf("+%g GB，共 %.2f GB", req.GB, float64(u.TrafficLimit+add)/(1<<30))
		s.recomputeStatus(u.ID)
		s.MarkDirty("traffic added", s.nodesForUsers(u.ID)...)
	}
	s.auditCtx(c, "user.add_traffic", u.Username, detail)
	s.userJSON(c, u.ID)
}

// userJSON answers with the user as GET /users/:id would.
func (s *Server) userJSON(c *gin.Context, id string) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", id).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(http.StatusOK, u)
}
