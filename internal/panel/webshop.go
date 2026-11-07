package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Web shop: plans and traffic packs sold on a page of the user center
// (/u/shop), next to the Telegram bot shop. Signed-in users (Telegram login
// of the user center) renew their own account and buy traffic packs; anyone
// else buys a new account, which the order page then shows. A browser is
// known by a random key in an HttpOnly cookie; orders store its hash, and
// the order page's link with ?k=<key> opens them on another device. Online
// payments (EPay, USDT) follow the shop's payment settings; without them
// the page still redeems codes.

const (
	shopWebPath   = portalPath + "/shop"
	shopWebCookie = "polaris_shop"
	shopWebTTL    = 180 * 24 * time.Hour
)

func (s *Server) webShopOn() bool {
	return s.portalEnabled() && s.shopOn() && s.st.GetSetting("shop_web_enabled", "false") == "true"
}

func webKeyHash(k string) string {
	h := sha256.Sum256([]byte("polaris web shop\x00" + k))
	return hex.EncodeToString(h[:])
}

func validWebKey(k string) bool {
	if len(k) != 32 {
		return false
	}
	_, err := hex.DecodeString(k)
	return err == nil
}

// shopWebKey is this browser's key; create makes one when there is none.
func (s *Server) shopWebKey(c *gin.Context, create bool) string {
	if ck, err := c.Cookie(shopWebCookie); err == nil && validWebKey(ck) {
		return ck
	}
	if !create {
		return ""
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	k := hex.EncodeToString(b)
	s.setShopWebCookie(c, k)
	return k
}

func (s *Server) setShopWebCookie(c *gin.Context, k string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: shopWebCookie, Value: k, Path: portalPath, MaxAge: int(shopWebTTL.Seconds()), HttpOnly: true,
		Secure: c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https",
		// Lax: coming back from the payment page (a top-level GET) keeps it.
		SameSite: http.SameSiteLaxMode,
	})
}

// shopBuyerOf is the buyer behind a web shop request.
func (s *Server) shopBuyerOf(c *gin.Context, u *store.User) (shopBuyer, bool) {
	k := s.shopWebKey(c, true)
	if k == "" {
		return shopBuyer{}, false
	}
	b := shopBuyer{WebKey: webKeyHash(k), TGName: "网页"}
	if u != nil {
		b.TGID, b.UserID, b.TGName = u.TelegramID, u.ID, "网页 · "+u.Username
	}
	return b, true
}

// webOrder loads an order this request may see: the browser holds its key
// (cookie, or ?k= from a saved link), or the signed-in user owns it.
func (s *Server) webOrder(c *gin.Context, id string) (*store.Order, *store.User, bool) {
	var o store.Order
	if s.st.DB.First(&o, "id = ?", id).Error != nil {
		return nil, nil, false
	}
	u := s.portalUser(c)
	if u != nil && o.UserID == u.ID {
		return &o, u, true
	}
	if o.WebKey == "" {
		return nil, nil, false
	}
	for _, k := range []string{c.Query("k"), s.shopWebKey(c, false)} {
		if validWebKey(k) && subtle.ConstantTimeCompare([]byte(webKeyHash(k)), []byte(o.WebKey)) == 1 {
			if c.Query("k") == k && s.shopWebKey(c, false) != k {
				s.setShopWebCookie(c, k) // a saved link: remember it here
			}
			return &o, u, true
		}
	}
	return nil, nil, false
}

type webShopItem struct {
	Kind, ID, Name, Line, Note string
	CNY, USDT                  string
}

type webShopOrder struct {
	ID, Name, Amount, Status, When string
}

