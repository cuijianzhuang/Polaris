package panel

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"github.com/cuijianzhuang/polaris/internal/uitheme"
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
	portalLangCookie  = "polaris_portal_lang"
	portalLoginPrefix = "ulogin_"
	portalSessionTTL  = 30 * 24 * time.Hour
	portalHeader      = "X-Polaris-Portal" // required on state-changing requests (CSRF)
)

func (s *Server) portalEnabled() bool { return s.st.GetSetting("portal_enabled", "false") == "true" }

func portalLang(c *gin.Context) string {
	v, _ := c.Cookie(portalLangCookie)
	return sub.PortalLocale(v)
}

func (s *Server) setPortalLanguage(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	var req struct {
		Lang string `json:"lang"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || (req.Lang != "zh-CN" && req.Lang != "en") {
		badRequest(c, fmt.Errorf("unsupported portal language"))
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: portalLangCookie, Value: req.Lang, Path: portalPath, MaxAge: 365 * 24 * 3600, HttpOnly: true, Secure: c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https", SameSite: http.SameSiteLaxMode})
	c.Status(http.StatusNoContent)
}

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

func (s *Server) issuePortalToken(u *store.User, contexts ...*gin.Context) (string, error) {
	return s.issuePortalTokenVia(u, false, contexts...)
}

// issuePortalMailToken signs a session for an e-mail sign-in.
func (s *Server) issuePortalMailToken(u *store.User, contexts ...*gin.Context) (string, error) {
	return s.issuePortalTokenVia(u, true, contexts...)
}

// pwFP fingerprints the sign-in password hash for portal claims: a
// password session stays valid only while the hash it was issued
// against is current, so setting or changing the password ends it even
// apart from the portal generation bump.
func pwFP(hash string) string {
	if hash == "" {
		return ""
	}
	h := sha256.Sum256([]byte("pw:" + hash))
	return hex.EncodeToString(h[:8])
}

func (s *Server) issuePortalTokenVia(u *store.User, viaMail bool, contexts ...*gin.Context) (string, error) {
	if viaMail {
		return s.issuePortalTokenMode(u, "email", contexts...)
	}
	return s.issuePortalTokenMode(u, "telegram", contexts...)
}

// issuePortalPasswordToken signs a user-center session from a password
// sign-in (userpassword.go).
func (s *Server) issuePortalPasswordToken(u *store.User, contexts ...*gin.Context) (string, error) {
	return s.issuePortalTokenMode(u, "password", contexts...)
}

// ghFP fingerprints the bound GitHub account id for portal claims: a
// GitHub session stays valid only while that same account stays bound,
// so unbinding ends it even apart from the portal generation bump.
func ghFP(id int64) string {
	if id == 0 {
		return ""
	}
	h := sha256.Sum256([]byte("gh:" + strconv.FormatInt(id, 10)))
	return hex.EncodeToString(h[:8])
}

// issuePortalGitHubToken signs a user-center session from a GitHub
// sign-in (usergithub.go).
func (s *Server) issuePortalGitHubToken(u *store.User, contexts ...*gin.Context) (string, error) {
	return s.issuePortalTokenMode(u, "github", contexts...)
}

// pkFP fingerprints the passkey a user-center session was issued with,
// by the key's credential hash: the session stays valid only while
// that key still exists for the user, so deleting the key ends it
// even apart from the portal generation bump (userpasskey.go).
func pkFP(credentialHash string) string {
	if credentialHash == "" {
		return ""
	}
	h := sha256.Sum256([]byte("pk:" + credentialHash))
	return hex.EncodeToString(h[:8])
}

// issuePortalPasskeyToken signs a user-center session from a passkey
// sign-in, stamped with the issuing key's fingerprint.
func (s *Server) issuePortalPasskeyToken(u *store.User, credentialHash string, contexts ...*gin.Context) (string, error) {
	return s.issuePortalTokenModeFP(u, "passkey", pkFP(credentialHash), contexts...)
}

// ssoFP fingerprints the SSO binding a portal session was issued
// against: the session stays valid only while a binding row with this
// (provider, subject) still belongs to the user, so unbinding ends it
// even apart from the portal generation bump (usersso.go).
func ssoFP(providerID, subject string) string {
	if providerID == "" || subject == "" {
		return ""
	}
	h := sha256.Sum256([]byte("sso:" + providerID + ":" + subject))
	return hex.EncodeToString(h[:8])
}

// issuePortalSSOToken signs a user-center session from an SSO sign-in
// (usersso.go), stamped with the issuing binding's fingerprint.
func (s *Server) issuePortalSSOToken(u *store.User, providerID, subject string, contexts ...*gin.Context) (string, error) {
	return s.issuePortalTokenModeFP(u, "sso", ssoFP(providerID, subject), contexts...)
}

// issuePortalTokenMode signs a user-center session for one sign-in
// mode: "telegram" (bound chat), "email" (bound address), "password"
// (current password hash) or "github" (bound account). The
// fingerprint-bound modes ("passkey", "sso") go through
// issuePortalTokenModeFP. The mode is stamped into the claims as the
// identity the session is later checked against in portalUser.
func (s *Server) issuePortalTokenMode(u *store.User, via string, contexts ...*gin.Context) (string, error) {
	return s.issuePortalTokenModeFP(u, via, "", contexts...)
}

// issuePortalTokenModeFP is issuePortalTokenMode with an explicit
// fingerprint for the modes whose identity the user row alone cannot
// name: a passkey session is bound to one key, an SSO session to one
// binding row. Mode-preserving re-issues (session renewal,
// subscription reset) pass no fingerprint and inherit the current
// session's, exactly like the other modes inherit theirs from the
// user row.
func (s *Server) issuePortalTokenModeFP(u *store.User, via, fp string, contexts ...*gin.Context) (string, error) {
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "u:" + u.ID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(portalSessionTTL)),
		},
		Username: u.Username,
		User:     u.ID,
		SubFP:    subFingerprint(u.SubToken),
	}
	switch via {
	case "email":
		c.MailFP = mailFP(u.Email)
	case "password":
		c.PwFP = pwFP(u.PasswordHash)
	case "github":
		c.GhFP = ghFP(u.GitHubID)
	case "passkey":
		c.PkFP = fp
		if c.PkFP == "" && len(contexts) > 0 && contexts[0] != nil {
			if prev := s.portalClaims(contexts[0]); prev != nil && prev.User == u.ID {
				c.PkFP = prev.PkFP
			}
		}
		if c.PkFP == "" {
			return "", errors.New("passkey session needs a key fingerprint")
		}
	case "sso":
		c.SsoFP = fp
		if c.SsoFP == "" && len(contexts) > 0 && contexts[0] != nil {
			if prev := s.portalClaims(contexts[0]); prev != nil && prev.User == u.ID {
				c.SsoFP = prev.SsoFP
			}
		}
		if c.SsoFP == "" {
			return "", errors.New("sso session needs a binding fingerprint")
		}
	default:
		via = "telegram"
		c.Chat = u.TelegramID
	}
	if err := s.recordPortalSession(u, &c, via, contexts...); err != nil {
		return "", err
	}
	token, err := s.signToken(jwt.NewWithClaims(jwt.SigningMethodHS256, c))
	return token, err
}

// portalSessionMode names the sign-in mode of the current session, so
// credential renewals keep the identity they started with.
func (s *Server) portalSessionMode(c *gin.Context) string {
	cl := s.portalClaims(c)
	switch {
	case cl != nil && cl.MailFP != "":
		return "email"
	case cl != nil && cl.PwFP != "":
		return "password"
	case cl != nil && cl.GhFP != "":
		return "github"
	case cl != nil && cl.PkFP != "":
		return "passkey"
	case cl != nil && cl.SsoFP != "":
		return "sso"
	default:
		return "telegram"
	}
}

// portalClaims are the session's claims, or nil.
func (s *Server) portalClaims(c *gin.Context) *claims {
	if cached, ok := c.Get("portalClaims"); ok {
		return cached.(*claims)
	}
	ck, err := c.Cookie(portalCookie)
	if err != nil || ck == "" {
		return nil
	}
	cl, err := s.parseToken(ck)
	if err != nil || cl.User == "" || !s.portalSessionAlive(cl) {
		return nil
	}
	c.Set("portalClaims", cl)
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

// portalActorForClaims names the session's user by its sign-in mode:
// mail: for e-mail sessions, pw: for password sessions, gh: for GitHub
// sessions, pk: for passkey sessions, sso: for SSO sessions, tg:
// otherwise.
func portalActorForClaims(cl *claims, u *store.User) string {
	switch {
	case cl != nil && cl.MailFP != "":
		return "mail:" + u.Username
	case cl != nil && cl.PwFP != "":
		return "pw:" + u.Username
	case cl != nil && cl.GhFP != "":
		return "gh:" + u.Username
	case cl != nil && cl.PkFP != "":
		return "pk:" + u.Username
	case cl != nil && cl.SsoFP != "":
		return "sso:" + u.Username
	default:
		return "tg:" + u.Username
	}
}

// portalActorFor is portalActorForClaims for the current request.
func (s *Server) portalActorFor(c *gin.Context, u *store.User) string {
	return portalActorForClaims(s.portalClaims(c), u)
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
	case cl.PwFP != "" && u.PasswordHash != "" && pwFP(u.PasswordHash) == cl.PwFP:
	case cl.GhFP != "" && u.GitHubID != 0 && ghFP(u.GitHubID) == cl.GhFP:
	case cl.PkFP != "" && s.userPasskeyFPExists(u.ID, cl.PkFP):
	case cl.SsoFP != "" && s.userSSOBindingFPExists(u.ID, cl.SsoFP):
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
		ssoProviders := s.publicSSOProviders()
		var buf strings.Builder
		if err := portalLoginTpl.Execute(&buf, map[string]any{
			"Lang":         portalLang(c),
			"Title":        s.subTitle(s.st.GetSetting("site_title", "Polaris")),
			"Bot":          s.portalTelegramConfigured(),
			"GitHub":       s.githubEnabled(),
			"SSO":          s.ssoUserLoginOn() && len(ssoProviders) > 0,
			"SSOProviders": ssoProviders,
			"Passkey":      s.userPasskeyAvailable("portal"),
			"Mail":         s.mailLoginOn(),
			"Register":     s.mailRegisterOn(),
			"PwRegister":   s.passwordRegisterOn(),
			"AnyRegister":  s.mailRegisterOn() || s.passwordRegisterOn(),
			"RegisterMode": c.Request.URL.Path == portalPath+"/register" && (s.mailRegisterOn() || s.passwordRegisterOn()),
			"Next":         next,
			"Shop":         s.webShopOn(),
		}); err != nil {
			serverError(c, err)
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(buf.String()))
		return
	}
	info := sub.Info{Title: s.st.GetSetting("site_title", "Polaris"), Lang: portalLang(c), Username: u.Username, Upload: u.UsedUp, Download: u.UsedDown,
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
	info.TelegramBound = u.TelegramID != 0
	info.TelegramBind = s.portalTelegramConfigured()
	info.LoginVia, info.MailBind, info.Email = "Telegram", s.mailLoginOn(), u.Email
	if s.portalViaMail(c) {
		info.LoginVia = "邮箱"
	}
	if cl := s.portalClaims(c); cl != nil && cl.PwFP != "" {
		info.LoginVia = "密码"
	}
	if cl := s.portalClaims(c); cl != nil && cl.GhFP != "" {
		info.LoginVia = "GitHub"
	}
	if cl := s.portalClaims(c); cl != nil && cl.PkFP != "" {
		info.LoginVia = "通行密钥"
	}
	if cl := s.portalClaims(c); cl != nil && cl.SsoFP != "" {
		info.LoginVia = "单点登录"
	}
	info.PasswordSet = u.PasswordHash != ""
	info.GitHubBound, info.GitHubLogin = u.GitHubID != 0, u.GitHubLogin
	info.GitHubBind = s.githubEnabled()
	info.PasskeyCount = s.userPasskeyCount(u.ID)
	info.PasskeyBind = s.userPasskeyAvailable("portal")
	for _, b := range s.userSSOBindings(u.ID) {
		info.SSOBindings = append(info.SSOBindings, sub.SSOBindingInfo{ProviderName: s.ssoProviderName(b.ProviderID), Login: b.Login})
	}
	info.SSOUserLogin = s.ssoUserLoginOn()
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
	if s.effectiveUserNodesMode() != userNodesOff {
		info.Extras = append(info.Extras, sub.ExtraLink{Title: "🖥️ 我的节点", URL: portalPath + "/my-nodes-page"})
	}
	if s.aiPortalOn() {
		info.PortalAIURL = portalPath + "/assistant"
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
	tok, err := s.issuePortalToken(&u, c)
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
	// Storage failures must not look like a successful server-side revocation.
	if token, err := c.Cookie(portalCookie); err == nil {
		if cl, err := s.parseToken(token); err == nil && cl.User != "" && cl.SID != "" {
			err = s.st.DB.Transaction(func(tx *gorm.DB) error {
				epoch, e := lockPortalEpoch(tx, cl.User)
				if e != nil {
					return e
				}
				if epoch.Version != cl.PortalEpoch {
					return nil
				} // this cookie is already invalid
				return tx.Model(&store.PortalSession{}).Where("id = ? AND user_id = ? AND epoch = ? AND sub_fp = ? AND identity_fp = ?", cl.SID, cl.User, cl.PortalEpoch, cl.SubFP, portalIdentity(cl)).Update("revoked", true).Error
			})
			if err != nil {
				serverError(c, err)
				return
			}
		}
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
	s.audit(s.portalActorFor(c, u), "user.reset_token", u.Username, "user center", clientIP(c.Request))
	tok, err := s.issuePortalTokenMode(u, s.portalSessionMode(c), c)
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

var portalLoginTpl = template.Must(template.New("login").Funcs(template.FuncMap{"uiTokens": func() template.CSS { return template.CSS(uitheme.CSS) }, "t": sub.PortalText}).Parse(portalLoginHTML))
