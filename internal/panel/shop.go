package panel

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/license"
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

	kindPack = "pack" // Order.Kind of a traffic pack

	maxPendingOrders = 5
	maxWebPending    = 200 // open anonymous web shop orders, shop-wide
)

// unpaidStatuses are the order states a payment can still settle: a
// transfer may arrive after the order expired or was cancelled, and the
// money is delivered all the same.
var unpaidStatuses = []string{"pending", "expired", "cancelled", "processing"}

const (
	epayNotifyPath = "/pay/epay/notify"
	epayReturnPath = "/pay/return"
	// usdtLookback is how long a USDT order's amount stays reserved and late
	// transfers are still matched.
	usdtLookback = 3 * time.Hour
)

func (s *Server) epayNotifyURL() string { return s.panelBase() + epayNotifyPath }

// epayReturnURL is where a payment gateway sends the buyer back: a page on
// the panel's own domain (the one authorized with the gateway), which then
// opens the bot.
func (s *Server) epayReturnURL() string { return s.panelBase() + epayReturnPath }

// payReturn is GET /pay/return: back from the payment page to the bot.
func (s *Server) payReturn(c *gin.Context) {
	// Gateways append the order (out_trade_no) when they send the buyer back;
	// a web shop order returns to its page instead of the bot.
	if id := c.Query("out_trade_no"); id != "" && len(id) <= 64 {
		var o store.Order
		if s.st.DB.Select("id, web_key").First(&o, "id = ?", id).Error == nil && o.WebKey != "" {
			c.Redirect(http.StatusFound, shopWebPath+"/order/"+o.ID)
			return
		}
	}
	if bot := s.tg.username(); bot != "" {
		c.Redirect(http.StatusFound, "https://t.me/"+bot)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">`+
		`<title>支付完成</title><p style="font:16px sans-serif;text-align:center;margin-top:30vh">支付完成，请回到 Telegram 查看开通结果。</p>`))
}

var (
	errShopClosed = errors.New("商店未开启")
	errPayments   = errors.New("在线收款是专业版功能")
)

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
// Each way to pay needs its settings and online payments (professional).
func (s *Server) starsOn() bool {
	return s.payments() && s.st.GetSetting("shop_stars", "false") == "true"
}

func (s *Server) epayOn() bool {
	return s.payments() && s.st.GetSetting("shop_epay_enabled", "true") == "true" && len(s.epayTypes()) > 0 && s.st.GetSetting("shop_epay_url", "") != "" && s.st.GetSetting("shop_epay_pid", "") != "" && s.st.GetSetting("shop_epay_key", "") != ""
}