// webShopPage is GET /u/shop.
func (s *Server) webShopPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	data := map[string]any{"Title": s.subTitle(s.st.GetSetting("site_title", "Polaris")), "Open": s.webShopOn()}
	u := s.portalUser(c)
	data["Viewer"] = u
	var items []webShopItem
	for _, p := range s.salePlans() {
		it := webShopItem{Kind: "plan", ID: p.ID, Name: p.Name, Line: s.tgPlanLine(&p), Note: p.Price}
		if a, _, ok := s.orderPrice(&p, payEPay); ok && s.epayOn() {
			it.CNY = a
		}
		if _, _, ok := s.orderPrice(&p, payUSDT); ok && s.usdtOn() {
			it.USDT = strconv.FormatFloat(p.PriceUSDT, 'f', -1, 64)
		}
		items = append(items, it)
	}
	var packs []webShopItem
	for _, p := range s.salePacks(u) {
		it := webShopItem{Kind: "pack", ID: p.ID, Name: p.Name, Line: "本周期增加 " + sub.HumanBytes(p.Traffic)}
		if a, _, ok := s.priceFor(p.PriceStars, p.PriceCNY, p.PriceUSDT, payEPay); ok && s.epayOn() {
			it.CNY = a
		}
		if _, _, ok := s.priceFor(p.PriceStars, p.PriceCNY, p.PriceUSDT, payUSDT); ok && s.usdtOn() {
			it.USDT = strconv.FormatFloat(p.PriceUSDT, 'f', -1, 64)
		}
		if it.CNY != "" || it.USDT != "" {
			packs = append(packs, it)
		}
	}
	var types []map[string]string
	if s.epayOn() {
		for _, t := range s.epayTypes() {
			types = append(types, map[string]string{"ID": t, "Name": orDefault(epayTypeName[t], t)})
		}
	}
	var os []store.Order
	q := s.st.DB.Order("created_at desc").Limit(10)
	if u != nil {
		// A shared browser may hold another buyer's receipt. The signed-in
		// history belongs to this account, including its Telegram purchases.
		q = q.Where("user_id = ?", u.ID)
	} else if k := s.shopWebKey(c, false); k != "" {
		q = q.Where("web_key = ?", webKeyHash(k))
	} else {
		q = nil
	}
	var orders []webShopOrder
	if q != nil {
		if err := q.Find(&os).Error; err != nil {
			serverError(c, err)
			return
		}
		for _, o := range os {
			orders = append(orders, webShopOrder{ID: o.ID, Name: o.PlanName, Amount: webAmount(&o), Status: orderStatusName[o.Status], When: o.CreatedAt.Format("01-02 15:04")})
		}
	}
	data["User"] = u
	if u != nil {
		data["UserLine"] = s.userLine(u)
	}
	data["Items"], data["Packs"], data["Types"], data["Orders"] = items, packs, types, orders
	data["USDT"], data["Pay"], data["Nets"] = s.usdtOn(), s.epayOn() || s.usdtOn(), s.usdtNetsOn()
	data["Code"] = s.codesOn()
	data["AutoCreate"], data["Bot"] = s.shopAutoCreate(), s.tgWebLoginEnabled() || s.mailLoginOn()
	data["Terms"], data["Support"] = s.shopTerms(), strings.TrimSpace(s.st.GetSetting("shop_support", ""))
	s.renderWebShop(c, webShopTpl, data)
}

func (s *Server) userLine(u *store.User) string {
	parts := []string{u.Username}
	if n, _ := s.planNames(u); n != "" {
		parts = append(parts, n)
	}
	if u.ExpireAt != nil {
		parts = append(parts, "到期 "+u.ExpireAt.Format("2006-01-02"))
	}
	used := sub.HumanBytes(u.Used())
	if u.TrafficLimit > 0 {
		used += " / " + sub.HumanBytes(u.TrafficLimit)
	}
	return strings.Join(append(parts, "已用 "+used), " · ")
}

func webAmount(o *store.Order) string {
	switch o.Method {
	case payCode:
		return "兑换码"
	case payCoupon:
		return "优惠券免费"
	case payEPay:
		return "¥" + o.Amount
	case payStars:
		return o.Amount + " ⭐"
	}
	return o.Amount + " " + o.Currency
}

