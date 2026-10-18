package panel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// The shop sells plans in the Telegram bot. An order is created when a user
// picks a plan and a way to pay; the payment (Telegram Stars, EPay, USDT on
// TRON, or a redeem code) fulfils it exactly once: the order flips to paid
// with the provider's transaction id (unique), then the plan is applied —
// renewing the buyer's plan or starting a new one — and, for a Telegram user
// without an account, an account is created and bound.

const (
	payStars  = "stars"
	payEPay   = "epay"
	payUSDT   = "usdt"
	payCode   = "code"
	payManual = "manual"

	maxPendingOrders = 5
)

var errShopClosed = errors.New("商店未开启")

func (s *Server) shopOn() bool { return s.st.GetSetting("shop_enabled", "false") == "true" }

func (s *Server) shopAutoCreate() bool { return s.st.GetSetting("shop_auto_create", "true") == "true" }

func (s *Server) orderTTL() time.Duration {
	m, err := strconv.Atoi(s.st.GetSetting("shop_order_minutes", "30"))
	if err != nil || m < 5 || m > 1440 {
		m = 30
	}
	return time.Duration(m) * time.Minute
}

// payMethods lists the ways to pay that are configured (EPay types expanded).
func (s *Server) starsOn() bool { return s.st.GetSetting("shop_stars", "false") == "true" }

func (s *Server) epayOn() bool {
	return s.st.GetSetting("shop_epay_url", "") != "" && s.st.GetSetting("shop_epay_pid", "") != "" && s.st.GetSetting("shop_epay_key", "") != ""
}

func (s *Server) epayTypes() []string {
	var out []string
	for _, t := range strings.Split(s.st.GetSetting("shop_epay_types", "alipay,wxpay"), ",") {
		if t = strings.TrimSpace(t); t != "" && len(t) <= 16 {
			out = append(out, t)
		}
	}
	return out
}

func (s *Server) usdtOn() bool { return s.st.GetSetting("shop_usdt_address", "") != "" }

// salePlans lists the plans on sale, in display order.
func (s *Server) salePlans() []store.Group {
	var ps []store.Group
	s.st.DB.Where("plan = ? AND sale = ?", true, true).Order("sort, name").Find(&ps)
	return ps
}

// orderPrice is what plan p costs with method, as decimal text and currency;
// ok is false when it is not sold that way.
func (s *Server) orderPrice(p *store.Group, method string) (amount, currency string, ok bool) {
	switch method {
	case payStars:
		return strconv.Itoa(p.PriceStars), "XTR", s.starsOn() && p.PriceStars > 0
	case payEPay:
		return fmt.Sprintf("%.2f", p.PriceCNY), "CNY", s.epayOn() && p.PriceCNY >= 0.01
	case payUSDT:
		return fmt.Sprintf("%.4f", p.PriceUSDT), "USDT", s.usdtOn() && p.PriceUSDT >= 0.01
	}
	return "", "", false
}

// newOrder opens an order for a Telegram user. userID is the bound account
// ("" = none yet).
func (s *Server) newOrder(tgID int64, tgName, userID, planID, method string) (*store.Order, error) {
	if !s.shopOn() {
		return nil, errShopClosed
	}
	if userID == "" && !s.shopAutoCreate() {
		return nil, errors.New("请先绑定账号再购买")
	}
	var p store.Group
	if s.st.DB.First(&p, "id = ? AND plan = ? AND sale = ?", planID, true, true).Error != nil {
		return nil, errors.New("这个套餐已下架")
	}
	amount, currency, ok := s.orderPrice(&p, method)
	if !ok {
		return nil, errors.New("这个套餐不支持该付款方式")
	}
	now := time.Now()
	var pending int64
	s.st.DB.Model(&store.Order{}).Where("telegram_id = ? AND status = ? AND expires_at > ?", tgID, "pending", now).Count(&pending)
	if pending >= maxPendingOrders {
		return nil, errors.New("未支付的订单太多，请先完成或等待过期")
	}
	if method == payUSDT {
		var err error
		if amount, err = s.uniqueUSDTAmount(p.PriceUSDT, now); err != nil {
			return nil, err
		}
	}
	o := &store.Order{ID: store.NewID(), TelegramID: tgID, TGName: truncate(tgName, 64), UserID: userID, PlanID: p.ID, PlanName: p.Name,
		Method: method, Amount: amount, Currency: currency, Status: "pending", CreatedAt: now, ExpiresAt: now.Add(s.orderTTL())}
	if err := s.st.DB.Create(o).Error; err != nil {
		return nil, err
	}
	return o, nil
}

