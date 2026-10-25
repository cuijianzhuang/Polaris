package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Coupons: a code entered in the web shop or the bot (/coupon CODE) takes a
// percentage or a fixed amount off a plan or traffic pack. Limits (total
// uses, uses per buyer) are checked when the order is opened, counting paid
// orders and open ones; a paid order always counts, even past the limit, so
// a payment is never refused for it. Refunds do not give a use back.

const payCoupon = "coupon" // an order the coupon made free

var couponCodeRe = regexp.MustCompile(`^[A-Z0-9_-]{3,32}$`)

func normCoupon(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

func couponPlans(c *store.Coupon) []string {
	var ids []string
	if c.PlanIDs != "" {
		_ = json.Unmarshal([]byte(c.PlanIDs), &ids)
	}
	return ids
}

// couponFor checks that code may be used by buyer b for a product (kind
// "plan" or "pack").
func (s *Server) couponFor(code, kind, productID string, b shopBuyer) (*store.Coupon, error) {
	code = normCoupon(code)
	var c store.Coupon
	if !couponCodeRe.MatchString(code) || s.st.DB.First(&c, "code = ?", code).Error != nil || !c.Enabled {
		return nil, errors.New("优惠码无效")
	}
	now := time.Now()
	if c.StartsAt != nil && now.Before(*c.StartsAt) {
		return nil, errors.New("优惠码还未到使用时间")
	}
	if c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
		return nil, errors.New("优惠码已过期")
	}
	switch kind {
	case kindPack:
		if !c.Packs {
			return nil, errors.New("这个优惠码不能用于流量包")
		}
	default:
		if !c.Plans {
			return nil, errors.New("这个优惠码不能用于套餐")
		}
		if ids := couponPlans(&c); len(ids) > 0 && !slices.Contains(ids, productID) {
			return nil, errors.New("这个优惠码不能用于该套餐")
		}
	}
	if c.MaxUses > 0 {
		var open int64
		s.st.DB.Model(&store.Order{}).Where("coupon_code = ? AND status = ? AND expires_at > ?", code, "pending", now).Count(&open)
		if int64(c.Used)+open >= int64(c.MaxUses) {
			return nil, errors.New("优惠码已被领完")
		}
	}
	if c.PerUser > 0 {
		if n := couponUsesBy(s.st.DB, code, b); n >= int64(c.PerUser) {
			return nil, errors.New("你已经用过这个优惠码")
		}
	}
	return &c, nil
}

// couponUsesBy counts buyer b's paid orders with the coupon. Pending ones
// are not counted: a repeated Stars tap reuses the buyer's open order.
func couponUsesBy(db *gorm.DB, code string, b shopBuyer) int64 {
	var conds []string
	var args []any
	if b.UserID != "" {
		conds, args = append(conds, "user_id = ?"), append(args, b.UserID)
	}
	if b.TGID != 0 {
		conds, args = append(conds, "telegram_id = ?"), append(args, b.TGID)
	}
	if b.WebKey != "" {
		conds, args = append(conds, "web_key = ?"), append(args, b.WebKey)
	}
	if len(conds) == 0 {
		return 0
	}
	var n int64
	db.Model(&store.CouponUse{}).Where("code = ?", code).Where("("+strings.Join(conds, " OR ")+")", args...).Count(&n)
	return n
}

var errCouponGone = errors.New("优惠码已被领完或你已用过")

// discounted applies coupon c to a product's prices.
func discounted(c *store.Coupon, stars int, cny, usdt float64) (int, float64, float64) {
	if c == nil {
		return stars, cny, usdt
	}
	if c.Percent > 0 {
		f := float64(100-min(c.Percent, 100)) / 100
		return int(math.Ceil(float64(stars) * f)), math.Round(cny*f*100) / 100, math.Round(usdt*f*1e4) / 1e4
	}
	return max(stars-c.OffStars, 0), math.Max(math.Round((cny-c.OffCNY)*100)/100, 0), math.Max(math.Round((usdt-c.OffUSDT)*1e4)/1e4, 0)
}