func (s *Server) epayTypes() []string {
	var disabled []string
	if json.Unmarshal([]byte(s.st.GetSetting("shop_epay_disabled_types", "[]")), &disabled) != nil {
		return nil
	}
	var out []string
	for _, t := range strings.Split(s.st.GetSetting("shop_epay_types", "alipay,wxpay"), ",") {
		if t = strings.TrimSpace(t); t != "" && len(t) <= 16 && !slices.Contains(disabled, t) && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// methodOn reports whether orders may still be paid with method.
func (s *Server) methodOn(method string) bool {
	switch method {
	case payStars:
		return s.starsOn()
	case payEPay:
		return s.epayOn()
	case payUSDT:
		return s.usdtOn()
	}
	return false
}

func (s *Server) usdtOn() bool { return len(s.usdtNetsOn()) > 0 }

func (s *Server) codesOn() bool { return s.st.GetSetting("shop_code_enabled", "true") == "true" }

// paymentType validates old bot buttons as well as current purchase entries.
func (s *Server) paymentType(method, typ string) (string, error) {
	if !s.methodOn(method) {
		return "", errors.New("这个付款方式已停用，请重新选择")
	}
	if method != payEPay {
		return "", nil
	}
	types := s.epayTypes()
	if typ == "" && len(types) > 0 {
		typ = types[0]
	}
	if !slices.Contains(types, typ) {
		return "", errors.New("这个付款渠道已停用，请重新选择")
	}
	return typ, nil
}

func validatePaymentSetting(k, v string) error {
	switch k {
	case "shop_epay_enabled", "shop_usdt_enabled", "shop_code_enabled":
		if v != "true" && v != "false" {
			return errors.New("支付开关必须为 true 或 false")
		}
	case "shop_epay_disabled_types":
		var disabled []string
		if len(v) > 1024 || json.Unmarshal([]byte(v), &disabled) != nil || disabled == nil || len(disabled) > 32 {
			return errors.New("易支付停用渠道必须为 JSON 字符串数组，最多32项")
		}
		for _, typ := range disabled {
			if len(typ) < 1 || len(typ) > 16 || strings.TrimSpace(typ) != typ {
				return errors.New("易支付渠道标识须为1至16字节，不能包含首尾空白")
			}
		}
	}
	return nil
}

// salePlans lists the plans on sale, in display order.
func (s *Server) salePlans() []store.Group {
	var ps []store.Group
	s.st.DB.Where("plan = ? AND sale = ?", true, true).Order("sort, name").Find(&ps)
	return ps
}

// orderPrice is what plan p costs with method, as decimal text and currency;
// ok is false when it is not sold that way.
func (s *Server) orderPrice(p *store.Group, method string) (amount, currency string, ok bool) {
	return s.priceFor(p.PriceStars, p.PriceCNY, p.PriceUSDT, method)
}

// priceFor picks the price for a way to pay among a product's prices.
func (s *Server) priceFor(stars int, cny, usdt float64, method string) (amount, currency string, ok bool) {
	switch method {
	case payStars:
		return strconv.Itoa(stars), "XTR", s.starsOn() && stars > 0
	case payEPay:
		return fmt.Sprintf("%.2f", cny), "CNY", s.epayOn() && cny >= 0.01
	case payUSDT:
		return fmt.Sprintf("%.4f", usdt), "USDT", s.usdtOn() && usdt >= 0.01
	}
	return "", "", false
}

// shopBuyer is who places an order: a Telegram chat, an account, or a web
// shop browser (WebKey, the hash of its key) — any mix of them.
type shopBuyer struct {
	TGID           int64
	TGName, UserID string
	WebKey         string
}

// newOrder opens an order for a Telegram user. userID is the bound account
// ("" = none yet).
func (s *Server) newOrder(tgID int64, tgName, userID, planID, method string) (*store.Order, error) {
	return s.newOrderFor(shopBuyer{TGID: tgID, TGName: tgName, UserID: userID}, planID, method, "")
}

// newOrderFor opens a plan order; chain is the USDT network ("" = the
// first one on).
func (s *Server) newOrderFor(b shopBuyer, planID, method, chain string) (*store.Order, error) {
	if !s.shopOn() {
		return nil, errShopClosed
	}
	if b.UserID == "" && !s.shopAutoCreate() {
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
	o := &store.Order{ID: store.NewID(), TelegramID: b.TGID, TGName: truncate(b.TGName, 64), UserID: b.UserID, WebKey: b.WebKey, PlanID: p.ID, PlanName: p.Name,
		Method: method, Amount: amount, Currency: currency, Status: "pending", CreatedAt: now, ExpiresAt: now.Add(s.orderTTL()), Entitlements: freezePlan(&p)}
	if err := s.setUSDTChain(o, chain); err != nil {
		return nil, err
	}
	return s.openShopOrder(o, p.PriceUSDT)
}

// openShopOrder reuses an unexpired Stars checkout for the same buyer,
// account, product and price. Closing Telegram's payment window sends no
// cancellation event; repeated purchase taps must not open another order.
func (s *Server) openShopOrder(o *store.Order, usdtPrice float64) (*store.Order, error) {
	s.shopMu.Lock()
	defer s.shopMu.Unlock()
	now := time.Now()
	// PostgreSQL keeps microseconds: the returned order must equal what a
	// later read of it gives.
	o.CreatedAt, o.ExpiresAt = o.CreatedAt.Truncate(time.Microsecond), o.ExpiresAt.Truncate(time.Microsecond)
	if o.Method == payStars {
		var existing store.Order
		err := s.st.DB.Where("telegram_id = ? AND user_id = ? AND plan_id = ? AND pack_id = ? AND kind = ? AND method = ? AND amount = ? AND currency = ? AND status = ? AND expires_at > ?",
			o.TelegramID, o.UserID, o.PlanID, o.PackID, o.Kind, payStars, o.Amount, o.Currency, "pending", now).Where("entitlements = ?", o.Entitlements).Order("created_at desc").First(&existing).Error
		if err == nil {
			return &existing, nil
		}
		if !store.IsNotFound(err) {
			return nil, err
		}
	}
	var pending int64
	q := s.st.DB.Model(&store.Order{}).Where("status = ? AND expires_at > ?", "pending", now)
	if o.TelegramID != 0 {
		q = q.Where("telegram_id = ?", o.TelegramID)
	} else {
		q = q.Where("web_key = ? AND telegram_id = 0", o.WebKey)
	}
	if err := q.Count(&pending).Error; err != nil {
		return nil, err
	}
	if pending >= maxPendingOrders {
		return nil, errors.New("未支付的订单太多，请先完成或等待过期")
	}
	if o.TelegramID == 0 {
		// Anonymous web orders: a cap on the whole shop keeps a script from
		// filling the order book.
		var all int64
		s.st.DB.Model(&store.Order{}).Where("status = ? AND expires_at > ? AND telegram_id = 0", "pending", now).Count(&all)
		if all >= maxWebPending {
			return nil, errors.New("下单的人太多，请稍后再试")
		}
	}
	if o.Method == payUSDT {
		return s.reserveUSDTOrder(o, usdtPrice, now)
	}
	if err := s.st.DB.Create(o).Error; err != nil {
		return nil, err
	}
	return o, nil
}

// setUSDTChain picks the network of a USDT order.
func (s *Server) setUSDTChain(o *store.Order, chain string) error {
	if o.Method != payUSDT {
		return nil
	}
	n, ok := s.usdtNetFor(chain)
	if !ok {
		return errors.New("这个 USDT 网络暂不可用")
	}
	o.Chain = n.ID
	return nil
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
		res := tx.Model(&store.Order{}).Where("id = ? AND status IN ?", id, unpaidStatuses).
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
		// Cross-instance first-account creation is serialized by buyer; account
		// changes then use the same row lock as admin/API user commands.
		key := "web:" + o.WebKey
		if o.TelegramID != 0 {
			key = "tg:" + strconv.FormatInt(o.TelegramID, 10)
		} else if o.UserID != "" {
			key = "user:" + o.UserID
		}
		if e := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"updated_at"})}).Create(&store.ShopBuyerLock{Key: key, UpdatedAt: now}).Error; e != nil {
			return e
		}
		if o.Kind == kindPack {
			var pk store.TrafficPack
			if err := orderPack(tx, o, &pk); err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, errOrderTerms) {
					return commandInputError{"流量包不可用或订单权益无效，请联系管理员处理"}
				}
				return err
			}
			if e := lockOrderUser(tx, o, u); e != nil {
				if errors.Is(e, gorm.ErrRecordNotFound) {
					return commandInputError{"没有找到要加流量的账号"}
				}
				return e
			}
			var err error
			if detail, err = s.applyPack(tx, u, &pk, now); err != nil {
				return err
			}
			u.Status = computeStatus(u, now)
			if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Update("status", u.Status).Error; err != nil {
				return err
			}
			o.UserID, o.Detail = u.ID, truncate(detail, 255)
			return tx.Model(&store.Order{}).Where("id = ?", id).Updates(map[string]any{"user_id": u.ID, "detail": o.Detail}).Error
		}
		if err := orderPlan(tx, o, &p); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, errOrderTerms) {
				return commandInputError{"套餐不可用或订单权益无效，请联系管理员处理"}
			}
			return err
		}
		if e := lockOrderUser(tx, o, u); e != nil {
			if !errors.Is(e, gorm.ErrRecordNotFound) {
				return e
			}
			if !autoCreate {
				return commandInputError{"没有可开通的账号"}
			}
			nu, e := shopNewUser(tx, o, prefix)
			if e != nil {
				return e
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
			s.st.DB.Model(&store.Order{}).Where("id = ? AND status IN ?", id, unpaidStatuses).
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
	if o.Kind == kindPack {
		s.userEvent("user.traffic_added", u, map[string]any{"pack_id": o.PackID, "order_id": o.ID, "detail": detail})
	} else {
		s.userEvent("user.plan_changed", u, map[string]any{"plan_id": u.PlanID, "order_id": o.ID, "detail": detail})
	}
	s.notify(Event{Kind: "order.paid", Text: fmt.Sprintf("💰 新订单：%s 购买 %s（%s %s，%s）", who, o.PlanName, o.Amount, o.Currency, methodName[o.Method]),
		Data: map[string]any{"order": orderBrief(o), "user": userBrief(u)}})
	go s.tg.orderDelivered(o, u, created)
	go s.mailOrderDelivered(o, u, created)
	return o, u, true, nil
}

var methodName = map[string]string{payStars: "Telegram Stars", payEPay: "易支付", payUSDT: "USDT", payCode: "兑换码", payManual: "手动"}

func orderBrief(o *store.Order) map[string]any {
	return map[string]any{"id": o.ID, "plan_id": o.PlanID, "plan_name": o.PlanName, "method": o.Method, "amount": o.Amount,
		"currency": o.Currency, "telegram_id": o.TelegramID, "user_id": o.UserID}
}

// shopNewUser creates and binds an account for a Telegram buyer, or an
// unbound one for a web shop buyer.
func shopNewUser(tx *gorm.DB, o *store.Order, prefix string) (*store.User, error) {
	if o.TelegramID == 0 && o.WebKey != "" {
		name := prefix + "w" + strings.ToLower(pki.Token(6))
		u := newUser(&userReq{Username: name, Remark: "网页购买 " + o.ID})
		u.SelfService = true
		if err := tx.Create(&u).Error; err != nil {
			return nil, err
		}
		return &u, nil
	}
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
	u.TelegramID, u.SelfService = o.TelegramID, true
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
	return s.redeemFor(shopBuyer{TGID: tgID, TGName: tgName, UserID: userID}, code)
}

func (s *Server) redeemFor(b shopBuyer, code string) (*store.Order, *store.User, error) {
	if !s.codesOn() {
		return nil, nil, errors.New("兑换码兑换已停用，请联系管理员")
	}
	tgID, tgName, userID := b.TGID, b.TGName, b.UserID
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
	now := time.Now()
	o := &store.Order{ID: store.NewID(), TelegramID: tgID, TGName: truncate(tgName, 64), UserID: userID, WebKey: b.WebKey,
		Method: payCode, Amount: code, Status: "pending", CreatedAt: now, ExpiresAt: now}
	if rc.PackID != "" {
		// A traffic pack code tops up an existing account.
		var pk store.TrafficPack
		if s.st.DB.First(&pk, "id = ?", rc.PackID).Error != nil {
			return nil, nil, errors.New("兑换码对应的流量包已删除，请联系管理员")
		}
		var u *store.User
		if userID != "" {
			u = &store.User{}
			if s.st.DB.First(u, "id = ?", userID).Error != nil {
				u = nil
			}
		}
		if err := packFor(&pk, u); err != nil {
			return nil, nil, err
		}
		o.Kind, o.PackID, o.PlanName = kindPack, pk.ID, pk.Name
	} else {
		var p store.Group
		if s.st.DB.First(&p, "id = ? AND plan = ?", rc.PlanID, true).Error != nil {
			return nil, nil, errors.New("兑换码对应的套餐已删除，请联系管理员")
		}
		o.PlanID, o.PlanName = p.ID, p.Name
	}
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

// cancelOrder cancels an unpaid order. tgID limits it to that buyer's
// orders (0 = any, for the admin). Stars invoices of a cancelled order are
// refused at checkout; an EPay or USDT payment made anyway still delivers.
func (s *Server) cancelOrder(tgID int64, id string) (*store.Order, error) {
	q := s.st.DB.Model(&store.Order{}).Where("id = ? AND status = ?", id, "pending")
	if tgID != 0 {
		q = q.Where("telegram_id = ?", tgID)
	}
	res := q.Update("status", "cancelled")
	if res.Error != nil {
		return nil, res.Error
	}
	var o store.Order
	if err := s.st.DB.First(&o, "id = ?", id).Error; err != nil || (tgID != 0 && o.TelegramID != tgID) {
		return nil, errors.New("订单不存在")
	}
	if res.RowsAffected == 0 && o.Status != "cancelled" {
		return &o, errors.New("订单" + orderStatusName[o.Status] + "，无法取消")
	}
	return &o, nil
}

var orderStatusName = map[string]string{"pending": "待支付", "processing": "已收款待交付", "refund_pending": "退款处理中", "paid": "已支付", "expired": "已过期", "failed": "已付款但开通失败", "refunded": "已退款", "cancelled": "已取消"}

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
		q = q.Where(s.st.Like("id = ? OR tg_name LIKE ? OR plan_name LIKE ? OR user_id IN (?)"), v, like, like,
			s.st.DB.Model(&store.User{}).Select("id").Where(s.st.Like("username LIKE ?"), like))
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
	sums := []sum{} // [] rather than null when nothing was sold
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
	orderIDs := make([]string, 0, len(os))
	for _, o := range os {
		orderIDs = append(orderIDs, o.ID)
	}
	receipts := map[string]store.PaymentReceipt{}
	refunds := map[string]store.RefundJob{}
	if len(orderIDs) > 0 {
		var ps []store.PaymentReceipt
		var rs []store.RefundJob
		if e := s.st.DB.Where("order_id IN ?", orderIDs).Find(&ps).Error; e != nil {
			serverError(c, e)
			return
		}
		if e := s.st.DB.Where("order_id IN ?", orderIDs).Find(&rs).Error; e != nil {
			serverError(c, e)
			return
		}
		for _, p := range ps {
			old, ok := receipts[p.OrderID]
			if !ok || old.Status == "done" {
				receipts[p.OrderID] = p
			}
		}
		for _, r := range rs {
			refunds[r.OrderID] = r
		}
	}
	cut := time.Now().Add(-orderDeleteGrace)
	for i := range os {
		os[i].Web = os[i].WebKey != ""
		_, hasReceipt := receipts[os[i].ID]
		os[i].Deletable = slices.Contains(deletableOrders, os[i].Status) && os[i].PayRef == nil && os[i].ExpiresAt.Before(cut) && !hasReceipt
	}
	c.JSON(http.StatusOK, gin.H{"items": os, "total": total, "usernames": names, "revenue": sums, "status": s.shopStatus(), "paymentRecovery": receipts, "refundRecovery": refunds})
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
		"botRunning": s.tg.isRunning(), "notifyUrl": s.epayNotifyURL(), "returnUrl": s.epayReturnURL(), "publicUrl": s.cfg.PublicURL != "", "usdtChains": s.usdtChainIDs(), "payments": s.payments(), "webShop": s.webShopOn(), "webShopUrl": s.panelBase() + shopWebPath, "portal": s.portalEnabled()}
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

// adminCancelOrder is POST /orders/:id/cancel.
func (s *Server) adminCancelOrder(c *gin.Context) {
	o, err := s.cancelOrder(0, c.Param("id"))
	if err != nil {
		if o == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		} else {
			badRequest(c, err)
		}
		return
	}
	s.auditCtx(c, "order.cancel", o.ID, "")
	c.JSON(http.StatusOK, o)
}

