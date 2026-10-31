package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// User center: proxy users sign in at /u with Telegram — no passwords. The
// page shows a number, the user opens t.me/<bot>?start=ulogin_<id> and taps
// the same number (the admin login page's flow, see tgweblogin.go). The
// session is an HttpOnly cookie scoped to /u; it stays valid while the user
// is bound to the same Telegram chat and keeps the same subscription link,
// so unbinding or resetting the link signs every browser out. It never
// grants access to the admin API.

const (
	portalPath        = "/u"
	portalCookie      = "polaris_u"
	portalLoginPrefix = "ulogin_"
	portalSessionTTL  = 30 * 24 * time.Hour
	portalHeader      = "X-Polaris-Portal" // required on state-changing requests (CSRF)
)

func (s *Server) portalEnabled() bool { return s.st.GetSetting("portal_enabled", "false") == "true" }

// portalURL is the user center address shown to admins and users.
func (s *Server) portalURL() string { return s.subBase() + portalPath }

func subFingerprint(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:8])
}

func (s *Server) issuePortalToken(u *store.User) (string, error) {
	return s.issuePortalTokenVia(u, false)
}

// issuePortalMailToken signs a session for an e-mail sign-in.
func (s *Server) issuePortalMailToken(u *store.User) (string, error) {
	return s.issuePortalTokenVia(u, true)
}

func (s *Server) issuePortalTokenVia(u *store.User, viaMail bool) (string, error) {
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "u:" + u.ID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(portalSessionTTL)),
		},
		Username: u.Username,
		User:     u.ID,
		Chat:     u.TelegramID,
		SubFP:    subFingerprint(u.SubToken),
	}
	if viaMail {
		c.Chat, c.MailFP = 0, mailFP(u.Email)
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.jwtKey)
}

// portalClaims are the session's claims, or nil.
func (s *Server) portalClaims(c *gin.Context) *claims {
	ck, err := c.Cookie(portalCookie)
	if err != nil || ck == "" {
		return nil
	}
	cl, err := s.parseToken(ck)
	if err != nil || cl.User == "" {
		return nil
	}
	return cl
}

// portalViaMail reports whether the session came from an e-mail sign-in.
func (s *Server) portalViaMail(c *gin.Context) bool {
	cl := s.portalClaims(c)
	return cl != nil && cl.Chat == 0 && cl.MailFP != ""
}

// portalActor names the signed-in user in the audit log.
func portalActor(u *store.User, viaMail bool) string {
	if viaMail {
		return "mail:" + u.Username
	}
	return "tg:" + u.Username
}

// portalUser returns the signed-in user, or nil. A Telegram session needs
// the same bound chat, an e-mail one the same address; both end when the
// subscription link is reset.
func (s *Server) portalUser(c *gin.Context) *store.User {
	cl := s.portalClaims(c)
	if cl == nil {
		return nil
	}
	var u store.User
	if s.st.DB.First(&u, "id = ? AND is_service = ?", cl.User, false).Error != nil {
		return nil
	}
	if subFingerprint(u.SubToken) != cl.SubFP {
		return nil
	}
	switch {
	case cl.Chat != 0 && u.TelegramID == cl.Chat:
	case cl.Chat == 0 && cl.MailFP != "" && u.Email != "" && mailFP(u.Email) == cl.MailFP:
	default:
		return nil
	}
	return &u
}

func (s *Server) setPortalCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: portalCookie, Value: value, Path: portalPath, MaxAge: maxAge, HttpOnly: true,
		Secure: c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https",
		// Lax: opening /u from a link (e.g. in Telegram) keeps the session;
		// cross-site POSTs carry no cookie, and they lack portalHeader anyway.
		SameSite: http.SameSiteLaxMode,
	})
}

// portalGate serves the decoy while the user center is off.
func (s *Server) portalGate(decoy, h gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.portalEnabled() {
			decoy(c)
			return
		}
		h(c)
	}
}