// couponPrice is what a product costs with method after coupon c (nil =
// none): the amount, the price before the coupon, and free when the coupon
// covers it all. ok is false when the product is not sold that way. A
// discounted price never goes below the method's smallest amount.
func (s *Server) couponPrice(c *store.Coupon, stars int, cny, usdt float64, method string) (amount, orig, currency string, usdtBase float64, free, ok bool) {
	orig, currency, ok = s.priceFor(stars, cny, usdt, method)
	if !ok {
		return
	}
	if c == nil {
		return orig, orig, currency, usdt, false, true
	}
	ds, dc, du := discounted(c, stars, cny, usdt)
	switch method {
	case payStars:
		if ds <= 0 {
			return "0", orig, currency, 0, true, true
		}
		amount = strconv.Itoa(ds)
	case payEPay:
		if dc <= 0 {
			return "0", orig, currency, 0, true, true
		}
		amount = fmt.Sprintf("%.2f", math.Max(dc, 0.01))
	case payUSDT:
		if du <= 0 {
			return "0", orig, currency, 0, true, true
		}
		du = math.Max(du, 0.01)
		amount = fmt.Sprintf("%.4f", du)
	}
	return amount, orig, currency, du, false, true
}

// couponLabel says what the coupon takes off ("8 折" / "减 ¥5 · 10 ⭐").
func couponLabel(c *store.Coupon) string {
	if c.Percent > 0 {
		if c.Percent >= 100 {
			return "免费"
		}
		return fmt.Sprintf("减 %d%%", c.Percent)
	}
	var parts []string
	if c.OffCNY > 0 {
		parts = append(parts, "¥"+strconv.FormatFloat(c.OffCNY, 'f', -1, 64))
	}
	if c.OffUSDT > 0 {
		parts = append(parts, strconv.FormatFloat(c.OffUSDT, 'f', -1, 64)+" USDT")
	}
	if c.OffStars > 0 {
		parts = append(parts, strconv.Itoa(c.OffStars)+" ⭐")
	}
	return "减 " + strings.Join(parts, " / ")
}

// openCouponOrder opens order o, or fulfils it at once when the coupon made
// it free.
func (s *Server) openCouponOrder(o *store.Order, usdtBase float64, free bool) (*store.Order, error) {
	if !free {
		return s.openShopOrder(o, usdtBase)
	}
	o.Method, o.Amount, o.Chain = payCoupon, "0", ""
	o.ExpiresAt = o.CreatedAt
	if err := s.st.DB.Create(o).Error; err != nil {
		return nil, err
	}
	// A non-nil pre: an undeliverable free order is removed, not kept as paid.
	paid, _, _, err := s.fulfil(o.ID, "coupon:"+o.ID, func(*gorm.DB) error { return nil })
	if err != nil {
		s.st.DB.Delete(&store.Order{}, "id = ? AND status = ?", o.ID, "pending")
		return nil, err
	}
	return paid, nil
}

// recordCouponUse counts paid order o against its coupon, inside fulfil's
// transaction. It never fails the payment.
func recordCouponUse(tx *gorm.DB, o *store.Order, now time.Time) error {
	if o.CouponCode == "" {
		return nil
	}
	use := store.CouponUse{ID: store.NewID(), Code: o.CouponCode, OrderID: o.ID, UserID: o.UserID, TelegramID: o.TelegramID, WebKey: o.WebKey, CreatedAt: now}
	if o.Method == payCoupon {
		// A free order is not paid for, so its limits are enforced here,
		// where concurrent claims serialise: the conditional UPDATE takes the
		// coupon row's lock before the per-buyer count runs.
		res := tx.Model(&store.Coupon{}).Where("code = ? AND (max_uses = 0 OR used < max_uses)", o.CouponCode).UpdateColumn("used", gorm.Expr("used + 1"))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errCouponGone
		}
		var c store.Coupon
		if err := tx.First(&c, "code = ?", o.CouponCode).Error; err != nil {
			return err
		}
		if c.PerUser > 0 && couponUsesBy(tx, o.CouponCode, shopBuyer{UserID: o.UserID, TGID: o.TelegramID, WebKey: o.WebKey}) >= int64(c.PerUser) {
			return errCouponGone
		}
		return tx.Create(&use).Error
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&use).Error; err != nil {
		return err
	}
	return tx.Model(&store.Coupon{}).Where("code = ?", o.CouponCode).UpdateColumn("used", gorm.Expr("used + 1")).Error
}

// --- admin API ---