// deletableOrders are the statuses whose records the admin may delete: no
// money came in for them.
var deletableOrders = []string{"expired", "cancelled"}

const orderDeleteGrace = 24 * time.Hour

// deleteOrders is DELETE /orders {ids} or ?status=expired|cancelled&days=N
// (older than N days). Paid, refunded, failed and open orders stay: they are
// the payment record.
func (s *Server) deleteOrders(c *gin.Context) {
	var req struct {
		IDs []string `json:"ids"`
	}
	_ = c.ShouldBindJSON(&req)
	// A day's grace: an EPay callback or a USDT transfer for an order that
	// just closed still pays it, and needs the order to be there.
	q := s.st.DB.Where("status IN ? AND pay_ref IS NULL AND expires_at < ? AND id NOT IN (?)", deletableOrders, time.Now().Add(-orderDeleteGrace), s.st.DB.Model(&store.PaymentReceipt{}).Select("order_id"))
	switch st := c.Query("status"); {
	case len(req.IDs) > 0:
		if len(req.IDs) > 5000 {
			badRequest(c, errors.New("一次最多删除 5000 笔订单"))
			return
		}
		q = q.Where("id IN ?", req.IDs)
	case slices.Contains(deletableOrders, st):
		q = q.Where("status = ?", st)
		if days := atoiDefault(c.Query("days"), 0); days > 0 {
			q = q.Where("created_at < ?", time.Now().AddDate(0, 0, -days))
		}
	default:
		badRequest(c, errors.New("只能删除已过期或已取消的订单"))
		return
	}
	res := q.Delete(&store.Order{})
	if res.Error != nil {
		dbError(c, res.Error)
		return
	}
	s.auditCtx(c, "order.delete", c.Query("status"), fmt.Sprintf("%d orders", res.RowsAffected))
	c.JSON(http.StatusOK, gin.H{"deleted": res.RowsAffected})
}

