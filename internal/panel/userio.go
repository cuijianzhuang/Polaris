package panel

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// userExport is the portable form of a user (GET /users/export,
// POST /users/import). Import also accepts 3x-ui clients (email, id,
// totalGB, expiryTime in ms — negative means "days after first use" —,
// limitIp, enable, tgId, subId, comment).
type userExport struct {
	Username        string     `json:"username"`
	UUID            string     `json:"uuid"`
	Password        string     `json:"password"`
	SSSeed          string     `json:"ssSeed,omitempty"`
	SubToken        string     `json:"subToken,omitempty"`
	TrafficLimit    int64      `json:"trafficLimit"`
	UsedUp          int64      `json:"usedUp"`
	UsedDown        int64      `json:"usedDown"`
	ExpireAt        *time.Time `json:"expireAt,omitempty"`
	ExpireAfterDays int        `json:"expireAfterDays,omitempty"`
	IPLimit         int        `json:"ipLimit"`
	DeviceLimit     int        `json:"deviceLimit"`
	ResetPolicy     string     `json:"resetPolicy,omitempty"`
	ResetDay        int        `json:"resetDay,omitempty"`
	RenewDays       int        `json:"renewDays,omitempty"`
	RenewMax        int        `json:"renewMax,omitempty"`
	Status          string     `json:"status"`
	TelegramID      int64      `json:"telegramId,omitempty"`
	Mail            string     `json:"mail,omitempty"` // e-mail address ("email" is 3x-ui's client name)
	Remark          string     `json:"remark,omitempty"`
	Groups          []string   `json:"groups,omitempty"` // group names
	Plan            string     `json:"plan,omitempty"`   // plan name

	// 3x-ui client fields.
	Email      string `json:"email,omitempty"`
	ID         string `json:"id,omitempty"`
	TotalGB    int64  `json:"totalGB,omitempty"` // bytes, despite the name
	ExpiryTime int64  `json:"expiryTime,omitempty"`
	LimitIP    int    `json:"limitIp,omitempty"`
	Enable     *bool  `json:"enable,omitempty"`
	TgID       any    `json:"tgId,omitempty"`
	SubID      string `json:"subId,omitempty"`
	Comment    string `json:"comment,omitempty"`
	Up         int64  `json:"up,omitempty"`
	Down       int64  `json:"down,omitempty"`
}

// normalize folds 3x-ui fields into the Polaris ones.
func (e *userExport) normalize() {
	if e.Username == "" {
		e.Username = e.Email
	}
	if e.UUID == "" && len(e.ID) == 36 {
		e.UUID = e.ID
	}
	if e.TrafficLimit == 0 {
		e.TrafficLimit = e.TotalGB
	}
	if e.ExpireAt == nil && e.ExpireAfterDays == 0 {
		switch {
		case e.ExpiryTime > 0:
			t := time.UnixMilli(e.ExpiryTime)
			e.ExpireAt = &t
		case e.ExpiryTime < 0:
			e.ExpireAfterDays = int(-e.ExpiryTime / 86400000)
		}
	}
	if e.IPLimit == 0 {
		e.IPLimit = e.LimitIP
	}
	if e.Status == "" {
		e.Status = "active"
		if e.Enable != nil && !*e.Enable {
			e.Status = "disabled"
		}
	}
	if e.TelegramID == 0 {
		switch v := e.TgID.(type) {
		case float64:
			e.TelegramID = int64(v)
		case string:
			fmt.Sscan(v, &e.TelegramID)
		}
	}
	if e.SubToken == "" && len(e.SubID) >= 16 {
		e.SubToken = e.SubID
	}
	if e.Remark == "" {
		e.Remark = e.Comment
	}
	if e.UsedUp == 0 && e.UsedDown == 0 {
		e.UsedUp, e.UsedDown = e.Up, e.Down
	}
	e.Username = strings.TrimSpace(e.Username)
}

// exportUsers is GET /users/export (optionally ?ids=a,b).
func (s *Server) exportUsers(c *gin.Context) {
	q := s.st.DB.Where("is_service = ?", false).Order("created_at")
	if ids := c.Query("ids"); ids != "" {
		q = q.Where("id IN ?", strings.Split(ids, ","))
	}
	var us []store.User
	q.Find(&us)
	s.withGroups(us)
	gname := map[string]string{}
	var gs []store.Group
	s.st.DB.Find(&gs)
	for _, g := range gs {
		gname[g.ID] = g.Name
	}
	out := make([]userExport, 0, len(us))
	for _, u := range us {
		e := userExport{Username: u.Username, UUID: u.UUID, Password: u.Password, SSSeed: u.SSSeed, SubToken: u.SubToken,
			TrafficLimit: u.TrafficLimit, UsedUp: u.UsedUp, UsedDown: u.UsedDown, ExpireAt: u.ExpireAt, ExpireAfterDays: u.ExpireAfterDays,
			IPLimit: u.IPLimit, DeviceLimit: u.DeviceLimit, ResetPolicy: u.ResetPolicy, ResetDay: u.ResetDay, RenewDays: u.RenewDays,
			RenewMax: u.RenewMax, Status: u.Status, TelegramID: u.TelegramID, Mail: u.Email, Remark: u.Remark, Plan: gname[u.PlanID]}
		for _, g := range u.GroupIDs {
			if n := gname[g]; n != "" {
				e.Groups = append(e.Groups, n)
			}
		}
		out = append(out, e)
	}
	s.auditCtx(c, "user.export", "", fmt.Sprintf("%d users", len(out)))
	c.Header("Content-Disposition", "attachment; filename=polaris-users.json")
	c.JSON(http.StatusOK, out)
}

