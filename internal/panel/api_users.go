package panel

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

type userReq struct {
	Username        string     `json:"username" binding:"required"`
	TrafficLimit    int64      `json:"trafficLimit"`
	ExpireAt        *time.Time `json:"expireAt"`
	ExpireAfterDays int        `json:"expireAfterDays"`
	IPLimit         int        `json:"ipLimit"`
	Status          string     `json:"status"` // active / disabled
	ResetPolicy     string     `json:"resetPolicy"`
	ResetDay        int        `json:"resetDay"`
	RenewDays       int        `json:"renewDays"`
	RenewMax        int        `json:"renewMax"`
	DeviceLimit     int        `json:"deviceLimit"`
	Remark          string     `json:"remark"`
	GroupIDs        []string   `json:"groupIds"`
	NodeIDs         []string   `json:"nodeIds"`    // nil = unchanged on update
	InboundIDs      []string   `json:"inboundIds"` // nil = unchanged on update
	TelegramID      *int64     `json:"telegramId"` // nil = unchanged, 0 = unbind
	UUID            string     `json:"uuid"`
	Password        string     `json:"password"`
}

func (s *Server) withGroups(us []store.User) {
	if len(us) == 0 {
		return
	}
	ids := make([]string, len(us))
	for i, u := range us {
		ids[i] = u.ID
	}
	var ugs []store.UserGroup
	s.st.DB.Where("user_id IN ?", ids).Find(&ugs)
	m := map[string][]string{}
	for _, g := range ugs {
		m[g.UserID] = append(m[g.UserID], g.GroupID)
	}
	var acc []store.UserAccess
	s.st.DB.Where("user_id IN ?", ids).Find(&acc)
	nodes, ins := map[string][]string{}, map[string][]string{}
	for _, a := range acc {
		if a.Kind == "node" {
			nodes[a.UserID] = append(nodes[a.UserID], a.TargetID)
		} else {
			ins[a.UserID] = append(ins[a.UserID], a.TargetID)
		}
	}
	for i := range us {
		us[i].GroupIDs, us[i].NodeIDs, us[i].InboundIDs = nonNilStrs(m[us[i].ID]), nonNilStrs(nodes[us[i].ID]), nonNilStrs(ins[us[i].ID])
	}
}

// setAccess replaces a user's direct node or inbound bindings (nil = keep).
func setAccess(tx *gorm.DB, userID, kind string, ids []string) error {
	if ids == nil {
		return nil
	}
	if err := tx.Where("user_id = ? AND kind = ?", userID, kind).Delete(&store.UserAccess{}).Error; err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if err := tx.Create(&store.UserAccess{UserID: userID, Kind: kind, TargetID: id}).Error; err != nil {
			return err
		}
	}
	return nil
}

