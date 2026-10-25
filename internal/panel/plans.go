package panel

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Plans are groups sold as packages, the way airports sell subscriptions:
// the group decides which servers a user reaches (level, nodes, inbounds,
// speed) and the plan fields set the account's traffic, validity and device
// limits when it is applied. A user is on at most one plan; ordinary groups
// and direct grants are left alone. On expiry a plan may hand the user to a
// fallback plan (a free tier) instead of stopping the account.

var planResetPolicies = []string{"", "none", "daily", "weekly", "monthly", "cycle"}

// plansOnly selects the ids of every plan group, for subqueries.
func plansOnly(tx *gorm.DB) *gorm.DB {
	return tx.Model(&store.Group{}).Select("id").Where("plan = ?", true)
}

// setPlanMembership makes planID ("" = none) the user's only plan group.
func setPlanMembership(tx *gorm.DB, userID, planID string) error {
	if err := tx.Where("user_id = ? AND group_id IN (?)", userID, plansOnly(tx)).Delete(&store.UserGroup{}).Error; err != nil {
		return err
	}
	if planID == "" {
		return nil
	}
	return tx.Create(&store.UserGroup{UserID: userID, GroupID: planID}).Error
}

// dropPlanGroups removes plan groups from a list of ordinary group ids: plan
// membership follows User.PlanID only.
func dropPlanGroups(tx *gorm.DB, ids []string) []string {
	if len(ids) == 0 {
		return ids
	}
	var plans []string
	tx.Model(&store.Group{}).Where("plan = ? AND id IN ?", true, ids).Pluck("id", &plans)
	return slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return slices.Contains(plans, id) })
}

// loadPlan reads a plan group by id.
func loadPlan(tx *gorm.DB, id string) (*store.Group, error) {
	var p store.Group
	if err := tx.First(&p, "id = ? AND plan = ?", id, true).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, commandInputError{"套餐不存在"}
		}
		return nil, err
	}
	return &p, nil
}

// applyPlan puts u on plan p. Renewing the plan the user is already on moves
// the expiry on from the current one and keeps the traffic used, unless the
// account had stopped (expired / out of traffic); anything else — a new
// plan, a change of plan, or switch — starts a fresh period from now with
// the traffic used cleared. The account's speed falls back to the plan's.
func applyPlan(tx *gorm.DB, u *store.User, p *store.Group, renew bool, now time.Time) (string, error) {
	renew = renew && u.PlanID == p.ID
	var exp *time.Time
	if p.Days > 0 {
		from := now
		if renew && u.ExpireAt != nil && u.ExpireAt.After(now) {
			from = *u.ExpireAt
		}
		e := from.AddDate(0, 0, p.Days)
		if e.Year() > 9999 {
			return "", commandInputError{"到期日期超出支持范围"}
		}
		exp = &e
	}
	reset := !renew || computeStatus(u, now) != "active"
	// Traffic packs last for the current period: a renewal keeps them, a
	// fresh period drops them.
	extra := u.ExtraTraffic
	if reset || p.TrafficLimit == 0 {
		extra = 0
	}
	limit := p.TrafficLimit
	if limit > 0 {
		limit += extra
	}
	upd := map[string]any{"plan_id": p.ID, "traffic_limit": limit, "extra_traffic": extra, "expire_at": exp, "expire_after_days": 0,
		"reset_policy": orDefault(p.ResetPolicy, "none"), "reset_day": p.ResetDay, "device_limit": p.DeviceLimit, "ip_limit": p.IPLimit,
		"speed_up_mbps": 0, "speed_down_mbps": 0, "warned_expire": nil}
	if reset {
		upd["used_up"], upd["used_down"], upd["last_reset_at"], upd["warned_usage"] = 0, 0, now, false
		if err := tx.Model(&store.UserNodeQuota{}).Where("user_id = ?", u.ID).Update("used", 0).Error; err != nil {
			return "", err
		}
	}
	if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Updates(upd).Error; err != nil {
		return "", err
	}
	if err := setPlanMembership(tx, u.ID, p.ID); err != nil {
		return "", err
	}
	u.PlanID, u.TrafficLimit, u.ExtraTraffic, u.ExpireAt, u.ExpireAfterDays = p.ID, limit, extra, exp, 0
	u.ResetPolicy, u.ResetDay, u.DeviceLimit, u.IPLimit = orDefault(p.ResetPolicy, "none"), p.ResetDay, p.DeviceLimit, p.IPLimit
	u.SpeedUpMbps, u.SpeedDownMbps = 0, 0
	if reset {
		u.UsedUp, u.UsedDown, u.LastResetAt = 0, 0, &now
	}
	verb := "开通"
	if renew {
		verb = "续费"
	}
	detail := fmt.Sprintf("套餐 %s %s，", p.Name, verb)
	if exp != nil {
		detail += "至 " + exp.Format("2006-01-02")
	} else {
		detail += "不限期"
	}
	if reset {
		detail += "，流量已清零"
	}
	return detail, nil
}