// importUsers is POST /users/import {users: [...]}: users whose name exists
// are skipped; credentials and subscription tokens are kept when given (and
// still free), so clients keep working after a migration.
func (s *Server) importUsers(c *gin.Context) {
	var req struct {
		Users    []userExport `json:"users" binding:"required"`
		GroupIDs []string     `json:"groupIds"` // added to every imported user
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if !s.limitOK(c, license.LimitUsers, len(req.Users)) {
		return
	}
	// Lookups happen before the transaction (single SQLite connection).
	taken := map[string]bool{}
	var names, toks, uuids []string
	s.st.DB.Model(&store.User{}).Pluck("username", &names)
	s.st.DB.Model(&store.User{}).Pluck("sub_token", &toks)
	s.st.DB.Model(&store.User{}).Pluck("uuid", &uuids)
	for _, n := range names {
		taken["u:"+n] = true
	}
	for _, t := range toks {
		taken["t:"+t] = true
	}
	for _, u := range uuids {
		taken["id:"+u] = true
	}
	gid, planID := map[string]string{}, map[string]string{}
	var gs []store.Group
	s.st.DB.Find(&gs)
	for _, g := range gs {
		if g.Plan {
			planID[g.Name] = g.ID
		} else {
			gid[g.Name] = g.ID
		}
	}
	defaults := req.GroupIDs
	if defaults == nil {
		defaults = s.defaultGroupIDs()
	}
	var created, skipped []string
	var ids []string
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		for _, e := range req.Users {
			e.normalize()
			if e.Username == "" || taken["u:"+e.Username] {
				skipped = append(skipped, e.Username)
				continue
			}
			taken["u:"+e.Username] = true
			r := userReq{Username: e.Username, TrafficLimit: max(e.TrafficLimit, 0), ExpireAt: e.ExpireAt, ExpireAfterDays: max(e.ExpireAfterDays, 0),
				IPLimit: max(e.IPLimit, 0), ResetPolicy: e.ResetPolicy, ResetDay: e.ResetDay, RenewDays: e.RenewDays, RenewMax: e.RenewMax,
				DeviceLimit: e.DeviceLimit, Remark: e.Remark, Status: e.Status, Password: e.Password}
			if e.UUID != "" && !taken["id:"+e.UUID] {
				r.UUID = e.UUID
				taken["id:"+e.UUID] = true
			}
			u := newUser(&r)
			if e.SSSeed != "" {
				u.SSSeed = e.SSSeed
			}
			if e.SubToken != "" && !taken["t:"+e.SubToken] {
				u.SubToken = e.SubToken
				taken["t:"+e.SubToken] = true
			} else {
				u.SubToken = pki.Token(32)
			}
			u.UsedUp, u.UsedDown, u.TelegramID = max(e.UsedUp, 0), max(e.UsedDown, 0), max(e.TelegramID, 0)
			if m, err := checkUserEmail(tx, e.Mail, u.ID); err == nil && m != "" && !taken["m:"+m] {
				u.Email = m
				taken["m:"+m] = true
			}
			u.PlanID = planID[e.Plan]
			u.Status = computeStatus(&u, time.Now())
			if err := tx.Create(&u).Error; err != nil {
				return fmt.Errorf("%s: %w", e.Username, err)
			}
			groups := map[string]bool{}
			for _, n := range e.Groups {
				if g := gid[n]; g != "" {
					groups[g] = true
				}
			}
			if u.PlanID != "" {
				groups[u.PlanID] = true
			} else if len(e.Groups) == 0 {
				for _, g := range defaults {
					groups[g] = true
				}
			}
			for g := range groups {
				if err := tx.Create(&store.UserGroup{UserID: u.ID, GroupID: g}).Error; err != nil {
					return err
				}
			}
			created = append(created, u.Username)
			ids = append(ids, u.ID)
		}
		return nil
	})
	if err != nil {
		badRequest(c, err)
		return
	}
	if len(ids) > 0 {
		s.MarkDirty("users imported", s.nodesForUsers(ids...)...)
	}
	s.auditCtx(c, "user.import", "", fmt.Sprintf("%d created, %d skipped", len(created), len(skipped)))
	c.JSON(http.StatusOK, gin.H{"created": len(created), "skipped": skipped})
}

// setUserTraffic is PUT /users/:id/traffic {usedUp, usedDown}: corrects the
// counters (migration, mistakes).
func (s *Server) setUserTraffic(c *gin.Context) {
	var req struct {
		UsedUp   int64 `json:"usedUp"`
		UsedDown int64 `json:"usedDown"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var u store.User
	if err := s.st.DB.First(&u, "id = ? AND is_service = ?", c.Param("id"), false).Error; err != nil {
		dbError(c, err)
		return
	}
	s.st.DB.Model(&u).Updates(map[string]any{"used_up": max(req.UsedUp, 0), "used_down": max(req.UsedDown, 0)})
	s.recomputeStatus(u.ID)
	s.MarkDirty("traffic set", s.nodesForUsers(u.ID)...)
	s.auditCtx(c, "user.set_traffic", u.Username, fmt.Sprintf("up=%d down=%d", req.UsedUp, req.UsedDown))
	c.Status(http.StatusNoContent)
}