// addAccess adds one binding to many users (idempotent).
func addAccess(tx *gorm.DB, userIDs []string, kind string, targets []string) error {
	for _, u := range userIDs {
		for _, t := range targets {
			if err := tx.Where(store.UserAccess{UserID: u, Kind: kind, TargetID: t}).FirstOrCreate(&store.UserAccess{UserID: u, Kind: kind, TargetID: t}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// listUsers supports ?q= search and ?filter=online|expiring|limited|disabled.
func (s *Server) listUsers(c *gin.Context) {
	q := s.st.DB.Where("is_service = ?", false).Order("created_at desc")
	if v := strings.TrimSpace(c.Query("q")); v != "" {
		like := "%" + v + "%"
		q = q.Where("username LIKE ? OR uuid LIKE ? OR remark LIKE ?", like, like, like)
	}
	now := time.Now()
	switch c.Query("filter") {
	case "online":
		q = q.Where("online_at > ?", now.Add(-2*time.Minute))
	case "expiring":
		q = q.Where("expire_at IS NOT NULL AND expire_at > ? AND expire_at < ?", now, now.Add(7*24*time.Hour))
	case "limited":
		q = q.Where("status IN ?", []string{"limited", "expired"})
	case "disabled":
		q = q.Where("status = ?", "disabled")
	}
	if g := c.Query("group"); g != "" {
		q = q.Where("id IN (?)", s.st.DB.Model(&store.UserGroup{}).Select("user_id").Where("group_id = ?", g))
	}
	var total int64
	q.Model(&store.User{}).Count(&total)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var us []store.User
	if err := q.Limit(limit).Offset(offset).Find(&us).Error; err != nil {
		serverError(c, err)
		return
	}
	s.withGroups(us)
	c.JSON(http.StatusOK, gin.H{"items": us, "total": total})
}

func (s *Server) getUser(c *gin.Context) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	us := []store.User{u}
	s.withGroups(us)
	c.JSON(http.StatusOK, us[0])
}

// newUser fills generated credentials: one UUID and one password per user so
// the same account works on every protocol.
func newUser(req *userReq) store.User {
	u := store.User{
		ID: store.NewID(), Username: req.Username, UUID: req.UUID, Password: req.Password, SSSeed: pki.Token(24),
		TrafficLimit: req.TrafficLimit, ExpireAt: req.ExpireAt, ExpireAfterDays: req.ExpireAfterDays, IPLimit: req.IPLimit, Status: "active",
		SubToken: pki.Token(32), ResetPolicy: orDefault(req.ResetPolicy, "none"), ResetDay: req.ResetDay,
		RenewDays: max(req.RenewDays, 0), RenewMax: max(req.RenewMax, 0), DeviceLimit: max(req.DeviceLimit, 0),
		Remark: req.Remark, OnlineIPs: store.NewJSON([]string{}),
	}
	if u.UUID == "" {
		u.UUID = pki.UUID()
	}
	if u.Password == "" {
		u.Password = pki.Password(20)
	}
	if req.Status == "disabled" {
		u.Status = "disabled"
	}
	u.Status = computeStatus(&u, time.Now())
	return u
}

func (s *Server) createUser(c *gin.Context) {
	var req userReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	u := newUser(&req)
	if req.TelegramID != nil && *req.TelegramID < 0 {
		badRequest(c, fmt.Errorf("Telegram User ID 无效"))
		return
	}
	if req.TelegramID != nil {
		u.TelegramID = *req.TelegramID
	}
	if req.GroupIDs == nil && req.NodeIDs == nil && req.InboundIDs == nil {
		req.GroupIDs = s.defaultGroupIDs() // outside the transaction (single SQLite connection)
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if u.TelegramID != 0 { // one account per Telegram user, as with /bind
			tx.Model(&store.User{}).Where("telegram_id = ?", u.TelegramID).Update("telegram_id", 0)
		}
		if err := tx.Create(&u).Error; err != nil {
			return err
		}
		for _, g := range req.GroupIDs {
			if err := tx.Create(&store.UserGroup{UserID: u.ID, GroupID: g}).Error; err != nil {
				return err
			}
		}
		if err := setAccess(tx, u.ID, "node", req.NodeIDs); err != nil {
			return err
		}
		return setAccess(tx, u.ID, "inbound", req.InboundIDs)
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("user "+u.Username+" created", s.nodesForUsers(u.ID)...)
	s.auditCtx(c, "user.create", u.Username, "")
	c.JSON(http.StatusCreated, u)
}

// bulkUsers creates N users from a prefix, or applies an action to many.
func (s *Server) bulkUsers(c *gin.Context) {
	var req struct {
		Action   string   `json:"action"` // create / delete / enable / disable / reset-traffic / extend / add-group
		IDs      []string `json:"ids"`
		Prefix   string   `json:"prefix"`
		Count    int      `json:"count"`
		Template *userReq `json:"template"` // create only (a pointer, so other actions need not send it)
		Days     int      `json:"days"`
		GroupID  string   `json:"groupId"`
		NodeIDs  []string `json:"nodeIds"`    // attach-nodes / detach-nodes
		Inbounds []string `json:"inboundIds"` // attach-inbounds / detach-inbounds
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var affected []string
	// Nodes the users are on before the change (they must be republished when
	// users leave them). Looked up outside the transaction: SQLite has a
	// single connection, so a query on s.st.DB inside it would deadlock.
	var before []string
	if len(req.IDs) > 0 {
		before = s.nodesForUsers(req.IDs...)
	}
	if t := req.Template; req.Action == "create" && t != nil && t.GroupIDs == nil && t.NodeIDs == nil && t.InboundIDs == nil {
		t.GroupIDs = s.defaultGroupIDs()
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		switch req.Action {
		case "create":
			if req.Count <= 0 || req.Count > 1000 || req.Prefix == "" || req.Template == nil {
				return fmt.Errorf("prefix, count (1-1000) and template are required")
			}
			for i := 1; i <= req.Count; i++ {
				t := *req.Template
				t.Username = fmt.Sprintf("%s%03d", req.Prefix, i)
				t.UUID, t.Password = "", ""
				u := newUser(&t)
				if err := tx.Create(&u).Error; err != nil {
					return err
				}
				for _, g := range t.GroupIDs {
					if err := tx.Create(&store.UserGroup{UserID: u.ID, GroupID: g}).Error; err != nil {
						return err
					}
				}
				if err := setAccess(tx, u.ID, "node", t.NodeIDs); err != nil {
					return err
				}
				if err := setAccess(tx, u.ID, "inbound", t.InboundIDs); err != nil {
					return err
				}
				affected = append(affected, u.ID)
			}
		case "delete":
			tx.Where("user_id IN ?", req.IDs).Delete(&store.UserGroup{})
			tx.Where("user_id IN ?", req.IDs).Delete(&store.UserAccess{})
			if err := tx.Where("id IN ? AND is_service = ?", req.IDs, false).Delete(&store.User{}).Error; err != nil {
				return err
			}
			affected = nil
		case "enable":
			affected = req.IDs
			if err := tx.Model(&store.User{}).Where("id IN ?", req.IDs).Update("status", "active").Error; err != nil {
				return err
			}
		case "disable":
			affected = req.IDs
			if err := tx.Model(&store.User{}).Where("id IN ?", req.IDs).Update("status", "disabled").Error; err != nil {
				return err
			}
		case "reset-traffic":
			affected = req.IDs
			if err := tx.Model(&store.User{}).Where("id IN ?", req.IDs).Updates(map[string]any{"used_up": 0, "used_down": 0, "last_reset_at": time.Now()}).Error; err != nil {
				return err
			}
		case "extend":
			affected = req.IDs
			var us []store.User
			tx.Where("id IN ?", req.IDs).Find(&us)
			for _, u := range us {
				base := time.Now()
				if u.ExpireAt != nil && u.ExpireAt.After(base) {
					base = *u.ExpireAt
				}
				exp := base.AddDate(0, 0, req.Days)
				if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Update("expire_at", exp).Error; err != nil {
					return err
				}
			}
		case "add-group":
			affected = req.IDs
			for _, id := range req.IDs {
				tx.Where("user_id = ? AND group_id = ?", id, req.GroupID).Delete(&store.UserGroup{})
				if err := tx.Create(&store.UserGroup{UserID: id, GroupID: req.GroupID}).Error; err != nil {
					return err
				}
			}
		case "attach-nodes", "attach-inbounds":
			kind, targets := "node", req.NodeIDs
			if req.Action == "attach-inbounds" {
				kind, targets = "inbound", req.Inbounds
			}
			affected = req.IDs
			if err := addAccess(tx, req.IDs, kind, targets); err != nil {
				return err
			}
		case "detach-nodes", "detach-inbounds":
			kind, targets := "node", req.NodeIDs
			if req.Action == "detach-inbounds" {
				kind, targets = "inbound", req.Inbounds
			}
			affected = req.IDs
			if err := tx.Where("user_id IN ? AND kind = ? AND target_id IN ?", req.IDs, kind, targets).Delete(&store.UserAccess{}).Error; err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown action %q", req.Action)
		}
		return nil
	})
	if err != nil {
		badRequest(c, err)
		return
	}
	if len(affected) > 0 {
		s.recomputeStatus(affected...)
	}
	if nodes := append(before, s.nodesForUsers(affected...)...); len(nodes) > 0 {
		s.MarkDirty("bulk "+req.Action, nodes...)
	}
	s.auditCtx(c, "user.bulk", req.Action, fmt.Sprintf("%d users", max(len(req.IDs), req.Count)))
	c.JSON(http.StatusOK, gin.H{"affected": len(affected)})
}

// recomputeStatus re-derives limited/expired/active after quota changes.
func (s *Server) recomputeStatus(ids ...string) {
	var us []store.User
	s.st.DB.Where("id IN ?", ids).Find(&us)
	now := time.Now()
	for _, u := range us {
		if want := computeStatus(&u, now); want != u.Status {
			s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("status", want)
		}
	}
}

func (s *Server) updateUser(c *gin.Context) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req userReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	before := s.nodesForUsers(u.ID)
	u.Username, u.TrafficLimit, u.ExpireAt, u.IPLimit = req.Username, req.TrafficLimit, req.ExpireAt, req.IPLimit
	u.ExpireAfterDays = req.ExpireAfterDays
	u.ResetPolicy, u.ResetDay, u.Remark = orDefault(req.ResetPolicy, "none"), req.ResetDay, req.Remark
	if req.RenewDays != u.RenewDays || req.RenewMax != u.RenewMax {
		u.RenewCount = 0 // a new renewal plan starts counting again
	}
	u.RenewDays, u.RenewMax, u.DeviceLimit = max(req.RenewDays, 0), max(req.RenewMax, 0), max(req.DeviceLimit, 0)
	if req.UUID != "" {
		u.UUID = req.UUID
	}
	if req.Password != "" {
		u.Password = req.Password
	}
	if req.TelegramID != nil {
		if *req.TelegramID < 0 {
			badRequest(c, fmt.Errorf("Telegram User ID 无效"))
			return
		}
		u.TelegramID = *req.TelegramID
	}
	switch req.Status {
	case "disabled":
		u.Status = "disabled"
	case "active":
		u.Status = "active"
	}
	u.Status = computeStatus(&u, time.Now())
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if u.TelegramID != 0 {
			tx.Model(&store.User{}).Where("telegram_id = ? AND id <> ?", u.TelegramID, u.ID).Update("telegram_id", 0)
		}
		// Only the edited columns: traffic settlement, IP bans and resets
		// update the counters concurrently and must not be overwritten.
		if err := tx.Model(&u).Select("username", "traffic_limit", "expire_at", "ip_limit", "expire_after_days",
			"reset_policy", "reset_day", "remark", "uuid", "password", "telegram_id", "status",
			"renew_days", "renew_max", "renew_count", "device_limit").Updates(&u).Error; err != nil {
			return err
		}
		if req.GroupIDs != nil {
			if err := tx.Where("user_id = ?", u.ID).Delete(&store.UserGroup{}).Error; err != nil {
				return err
			}
			for _, g := range req.GroupIDs {
				if err := tx.Create(&store.UserGroup{UserID: u.ID, GroupID: g}).Error; err != nil {
					return err
				}
			}
		}
		if err := setAccess(tx, u.ID, "node", req.NodeIDs); err != nil {
			return err
		}
		return setAccess(tx, u.ID, "inbound", req.InboundIDs)
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("user "+u.Username+" updated", append(before, s.nodesForUsers(u.ID)...)...)
	s.auditCtx(c, "user.update", u.Username, "")
	c.JSON(http.StatusOK, u)
}

func (s *Server) deleteUser(c *gin.Context) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ? AND is_service = ?", c.Param("id"), false).Error; err != nil {
		dbError(c, err)
		return
	}
	nodes := s.nodesForUsers(u.ID)
	s.st.DB.Where("user_id = ?", u.ID).Delete(&store.UserGroup{})
	s.st.DB.Where("user_id = ?", u.ID).Delete(&store.UserAccess{})
	s.st.DB.Delete(&u)
	s.MarkDirty("user deleted", nodes...)
	s.auditCtx(c, "user.delete", u.Username, "")
	c.Status(http.StatusNoContent)
}

func (s *Server) resetUserTraffic(c *gin.Context) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	s.st.DB.Model(&u).Updates(map[string]any{"used_up": 0, "used_down": 0, "last_reset_at": time.Now()})
	s.recomputeStatus(u.ID)
	s.MarkDirty("traffic reset", s.nodesForUsers(u.ID)...)
	s.auditCtx(c, "user.reset_traffic", u.Username, "")
	c.Status(http.StatusNoContent)
}

