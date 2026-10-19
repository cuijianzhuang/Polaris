package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	if !s.webShopOn() {
		s.renderWebShop(c, webShopTpl, data)
		return
	}
	u := s.portalUser(c)
	var items []webShopItem
	for _, p := range s.salePlans() {
		it := webShopItem{Kind: "plan", ID: p.ID, Name: p.Name, Line: s.tgPlanLine(&p), Note: p.Price}
		if a, _, ok := s.orderPrice(&p, payEPay); ok {
			it.CNY = a
		}
		if _, _, ok := s.orderPrice(&p, payUSDT); ok {
			it.USDT = strconv.FormatFloat(p.PriceUSDT, 'f', -1, 64)
		}
		items = append(items, it)
	}
	var packs []webShopItem
	for _, p := range s.salePacks(u) {
		it := webShopItem{Kind: "pack", ID: p.ID, Name: p.Name, Line: "本周期增加 " + sub.HumanBytes(p.Traffic)}
		if a, _, ok := s.priceFor(p.PriceStars, p.PriceCNY, p.PriceUSDT, payEPay); ok {
			it.CNY = a
		}
		if _, _, ok := s.priceFor(p.PriceStars, p.PriceCNY, p.PriceUSDT, payUSDT); ok {
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
	if k := s.shopWebKey(c, false); k != "" && u != nil {
		q = q.Where("web_key = ? OR user_id = ?", webKeyHash(k), u.ID)
	} else if k != "" {
		q = q.Where("web_key = ?", webKeyHash(k))
	} else if u != nil {
		q = q.Where("user_id = ? AND web_key <> ''", u.ID)
	} else {
		q = nil
	}
	var orders []webShopOrder
	if q != nil {
		q.Find(&os)
		for _, o := range os {
			orders = append(orders, webShopOrder{ID: o.ID, Name: o.PlanName, Amount: webAmount(&o), Status: orderStatusName[o.Status], When: o.CreatedAt.Format("01-02 15:04")})
		}
	}
	data["User"] = u
	if u != nil {
		data["UserLine"] = s.userLine(u)
	}
	data["Items"], data["Packs"], data["Types"], data["Orders"] = items, packs, types, orders
	data["USDT"], data["Pay"] = s.usdtOn(), s.epayOn() || s.usdtOn()
	data["Code"] = s.codesOn()
	data["AutoCreate"], data["Bot"] = s.shopAutoCreate(), s.tgWebLoginEnabled()
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
	case payEPay:
		return "¥" + o.Amount
	case payStars:
		return o.Amount + " ⭐"
	}
	return o.Amount + " " + o.Currency
}

