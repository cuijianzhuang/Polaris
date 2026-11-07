package panel

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
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

// User center: proxy users sign in at /u with an email code or Telegram. The
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

// Keep the portal login method visible while a configured bot reconnects.
func (s *Server) portalTelegramConfigured() bool {
	return s.st.GetSetting("tg_enabled", "false") == "true" && s.st.Box.MustOpen(s.st.GetSetting("tg_token", "")) != ""
}

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
	next := portalPath
	if c.Query("next") == shopWebPath {
		next = shopWebPath
	}
	u := s.portalUser(c)
	if u != nil && c.Request.URL.Path != portalPath {
		c.Redirect(http.StatusFound, next)
		return
	}
	if u == nil {
		var buf strings.Builder
		if err := portalLoginTpl.Execute(&buf, map[string]any{
			"Title":        s.subTitle(s.st.GetSetting("site_title", "Polaris")),
			"Bot":          s.portalTelegramConfigured(),
			"Mail":         s.mailLoginOn(),
			"Register":     s.mailRegisterOn(),
			"RegisterMode": c.Request.URL.Path == portalPath+"/register" && s.mailRegisterOn(),
			"Next":         next,
			"Shop":         s.webShopOn(),
		}); err != nil {
			serverError(c, err)
			return
		}
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
	if len(info.Daily) > 7 {
		info.Daily = info.Daily[len(info.Daily)-7:]
	}
	if err := s.portalNodeDetails(&info, nodes); err != nil {
		serverError(c, err)
		return
	}
	// Limit the initial page, while telling users when older orders exist.
	var orders []store.Order
	if err := s.st.DB.Where("user_id = ?", u.ID).Order("created_at DESC, id DESC").Limit(51).Find(&orders).Error; err != nil {
		serverError(c, err)
		return
	}
	info.PortalOrdersMore = len(orders) > 50
	if info.PortalOrdersMore {
		orders = orders[:50]
	}
	for _, o := range orders {
		info.PortalOrders = append(info.PortalOrders, sub.PortalOrder{ID: o.ID, Name: orDefault(o.PlanName, "套餐订单"), Amount: webAmount(&o), Status: orDefault(orderStatusName[o.Status], "处理中"), CreatedAt: o.CreatedAt.Local().Format("2006-01-02 15:04")})
	}
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
		info.PortalShopURL = shopWebPath
		info.Extras = append(info.Extras, sub.ExtraLink{Title: "🛒 购买 / 续费", URL: shopWebPath})
	}
	if u.FwdMax > 0 && s.license().Allow(license.FeaturePlanAddons) {
		info.Extras = append(info.Extras, sub.ExtraLink{Title: "🔀 我的转发", URL: portalPath + "/forwards"})
	}
	if s.ticketsOn() {
		info.PortalTicketsURL = portalPath + "/tickets"
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
	if !s.portalTelegramConfigured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "管理员尚未启用 Telegram 机器人登录"})
		return
	}
	if !s.tgWebLoginEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Telegram 机器人暂时未就绪，请稍后重试或联系管理员"})
		return
	}
	s.startWebLogin(c, &s.portalWeb, portalLoginPrefix)
}

// portalLoginCancel invalidates only the request owned by this browser secret.
func (s *Server) portalLoginCancel(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	var req struct {
		ID     string `json:"id" binding:"required"`
		Secret string `json:"secret" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var pending tgWebLogin
	s.portalWeb.eph.update(s.portalWeb.key(req.ID), &pending, func() ephAction {
		if subtle.ConstantTimeCompare([]byte(pending.Secret), []byte(req.Secret)) == 1 {
			return ephDelete
		}
		return ephKeep
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// portalLoginPoll: POST /u/poll {id, secret}; sets the session cookie once
// the user taps the right number.
func (s *Server) portalLoginPoll(c *gin.Context) {
	// The header stops a cross-site form from planting someone else's
	// session (login CSRF).
	if !portalWrite(c) {
		return
	}
	if !s.portalTelegramConfigured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "管理员已关闭 Telegram 机器人登录"})
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

//go:embed assets/portal-login.html
var portalLoginHTML string

var portalLoginTpl = template.Must(template.New("login").Parse(portalLoginHTML))