// uniqueUSDTAmount adds 0.0001–0.0500 to the price so each recent
// USDT order has its own amount: a transfer is matched to its order by
// amount alone.
func (s *Server) uniqueUSDTAmount(price float64, now time.Time) (string, error) {
	var used []string
	s.st.DB.Model(&store.Order{}).Where("method = ? AND status IN ? AND created_at > ?", payUSDT, []string{"pending", "expired"}, now.Add(-usdtLookback)).
		Pluck("amount", &used)
	base := int64(math.Round(price * 1e4))
	for range 50 {
		n, _ := rand.Int(rand.Reader, big.NewInt(500)) // at most 0.05 USDT over the price
		a := fmt.Sprintf("%.4f", float64(base+n.Int64()+1)/1e4)
		if !slices.Contains(used, a) {
			return a, nil
		}
	}
	return "", errors.New("同一时间的 USDT 订单太多，请稍后再试")
}

// fulfil marks order id paid with the provider's reference and applies its
// plan. pre runs first in the same transaction (a redeem code claims itself
// there). A payment that was already recorded returns the order unchanged
// with fresh = false.
func (s *Server) fulfil(id, payRef string, pre func(tx *gorm.DB) error) (o *store.Order, u *store.User, fresh bool, err error) {
	var p store.Group
	var before []string
	var created bool
	var detail string
	now := time.Now()
	o, u = &store.Order{}, &store.User{}
	if s.st.DB.First(o, "id = ?", id).Error == nil && o.UserID != "" {
		before = s.nodesForUsers(o.UserID)
	}
	// Settings are read before the transaction: SQLite has one connection.
	autoCreate := s.shopAutoCreate()
	prefix := orDefault(strings.TrimSpace(s.st.GetSetting("shop_user_prefix", "tg")), "tg")
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if pre != nil {
			if err := pre(tx); err != nil {
				return err
			}
		}
		res := tx.Model(&store.Order{}).Where("id = ? AND status IN ?", id, []string{"pending", "expired"}).
			Updates(map[string]any{"status": "paid", "pay_ref": payRef, "paid_at": now})
		if res.Error != nil {
			return res.Error
		}
		if err := tx.First(o, "id = ?", id).Error; err != nil {
			return err
		}
		if res.RowsAffected == 0 {
			return nil // already paid
		}
		fresh = true
		if err := tx.First(&p, "id = ? AND plan = ?", o.PlanID, true).Error; err != nil {
			return commandInputError{"套餐 " + o.PlanName + " 已删除，请联系管理员处理"}
		}
		switch {
		case o.UserID != "" && tx.First(u, "id = ? AND is_service = ?", o.UserID, false).Error == nil:
		case o.TelegramID != 0 && tx.First(u, "telegram_id = ? AND is_service = ?", o.TelegramID, false).Error == nil:
		default:
			if !autoCreate {
				return commandInputError{"没有可开通的账号"}
			}
			nu, err := shopNewUser(tx, o, prefix)
			if err != nil {
				return err
			}
			u, created = nu, true
		}
		var err error
		if detail, err = applyPlan(tx, u, &p, true, now); err != nil {
			return err
		}
		u.Status = computeStatus(u, now)
		if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Update("status", u.Status).Error; err != nil {
			return err
		}
		o.UserID, o.Detail = u.ID, truncate(detail, 255)
		return tx.Model(&store.Order{}).Where("id = ?", id).Updates(map[string]any{"user_id": u.ID, "detail": o.Detail}).Error
	})
	if err != nil {
		var input commandInputError
		if errors.As(err, &input) && o.ID != "" && pre == nil {
			// Paid for something that cannot be delivered: keep the payment
			// on record for the admin to settle.
			s.st.DB.Model(&store.Order{}).Where("id = ? AND status IN ?", id, []string{"pending", "expired"}).
				Updates(map[string]any{"status": "failed", "pay_ref": payRef, "paid_at": now, "detail": truncate(err.Error(), 255)})
			s.notify(Event{Kind: "order.failed", Text: fmt.Sprintf("❗ 订单 %s 已付款但无法开通：%s", id, err.Error()), Data: map[string]any{"order": id}})
		}
		return o, nil, false, err
	}
	if !fresh {
		if o.UserID != "" {
			s.st.DB.First(u, "id = ?", o.UserID)
		}
		return o, u, false, nil
	}
	s.MarkDirty("order paid", append(before, s.nodesForUsers(u.ID)...)...)
	who := o.TGName
	if who == "" {
		who = strconv.FormatInt(o.TelegramID, 10)
	}
	s.audit("shop", "order.paid", u.Username, fmt.Sprintf("%s %s %s · %s · %s", o.ID, o.Amount, o.Currency, o.Method, detail), "")
	if created {
		s.userEvent("user.created", u, map[string]any{"via": "shop"})
	}
	s.userEvent("user.plan_changed", u, map[string]any{"plan_id": u.PlanID, "order_id": o.ID, "detail": detail})
	s.notify(Event{Kind: "order.paid", Text: fmt.Sprintf("💰 新订单：%s 购买 %s（%s %s，%s）", who, o.PlanName, o.Amount, o.Currency, methodName[o.Method]),
		Data: map[string]any{"order": orderBrief(o), "user": userBrief(u)}})
	go s.tg.orderDelivered(o, u, created)
	return o, u, true, nil
}

