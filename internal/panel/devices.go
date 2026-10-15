package panel

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Device (HWID) limits: clients such as Happ, v2RayTun and Hiddify send a
// hardware id with subscription requests. A user with DeviceLimit > 0 may
// fetch the subscription from at most that many devices; requests without
// an id are served (the client cannot be told apart).

func hwidOf(r *http.Request) string {
	for _, h := range []string{"X-Hwid", "X-Device-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			if len(v) > 128 {
				v = v[:128]
			}
			return v
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// checkDevice records the requesting device and reports whether it may
// fetch the subscription (false: the device limit is reached).
func (s *Server) checkDevice(u *store.User, r *http.Request) bool {
	now := time.Now()
	ua := clip(r.UserAgent(), 255)
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"last_sub_at": now, "last_sub_ua": ua})
	hwid := hwidOf(r)
	if hwid == "" {
		return true
	}
	upd := map[string]any{"last_seen": now, "user_agent": ua, "ip": clientIP(r),
		"os": clip(r.Header.Get("X-Device-Os"), 64), "model": clip(r.Header.Get("X-Device-Model"), 128)}
	res := s.st.DB.Model(&store.UserDevice{}).Where("user_id = ? AND hwid = ?", u.ID, hwid).Updates(upd)
	if res.Error == nil && res.RowsAffected > 0 {
		return true // known device
	}
	if u.DeviceLimit > 0 {
		var n int64
		s.st.DB.Model(&store.UserDevice{}).Where("user_id = ?", u.ID).Count(&n)
		if int(n) >= u.DeviceLimit {
			if s.alertOnce("hwid:"+u.ID, time.Hour) {
				s.audit("system", "user.device_limit", u.Username, fmt.Sprintf("refused device %s (%d/%d)", clip(hwid, 16), n, u.DeviceLimit), clientIP(r))
			}
			return false
		}
	}
	s.st.DB.Create(&store.UserDevice{ID: store.NewID(), UserID: u.ID, HWID: hwid, OS: upd["os"].(string), Model: upd["model"].(string),
		UserAgent: ua, IP: clientIP(r), FirstSeen: now, LastSeen: now})
	return true
}

// listDevices is GET /users/:id/devices.
func (s *Server) listDevices(c *gin.Context) {
	var ds []store.UserDevice
	s.st.DB.Where("user_id = ?", c.Param("id")).Order("last_seen desc").Find(&ds)
	if ds == nil {
		ds = []store.UserDevice{}
	}
	c.JSON(http.StatusOK, ds)
}

// deleteDevice is DELETE /users/:id/devices/:did (all devices with did "all").
func (s *Server) deleteDevice(c *gin.Context) {
	q := s.st.DB.Where("user_id = ?", c.Param("id"))
	if did := c.Param("did"); did != "all" {
		q = q.Where("id = ?", did)
	}
	if err := q.Delete(&store.UserDevice{}).Error; err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "user.device_delete", c.Param("id"), c.Param("did"))
	c.Status(http.StatusNoContent)
}
