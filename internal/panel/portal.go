package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

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
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.jwtKey)
}

// portalUser returns the signed-in user, or nil.
func (s *Server) portalUser(c *gin.Context) *store.User {
	ck, err := c.Cookie(portalCookie)
	if err != nil || ck == "" {
		return nil
	}
	cl, err := s.parseToken(ck)
	if err != nil || cl.User == "" {
		return nil
	}
	var u store.User
	if s.st.DB.First(&u, "id = ? AND is_service = ?", cl.User, false).Error != nil {
		return nil
	}
	if u.TelegramID == 0 || u.TelegramID != cl.Chat || subFingerprint(u.SubToken) != cl.SubFP {
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
			"Title": s.subTitle(s.st.GetSetting("site_title", "Polaris")),
			"Bot":   s.tgWebLoginEnabled(),
		})
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(buf.String()))
		return
	}
	info := sub.Info{Title: s.st.GetSetting("site_title", "Polaris"), Username: u.Username, Upload: u.UsedUp, Download: u.UsedDown,
		Total: u.TrafficLimit, Expire: u.ExpireAt, URL: s.subURL(u.SubToken)}
	var nodes []sub.Node
	if u.Status == "active" {
		var err error
		if nodes, err = s.subNodes(u); err != nil {
			serverError(c, err)
			return
		}
	}
	info.Title = s.subTitle(info.Title)
	s.pageExtras(&info, u, nodes)
	info.BotURL = "" // already bound: that is how they signed in
	info.Portal = true
	b, err := sub.RenderPage(info)
	if err != nil {
		serverError(c, err)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", b)
}

// portalLoginStart: POST /u/login.
func (s *Server) portalLoginStart(c *gin.Context) {
	if !s.tgWebLoginEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Telegram 机器人未运行，请联系管理员"})
		return
	}
	s.startWebLogin(c, &s.portalWeb, portalLoginPrefix)
}

// portalLoginPoll: POST /u/poll {id, secret}; sets the session cookie once
// the user taps the right number.
func (s *Server) portalLoginPoll(c *gin.Context) {
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
	s.audit("tg:"+u.Username, "user.reset_token", u.Username, "user center", clientIP(c.Request))
	tok, err := s.issuePortalToken(u)
	if err != nil {
		serverError(c, err)
		return
	}
	s.setPortalCookie(c, tok, int(portalSessionTTL.Seconds()))
	c.JSON(http.StatusOK, gin.H{"ok": true})
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
.btn{display:flex;align-items:center;justify-content:center;min-height:44px;border-radius:8px;border:1px solid var(--pri);background:var(--pri);color:#fff;text-decoration:none;font:inherit;font-weight:500;cursor:pointer;padding:0 14px}
.btn.alt{background:var(--card);color:var(--ink);border-color:var(--line)}
.btn:disabled{opacity:.6;cursor:default}
.num{font:600 44px/1 "IBM Plex Mono",ui-monospace,monospace;letter-spacing:.08em;color:var(--pri)}
.err{color:var(--bad);font-size:13px;margin:0}
[hidden]{display:none!important}
</style></head><body><main><div class="card">
<h1>{{.Title}}</h1>
<p class="mute">用户中心：查看流量、到期时间和订阅链接</p>
{{if .Bot}}
<div id="s1"><button class="btn" id="go" type="button">使用 Telegram 登录</button></div>
<div id="s2" hidden>
<p class="mute">在 Telegram 中点击下面这个数字</p>
<div class="num" id="num"></div>
<a class="btn" id="open" href="#" target="_blank" rel="noopener" style="margin-top:14px">打开 Telegram</a>
<p class="mute" id="state" style="margin-top:10px">等待确认…</p>
</div>
<p class="err" id="err" hidden></p>
<p class="mute">还没绑定 Telegram？打开订阅链接，点「Telegram 机器人绑定」。</p>
{{else}}
<p class="err">管理员尚未启用 Telegram 机器人，暂时无法登录。</p>
{{end}}
</div></main>
{{if .Bot}}<script>
(function(){
var go=document.getElementById('go'),err=document.getElementById('err'),req=null,timer=0;
function show(m){err.textContent=m;err.hidden=!m}
function post(p,b){return fetch('/u/'+p,{method:'POST',headers:{'Content-Type':'application/json'},body:b?JSON.stringify(b):null,credentials:'same-origin'}).then(function(r){return r.json().then(function(j){if(!r.ok)throw new Error(j.error||r.statusText);return j})})}
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