func (s *Server) renderWebShop(c *gin.Context, t *template.Template, data map[string]any) {
	active, title := "shop", "购买套餐"
	if t == webShopOrderTpl {
		active, title = "orders", "订单详情"
	}
	u, _ := data["Viewer"].(*store.User)
	s.renderPortalSection(c, http.StatusOK, active, title, t, data, u)
}

// webShopBuy is POST /u/shop/order {kind, id, method, type}.
func (s *Server) webShopBuy(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if !s.webShopOn() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "商店暂未开放"})
		return
	}
	var req struct{ Kind, ID, Method, Type, Coupon string }
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if !s.webShopLimit(c) {
		return
	}
	u := s.portalUser(c)
	b, ok := s.shopBuyerOf(c, u)
	if !ok {
		serverError(c, errors.New("无法创建订单"))
		return
	}
	switch req.Method {
	case payEPay:
		if !slices.Contains(s.epayTypes(), req.Type) {
			badRequest(c, errors.New("请选择付款方式"))
			return
		}
	case payUSDT:
		if _, ok := s.usdtNetFor(req.Type); !ok {
			badRequest(c, errors.New("这个 USDT 网络暂不可用"))
			return
		}
	default:
		badRequest(c, errors.New("请选择付款方式"))
		return
	}
	chain := ""
	if req.Method == payUSDT {
		chain = req.Type
	}
	b.Coupon = strings.TrimSpace(req.Coupon)
	var o *store.Order
	var err error
	switch req.Kind {
	case "pack":
		if u == nil {
			badRequest(c, errors.New("请先登录用户中心再购买流量包"))
			return
		}
		o, err = s.newPackOrderFor(b, u, req.ID, req.Method, chain)
	default:
		o, err = s.newOrderFor(b, req.ID, req.Method, chain)
	}
	if err != nil {
		badRequest(c, err)
		return
	}
	res := gin.H{"id": o.ID, "url": shopWebPath + "/order/" + o.ID}
	if o.Method == payEPay {
		if l := s.payLink(o, req.Type); l != "" {
			res["pay"] = l
		}
	}
	c.JSON(http.StatusOK, res)
}

// payLink is where a web shop buyer pays EPay order o with type typ ("" when
// online payments are off).
func (s *Server) payLink(o *store.Order, typ string) string {
	selected, err := s.paymentType(payEPay, typ)
	if err != nil {
		return ""
	}
	return s.epayLink(o, selected)
}

// webShopRedeem is POST /u/shop/redeem {code}.
func (s *Server) webShopRedeem(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if !s.webShopOn() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "商店暂未开放"})
		return
	}
	var req struct{ Code string }
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if !s.webShopLimit(c) {
		return
	}
	b, ok := s.shopBuyerOf(c, s.portalUser(c))
	if !ok {
		serverError(c, errors.New("无法兑换"))
		return
	}
	o, _, err := s.redeemFor(b, req.Code)
	if err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": o.ID, "url": shopWebPath + "/order/" + o.ID})
}

// webShopLimit slows down guessing redeem codes and order spam from one
// address.
func (s *Server) webShopLimit(c *gin.Context) bool {
	if !s.shopLimit.Allow(clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "操作太频繁，请稍后再试"})
		return false
	}
	return true
}