// resetUserToken rotates the subscription token; the old link stops working at once.
func (s *Server) resetUserToken(c *gin.Context) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	tok := pki.Token(32)
	s.st.DB.Model(&u).Update("sub_token", tok)
	s.auditCtx(c, "user.reset_token", u.Username, "")
	c.JSON(http.StatusOK, gin.H{"subToken": tok, "url": s.subURL(tok)})
}

func (s *Server) subURL(token string) string {
	return s.subBase() + "/sub/" + token
}

// userLinks returns the subscription URL and share links for one user.
func (s *Server) userLinks(c *gin.Context) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	nodes, err := s.subNodes(&u)
	if err != nil {
		serverError(c, err)
		return
	}
	type link struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	var links []link
	for _, n := range nodes {
		if l := shareLink(n); l != "" {
			links = append(links, link{n.Name, l})
		}
	}
	c.JSON(http.StatusOK, gin.H{"url": s.subURL(u.SubToken), "links": links})
}

// --- groups ---

func (s *Server) listGroups(c *gin.Context) {
	var gs []store.Group
	s.st.DB.Order("name").Find(&gs)
	for i := range gs {
		s.st.DB.Model(&store.GroupInbound{}).Where("group_id = ?", gs[i].ID).Pluck("inbound_id", &gs[i].InboundIDs)
		if gs[i].InboundIDs == nil {
			gs[i].InboundIDs = []string{}
		}
		s.st.DB.Model(&store.UserGroup{}).Where("group_id = ?", gs[i].ID).Count(&gs[i].UserCount)
	}
	c.JSON(http.StatusOK, gs)
}