// refundOrder is POST /orders/:id/refund: returns the Stars of a paid
// Telegram Stars order. The plan it started is left as it is; adjust the
// user if the refund should take it back.
func (s *Server) refundOrder(c *gin.Context) {
	if err := s.requestRefund(c.Param("id"), c.GetString("admin")); err != nil {
		badRequest(c, err)
		return
	}
	s.processRefund(c.Param("id"))
	var o store.Order
	if err := s.st.DB.First(&o, "id = ?", c.Param("id")).Error; err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, o)
}

// codesQuery filters redeem codes by batch, state (no = usable, yes = used,
// expired = unused past its date) and code text.
func (s *Server) codesQuery(batch, state, text string) *gorm.DB {
	q := s.st.DB.Model(&store.RedeemCode{})
	if batch != "" {
		q = q.Where("batch = ?", batch)
	}
	now := time.Now()
	switch state {
	case "yes":
		q = q.Where("used_at IS NOT NULL")
	case "no":
		q = q.Where("used_at IS NULL AND (expires_at IS NULL OR expires_at >= ?)", now)
	case "expired":
		q = q.Where("used_at IS NULL AND expires_at < ?", now)
	}
	if v := strings.ToUpper(strings.TrimSpace(text)); v != "" {
		q = q.Where("code LIKE ?", "%"+v+"%")
	}
	return q
}