// --- API ---

// planView is a plan with its reach, for the admin UI and plugins.
type planView struct {
	store.Group
	FallbackName string `json:"fallbackName,omitempty"`
}

// listPlans is GET /plans.
func (s *Server) listPlans(c *gin.Context) {
	var ps []store.Group
	s.st.DB.Where("plan = ?", true).Order("sort, name").Find(&ps)
	names := map[string]string{}
	for _, p := range ps {
		names[p.ID] = p.Name
	}
	out := make([]planView, len(ps))
	for i := range ps {
		s.st.DB.Model(&store.GroupInbound{}).Where("group_id = ?", ps[i].ID).Pluck("inbound_id", &ps[i].InboundIDs)
		s.st.DB.Model(&store.GroupNode{}).Where("group_id = ?", ps[i].ID).Pluck("node_id", &ps[i].NodeIDs)
		ps[i].InboundIDs, ps[i].NodeIDs = nonNilStrs(ps[i].InboundIDs), nonNilStrs(ps[i].NodeIDs)
		s.st.DB.Model(&store.User{}).Where("plan_id = ?", ps[i].ID).Count(&ps[i].UserCount)
		ps[i].NodeRates = s.planNodeRates(ps[i].ID)
		out[i] = planView{Group: ps[i], FallbackName: names[ps[i].FallbackID]}
	}
	c.JSON(http.StatusOK, out)
}

// planReq is a plan's own fields on top of groupReq.
type planReq struct {
	groupReq
	TrafficLimit int64   `json:"trafficLimit"`
	Days         int     `json:"days"`
	ResetPolicy  string  `json:"resetPolicy"`
	ResetDay     int     `json:"resetDay"`
	DeviceLimit  int     `json:"deviceLimit"`
	IPLimit      int     `json:"ipLimit"`
	Price        string  `json:"price"`
	FallbackID   string  `json:"fallbackId"`
	Sort         int     `json:"sort"`
	Sale         bool    `json:"sale"`
	PriceStars   int     `json:"priceStars"`
	PriceCNY     float64 `json:"priceCny"`
	PriceUSDT    float64 `json:"priceUsdt"`
	// NodeRates replaces node multipliers for the plan's users (nil =
	// unchanged; professional edition).
	NodeRates map[string]float64 `json:"nodeRates"`
}