type groupReq struct {
	Name       string   `json:"name" binding:"required"`
	Remark     string   `json:"remark"`
	InboundIDs []string `json:"inboundIds"`
}

func (s *Server) saveGroup(c *gin.Context, g *store.Group, req *groupReq, create bool) {
	var before []string
	if !create {
		s.st.DB.Model(&store.GroupInbound{}).Where("group_id = ?", g.ID).Pluck("inbound_id", &before)
	}
	g.Name, g.Remark = req.Name, req.Remark
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(g).Error; err != nil {
			return err
		}
		if req.InboundIDs != nil {
			if err := tx.Where("group_id = ?", g.ID).Delete(&store.GroupInbound{}).Error; err != nil {
				return err
			}
			for _, id := range req.InboundIDs {
				if err := tx.Create(&store.GroupInbound{GroupID: g.ID, InboundID: id}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.MarkDirty("group "+g.Name+" changed", s.nodesForInbounds(append(before, req.InboundIDs...)...)...)
	s.auditCtx(c, "group.save", g.Name, fmt.Sprintf("%d inbounds", len(req.InboundIDs)))
	c.JSON(http.StatusOK, g)
}

func (s *Server) createGroup(c *gin.Context) {
	var req groupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.saveGroup(c, &store.Group{ID: store.NewID()}, &req, true)
}

func (s *Server) updateGroup(c *gin.Context) {
	var g store.Group
	if err := s.st.DB.First(&g, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req groupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.saveGroup(c, &g, &req, false)
}

func (s *Server) deleteGroup(c *gin.Context) {
	var g store.Group
	if err := s.st.DB.First(&g, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var ins []string
	s.st.DB.Model(&store.GroupInbound{}).Where("group_id = ?", g.ID).Pluck("inbound_id", &ins)
	s.st.DB.Where("group_id = ?", g.ID).Delete(&store.GroupInbound{})
	s.st.DB.Where("group_id = ?", g.ID).Delete(&store.UserGroup{})
	s.st.DB.Delete(&g)
	s.MarkDirty("group deleted", s.nodesForInbounds(ins...)...)
	s.auditCtx(c, "group.delete", g.Name, "")
	c.Status(http.StatusNoContent)
}