// listCodes is GET /redeem-codes?batch=&used=&q=&limit=&offset=. Used codes
// come with who used them and the order they made.
func (s *Server) listCodes(c *gin.Context) {
	q := s.codesQuery(c.Query("batch"), c.Query("used"), c.Query("q"))
	var total int64
	q.Count(&total)
	var cs []store.RedeemCode
	q.Order("created_at desc, code").Limit(min(max(atoiDefault(c.Query("limit"), 200), 1), 2000)).Offset(max(atoiDefault(c.Query("offset"), 0), 0)).Find(&cs)
	var batches []string
	s.st.DB.Model(&store.RedeemCode{}).Distinct().Where("batch <> ''").Order("batch").Pluck("batch", &batches)
	names, orders := map[string]string{}, map[string]string{}
	var userIDs, used []string
	for _, rc := range cs {
		if rc.UsedAt != nil {
			used = append(used, rc.Code)
			if rc.UsedBy != "" {
				userIDs = append(userIDs, rc.UsedBy)
			}
		}
	}
	if len(userIDs) > 0 {
		var us []store.User
		s.st.DB.Select("id, username").Where("id IN ?", userIDs).Find(&us)
		for _, u := range us {
			names[u.ID] = u.Username
		}
	}
	if len(used) > 0 {
		var os []store.Order
		s.st.DB.Select("id, amount").Where("method = ? AND amount IN ?", payCode, used).Find(&os)
		for _, o := range os {
			orders[o.Amount] = o.ID
		}
	}
	var counts struct{ Unused, Used, Expired int64 }
	s.codesQuery(c.Query("batch"), "no", "").Count(&counts.Unused)
	s.codesQuery(c.Query("batch"), "yes", "").Count(&counts.Used)
	s.codesQuery(c.Query("batch"), "expired", "").Count(&counts.Expired)
	c.JSON(http.StatusOK, gin.H{"items": cs, "total": total, "batches": nonNilStrs(batches), "usernames": names, "orders": orders,
		"counts": gin.H{"unused": counts.Unused, "used": counts.Used, "expired": counts.Expired}})
}