type couponReq struct {
	Code      string     `json:"code"`
	Prefix    string     `json:"prefix"` // with count: generate codes
	Count     int        `json:"count"`
	Name      string     `json:"name"`
	Percent   int        `json:"percent"`
	OffCNY    float64    `json:"offCny"`
	OffUSDT   float64    `json:"offUsdt"`
	OffStars  int        `json:"offStars"`
	PlanIDs   []string   `json:"planIds"`
	Plans     *bool      `json:"plans"`
	Packs     bool       `json:"packs"`
	MaxUses   int        `json:"maxUses"`
	PerUser   *int       `json:"perUser"`
	StartsAt  *time.Time `json:"startsAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
	Enabled   *bool      `json:"enabled"`
}

// apply validates the request into c.
func (s *Server) applyCouponReq(r *couponReq, c *store.Coupon) error {
	if len(r.Name) > 64 {
		return errors.New("名称最长 64 个字符")
	}
	switch {
	case r.Percent < 0 || r.Percent > 100:
		return errors.New("折扣比例为 1–100")
	case r.OffCNY < 0 || r.OffUSDT < 0 || r.OffStars < 0:
		return errors.New("减免金额不能为负")
	case r.Percent == 0 && r.OffCNY == 0 && r.OffUSDT == 0 && r.OffStars == 0:
		return errors.New("请填写折扣比例或减免金额")
	case r.Percent > 0 && (r.OffCNY > 0 || r.OffUSDT > 0 || r.OffStars > 0):
		return errors.New("折扣比例和减免金额只能选一种")
	case r.MaxUses < 0 || r.MaxUses > 1e7:
		return errors.New("总次数无效")
	case r.StartsAt != nil && r.ExpiresAt != nil && !r.ExpiresAt.After(*r.StartsAt):
		return errors.New("结束时间要晚于开始时间")
	}
	plans := r.Plans == nil || *r.Plans
	if !plans && !r.Packs {
		return errors.New("请选择适用于套餐或流量包")
	}
	for _, id := range r.PlanIDs {
		if _, err := loadPlan(s.st.DB, id); err != nil {
			return err
		}
	}
	c.Name, c.Percent, c.OffCNY, c.OffUSDT, c.OffStars = strings.TrimSpace(r.Name), r.Percent, r.OffCNY, r.OffUSDT, r.OffStars
	c.PlanIDs = ""
	if len(r.PlanIDs) > 0 {
		b, _ := json.Marshal(r.PlanIDs)
		c.PlanIDs = string(b)
	}
	c.Plans, c.Packs, c.MaxUses = plans, r.Packs, r.MaxUses
	c.PerUser = 1
	if r.PerUser != nil {
		if *r.PerUser < 0 {
			return errors.New("每人次数无效")
		}
		c.PerUser = *r.PerUser
	}
	c.StartsAt, c.ExpiresAt = r.StartsAt, r.ExpiresAt
	c.Enabled = r.Enabled == nil || *r.Enabled
	return nil
}

// listCoupons is GET /coupons.
func (s *Server) listCoupons(c *gin.Context) {
	var cs []store.Coupon
	s.st.DB.Order("created_at desc, code").Limit(2000).Find(&cs)
	for i := range cs {
		cs[i].PlanList = nonNilStrs(couponPlans(&cs[i]))
	}
	c.JSON(http.StatusOK, gin.H{"items": cs})
}

// createCoupons is POST /coupons: one code, or count random codes with a
// prefix.
func (s *Server) createCoupons(c *gin.Context) {
	var r couponReq
	if err := c.ShouldBindJSON(&r); err != nil {
		badRequest(c, err)
		return
	}
	base := store.Coupon{CreatedBy: c.GetString("admin"), CreatedAt: time.Now()}
	if err := s.applyCouponReq(&r, &base); err != nil {
		badRequest(c, err)
		return
	}
	var codes []string
	if r.Count > 0 {
		prefix := normCoupon(r.Prefix)
		if r.Count > 1000 || len(prefix) > 16 || (prefix != "" && !couponCodeRe.MatchString(prefix+"XXX")) {
			badRequest(c, errors.New("数量 1–1000，前缀最长 16 个字母或数字"))
			return
		}
		for range r.Count {
			codes = append(codes, prefix+strings.ReplaceAll(newRedeemCode(), "-", "")[:10])
		}
	} else {
		code := normCoupon(r.Code)
		if !couponCodeRe.MatchString(code) {
			badRequest(c, errors.New("优惠码为 3–32 位字母、数字、- 或 _"))
			return
		}
		codes = []string{code}
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		for _, code := range codes {
			row := base
			row.Code = code
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if len(codes) == 1 {
			badRequest(c, errors.New("优惠码已存在"))
			return
		}
		dbError(c, err)
		return
	}
	s.auditCtx(c, "coupon.create", strings.Join(codes[:min(len(codes), 5)], ","), fmt.Sprintf("%d codes, %s", len(codes), couponLabel(&base)))
	c.JSON(http.StatusOK, gin.H{"codes": codes})
}

// updateCoupon is PUT /coupons/:code.
func (s *Server) updateCoupon(c *gin.Context) {
	var cp store.Coupon
	if s.st.DB.First(&cp, "code = ?", c.Param("code")).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var r couponReq
	if err := c.ShouldBindJSON(&r); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.applyCouponReq(&r, &cp); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.st.DB.Select("*").Omit("code", "used", "created_by", "created_at").Save(&cp).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "coupon.update", cp.Code, couponLabel(&cp))
	c.JSON(http.StatusOK, cp)
}

// deleteCoupon is DELETE /coupons/:code. Orders keep the code they used.
func (s *Server) deleteCoupon(c *gin.Context) {
	code := c.Param("code")
	if err := s.st.DB.Delete(&store.Coupon{}, "code = ?", code).Error; err != nil {
		dbError(c, err)
		return
	}
	s.st.DB.Delete(&store.CouponUse{}, "code = ?", code)
	s.auditCtx(c, "coupon.delete", code, "")
	c.Status(http.StatusNoContent)
}

// couponUses is GET /coupons/:code/uses: the paid orders that used it.
func (s *Server) couponUses(c *gin.Context) {
	var os []store.Order
	s.st.DB.Where("coupon_code = ? AND status IN ?", c.Param("code"), []string{"paid", "refunded", "failed"}).Order("created_at desc").Limit(500).Find(&os)
	names := map[string]string{}
	var ids []string
	for _, o := range os {
		if o.UserID != "" {
			ids = append(ids, o.UserID)
		}
	}
	if len(ids) > 0 {
		var us []store.User
		s.st.DB.Select("id, username").Where("id IN ?", ids).Find(&us)
		for _, u := range us {
			names[u.ID] = u.Username
		}
	}
	c.JSON(http.StatusOK, gin.H{"orders": os, "usernames": names})
}

// --- web shop ---

type couponItem struct {
	EPay  string `json:"epay,omitempty"`
	USDT  string `json:"usdt,omitempty"`
	Free  bool   `json:"free,omitempty"`
	Error string `json:"error,omitempty"`
}

type couponQuote struct {
	OK    bool                  `json:"ok"`
	Error string                `json:"error,omitempty"`
	Code  string                `json:"code,omitempty"`
	Label string                `json:"label,omitempty"`
	Items map[string]couponItem `json:"items,omitempty"` // "plan:<id>" / "pack:<id>"
}

// quoteCoupon prices every product of the web shop with a coupon.
func (s *Server) quoteCoupon(code string, b shopBuyer, u *store.User) couponQuote {
	code = normCoupon(code)
	var cp store.Coupon
	if !couponCodeRe.MatchString(code) || s.st.DB.First(&cp, "code = ?", code).Error != nil || !cp.Enabled {
		return couponQuote{Error: "优惠码无效"}
	}
	q := couponQuote{Code: code, Label: couponLabel(&cp), Items: map[string]couponItem{}}
	price := func(key, kind, id string, stars int, cny, usdt float64) {
		c, err := s.couponFor(code, kind, id, b)
		if err != nil {
			q.Items[key] = couponItem{Error: err.Error()}
			return
		}
		var it couponItem
		if a, _, _, _, free, ok := s.couponPrice(c, stars, cny, usdt, payEPay); ok {
			it.EPay, it.Free = a, free
		}
		if a, _, _, _, free, ok := s.couponPrice(c, stars, cny, usdt, payUSDT); ok {
			it.USDT, it.Free = strings.TrimRight(strings.TrimRight(a, "0"), "."), it.Free || free
		}
		q.Items[key] = it
		q.OK = true
	}
	for _, p := range s.salePlans() {
		price("plan:"+p.ID, "plan", p.ID, p.PriceStars, p.PriceCNY, p.PriceUSDT)
	}
	for _, p := range s.salePacks(u) {
		price("pack:"+p.ID, kindPack, p.ID, p.PriceStars, p.PriceCNY, p.PriceUSDT)
	}
	if !q.OK {
		q.Error = "这个优惠码不能用于在售的商品"
		for _, it := range q.Items {
			q.Error = it.Error // the reason for one of them
			break
		}
	}
	return q
}

// webShopCoupon is GET /u/shop/coupon?code=.
func (s *Server) webShopCoupon(c *gin.Context) {
	if !s.webShopOn() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "商店暂未开放"})
		return
	}
	if !s.webShopLimit(c) {
		return
	}
	u := s.portalUser(c)
	b, _ := s.shopBuyerOf(c, u)
	c.JSON(http.StatusOK, s.quoteCoupon(c.Query("code"), b, u))
}

// --- bot ---

const tgCouponTTL = 30 * time.Minute

func tgCouponKey(chat int64) string { return "tgcoupon:" + strconv.FormatInt(chat, 10) }

// tgCoupon is the coupon the chat entered with /coupon ("" = none).
func (s *Server) tgCoupon(chat int64) string {
	var code string
	if s.eph.get(tgCouponKey(chat), &code) {
		return code
	}
	return ""
}

func couponHelp() string {
	return "🏷 <b>优惠码</b>\n" + tgRule + "\n发送：\n<code>/coupon 优惠码</code>\n\n之后 30 分钟内购买套餐或流量包时自动使用。发送 <code>/coupon off</code> 取消。"
}

// couponCommand handles /coupon <code> | off.
func (b *tgBot) couponCommand(chat int64, arg string, id tgIdentity) {
	back := []button{{Text: "🛒 去购买", Data: "v:shop"}, btnHome}
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "":
		b.send(chat, couponHelp(), back)
		return
	case "off", "取消":
		var old string
		b.s.eph.take(tgCouponKey(chat), &old)
		b.send(chat, "已取消优惠码。", back)
		return
	}
	buyer := shopBuyer{TGID: chat}
	if id.user != nil {
		buyer.UserID = id.user.ID
	}
	code := normCoupon(arg)
	var cp store.Coupon
	if !couponCodeRe.MatchString(code) || b.s.st.DB.First(&cp, "code = ?", code).Error != nil || !cp.Enabled {
		b.send(chat, "❌ 优惠码无效", back)
		return
	}
	// The product is not known yet: check what does not depend on it.
	kind, product := "plan", ""
	if !cp.Plans {
		kind = kindPack
	} else if ids := couponPlans(&cp); len(ids) > 0 {
		product = ids[0]
	}
	if _, err := b.s.couponFor(code, kind, product, buyer); err != nil {
		b.send(chat, "❌ "+esc(err.Error()), back)
		return
	}
	if err := b.s.eph.put(tgCouponKey(chat), code, tgCouponTTL); err != nil {
		b.send(chat, "❌ "+esc(err.Error()), back)
		return
	}
	scope := "全部套餐"
	if ids := couponPlans(&cp); len(ids) > 0 {
		var names []string
		b.s.st.DB.Model(&store.Group{}).Where("id IN ?", ids).Pluck("name", &names)
		scope = strings.Join(names, "、")
	}
	if !cp.Plans {
		scope = ""
	}
	if cp.Packs {
		scope = strings.Trim(scope+"、流量包", "、")
	}
	b.send(chat, fmt.Sprintf("🏷 已使用优惠码 <code>%s</code>（%s）\n适用：%s\n30 分钟内下单自动抵扣，价格按钮会显示优惠后的价格。", esc(code), esc(couponLabel(&cp)), esc(scope)), back)
}

// botCoupon is the chat's coupon if it applies to the product, else nil;
// note explains why it does not (shown on the card).
func (s *Server) botCoupon(chat int64, kind, productID string, b shopBuyer) (cp *store.Coupon, code, note string) {
	code = s.tgCoupon(chat)
	if code == "" {
		return nil, "", ""
	}
	cp, err := s.couponFor(code, kind, productID, b)
	if err != nil {
		return nil, code, err.Error()
	}
	return cp, code, ""
}