func (s *Server) savePlan(c *gin.Context, g *store.Group, create bool) {
	var req planReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	switch {
	case req.TrafficLimit < 0 || req.Days < 0 || req.Days > 3650 || req.DeviceLimit < 0 || req.IPLimit < 0:
		badRequest(c, errors.New("流量、天数（0–3650）、设备数和 IP 数不能为负"))
		return
	case !slices.Contains(planResetPolicies, req.ResetPolicy):
		badRequest(c, fmt.Errorf("未知的重置周期 %q", req.ResetPolicy))
		return
	case req.PriceStars < 0 || req.PriceStars > 100000 || req.PriceCNY < 0 || req.PriceCNY > 1e6 || req.PriceUSDT < 0 || req.PriceUSDT > 1e6:
		badRequest(c, errors.New("价格超出范围"))
		return
	case len(req.Price) > 32:
		badRequest(c, errors.New("售价最长 32 个字符"))
		return
	case req.FallbackID != "" && req.FallbackID == g.ID:
		badRequest(c, errors.New("到期降级不能指向套餐自己"))
		return
	}
	for _, r := range req.NodeRates {
		if r < 0 || r > 100 || math.IsNaN(r) {
			badRequest(c, errors.New("节点倍率应在 0–100 之间（0 = 用节点自己的倍率）"))
			return
		}
	}
	if req.NodeRates != nil {
		if !maps.Equal(nonZeroRates(req.NodeRates), s.planNodeRates(g.ID)) && !s.requireFeature(c, license.FeaturePlanAddons) {
			return
		}
		if err := s.st.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Where("plan_id = ?", g.ID).Delete(&store.PlanNodeRate{}).Error; err != nil {
				return err
			}
			for node, r := range nonZeroRates(req.NodeRates) {
				if err := tx.Create(&store.PlanNodeRate{PlanID: g.ID, NodeID: node, Rate: r}).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			dbError(c, err)
			return
		}
		g.NodeRates = nonZeroRates(req.NodeRates)
	}
	if req.FallbackID != "" {
		if _, err := loadPlan(s.st.DB, req.FallbackID); err != nil {
			badRequest(c, errors.New("到期降级的套餐不存在"))
			return
		}
	}
	g.Plan, g.IsDefault = true, false
	g.TrafficLimit, g.Days, g.ResetPolicy, g.ResetDay = req.TrafficLimit, req.Days, req.ResetPolicy, req.ResetDay
	g.DeviceLimit, g.IPLimit, g.Price, g.FallbackID, g.Sort = req.DeviceLimit, req.IPLimit, req.Price, req.FallbackID, req.Sort
	g.Sale, g.PriceStars = req.Sale, req.PriceStars
	g.PriceCNY, g.PriceUSDT = math.Round(req.PriceCNY*100)/100, math.Round(req.PriceUSDT*100)/100
	s.saveGroup(c, g, &req.groupReq, create)
}

// createPlan is POST /plans.
func (s *Server) createPlan(c *gin.Context) {
	s.savePlan(c, &store.Group{ID: store.NewID()}, true)
}

// updatePlan is PUT /plans/:id. Users already on the plan keep their
// current period's traffic and expiry; servers and speed follow the plan at
// once.
func (s *Server) updatePlan(c *gin.Context) {
	var g store.Group
	if err := s.st.DB.First(&g, "id = ? AND plan = ?", c.Param("id"), true).Error; err != nil {
		dbError(c, err)
		return
	}
	s.savePlan(c, &g, false)
}