func (s *Server) renderWebShop(c *gin.Context, t *template.Template, data map[string]any) {
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		serverError(c, err)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(buf.String()))
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
	var req struct{ Kind, ID, Method, Type string }
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if !webShopLimit(c) {
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
	default:
		badRequest(c, errors.New("请选择付款方式"))
		return
	}
	var o *store.Order
	var err error
	switch req.Kind {
	case "pack":
		if u == nil {
			badRequest(c, errors.New("请先登录用户中心再购买流量包"))
			return
		}
		o, err = s.newPackOrderFor(b, u, req.ID, req.Method)
	default:
		o, err = s.newOrderFor(b, req.ID, req.Method)
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
	if !webShopLimit(c) {
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

// webShopLimiter slows down guessing redeem codes and order spam from one
// address.
var webShopLimiter = newIPLimiter(10)

func webShopLimit(c *gin.Context) bool {
	if !webShopLimiter.Allow(clientIP(c.Request)) {
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
		c.Data(http.StatusNotFound, "text/html; charset=utf-8", []byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>订单不存在</title><p style="font:16px sans-serif;text-align:center;margin-top:30vh">订单不存在，或订单号、凭据不正确。<br><a href="`+shopWebPath+`#lookup">重新查询</a> · <a href="`+shopWebPath+`">返回商店</a></p>`))
		return
	}
	data := map[string]any{"Title": s.subTitle(s.st.GetSetting("site_title", "Polaris")), "O": o, "Amount": webAmount(o),
		"Status": orderStatusName[o.Status], "Pending": o.Status == "pending" && time.Now().Before(o.ExpiresAt), "Paid": o.Status == "paid",
		"Expires": o.ExpiresAt.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04（北京时间）"), "Support": strings.TrimSpace(s.st.GetSetting("shop_support", ""))}
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
			if s.usdtOn() {
				data["USDTAddress"] = s.st.GetSetting("shop_usdt_address", "")
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

const webShopStyle = `<style>
:root{--bg:#F4F5F2;--card:#fff;--ink:#15181C;--mute:#5B6470;--line:#E2E5E0;--pri:#0E7490;--ok:#15803D;--bad:#B91C1C;--warn:#FEF3C7;--warnink:#92400E}
@media (prefers-color-scheme:dark){:root{--bg:#101316;--card:#181C20;--ink:#E6E8EA;--mute:#9AA2AB;--line:#2A3036;--pri:#22A3BF;--ok:#4ADE80;--bad:#F87171;--warn:#3A2E12;--warnink:#FCD34D}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.6 "IBM Plex Sans","Noto Sans SC",system-ui,sans-serif}
main{max-width:640px;margin:0 auto;padding:24px 16px 48px;display:flex;flex-direction:column;gap:14px}
header{display:flex;justify-content:space-between;align-items:center;gap:8px}
header a{color:var(--pri);text-decoration:none;font-size:13px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:18px;display:flex;flex-direction:column;gap:10px}
h1{font-size:22px;margin:0}h2{font-size:16px;margin:0}
.mute{color:var(--mute);font-size:13px;margin:0}
.note{background:var(--warn);color:var(--warnink);border-radius:8px;padding:10px 12px;font-size:13px}
.row{display:flex;flex-wrap:wrap;gap:8px}
.btn{display:inline-flex;align-items:center;justify-content:center;min-height:40px;border-radius:8px;border:1px solid var(--pri);background:var(--pri);color:#fff;text-decoration:none;font:inherit;font-weight:500;cursor:pointer;padding:0 14px}
.btn.alt{background:var(--card);color:var(--ink);border-color:var(--line)}
.btn:disabled{opacity:.6;cursor:default}
.price{font-weight:600}
input{font:inherit;padding:8px 10px;border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--ink);min-width:0;flex:1}
code,.mono{font-family:"IBM Plex Mono",ui-monospace,monospace;word-break:break-all}
pre{white-space:pre-wrap;margin:0;font:inherit}
.err{color:var(--bad);font-size:13px;margin:0}.ok{color:var(--ok)}
table{width:100%;border-collapse:collapse;font-size:13px}td{padding:6px 4px;border-top:1px solid var(--line)}
[hidden]{display:none!important}
</style>`

var webShopTpl = template.Must(template.New("shop").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" type="image/png" href="/favicon.ico?v=7622f7a54f03"><meta name="robots" content="noindex"><meta name="referrer" content="no-referrer">
<title>{{.Title}} · 购买套餐</title>` + webShopStyle + `</head><body><main>
<header><h1>购买套餐</h1><span><a href="#lookup">查询订单</a> · <a href="/u">用户中心 →</a></span></header>
{{if not .Open}}<div class="card"><p class="mute">商店暂未开放。</p></div>{{else}}
{{if .User}}<div class="card"><p class="mute">当前账号</p><div>{{.UserLine}}</div><p class="mute">购买同一套餐为续费（顺延到期时间）；换套餐会从今天开始计时并清零已用流量。</p></div>
{{else}}<div class="card"><p class="mute">已有账号？{{if .Bot}}<a href="/u">登录用户中心</a>后可续费或加购流量包。{{else}}续费请联系管理员或使用 Telegram 机器人。{{end}}{{if .AutoCreate}}<br>没有账号也可以直接购买，付款后自动创建账号并显示订阅链接。{{end}}</p></div>{{end}}
{{if not .Pay}}<div class="note">暂未开放在线付款。{{if .Code}}可以使用兑换码开通。{{else}}请联系管理员。{{end}}</div>{{end}}
{{range .Items}}<div class="card" data-kind="{{.Kind}}" data-id="{{.ID}}"><h2>{{.Name}}</h2><p class="mute">{{.Line}}</p>{{if .Note}}<p class="mute">{{.Note}}</p>{{end}}
{{if or .CNY .USDT}}<div class="row">{{$it := .}}{{if .CNY}}{{range $.Types}}<button class="btn buy" type="button" data-method="epay" data-type="{{.ID}}">{{.Name}} ¥{{$it.CNY}}</button>{{end}}{{end}}{{if .USDT}}<button class="btn alt buy" type="button" data-method="usdt">USDT {{.USDT}}</button>{{end}}</div>{{end}}
</div>{{else}}<div class="card"><p class="mute">暂时没有在售的套餐。</p></div>{{end}}
{{if .Packs}}<h2>流量包</h2>{{range .Packs}}<div class="card" data-kind="pack" data-id="{{.ID}}"><h2>{{.Name}}</h2><p class="mute">{{.Line}}，下次流量重置或开始新周期时失效。</p>
<div class="row">{{$it := .}}{{if .CNY}}{{range $.Types}}<button class="btn buy" type="button" data-method="epay" data-type="{{.ID}}">{{.Name}} ¥{{$it.CNY}}</button>{{end}}{{end}}{{if .USDT}}<button class="btn alt buy" type="button" data-method="usdt">USDT {{.USDT}}</button>{{end}}</div></div>{{end}}{{end}}
{{if .Code}}<div class="card"><h2>兑换码</h2><div class="row"><input id="code" placeholder="输入兑换码" autocomplete="off" maxlength="32"><button class="btn" id="redeem" type="button">兑换</button></div></div>{{end}}
<p class="err" id="err" hidden></p>
{{if .Orders}}<div class="card"><h2>我的订单</h2><table>{{range .Orders}}<tr><td><a href="` + shopWebPath + `/order/{{.ID}}">{{.Name}}</a><div class="mute mono">{{.ID}}</div></td><td>{{.Amount}}</td><td>{{.Status}}</td><td class="mute">{{.When}}</td></tr>{{end}}</table></div>{{end}}
<div class="card" id="lookup"><h2>查询订单</h2><p class="mute">换了浏览器或设备时，用下单时保存的订单号和订单凭据查询。请勿把凭据交给他人。</p>
<input id="lid" placeholder="订单号" autocomplete="off" maxlength="32"><input id="lkey" placeholder="订单凭据（32 位）" autocomplete="off" maxlength="32"><div><button class="btn alt" id="lgo" type="button">查询订单</button></div><p class="err" id="lerr" hidden></p></div>
{{if .Terms}}<div class="card"><h2>购买条款</h2><pre class="mute">{{.Terms}}</pre>{{if .Support}}<p class="mute">售后：{{.Support}}</p>{{end}}<p class="mute">付款即表示同意以上条款。</p></div>{{end}}
{{end}}
</main>
<script>
(function(){
var err=document.getElementById('err');if(!err)return;
function show(m){err.textContent=m;err.hidden=!m}
function post(p,b){return fetch('` + shopWebPath + `/'+p,{method:'POST',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},body:JSON.stringify(b),credentials:'same-origin'}).then(function(r){return r.json().then(function(j){if(!r.ok)throw new Error(j.error||r.statusText);return j})})}
document.querySelectorAll('.buy').forEach(function(b){b.addEventListener('click',function(){
var card=b.closest('[data-id]');b.disabled=true;show('');
post('order',{kind:card.dataset.kind,id:card.dataset.id,method:b.dataset.method,type:b.dataset.type||''}).then(function(j){location.href=j.pay||j.url}).catch(function(e){b.disabled=false;show(e.message)})})});
var lg=document.getElementById('lgo');if(lg)lg.addEventListener('click',function(){var id=document.getElementById('lid').value.trim(),k=document.getElementById('lkey').value.trim().toLowerCase(),le=document.getElementById('lerr');
if(!/^[a-z0-9]{8,32}$/.test(id)||!/^[0-9a-f]{32}$/.test(k)){le.textContent='请填写正确的订单号和 32 位订单凭据';le.hidden=false;return}
location.href='` + shopWebPath + `/order/'+encodeURIComponent(id)+'?k='+k});
var r=document.getElementById('redeem');if(r)r.addEventListener('click',function(){var c=document.getElementById('code').value.trim();if(!c)return;r.disabled=true;show('');
post('redeem',{code:c}).then(function(j){location.href=j.url}).catch(function(e){r.disabled=false;show(e.message)})});
})();
</script></body></html>`))

var webShopOrderTpl = template.Must(template.New("order").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" type="image/png" href="/favicon.ico?v=7622f7a54f03"><meta name="robots" content="noindex"><meta name="referrer" content="no-referrer">
<title>{{.Title}} · 订单</title>` + webShopStyle + `</head><body><main>
<header><h1>订单</h1><a href="` + shopWebPath + `">← 返回商店</a></header>
<div class="card"><h2>{{.O.PlanName}}</h2><div class="mono mute">{{.O.ID}}</div><div>金额：<span class="price">{{.Amount}}</span></div><div>状态：<b id="st" class="{{if .Paid}}ok{{end}}">{{.Status}}</b></div>
{{if .O.Detail}}<p class="mute">{{.O.Detail}}</p>{{end}}</div>
{{if .Pending}}<div class="card">
{{if .Links}}<p class="mute">选择方式完成付款，到账后自动开通；请在 {{.Expires}} 前完成。</p><div class="row">{{range .Links}}<a class="btn" href="{{.URL}}">{{.Name}} 付款</a>{{end}}</div>
{{else if .USDTAddress}}<p>网络：<b>TRON (TRC20)</b></p><p>地址：<code>{{.USDTAddress}}</code></p><p>金额：<code>{{.O.Amount}}</code> USDT</p><p class="note">请精确转账上面的金额（含小数），金额不符无法自动到账。请在 {{.Expires}} 前完成，到账确认后自动开通。</p>
{{else}}<p class="note">在线付款暂不可用，请取消订单或联系售后。</p>{{end}}
<div><button class="btn alt" id="cancel" type="button">取消订单</button></div><p class="err" id="err" hidden></p></div>{{end}}
{{if .Account}}<div class="card"><h2>你的账号</h2><div>用户名：<b>{{.Account}}</b></div><div>订阅链接：</div><code>{{.SubURL}}</code>
<div class="row"><a class="btn" href="{{.SubURL}}" target="_blank" rel="noopener">打开订阅页</a>{{if .BindURL}}<a class="btn alt" href="{{.BindURL}}" target="_blank" rel="noopener">绑定 Telegram</a>{{end}}</div>
<p class="mute">把订阅链接导入客户端即可使用。{{if .BindURL}}绑定 Telegram 后可以登录用户中心续费、加购流量包。{{end}}请妥善保管，不要分享给他人。</p></div>{{end}}
{{if .SaveLink}}<div class="card"><h2>订单凭据</h2><p class="mute">保存订单号和凭据（或直接保存下面的链接），换浏览器或设备时可在商店的「查询订单」找回这个订单{{if .Account}}和账号{{end}}。请勿分享。</p>
<div>订单号：<code>{{.O.ID}}</code></div><div>凭据：<code>{{.Key}}</code></div><code class="mute">{{.SaveLink}}</code></div>{{end}}
{{if .Support}}<p class="mute">售后：{{.Support}}</p>{{end}}
</main>
{{if .Pending}}<script>
(function(){
var st=document.getElementById('st'),err=document.getElementById('err'),base='` + shopWebPath + `/order/{{.O.ID}}';
function tick(){fetch(base+'/status'+location.search,{credentials:'same-origin'}).then(function(r){return r.json()}).then(function(j){if(j.status&&j.status!=='pending'){location.reload();return}setTimeout(tick,4000)}).catch(function(){setTimeout(tick,8000)})}
setTimeout(tick,4000);
document.getElementById('cancel').addEventListener('click',function(){if(!confirm('确定取消订单吗？取消后请不要再付款。'))return;
fetch(base+'/cancel'+location.search,{method:'POST',headers:{'X-Polaris-Portal':'1'},credentials:'same-origin'}).then(function(r){return r.json().then(function(j){if(!r.ok)throw new Error(j.error||r.statusText);location.reload()})}).catch(function(e){err.textContent=e.message;err.hidden=false})});
})();
</script>{{end}}
</body></html>`))