// webShopOrderPage is GET /u/shop/order/:id.
func (s *Server) webShopOrderPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	o, viewer, ok := s.webOrder(c, c.Param("id"))
	if !ok {
		s.renderPortalSection(c, http.StatusNotFound, "orders", "查询订单", webShopOrderMissingTpl, nil, s.portalUser(c))
		return
	}
	data := map[string]any{"Title": s.subTitle(s.st.GetSetting("site_title", "Polaris")), "O": o, "Amount": webAmount(o),
		"Status": orderStatusName[o.Status], "Pending": o.Status == "pending" && time.Now().Before(o.ExpiresAt), "Paid": o.Status == "paid",
		"Expires": o.ExpiresAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04（北京时间）"), "Support": strings.TrimSpace(s.st.GetSetting("shop_support", ""))}
	data["Viewer"] = viewer
	data["Tone"] = "unknown"
	if o.Status == "paid" {
		data["Tone"] = "verified"
	}
	if o.Status == "failed" || o.Status == "refund_pending" {
		data["Tone"] = "unavailable"
	}
	data["Received"] = o.PaidAt != nil || o.Status == "paid" || o.Status == "processing" || o.Status == "failed" || o.Status == "refund_pending" || o.Status == "refunded"
	data["Delivered"] = o.Status == "paid" || o.Status == "refund_pending" || o.Status == "refunded"
	data["Processing"] = o.Status == "processing"
	data["TimedOut"] = o.Status == "pending" && data["Pending"] != true
	if data["TimedOut"] == true {
		data["Status"] = "已过支付时限"
	}
	data["Watch"] = data["Pending"] == true || o.Status == "processing"
	data["ExpiryMS"] = o.ExpiresAt.UnixMilli()
	if data["Pending"] == true {
		switch o.Method {
		case payEPay:
			var links []map[string]string
			if s.epayOn() {
				for _, t := range s.epayTypes() {
					if l := s.payLink(o, t); l != "" {
						links = append(links, map[string]string{"Name": orDefault(epayTypeName[t], t), "URL": l})
					}
				}
			}
			data["Links"] = links
		case payUSDT:
			if n, addr := s.usdtPayTo(o); addr != "" && s.usdtOn() {
				data["USDTAddress"], data["USDTNet"], data["USDTQR"] = addr, n.Name, qrDataURL(addr)
			}
		}
	}
	if k := s.shopWebKey(c, false); k != "" && o.WebKey != "" && viewer == nil {
		data["SaveLink"] = s.panelBase() + shopWebPath + "/order/" + o.ID + "?k=" + k
		data["Key"] = k
	}
	// The account the order opened, for a buyer who is not signed in: the
	// order page is where they get their subscription link.
	if o.UserID != "" && (o.Status == "paid") && (viewer == nil || viewer.ID == o.UserID) {
		var u store.User
		if s.st.DB.First(&u, "id = ?", o.UserID).Error == nil {
			data["Account"] = u.Username
			data["SubURL"] = s.subURL(u.SubToken)
			if u.TelegramID == 0 {
				if bot := s.tg.username(); bot != "" {
					data["BindURL"] = "https://t.me/" + bot + "?start=" + u.SubToken
				}
			}
		}
	}
	data["SignedIn"] = viewer != nil
	s.renderWebShop(c, webShopOrderTpl, data)
}

// webShopOrderStatus is GET /u/shop/order/:id/status (the page polls it).
func (s *Server) webShopOrderStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	o, _, ok := s.webOrder(c, c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": o.Status})
}

// webShopCancel is POST /u/shop/order/:id/cancel.
func (s *Server) webShopCancel(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	o, _, ok := s.webOrder(c, c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在"})
		return
	}
	if _, err := s.cancelOrder(0, o.ID); err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

//go:embed assets/portal-shop.html
var portalShopHTML string

//go:embed assets/portal-order.html
var portalOrderHTML string

var webShopTpl = template.Must(template.New("shop").Parse(portalShopHTML))
var webShopOrderTpl = template.Must(template.New("order").Parse(portalOrderHTML))
var webShopOrderMissingTpl = template.Must(template.New("missing-order").Parse(`<section class="p-service-page"><div class="p-heading"><div><h1>订单不存在</h1><p class="mute">订单不存在，或订单号、凭据不正确。</p></div></div><section class="card"><p>请检查订单号和下单时保存的凭据，或登录订单所属账号后重试。</p><div class="p-inline-actions"><a class="btn pri" href="/u/shop#lookup">重新查询</a><a class="btn alt" href="/u#orders">我的订单</a></div></section></section>`))