// createCodes is POST /redeem-codes {planId, count, batch, expireDays}.
func (s *Server) createCodes(c *gin.Context) {
	var req struct {
		PlanID     string `json:"planId"`
		PackID     string `json:"packId"` // a traffic pack instead of a plan (professional edition)
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
	rc := store.RedeemCode{Batch: strings.TrimSpace(req.Batch), CreatedBy: c.GetString("admin")}
	target := ""
	if req.PackID != "" {
		if !s.requireFeature(c, license.FeaturePlanAddons) {
			return
		}
		var pk store.TrafficPack
		if err := s.st.DB.First(&pk, "id = ?", req.PackID).Error; err != nil {
			badRequest(c, errors.New("流量包不存在"))
			return
		}
		rc.PackID, target = pk.ID, pk.Name
	} else {
		p, err := loadPlan(s.st.DB, req.PlanID)
		if err != nil {
			badRequest(c, err)
			return
		}
		rc.PlanID, target = p.ID, p.Name
	}
	var exp *time.Time
	if req.ExpireDays > 0 {
		e := time.Now().AddDate(0, 0, req.ExpireDays)
		exp = &e
	}
	rc.ExpiresAt = exp
	codes := make([]string, 0, req.Count)
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		for range req.Count {
			code := newRedeemCode()
			row := rc
			row.Code = code
			if err := tx.Create(&row).Error; err != nil {
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
	s.auditCtx(c, "redeem.create", target, fmt.Sprintf("%d codes, batch %q", len(codes), req.Batch))
	c.JSON(http.StatusOK, gin.H{"codes": codes})
}

// deleteCodes is DELETE /redeem-codes {codes, used} or ?batch=&state=.
// Unused codes go unless used is set; ?state=used|expired clears the used
// or expired codes (of the batch, if given). A used code's order stays.
func (s *Server) deleteCodes(c *gin.Context) {
	var req struct {
		Codes []string `json:"codes"`
		Used  bool     `json:"used"` // also delete the records of used codes
	}
	_ = c.ShouldBindJSON(&req)
	var q *gorm.DB
	switch state := c.Query("state"); {
	case len(req.Codes) > 0:
		if len(req.Codes) > 5000 {
			badRequest(c, errors.New("一次最多删除 5000 个兑换码"))
			return
		}
		q = s.st.DB.Where("code IN ?", req.Codes)
		if !req.Used {
			q = q.Where("used_at IS NULL")
		}
	case state == "used" || state == "expired":
		q = s.codesQuery(c.Query("batch"), map[string]string{"used": "yes", "expired": "expired"}[state], "")
	case state == "" && c.Query("batch") != "":
		q = s.st.DB.Where("used_at IS NULL AND batch = ?", c.Query("batch"))
	default:
		badRequest(c, errors.New("请指定兑换码或批次"))
		return
	}
	res := q.Delete(&store.RedeemCode{})
	if res.Error != nil {
		dbError(c, res.Error)
		return
	}
	s.auditCtx(c, "redeem.delete", c.Query("batch"), fmt.Sprintf("%d codes %s", res.RowsAffected, c.Query("state")))
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