// portalPage: GET /u — the user's info page, or the sign-in page.
func (s *Server) portalPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalUser(c)
	if u == nil {
		var buf strings.Builder
		_ = portalLoginTpl.Execute(&buf, map[string]any{
			"Title":    s.subTitle(s.st.GetSetting("site_title", "Polaris")),
			"Bot":      s.tgWebLoginEnabled(),
			"Mail":     s.mailLoginOn(),
			"Register": s.mailRegisterOn(),
			"Shop":     s.webShopOn(),
		})
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(buf.String()))
		return
	}
	info := sub.Info{Title: s.st.GetSetting("site_title", "Polaris"), Username: u.Username, Upload: u.UsedUp, Download: u.UsedDown,
		Total: u.TrafficLimit, Expire: u.ExpireAt, URL: s.subURL(u.SubToken)}
	var nodes []sub.Node
	if s.servedUser(u) {
		var err error
		if nodes, err = s.subNodesFor(u, s.subCarrier(c.Request, u), s.subRegion(c.Request, u)); err != nil {
			serverError(c, err)
			return
		}
	}
	info.Title = s.subTitle(info.Title)
	s.pageExtras(&info, u, nodes)
	if u.TelegramID != 0 {
		info.BotURL = "" // already bound
	}
	info.Portal = true
	info.LoginVia, info.MailBind, info.Email = "Telegram", s.mailLoginOn(), u.Email
	if s.portalViaMail(c) {
		info.LoginVia = "邮箱"
	}
	info.Rename = u.SelfService && s.selfRenameOn()
	info.CarrierPick, info.Carrier = s.cdnCarrierSplitOn(), u.CDNCarrier
	if err := s.portalCDNInfo(&info, u, s.subCarrier(c.Request, u), s.subRegion(c.Request, u)); err != nil {
		serverError(c, err)
		return
	}
	if s.webShopOn() {
		info.Extras = append(info.Extras, sub.ExtraLink{Title: "🛒 购买 / 续费", URL: shopWebPath})
	}
	if u.FwdMax > 0 && s.license().Allow(license.FeaturePlanAddons) {
		info.Extras = append(info.Extras, sub.ExtraLink{Title: "🔀 我的转发", URL: portalPath + "/forwards"})
	}
	if s.ticketsOn() {
		title := "🎫 工单"
		var unread int64
		s.st.DB.Model(&store.Ticket{}).Where("user_id = ? AND user_unread = ?", u.ID, true).Count(&unread)
		if unread > 0 {
			title = "🎫 工单（" + strconv.FormatInt(unread, 10) + " 条新回复）"
		}
		info.Extras = append(info.Extras, sub.ExtraLink{Title: title, URL: portalPath + "/tickets"})
	}
	for _, l := range s.pluginPortalLinks(u) {
		info.Extras = append(info.Extras, sub.ExtraLink{Title: l.Title, URL: l.URL})
	}
	b, err := sub.RenderPage(info)
	if err != nil {
		serverError(c, err)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", b)
}

// portalLoginStart: POST /u/login.
func (s *Server) portalLoginStart(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if !s.tgWebLoginEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Telegram 机器人未运行，请联系管理员"})
		return
	}
	s.startWebLogin(c, &s.portalWeb, portalLoginPrefix)
}

