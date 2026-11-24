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

// hwidOf accepts the documented X-HWID family as well as the older client
// headers. Header field names are case-insensitive, but accepting X-HWID-ID
// makes clients that distinguish a device identifier from its display name
// interoperate without custom subscription URLs.
func hwidOf(r *http.Request) string {
	for _, h := range []string{"X-HWID", "X-HWID-ID", "X-Device-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			if len(v) > 128 {
				v = v[:128]
			}
			return v
		}
	}
	return ""
}

func hwidHeader(r *http.Request, names ...string) string {
	for _, h := range names {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
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
	now := time.Now().UTC()
	ua := clip(r.UserAgent(), 255)
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"last_sub_at": now, "last_sub_ua": ua})
	hwid := hwidOf(r)
	if hwid == "" {
		return true
	}
	upd := map[string]any{"last_seen": now, "user_agent": ua, "ip": clientIP(r),
		"os":    clip(hwidHeader(r, "X-HWID-OS", "X-Device-Os"), 64),
		"model": clip(hwidHeader(r, "X-HWID-Model", "X-Device-Model"), 128)}
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

type portalDeviceView struct {
	ID        string    `json:"id"`
	HWID      string    `json:"hwid"`
	OS        string    `json:"os"`
	Model     string    `json:"model"`
	IP        string    `json:"ip"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// portalDevices lists only the signed-in user's subscription devices. The
// opaque database ID, rather than a hardware ID supplied by the client, is
// used for removal so one user cannot target another user's device record.
func (s *Server) portalDevices(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var rows []store.UserDevice
	if err := s.st.DB.Where("user_id = ?", u.ID).Order("last_seen DESC, id DESC").Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	out := make([]portalDeviceView, 0, len(rows))
	for _, row := range rows {
		// Full client IDs are unnecessary on the portal and may be sensitive.
		masked := clip(row.HWID, 16)
		if len(row.HWID) > len(masked) {
			masked += "…"
		}
		out = append(out, portalDeviceView{ID: row.ID, HWID: masked, OS: row.OS, Model: row.Model, IP: row.IP, FirstSeen: row.FirstSeen, LastSeen: row.LastSeen})
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// portalDeleteDevice removes one device owned by the signed-in portal user.
func (s *Server) portalDeleteDevice(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	id := c.Param("did")
	if id == "" || id == "all" {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}
	res := s.st.DB.Where("id = ? AND user_id = ?", id, u.ID).Delete(&store.UserDevice{})
	if res.Error != nil {
		serverError(c, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}
	s.audit(s.portalActorFor(c, u), "user.device_delete", u.Username, id, clientIP(c.Request))
	c.Status(http.StatusNoContent)
}
