package panel

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/license"
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
	SpeedUpMbps     int        `json:"speedUpMbps"`
	SpeedDownMbps   int        `json:"speedDownMbps"`
	Remark          string     `json:"remark"`
	GroupIDs        []string   `json:"groupIds"`   // ordinary groups; plan groups are ignored
	PlanID          *string    `json:"planId"`     // nil = unchanged on update; sets the plan, not its limits
	NodeIDs         []string   `json:"nodeIds"`    // nil = unchanged on update
	InboundIDs      []string   `json:"inboundIds"` // nil = unchanged on update
	TelegramID      *int64     `json:"telegramId"` // nil = unchanged, 0 = unbind
	Email           *string    `json:"email"`      // nil = unchanged, "" = none
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
	s.st.DB.Where("user_id IN ? AND group_id NOT IN (?)", ids, plansOnly(s.st.DB)).Find(&ugs)
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

// userSorts maps ?sort= to a column expression (whitelist).
var userSorts = map[string]string{
	"created": "created_at", "username": "username", "used": "used_up + used_down", "expire": "expire_at IS NULL, expire_at",
	"online": "online_at IS NULL, online_at", "limit": "traffic_limit = 0, traffic_limit",
}

// userFilter narrows a user query by status bucket; warn is the "expiring" window.
func userFilter(q *gorm.DB, filter string, now time.Time, warn time.Duration) *gorm.DB {
	switch filter {
	case "active":
		return q.Where("status = ?", "active")
	case "online":
		return q.Where("online_at > ?", now.Add(-2*time.Minute))
	case "expiring":
		return q.Where("status = ? AND expire_at IS NOT NULL AND expire_at > ? AND expire_at < ?", "active", now, now.Add(warn))
	case "limited":
		return q.Where("status = ?", "limited")
	case "near": // 80%+ of the traffic limit used, still active
		return q.Where("status = ? AND traffic_limit > 0 AND used_up + used_down >= traffic_limit * 0.8", "active")
	case "expired":
		return q.Where("status = ?", "expired")
	case "depleted":
		return q.Where("status IN ?", []string{"limited", "expired"})
	case "disabled":
		return q.Where("status = ?", "disabled")
	}
	return q
}

// listUsers supports ?q= search, ?filter= (active / online / expiring /
// limited / expired / depleted / disabled), ?group=, ?renew=on|off,
// ?tg=yes|no and ?sort=…&order=asc|desc; it also returns per-status counts.
func (s *Server) listUsers(c *gin.Context) {
	base := s.st.DB.Model(&store.User{}).Where("is_service = ?", false)
	now := time.Now()
	warn := time.Duration(max(atoiDefault(s.st.GetSetting("expire_warn_days", "7"), 7), 1)) * 24 * time.Hour
	summary := map[string]int64{}
	for _, f := range []string{"active", "online", "expiring", "near", "limited", "expired", "disabled"} {
		var n int64
		userFilter(base.Session(&gorm.Session{}), f, now, warn).Count(&n)
		summary[f] = n
	}
	var all int64
	base.Session(&gorm.Session{}).Count(&all)
	summary["all"] = all

	q := base.Session(&gorm.Session{})
	if v := strings.TrimSpace(c.Query("q")); v != "" {
		like := "%" + v + "%"
		q = q.Where(s.st.Like("username LIKE ? OR uuid LIKE ? OR remark LIKE ? OR email LIKE ?"), like, like, like, like)
	}
	q = userFilter(q, c.Query("filter"), now, warn)
	if g := c.Query("group"); g != "" {
		q = q.Where("id IN (?)", s.st.DB.Model(&store.UserGroup{}).Select("user_id").Where("group_id = ?", g))
	}
	switch c.Query("renew") {
	case "on":
		q = q.Where("renew_days > 0 OR reset_policy NOT IN ?", []string{"", "none"})
	case "off":
		q = q.Where("renew_days = 0 AND reset_policy IN ?", []string{"", "none"})
	}
	switch c.Query("tg") {
	case "yes":
		q = q.Where("telegram_id <> 0")
	case "no":
		q = q.Where("telegram_id = 0")
	}
	order := "created_at desc"
	if col, ok := userSorts[c.Query("sort")]; ok {
		dir := " asc"
		if c.Query("order") == "desc" {
			dir = " desc"
		}
		parts := strings.Split(col, ", ")
		parts[len(parts)-1] += dir
		order = strings.Join(parts, ", ") + ", created_at desc"
	}
	q = q.Order(order)
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
	s.withOnline(us)
	c.JSON(http.StatusOK, gin.H{"items": us, "total": total, "summary": summary})
}