// portalLoginPoll: POST /u/poll {id, secret}; sets the session cookie once
// the user taps the right number.
func (s *Server) portalLoginPoll(c *gin.Context) {
	// The header stops a cross-site form from planting someone else's
	// session (login CSRF).
	if !portalWrite(c) {
		return
	}
	userID, chat, ok := s.pollWebLogin(c, &s.portalWeb)
	if !ok {
		return
	}
	var u store.User
	if s.st.DB.First(&u, "id = ? AND is_service = ?", userID, false).Error != nil || u.TelegramID != chat {
		c.JSON(http.StatusOK, gin.H{"status": "denied"})
		return
	}
	tok, err := s.issuePortalToken(&u)
	if err != nil {
		serverError(c, err)
		return
	}
	s.setPortalCookie(c, tok, int(portalSessionTTL.Seconds()))
	s.audit("tg:"+u.Username, "user.portal_login", u.Username, "telegram", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func portalWrite(c *gin.Context) bool {
	if c.GetHeader(portalHeader) != "1" {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return false
	}
	return true
}

// portalLogout: POST /u/logout.
func (s *Server) portalLogout(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	s.setPortalCookie(c, "", -1)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// portalReset: POST /u/reset — a new subscription link and node credentials;
// the old link, the nodes imported from it and every other signed-in browser
// stop working.
func (s *Server) portalReset(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	if err := s.rotateUserCredentials(u); err != nil {
		serverError(c, err)
		return
	}
	viaMail := s.portalViaMail(c)
	s.audit(portalActor(u, viaMail), "user.reset_token", u.Username, "user center", clientIP(c.Request))
	tok, err := s.issuePortalTokenVia(u, viaMail)
	if err != nil {
		serverError(c, err)
		return
	}
	s.setPortalCookie(c, tok, int(portalSessionTTL.Seconds()))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// portalRename: POST /u/rename {username} — buyers whose account the shop
// created pick their own username.
func (s *Server) portalRename(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.renameSelf(u, req.Username, "portal"); err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": u.Username})
}

// portalLoginPrompt answers /start ulogin_<id>.
func (b *tgBot) portalLoginPrompt(chat int64, reqID string, id tgIdentity) {
	if id.user == nil {
		b.send(chat, "🚫 这个 Telegram 还没有绑定账号。\n打开你的订阅链接点「Telegram 机器人绑定」，或发送 <code>/bind 订阅链接</code>，然后回到网页重新登录。", backRow(""))
		return
	}
	b.loginPrompt(&b.s.portalWeb, chat, reqID, "ul", "用户中心登录请求", "👤 账号："+esc(id.user.Username),
		"⌛ 登录请求已失效，请在网页上重新点击「使用 Telegram 登录」。")
}

// portalLoginAnswer handles the ul:<id>:<n|x> callback.
func (b *tgBot) portalLoginAnswer(chat, msg int64, reqID, pick string, id tgIdentity, answer func(string)) {
	subject := ""
	if id.user != nil {
		subject = id.user.ID
	}
	res, ip := b.loginPick(&b.s.portalWeb, chat, reqID, pick, subject)
	b.loginResult(chat, msg, res, ip, "网页将自动进入用户中心。", answer)
}

var portalLoginTpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" type="image/png" href="/favicon.ico?v=600cad3535ba"><link rel="apple-touch-icon" href="/apple-touch-icon.png?v=600cad3535ba">
<meta name="robots" content="noindex"><meta name="referrer" content="no-referrer">
<title>{{.Title}} · 用户中心</title>
<style>
:root{--bg:#F4F5F2;--card:#fff;--ink:#15181C;--mute:#5B6470;--line:#E2E5E0;--pri:#0E7490;--bad:#B91C1C}
@media (prefers-color-scheme:dark){:root{--bg:#101316;--card:#181C20;--ink:#E6E8EA;--mute:#9AA2AB;--line:#2A3036;--pri:#22A3BF;--bad:#F87171}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.6 "IBM Plex Sans","Noto Sans SC",system-ui,sans-serif}
main{max-width:420px;margin:0 auto;padding:12vh 16px 48px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:24px;display:flex;flex-direction:column;gap:14px;text-align:center}
h1{font-size:20px;margin:0}.mute{color:var(--mute);font-size:13px;margin:0}
.btn{display:flex;align-items:center;justify-content:center;width:100%;min-height:44px;border-radius:8px;border:1px solid var(--pri);background:var(--pri);color:#fff;text-decoration:none;font:inherit;font-weight:500;cursor:pointer;padding:0 14px}
.btn.alt{background:var(--card);color:var(--ink);border-color:var(--line)}
.btn:disabled{opacity:.6;cursor:default}
.num{font:600 44px/1 "IBM Plex Mono",ui-monospace,monospace;letter-spacing:.08em;color:var(--pri)}
.err{color:var(--bad);font-size:13px;margin:0}
.mail{display:flex;flex-direction:column;gap:10px}
input{width:100%;min-height:44px;border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--ink);font:inherit;padding:0 12px}
[hidden]{display:none!important}
</style></head><body><main><div class="card">
<h1>{{.Title}}</h1>
<p class="mute">用户中心：查看流量、到期时间和订阅链接</p>
{{if .Mail}}
<div id="m1" class="mail">
<input id="memail" type="email" placeholder="邮箱地址" autocomplete="email" maxlength="254">
<div id="m2" hidden><input id="mcode" inputmode="numeric" placeholder="6 位验证码" autocomplete="one-time-code" maxlength="6"></div>
<button class="btn" id="msend" type="button">发送验证码</button>
<button class="btn" id="mgo" type="button" hidden>登录</button>
<p class="mute" id="mnote" hidden></p>
<p class="err" id="merr" hidden></p>
{{if .Register}}<p class="mute">还没有账号？用邮箱验证后自动注册。</p>{{end}}
</div>
{{end}}
{{if and .Mail .Bot}}<p class="mute">或</p>{{end}}
{{if .Bot}}
<div id="s1"><button class="btn{{if .Mail}} alt{{end}}" id="go" type="button">使用 Telegram 登录</button></div>
<div id="s2" hidden>
<p class="mute">在 Telegram 中点击下面这个数字</p>
<div class="num" id="num"></div>
<a class="btn" id="open" href="#" target="_blank" rel="noopener" style="margin-top:14px">打开 Telegram</a>
<p class="mute" id="state" style="margin-top:10px">等待确认…</p>
</div>
<p class="err" id="err" hidden></p>
<p class="mute">还没绑定 Telegram？打开订阅链接，点「Telegram 机器人绑定」。</p>
{{else if not .Mail}}
<p class="err">管理员尚未启用 Telegram 机器人或邮箱登录，暂时无法登录。</p>
{{end}}
{{if .Shop}}<a class="btn alt" href="/u/shop">🛒 购买套餐</a>{{end}}
</div></main>
{{if .Mail}}<script>
(function(){
var em=document.getElementById('memail'),code=document.getElementById('mcode'),send=document.getElementById('msend'),go=document.getElementById('mgo'),
note=document.getElementById('mnote'),err=document.getElementById('merr'),timer=0;
function show(el,m){el.textContent=m;el.hidden=!m}
function post(p,b){return fetch('/u/'+p,{method:'POST',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},body:JSON.stringify(b),credentials:'same-origin'}).then(function(r){return r.json().then(function(j){if(!r.ok)throw new Error(j.error||r.statusText);return j})})}
function wait(n){clearInterval(timer);send.disabled=true;send.textContent='重新发送（'+n+'）';timer=setInterval(function(){n--;if(n<=0){clearInterval(timer);send.disabled=false;send.textContent='重新发送';return}send.textContent='重新发送（'+n+'）'},1000)}
send.addEventListener('click',function(){var m=em.value.trim();show(err,'');if(!m){show(err,'请填写邮箱地址');return}send.disabled=true;
post('email/code',{email:m,purpose:'login'}).then(function(j){document.getElementById('m2').hidden=false;go.hidden=false;send.className='btn alt';
show(note,'如果这个邮箱可以登录，验证码已经发出（'+Math.round(j.ttl/60)+' 分钟内有效）。没收到请看看垃圾邮件。');code.focus();wait(j.cooldown||60)}).catch(function(e){send.disabled=false;show(err,e.message)})});
function login(){var c=code.value.trim();if(!/^\d{6}$/.test(c)){show(err,'请输入 6 位验证码');return}go.disabled=true;show(err,'');
post('email/verify',{email:em.value.trim(),code:c,purpose:'login'}).then(function(){location.replace('/u')}).catch(function(e){go.disabled=false;show(err,e.message)})}
go.addEventListener('click',login);code.addEventListener('keydown',function(e){if(e.key==='Enter')login()});
})();
</script>{{end}}
{{if .Bot}}<script>
(function(){
var go=document.getElementById('go'),err=document.getElementById('err'),req=null,timer=0;
function show(m){err.textContent=m;err.hidden=!m}
function post(p,b){return fetch('/u/'+p,{method:'POST',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},body:b?JSON.stringify(b):null,credentials:'same-origin'}).then(function(r){return r.json().then(function(j){if(!r.ok)throw new Error(j.error||r.statusText);return j})})}
function reset(m){clearTimeout(timer);req=null;document.getElementById('s1').hidden=false;document.getElementById('s2').hidden=true;go.disabled=false;show(m)}
function poll(){if(!req)return;post('poll',{id:req.id,secret:req.secret}).then(function(j){
if(j.status==='ok'){location.replace('/u');return}
if(j.status==='denied'){reset('登录已被拒绝，请重试');return}
if(j.status==='expired'){reset('登录请求已过期，请重试');return}
if(j.opened)document.getElementById('state').textContent='已在 Telegram 打开，请点击相同的数字';
timer=setTimeout(poll,1500)}).catch(function(){timer=setTimeout(poll,3000)})}
go.addEventListener('click',function(){go.disabled=true;show('');post('login').then(function(j){req=j;
document.getElementById('num').textContent=j.code;document.getElementById('open').href=j.url;
document.getElementById('s1').hidden=true;document.getElementById('s2').hidden=false;timer=setTimeout(poll,1500);
try{window.open(j.url,'_blank','noopener')}catch(e){}}).catch(function(e){reset(e.message||'登录失败')})});
})();
</script>{{end}}
</body></html>`))