var methodName = map[string]string{payStars: "Telegram Stars", payEPay: "易支付", payUSDT: "USDT", payCode: "兑换码", payManual: "手动"}

func orderBrief(o *store.Order) map[string]any {
	return map[string]any{"id": o.ID, "plan_id": o.PlanID, "plan_name": o.PlanName, "method": o.Method, "amount": o.Amount,
		"currency": o.Currency, "telegram_id": o.TelegramID, "user_id": o.UserID}
}

// shopNewUser creates and binds an account for a Telegram buyer.
func shopNewUser(tx *gorm.DB, o *store.Order, prefix string) (*store.User, error) {
	if o.TelegramID == 0 {
		return nil, commandInputError{"没有可开通的账号"}
	}
	name := prefix + strconv.FormatInt(o.TelegramID, 10)
	var n int64
	tx.Model(&store.User{}).Where("username = ?", name).Count(&n)
	if n > 0 {
		name += "_" + strings.ToLower(pki.Token(4))
	}
	u := newUser(&userReq{Username: name, Remark: "Telegram 购买 " + o.TGName})
	u.TelegramID = o.TelegramID
	if err := tx.Model(&store.User{}).Where("telegram_id = ?", o.TelegramID).Update("telegram_id", 0).Error; err != nil {
		return nil, err
	}
	if err := tx.Create(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// redeem spends a code for a Telegram user.
func (s *Server) redeem(tgID int64, tgName, userID, code string) (*store.Order, *store.User, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" || len(code) > 32 {
		return nil, nil, errors.New("兑换码无效")
	}
	if userID == "" && !s.shopAutoCreate() {
		return nil, nil, errors.New("请先绑定账号再兑换")
	}
	var rc store.RedeemCode
	if s.st.DB.First(&rc, "code = ?", code).Error != nil {
		return nil, nil, errors.New("兑换码无效")
	}
	var p store.Group
	if s.st.DB.First(&p, "id = ? AND plan = ?", rc.PlanID, true).Error != nil {
		return nil, nil, errors.New("兑换码对应的套餐已删除，请联系管理员")
	}
	now := time.Now()
	o := &store.Order{ID: store.NewID(), TelegramID: tgID, TGName: truncate(tgName, 64), UserID: userID, PlanID: p.ID, PlanName: p.Name,
		Method: payCode, Amount: code, Status: "pending", CreatedAt: now, ExpiresAt: now}
	if err := s.st.DB.Create(o).Error; err != nil {
		return nil, nil, err
	}
	o, u, _, err := s.fulfil(o.ID, "code:"+code, func(tx *gorm.DB) error {
		res := tx.Model(&store.RedeemCode{}).Where("code = ? AND used_at IS NULL AND (expires_at IS NULL OR expires_at > ?)", code, now).
			Updates(map[string]any{"used_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return commandInputError{"兑换码已被使用或已过期"}
		}
		return nil
	})
	if err != nil {
		s.st.DB.Delete(&store.Order{}, "id = ? AND status = ?", o.ID, "pending")
		return nil, nil, err
	}
	s.st.DB.Model(&store.RedeemCode{}).Where("code = ?", code).Update("used_by", u.ID)
	return o, u, nil
}

// expireOrders closes unpaid orders past their time. A payment arriving
// later still fulfils the order.
func (s *Server) expireOrders() {
	s.st.DB.Model(&store.Order{}).Where("status = ? AND expires_at < ?", "pending", time.Now()).Update("status", "expired")
}

// --- admin API ---

// listOrders is GET /orders?status=&method=&q=&limit=&offset=.
func (s *Server) listOrders(c *gin.Context) {
	q := s.st.DB.Model(&store.Order{})
	if v := c.Query("status"); v != "" {
		q = q.Where("status = ?", v)
	}
	if v := c.Query("method"); v != "" {
		q = q.Where("method = ?", v)
	}
	if v := strings.TrimSpace(c.Query("q")); v != "" {
		like := "%" + v + "%"
		q = q.Where("id = ? OR tg_name LIKE ? OR plan_name LIKE ? OR user_id IN (?)", v, like, like,
			s.st.DB.Model(&store.User{}).Select("id").Where("username LIKE ?", like))
	}
	var total int64
	q.Count(&total)
	limit := min(max(atoiDefault(c.Query("limit"), 50), 1), 500)
	var os []store.Order
	q.Order("created_at desc").Limit(limit).Offset(max(atoiDefault(c.Query("offset"), 0), 0)).Find(&os)
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
	// Revenue of the last 30 days per currency.
	type sum struct {
		Currency string  `json:"currency"`
		Total    float64 `json:"total"`
		Count    int64   `json:"count"`
	}
	var sums []sum
	var paid []store.Order
	s.st.DB.Where("status = ? AND paid_at > ? AND method IN ?", "paid", time.Now().AddDate(0, 0, -30), []string{payStars, payEPay, payUSDT}).Find(&paid)
	for _, o := range paid {
		v, _ := strconv.ParseFloat(o.Amount, 64)
		i := slices.IndexFunc(sums, func(x sum) bool { return x.Currency == o.Currency })
		if i < 0 {
			sums, i = append(sums, sum{Currency: o.Currency}), len(sums)
		}
		sums[i].Total += v
		sums[i].Count++
	}
	c.JSON(http.StatusOK, gin.H{"items": os, "total": total, "usernames": names, "revenue": sums, "status": s.shopStatus()})
}

// ordersToday counts today's paid orders (panel local time) and sums them
// per currency; redeem codes count but add no revenue.
func (s *Server) ordersToday() (n int, revenue map[string]float64) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var os []store.Order
	s.st.DB.Where("status = ? AND paid_at >= ?", "paid", start).Find(&os)
	revenue = map[string]float64{}
	for _, o := range os {
		if o.Currency != "" {
			v, _ := strconv.ParseFloat(o.Amount, 64)
			revenue[o.Currency] += v
		}
	}
	return len(os), revenue
}

// shopStatus tells the admin UI what is set up.
func (s *Server) shopStatus() gin.H {
	return gin.H{"enabled": s.shopOn(), "stars": s.starsOn(), "epay": s.epayOn(), "usdt": s.usdtOn(), "bot": s.tg.username(),
		"botRunning": s.tg.isRunning(), "notifyUrl": s.epayNotifyURL(), "publicUrl": s.cfg.PublicURL != ""}
}

// fulfilOrder is POST /orders/:id/fulfil: the admin confirms a payment the
// panel did not see (e.g. a lost EPay callback).
func (s *Server) fulfilOrder(c *gin.Context) {
	ref := payManual + ":" + c.GetString("admin") + ":" + c.Param("id")
	o, u, fresh, err := s.fulfil(c.Param("id"), ref, nil)
	if err != nil {
		dbError(c, err)
		return
	}
	if !fresh && o.Status != "paid" {
		badRequest(c, errors.New("订单状态为 "+o.Status+"，无法开通"))
		return
	}
	s.auditCtx(c, "order.fulfil", o.ID, "manual")
	c.JSON(http.StatusOK, gin.H{"order": o, "user": u, "fresh": fresh})
}

// refundOrder is POST /orders/:id/refund: returns the Stars of a paid
// Telegram Stars order. The plan it started is left as it is; adjust the
// user if the refund should take it back.
func (s *Server) refundOrder(c *gin.Context) {
	var o store.Order
	if err := s.st.DB.First(&o, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if o.Method != payStars || o.Status != "paid" || o.PayRef == nil || !strings.HasPrefix(*o.PayRef, "stars:") {
		badRequest(c, errors.New("只能退款已支付的 Telegram Stars 订单"))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	if err := s.tg.call(ctx, "refundStarPayment", map[string]any{"user_id": o.TelegramID,
		"telegram_payment_charge_id": strings.TrimPrefix(*o.PayRef, "stars:")}, nil); err != nil {
		badRequest(c, fmt.Errorf("Telegram 退款失败：%v", err))
		return
	}
	s.st.DB.Model(&store.Order{}).Where("id = ?", o.ID).Update("status", "refunded")
	s.auditCtx(c, "order.refund", o.ID, o.Amount+" XTR")
	if s.tg.isRunning() {
		s.tg.send(o.TelegramID, fmt.Sprintf("↩️ 订单 <code>%s</code> 已退款 %s ⭐", o.ID, o.Amount))
	}
	o.Status = "refunded"
	c.JSON(http.StatusOK, o)
}

// listCodes is GET /redeem-codes?batch=&used=.
func (s *Server) listCodes(c *gin.Context) {
	q := s.st.DB.Model(&store.RedeemCode{})
	if v := c.Query("batch"); v != "" {
		q = q.Where("batch = ?", v)
	}
	switch c.Query("used") {
	case "yes":
		q = q.Where("used_at IS NOT NULL")
	case "no":
		q = q.Where("used_at IS NULL")
	}
	var total int64
	q.Count(&total)
	var cs []store.RedeemCode
	q.Order("created_at desc, code").Limit(min(max(atoiDefault(c.Query("limit"), 200), 1), 2000)).Find(&cs)
	var batches []string
	s.st.DB.Model(&store.RedeemCode{}).Distinct().Where("batch <> ''").Order("batch").Pluck("batch", &batches)
	c.JSON(http.StatusOK, gin.H{"items": cs, "total": total, "batches": nonNilStrs(batches)})
}

// createCodes is POST /redeem-codes {planId, count, batch, expireDays}.
func (s *Server) createCodes(c *gin.Context) {
	var req struct {
		PlanID     string `json:"planId" binding:"required"`
		Count      int    `json:"count"`
		Batch      string `json:"batch"`
		ExpireDays int    `json:"expireDays"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Count < 1 || req.Count > 1000 || req.ExpireDays < 0 || req.ExpireDays > 3650 || len(req.Batch) > 64 {
		badRequest(c, errors.New("数量 1–1000，有效天数 0–3650，批次名最长 64 个字符"))
		return
	}
	p, err := loadPlan(s.st.DB, req.PlanID)
	if err != nil {
		badRequest(c, err)
		return
	}
	var exp *time.Time
	if req.ExpireDays > 0 {
		e := time.Now().AddDate(0, 0, req.ExpireDays)
		exp = &e
	}
	codes := make([]string, 0, req.Count)
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		for range req.Count {
			code := newRedeemCode()
			if err := tx.Create(&store.RedeemCode{Code: code, PlanID: p.ID, Batch: strings.TrimSpace(req.Batch), CreatedBy: c.GetString("admin"), ExpiresAt: exp}).Error; err != nil {
				return err
			}
			codes = append(codes, code)
		}
		return nil
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "redeem.create", p.Name, fmt.Sprintf("%d codes, batch %q", len(codes), req.Batch))
	c.JSON(http.StatusOK, gin.H{"codes": codes})
}

// deleteCodes is DELETE /redeem-codes {codes} or ?batch=: only unused codes go.
func (s *Server) deleteCodes(c *gin.Context) {
	var req struct {
		Codes []string `json:"codes"`
	}
	_ = c.ShouldBindJSON(&req)
	q := s.st.DB.Where("used_at IS NULL")
	switch {
	case len(req.Codes) > 0:
		q = q.Where("code IN ?", req.Codes)
	case c.Query("batch") != "":
		q = q.Where("batch = ?", c.Query("batch"))
	default:
		badRequest(c, errors.New("请指定兑换码或批次"))
		return
	}
	res := q.Delete(&store.RedeemCode{})
	s.auditCtx(c, "redeem.delete", c.Query("batch"), fmt.Sprintf("%d codes", res.RowsAffected))
	c.JSON(http.StatusOK, gin.H{"deleted": res.RowsAffected})
}

// newRedeemCode is 16 characters from an alphabet without look-alikes,
// grouped XXXX-XXXX-XXXX-XXXX.
func newRedeemCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b strings.Builder
	for i := range 16 {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String()
}