func (s *Server) getUser(c *gin.Context) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	us := []store.User{u}
	s.withGroups(us)
	s.withOnline(us)
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
		SpeedUpMbps: max(req.SpeedUpMbps, 0), SpeedDownMbps: max(req.SpeedDownMbps, 0),
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
	if (req.SpeedUpMbps > 0 || req.SpeedDownMbps > 0) && !s.requireFeature(c, license.FeatureSpeedLimit) {
		return
	}
	if !s.limitOK(c, license.LimitUsers, 1) {
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
	if req.Email != nil {
		var err error
		if u.Email, err = checkUserEmail(s.st.DB, *req.Email, u.ID); err != nil {
			badRequest(c, err)
			return
		}
	}
	if req.GroupIDs == nil && req.NodeIDs == nil && req.InboundIDs == nil && (req.PlanID == nil || *req.PlanID == "") {
		req.GroupIDs = s.defaultGroupIDs() // outside the transaction (single SQLite connection)
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if u.TelegramID != 0 { // one account per Telegram user, as with /bind
			tx.Model(&store.User{}).Where("telegram_id = ?", u.TelegramID).Update("telegram_id", 0)
		}
		if err := userPlanID(tx, &u, req.PlanID); err != nil {
			return err
		}
		if err := tx.Create(&u).Error; err != nil {
			return err
		}
		if err := setUserGroups(tx, &u, req.GroupIDs); err != nil {
			return err
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
	s.userEvent("user.created", &u, nil)
	c.JSON(http.StatusCreated, u)
}

// bulkUsers creates N users from a prefix, or applies an action to many.
func (s *Server) bulkUsers(c *gin.Context) {
	var req struct {
		Action    string   `json:"action"` // create / delete / delete-depleted / enable / disable / reset-traffic / extend / set-limits / add-group / plan
		IDs       []string `json:"ids"`
		All       bool     `json:"all"` // reset-traffic: every user
		Prefix    string   `json:"prefix"`
		Postfix   string   `json:"postfix"`
		Start     int      `json:"start"`  // first number (default 1)
		Method    string   `json:"method"` // create: seq (prefix+number+postfix, default) / random
		Count     int      `json:"count"`
		AddBytes  int64    `json:"addBytes"`      // extend: added to a limited quota
		IPLimit   *int     `json:"ipLimit"`       // set-limits
		DevLimit  *int     `json:"deviceLimit"`   // set-limits
		SpeedUp   *int     `json:"speedUpMbps"`   // set-limits
		SpeedDown *int     `json:"speedDownMbps"` // set-limits
		Template  *userReq `json:"template"`      // create only (a pointer, so other actions need not send it)
		Days      int      `json:"days"`
		GroupID   string   `json:"groupId"`
		NodeIDs   []string `json:"nodeIds"`    // attach-nodes / detach-nodes
		Inbounds  []string `json:"inboundIds"` // attach-inbounds / detach-inbounds
		PlanID    string   `json:"planId"`     // plan
		Mode      string   `json:"mode"`       // plan: renew (default) / switch
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Action == "set-limits" && (req.SpeedUp != nil || req.SpeedDown != nil) && !s.requireFeature(c, license.FeatureSpeedLimit) {
		return
	}
	if req.Action == "create" && req.Template != nil && (req.Template.SpeedUpMbps > 0 || req.Template.SpeedDownMbps > 0) && !s.requireFeature(c, license.FeatureSpeedLimit) {
		return
	}
	if req.Action == "create" && !s.limitOK(c, license.LimitUsers, max(req.Count, 0)) {
		return
	}
	var affected, skipped []string
	switch {
	case req.Action == "delete-depleted":
		// Expired or over quota: the "delete depleted clients" of 3x-ui.
		req.IDs = nil
		s.st.DB.Model(&store.User{}).Where("is_service = ? AND status IN ?", false, []string{"limited", "expired"}).Pluck("id", &req.IDs)
		req.Action = "delete"
	case req.All && req.Action == "reset-traffic":
		req.IDs = nil
		s.st.DB.Model(&store.User{}).Where("is_service = ?", false).Pluck("id", &req.IDs)
	}
	if req.Action == "create" && req.Template != nil {
		// Existing names are skipped, not fatal (looked up before the transaction).
		if req.Start <= 0 {
			req.Start = 1
		}
		if req.Count > 0 && req.Count <= 1000 {
			var names []string
			s.st.DB.Model(&store.User{}).Pluck("username", &names)
			taken := map[string]bool{}
			for _, n := range names {
				taken[n] = true
			}
			req.IDs = nil
			for i := 0; len(req.IDs) < req.Count && i < req.Count*20; i++ {
				var name string
				if req.Method == "random" {
					name = req.Prefix + strings.ToLower(pki.Token(6)) + req.Postfix
				} else {
					if i >= req.Count {
						break
					}
					name = fmt.Sprintf("%s%0*d%s", req.Prefix, max(3, len(strconv.Itoa(req.Start+req.Count-1))), req.Start+i, req.Postfix)
				}
				if taken[name] {
					skipped = append(skipped, name)
					continue
				}
				taken[name] = true
				req.IDs = append(req.IDs, name) // names, not IDs, for the create branch
			}
		}
	}
	// Nodes the users are on before the change (they must be republished when
	// users leave them). Looked up outside the transaction: SQLite has a
	// single connection, so a query on s.st.DB inside it would deadlock.
	var before []string
	if len(req.IDs) > 0 && req.Action != "create" {
		before = s.nodesForUsers(req.IDs...)
	}
	if t := req.Template; req.Action == "create" && t != nil && t.GroupIDs == nil && t.NodeIDs == nil && t.InboundIDs == nil && (t.PlanID == nil || *t.PlanID == "") {
		t.GroupIDs = s.defaultGroupIDs()
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		switch req.Action {
		case "create":
			if req.Count <= 0 || req.Count > 1000 || (req.Prefix == "" && req.Postfix == "" && req.Method != "random") || req.Template == nil {
				return fmt.Errorf("prefix or postfix, count (1-1000) and template are required")
			}
			if len(req.IDs) == 0 {
				return fmt.Errorf("所有用户名都已存在")
			}
			for _, name := range req.IDs {
				t := *req.Template
				t.Username = name
				t.UUID, t.Password = "", ""
				u := newUser(&t)
				if err := userPlanID(tx, &u, t.PlanID); err != nil {
					return err
				}
				if err := tx.Create(&u).Error; err != nil {
					return err
				}
				if err := setUserGroups(tx, &u, t.GroupIDs); err != nil {
					return err
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
			if err := tx.Model(&store.UserNodeQuota{}).Where("user_id IN ?", req.IDs).Update("used", 0).Error; err != nil {
				return err
			}
		case "extend":
			affected = req.IDs
			var us []store.User
			tx.Where("id IN ?", req.IDs).Find(&us)
			for _, u := range us {
				if req.Days != 0 && u.ExpireAt != nil { // unlimited stays unlimited
					base := time.Now()
					if u.ExpireAt.After(base) {
						base = *u.ExpireAt
					}
					if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Update("expire_at", base.AddDate(0, 0, req.Days)).Error; err != nil {
						return err
					}
				}
				if req.AddBytes != 0 && u.TrafficLimit > 0 {
					if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Update("traffic_limit", max(u.TrafficLimit+req.AddBytes, 1)).Error; err != nil {
						return err
					}
				}
			}
		case "set-limits":
			affected = req.IDs
			upd := map[string]any{}
			if req.IPLimit != nil {
				upd["ip_limit"] = max(*req.IPLimit, 0)
			}
			if req.DevLimit != nil {
				upd["device_limit"] = max(*req.DevLimit, 0)
			}
			if req.SpeedUp != nil {
				upd["speed_up_mbps"] = max(*req.SpeedUp, 0)
			}
			if req.SpeedDown != nil {
				upd["speed_down_mbps"] = max(*req.SpeedDown, 0)
			}
			if req.Template != nil {
				upd["traffic_limit"], upd["extra_traffic"] = max(req.Template.TrafficLimit, 0), 0
			}
			if len(upd) == 0 {
				return fmt.Errorf("nothing to set")
			}
			if err := tx.Model(&store.User{}).Where("id IN ?", req.IDs).Updates(upd).Error; err != nil {
				return err
			}
		case "add-group":
			affected = req.IDs
			if len(dropPlanGroups(tx, []string{req.GroupID})) == 0 {
				return fmt.Errorf("套餐请用「开通套餐」操作")
			}
			for _, id := range req.IDs {
				tx.Where("user_id = ? AND group_id = ?", id, req.GroupID).Delete(&store.UserGroup{})
				if err := tx.Create(&store.UserGroup{UserID: id, GroupID: req.GroupID}).Error; err != nil {
					return err
				}
			}
		case "plan":
			affected = req.IDs
			p, err := loadPlan(tx, req.PlanID)
			if err != nil {
				return err
			}
			var us []store.User
			tx.Where("id IN ? AND is_service = ?", req.IDs, false).Find(&us)
			now := time.Now()
			for i := range us {
				if _, err := applyPlan(tx, &us[i], p, req.Mode != "switch", now); err != nil {
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
	s.auditCtx(c, "user.bulk", req.Action, fmt.Sprintf("%d users", len(req.IDs)))
	s.notify(Event{Kind: "user.bulk", Data: map[string]any{"action": req.Action, "user_ids": req.IDs}})
	n := len(affected)
	if req.Action == "delete" {
		n = len(req.IDs)
	}
	c.JSON(http.StatusOK, gin.H{"affected": n, "skipped": skipped})
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
	if (max(req.SpeedUpMbps, 0) != u.SpeedUpMbps || max(req.SpeedDownMbps, 0) != u.SpeedDownMbps) && !s.requireFeature(c, license.FeatureSpeedLimit) {
		return
	}
	before := s.nodesForUsers(u.ID)
	if req.TrafficLimit != u.TrafficLimit {
		u.ExtraTraffic = 0 // the admin set the quota outright
	}
	u.Username, u.TrafficLimit, u.ExpireAt, u.IPLimit = req.Username, req.TrafficLimit, req.ExpireAt, req.IPLimit
	u.ExpireAfterDays = req.ExpireAfterDays
	u.ResetPolicy, u.ResetDay, u.Remark = orDefault(req.ResetPolicy, "none"), req.ResetDay, req.Remark
	if req.RenewDays != u.RenewDays || req.RenewMax != u.RenewMax {
		u.RenewCount = 0 // a new renewal plan starts counting again
	}
	u.RenewDays, u.RenewMax, u.DeviceLimit = max(req.RenewDays, 0), max(req.RenewMax, 0), max(req.DeviceLimit, 0)
	u.SpeedUpMbps, u.SpeedDownMbps = max(req.SpeedUpMbps, 0), max(req.SpeedDownMbps, 0)
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
	if req.Email != nil {
		var err error
		if u.Email, err = checkUserEmail(s.st.DB, *req.Email, u.ID); err != nil {
			badRequest(c, err)
			return
		}
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
		if err := tx.Model(&u).Select("username", "traffic_limit", "extra_traffic", "expire_at", "ip_limit", "expire_after_days",
			"reset_policy", "reset_day", "remark", "uuid", "password", "telegram_id", "email", "status",
			"renew_days", "renew_max", "renew_count", "device_limit", "speed_up_mbps", "speed_down_mbps").Updates(&u).Error; err != nil {
			return err
		}
		if req.PlanID != nil {
			if err := userPlanID(tx, &u, req.PlanID); err != nil {
				return err
			}
			if err := tx.Model(&u).Update("plan_id", u.PlanID).Error; err != nil {
				return err
			}
		}
		if req.GroupIDs != nil {
			if err := tx.Where("user_id = ?", u.ID).Delete(&store.UserGroup{}).Error; err != nil {
				return err
			}
			if err := setUserGroups(tx, &u, req.GroupIDs); err != nil {
				return err
			}
		} else if req.PlanID != nil {
			if err := setPlanMembership(tx, u.ID, u.PlanID); err != nil {
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
	s.MarkDirty("user "+u.Username+" updated", append(before, s.nodesForUsers(u.ID)...)...)
	s.auditCtx(c, "user.update", u.Username, "")
	s.userEvent("user.updated", &u, nil)
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
	s.st.DB.Where("user_id = ?", u.ID).Delete(&store.UserNodeQuota{})
	s.st.DB.Delete(&u)
	s.MarkDirty("user deleted", nodes...)
	s.auditCtx(c, "user.delete", u.Username, "")
	s.userEvent("user.deleted", &u, nil)
	c.Status(http.StatusNoContent)
}

func (s *Server) resetUserTraffic(c *gin.Context) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	s.st.DB.Model(&u).Updates(map[string]any{"used_up": 0, "used_down": 0, "last_reset_at": time.Now()})
	s.resetNodeUsage(u.ID)
	s.recomputeStatus(u.ID)
	s.MarkDirty("traffic reset", s.nodesForUsers(u.ID)...)
	s.auditCtx(c, "user.reset_traffic", u.Username, "")
	s.userEvent("user.traffic_reset", &u, nil)
	c.Status(http.StatusNoContent)
}

// resetUserToken rotates the subscription token and the node credentials:
// the old link and every node imported from it stop working at once.
func (s *Server) resetUserToken(c *gin.Context) {
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if err := s.rotateUserCredentials(&u); err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "user.reset_token", u.Username, "")
	s.userEvent("user.token_reset", &u, nil)
	c.JSON(http.StatusOK, gin.H{"subToken": u.SubToken, "url": s.subURL(u.SubToken)})
}

// rotateUserCredentials gives u a new subscription token and new protocol
// credentials (UUID, password, Shadowsocks seed), then pushes the change to
// its nodes. A leaked link is useless afterwards, and so are the nodes
// already imported from it: they still carry the old credentials.
func (s *Server) rotateUserCredentials(u *store.User) error {
	u.SubToken, u.UUID, u.Password, u.SSSeed = pki.Token(32), pki.UUID(), pki.Password(20), pki.Token(24)
	err := s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{
		"sub_token": u.SubToken, "uuid": u.UUID, "password": u.Password, "ss_seed": u.SSSeed}).Error
	if err != nil {
		return err
	}
	s.MarkDirty("credentials reset", s.nodesForUsers(u.ID)...)
	return nil
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
		s.st.DB.Model(&store.GroupNode{}).Where("group_id = ?", gs[i].ID).Pluck("node_id", &gs[i].NodeIDs)
		if gs[i].NodeIDs == nil {
			gs[i].NodeIDs = []string{}
		}
		s.st.DB.Model(&store.UserGroup{}).Where("group_id = ?", gs[i].ID).Count(&gs[i].UserCount)
	}
	c.JSON(http.StatusOK, gs)
}

type groupReq struct {
	Name       string   `json:"name" binding:"required"`
	Remark     string   `json:"remark"`
	InboundIDs []string `json:"inboundIds"`
	NodeIDs    []string `json:"nodeIds"` // nil = unchanged
	Level      *int     `json:"level"`   // nil = unchanged
	// The group's speed plan, Mbps (nil = unchanged, 0 = none).
	SpeedUpMbps   *int `json:"speedUpMbps"`
	SpeedDownMbps *int `json:"speedDownMbps"`
}

func (s *Server) saveGroup(c *gin.Context, g *store.Group, req *groupReq, create bool) {
	var before []string
	if !create {
		s.st.DB.Model(&store.GroupInbound{}).Where("group_id = ?", g.ID).Pluck("inbound_id", &before)
	}
	if req.Level != nil && (*req.Level < 0 || *req.Level > 99) {
		badRequest(c, fmt.Errorf("等级应在 0–99 之间"))
		return
	}
	speedChanged := (req.SpeedUpMbps != nil && max(*req.SpeedUpMbps, 0) != g.SpeedUpMbps) ||
		(req.SpeedDownMbps != nil && max(*req.SpeedDownMbps, 0) != g.SpeedDownMbps)
	if speedChanged && !s.requireFeature(c, license.FeatureSpeedLimit) {
		return
	}
	nodesChanged := req.NodeIDs != nil || (req.Level != nil && *req.Level != g.Level)
	g.Name, g.Remark = req.Name, req.Remark
	if req.Level != nil {
		g.Level = *req.Level
	}
	if req.SpeedUpMbps != nil && max(*req.SpeedUpMbps, 0) != g.SpeedUpMbps {
		g.SpeedUpMbps, nodesChanged = max(*req.SpeedUpMbps, 0), true
	}
	if req.SpeedDownMbps != nil && max(*req.SpeedDownMbps, 0) != g.SpeedDownMbps {
		g.SpeedDownMbps, nodesChanged = max(*req.SpeedDownMbps, 0), true
	}
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
		if req.NodeIDs != nil {
			if err := tx.Where("group_id = ?", g.ID).Delete(&store.GroupNode{}).Error; err != nil {
				return err
			}
			for _, id := range req.NodeIDs {
				if err := tx.Create(&store.GroupNode{GroupID: g.ID, NodeID: id}).Error; err != nil {
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
	if nodesChanged {
		s.MarkAllDirty("group " + g.Name + " node access changed")
	} else {
		s.MarkDirty("group "+g.Name+" changed", s.nodesForInbounds(append(before, req.InboundIDs...)...)...)
	}
	s.auditCtx(c, "group.save", g.Name, fmt.Sprintf("level %d, %d nodes, %d inbounds", g.Level, len(req.NodeIDs), len(req.InboundIDs)))
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
	var gnodes int64
	s.st.DB.Model(&store.GroupNode{}).Where("group_id = ?", g.ID).Count(&gnodes)
	s.st.DB.Where("group_id = ?", g.ID).Delete(&store.GroupInbound{})
	s.st.DB.Where("group_id = ?", g.ID).Delete(&store.GroupNode{})
	s.st.DB.Where("group_id = ?", g.ID).Delete(&store.UserGroup{})
	if g.Plan { // its users keep their limits, without a plan
		s.st.DB.Where("plan_id = ?", g.ID).Delete(&store.PlanNodeRate{})
		s.st.DB.Model(&store.User{}).Where("plan_id = ?", g.ID).Update("plan_id", "")
		s.st.DB.Model(&store.Group{}).Where("fallback_id = ?", g.ID).Update("fallback_id", "")
	}
	s.st.DB.Delete(&g)
	if g.Level > 0 || gnodes > 0 {
		s.MarkAllDirty("group deleted")
	} else {
		s.MarkDirty("group deleted", s.nodesForInbounds(ins...)...)
	}
	s.auditCtx(c, "group.delete", g.Name, "")
	c.Status(http.StatusNoContent)
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