// setUserPlan is POST /users/:id/plan {planId, mode}: mode "renew" (the
// default) extends the plan the user is on or starts the one given; "switch"
// always starts a fresh period. planId "" takes the user off their plan and
// keeps the limits they have.
func (s *Server) setUserPlan(c *gin.Context) {
	var req struct {
		PlanID string `json:"planId"`
		Mode   string `json:"mode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Mode != "" && req.Mode != "renew" && req.Mode != "switch" {
		badRequest(c, errors.New("mode 应为 renew 或 switch"))
		return
	}
	before := s.nodesForUsers(c.Param("id"))
	s.userCommand(c, "user.plan", func(tx *gorm.DB, u *store.User, now time.Time) (string, error) {
		if req.PlanID == "" {
			if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Update("plan_id", "").Error; err != nil {
				return "", err
			}
			u.PlanID = ""
			return "取消套餐", setPlanMembership(tx, u.ID, "")
		}
		p, err := loadPlan(tx, req.PlanID)
		if err != nil {
			return "", err
		}
		return applyPlan(tx, u, p, req.Mode != "switch", now)
	}, before...)
}

// planFallback hands an expired user on a plan to the plan's fallback. It
// reports whether it did.
func (s *Server) planFallback(u *store.User, now time.Time) bool {
	if u.PlanID == "" || u.Status == "disabled" || u.ExpireAt == nil || !now.After(*u.ExpireAt) {
		return false
	}
	var from store.Group
	if s.st.DB.First(&from, "id = ? AND plan = ?", u.PlanID, true).Error != nil || from.FallbackID == "" {
		return false
	}
	var detail string
	applied := false
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		// Re-read locked: a renewal bought since the snapshot must not be
		// replaced by the fallback plan.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(u, "id = ?", u.ID).Error; err != nil {
			return err
		}
		if u.PlanID != from.ID || u.Status == "disabled" || u.ExpireAt == nil || !now.After(*u.ExpireAt) {
			return nil
		}
		to, err := loadPlan(tx, from.FallbackID)
		if err != nil {
			return err
		}
		detail, err = applyPlan(tx, u, to, false, now)
		applied = err == nil
		return err
	})
	if err != nil {
		s.log.Warn("plan fallback failed", "user", u.Username, "err", err)
		return false
	}
	if !applied {
		return false
	}
	s.audit("system", "user.plan", u.Username, "到期降级："+from.Name+" → "+detail, "")
	s.userEvent("user.plan_changed", u, map[string]any{"plan_id": u.PlanID, "from_plan_id": from.ID, "reason": "expired", "detail": detail})
	return true
}

// userPlanID validates a requested plan id (nil = keep) and sets it on u.
// Only the membership follows; the limits are whatever the request carries.
func userPlanID(tx *gorm.DB, u *store.User, id *string) error {
	if id == nil {
		return nil
	}
	if *id != "" {
		if _, err := loadPlan(tx, *id); err != nil {
			return err
		}
	}
	u.PlanID = *id
	return nil
}

// setUserGroups adds u's ordinary groups and its plan group. The caller has
// removed the old memberships.
func setUserGroups(tx *gorm.DB, u *store.User, groupIDs []string) error {
	for _, g := range dropPlanGroups(tx, groupIDs) {
		if err := tx.Create(&store.UserGroup{UserID: u.ID, GroupID: g}).Error; err != nil {
			return err
		}
	}
	if u.PlanID == "" {
		return nil
	}
	return tx.Create(&store.UserGroup{UserID: u.ID, GroupID: u.PlanID}).Error
}

// planNames is the name of u's plan and of the plan it falls back to ("" when
// none).
func (s *Server) planNames(u *store.User) (name, next string) {
	if u.PlanID == "" {
		return "", ""
	}
	var p store.Group
	if s.st.DB.First(&p, "id = ? AND plan = ?", u.PlanID, true).Error != nil {
		return "", ""
	}
	var f store.Group
	if p.FallbackID != "" && s.st.DB.First(&f, "id = ? AND plan = ?", p.FallbackID, true).Error == nil {
		next = f.Name
	}
	return p.Name, next
}

// planAction puts a user on a plan outside an HTTP request (the Telegram
// admin menu): renew extends the plan the user is on, otherwise a fresh
// period starts. It rebuilds the affected nodes and records who did it.
func (s *Server) planAction(actor, userID, planID string, renew bool) (string, error) {
	before := s.nodesForUsers(userID)
	var u store.User
	var detail string
	now := time.Now()
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&u, "id = ? AND is_service = ?", userID, false).Error; err != nil {
			return err
		}
		p, err := loadPlan(tx, planID)
		if err != nil {
			return err
		}
		if detail, err = applyPlan(tx, &u, p, renew, now); err != nil {
			return err
		}
		u.Status = computeStatus(&u, now)
		return tx.Model(&store.User{}).Where("id = ?", u.ID).Update("status", u.Status).Error
	})
	if err != nil {
		return "", err
	}
	s.MarkDirty("user.plan", append(before, s.nodesForUsers(u.ID)...)...)
	s.audit(actor, "user.plan", u.Username, detail, "")
	s.userEvent("user.plan_changed", &u, map[string]any{"plan_id": u.PlanID, "detail": detail})
	return detail, nil
}

// planNodeRates reads a plan's own node multipliers.
func (s *Server) planNodeRates(planID string) map[string]float64 {
	var rs []store.PlanNodeRate
	s.st.DB.Where("plan_id = ?", planID).Find(&rs)
	out := map[string]float64{}
	for _, r := range rs {
		out[r.NodeID] = r.Rate
	}
	return out
}

func nonZeroRates(m map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range m {
		if v > 0 {
			out[k] = math.Round(v*100) / 100
		}
	}
	return out
}
