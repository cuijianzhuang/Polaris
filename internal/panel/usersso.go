package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// SSO as a registered user's sign-in method — see
// docs/design/user-identity-oauth-passkey.md (batch 3b, D4–D6). The
// providers are the admins' own SSOProvider rows, shared as
// configuration only: identity resolution, bindings and sessions are
// a separate namespace (store.UserSSOBinding), and the shared
// callback dispatches user states on oauthState.Kind before the
// admin switch can see them — the same shape as the GitHub user flow
// (usergithub.go), including the ticket redemption for binds, because
// the callback request carries no usable session on either face.

// errSSOBound refuses a bind (or a registration race) when the SSO
// account already belongs to another user. SSO bindings never merge
// accounts — merging exists only on the e-mail path, where mailbox
// ownership is proven by a code (design D4).
var errSSOBound = errors.New("该单点登录账号已绑定其他账号")

// errSSOEmailMatched reports that an auto-registration lost the race
// for its verified e-mail: the address now belongs to an existing
// account, which the login flow answers with the bind-after-login
// guidance instead of a second account.
var errSSOEmailMatched = errors.New("该邮箱已注册账号")

// ssoUserLoginOn is the master switch (design D6): off, no SSO entry
// point serves registered users on either face and no user button is
// rendered. The admin login page is unaffected — publicSSOProviders
// never reads this.
func (s *Server) ssoUserLoginOn() bool {
	return s.st.GetSetting("sso_user_login", "false") == "true"
}

// userSSOProviders are the providers a registered user may use: the
// admin-curated enabled set, visible only while the master switch is
// on (design D6 — the provider's Enabled flag is the granularity).
func (s *Server) userSSOProviders() []gin.H {
	if !s.ssoUserLoginOn() {
		return []gin.H{}
	}
	return s.publicSSOProviders()
}

func (s *Server) userSSOBindings(userID string) []store.UserSSOBinding {
	var bs []store.UserSSOBinding
	s.st.DB.Where("user_id = ?", userID).Order("created_at").Find(&bs)
	return bs
}

func (s *Server) userSSOCount(userID string) int {
	var n int64
	s.st.DB.Model(&store.UserSSOBinding{}).Where("user_id = ?", userID).Count(&n)
	return int(n)
}

// userSSOBindingFPExists reports whether one of the user's binding
// rows still carries the fingerprint a portal SSO session was issued
// against — the per-request recheck that ends the session the moment
// the binding goes away (design D7.6, the GhFP idea applied to a
// binding row instead of a user-row field).
func (s *Server) userSSOBindingFPExists(userID, fp string) bool {
	if fp == "" {
		return false
	}
	for _, b := range s.userSSOBindings(userID) {
		if ssoFP(b.ProviderID, b.Subject) == fp {
			return true
		}
	}
	return false
}

func (s *Server) ssoProviderName(id string) string {
	var p store.SSOProvider
	if s.st.DB.First(&p, "id = ?", id).Error != nil {
		return id
	}
	return p.Name
}

// userSSOBindingViews is the bindings-card payload: each bound
// provider with the external login it shows.
func (s *Server) userSSOBindingViews(userID string) []gin.H {
	out := []gin.H{}
	for _, b := range s.userSSOBindings(userID) {
		out = append(out, gin.H{"providerId": b.ProviderID, "providerName": s.ssoProviderName(b.ProviderID), "login": b.Login})
	}
	return out
}

// --- identity reading ---

// ssoUserIdentity is the user flow's identity read: the same token
// exchange and userinfo posture as the admins' ssoIdentity (no
// id_token verification on either side), plus the two claims the user
// flow's no-silent-merge rule is built on — the e-mail address and
// whether the provider asserts it verified. email_verified accepts
// the boolean and the string form; a missing claim is unverified, and
// plain OAuth 2.0 providers, which do not issue the claim, are
// therefore always unverified here (design D4).
func (s *Server) ssoUserIdentity(ctx context.Context, p *store.SSOProvider, code, verifier, redirect string) (subject, login, email string, verified bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	secret := s.st.Box.MustOpen(p.ClientSecret)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {p.ClientID}, "client_secret": {secret}, "code_verifier": {verifier}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(secret))
	resp, err := ssoClient.Do(req)
	if err != nil {
		return "", "", "", false, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &tok) != nil || tok.AccessToken == "" {
		if v, perr := url.ParseQuery(string(body)); perr == nil && v.Get("access_token") != "" { // GitHub-style form reply
			tok.AccessToken = v.Get("access_token")
		} else {
			return "", "", "", false, fmt.Errorf("%s 授权失败：%s", p.Name, orDefault(tok.Description, orDefault(tok.Error, resp.Status)))
		}
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, p.UserInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err = ssoClient.Do(req)
	if err != nil {
		return "", "", "", false, err
	}
	defer resp.Body.Close()
	var info map[string]any
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info) != nil {
		return "", "", "", false, fmt.Errorf("读取 %s 用户信息失败：%s", p.Name, resp.Status)
	}
	str := func(k string) string {
		switch v := info[k].(type) {
		case string:
			return v
		case float64:
			return fmt.Sprintf("%.0f", v)
		}
		return ""
	}
	subject = str(p.SubjectField)
	if subject == "" {
		return "", "", "", false, fmt.Errorf("%s 用户信息中没有 %q 字段", p.Name, p.SubjectField)
	}
	login = orDefault(str(p.LoginField), orDefault(str("email"), orDefault(str("name"), subject)))
	email = strings.TrimSpace(str("email"))
	switch v := info["email_verified"].(type) {
	case bool:
		verified = v
	case string:
		verified = strings.EqualFold(v, "true")
	}
	return subject, login, email, verified, nil
}

// --- entry points ---

type userSSOURLReq struct {
	ProviderID string `json:"providerId" binding:"required"`
}

// userSSOAuthURL mints a user-flow state for one provider and answers
// the authorize URL, shaped exactly like the admin one (same provider
// app, same callback, PKCE) apart from the state payload.
func (s *Server) userSSOAuthURL(c *gin.Context, target, mode, userID, providerID string) {
	if !s.ssoUserLoginOn() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册用户单点登录未开启"})
		return
	}
	var p store.SSOProvider
	if s.st.DB.First(&p, "id = ? AND enabled = ?", providerID, true).Error != nil {
		notFound(c)
		return
	}
	verifier, challenge := pkcePair()
	state := s.ghAuth.put(oauthState{Mode: mode, Kind: "user", Target: target, UserID: userID, Provider: p.ID, Verifier: verifier})
	q := url.Values{"client_id": {p.ClientID}, "redirect_uri": {s.ssoCallbackURL(c.Request)}, "response_type": {"code"},
		"state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	if p.Scopes != "" {
		q.Set("scope", p.Scopes)
	}
	sep := "?"
	if strings.Contains(p.AuthURL, "?") {
		sep = "&"
	}
	c.JSON(http.StatusOK, gin.H{"url": p.AuthURL + sep + q.Encode()})
}

// userSSOLoginURLPanel: POST /auth/sso/user/login-url — the panel
// face's SSO sign-in entry (public, behind loginGuard).
func (s *Server) userSSOLoginURLPanel(c *gin.Context) {
	if !s.memberLoginEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册用户登录面板未开启"})
		return
	}
	var req userSSOURLReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.userSSOAuthURL(c, "panel", "login", "", req.ProviderID)
}

// userSSOBindURLPanel: POST /auth/sso/user/bind-url — a member starts
// binding an SSO account to their own account.
func (s *Server) userSSOBindURLPanel(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可绑定单点登录"})
		return
	}
	var req userSSOURLReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.userSSOAuthURL(c, "panel", "bind", u.ID, req.ProviderID)
}

// userSSOLoginURLPortal: POST /u/sso/login-url — the user center's SSO
// sign-in entry (no session yet, by definition).
func (s *Server) userSSOLoginURLPortal(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if until, banned := s.ipBannedUntil(clientIP(c.Request)); banned {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": fmt.Sprintf("尝试过于频繁，请在 %s 后再试", until.Format("15:04"))})
		return
	}
	var req userSSOURLReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.userSSOAuthURL(c, "portal", "login", "", req.ProviderID)
}

// userSSOBindURLPortal: POST /u/sso/bind-url — a signed-in portal user
// starts binding an SSO account to their own account.
func (s *Server) userSSOBindURLPortal(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req userSSOURLReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	s.userSSOAuthURL(c, "portal", "bind", u.ID, req.ProviderID)
}

// userSSOFail lands a failed user flow back on the face it came from,
// in the channel that face reads: the panel reads the URL fragment
// (the gh_error key the login and account pages already consume), the
// portal reads the query string.
func (s *Server) userSSOFail(c *gin.Context, st oauthState, msg string) {
	if st.Target == "portal" {
		if st.Mode == "bind" {
			c.Redirect(http.StatusFound, portalPath+"?sso_error="+url.QueryEscape(msg)+"#account")
			return
		}
		c.Redirect(http.StatusFound, portalPath+"/login?sso_error="+url.QueryEscape(msg))
		return
	}
	page := ""
	if st.Mode == "bind" {
		page = "account"
	}
	c.Redirect(http.StatusFound, s.adminPageURL(c.Request, page, "gh_error="+url.QueryEscape(msg)))
}

// --- the callback's user half ---

// ssoUserCallback is the user half of the shared SSO callback (sso.go
// dispatches on the state's Kind before the admin switch can see it).
func (s *Server) ssoUserCallback(c *gin.Context, st oauthState) {
	ip := clientIP(c.Request)
	if e := c.Query("error"); e != "" {
		s.userSSOFail(c, st, "已取消授权："+orDefault(c.Query("error_description"), e))
		return
	}
	if !s.ssoUserLoginOn() {
		s.userSSOFail(c, st, "注册用户单点登录未开启")
		return
	}
	var p store.SSOProvider
	if s.st.DB.First(&p, "id = ?", st.Provider).Error != nil {
		s.userSSOFail(c, st, "登录方式已被删除")
		return
	}
	subject, login, email, verified, err := s.ssoUserIdentity(c.Request.Context(), &p, c.Query("code"), st.Verifier, s.ssoCallbackURL(c.Request))
	if err != nil {
		s.recordIPFailure(ip, p.Name+" 用户登录失败")
		s.userSSOFail(c, st, err.Error())
		return
	}
	if st.Mode == "bind" {
		s.ssoUserBindCallback(c, st, &p, subject, login)
		return
	}
	s.ssoUserLoginCallback(c, st, &p, subject, login, email, verified)
}

// ssoUserLoginCallback resolves a user login in the order design D4
// fixes: binding hit → sign in; verified e-mail matching an existing
// account → refuse with bind-after-login guidance (never a silent
// merge — User.Email has unverified sources); verified e-mail
// matching nobody + registration open → register, bind, sign in;
// anything else → refuse.
func (s *Server) ssoUserLoginCallback(c *gin.Context, st oauthState, p *store.SSOProvider, subject, login, email string, verified bool) {
	ip := clientIP(c.Request)
	var u store.User
	var b store.UserSSOBinding
	if err := s.st.DB.First(&b, "provider_id = ? AND subject = ?", p.ID, subject).Error; err == nil {
		if err := s.st.DB.First(&u, "id = ? AND is_service = ?", b.UserID, false).Error; err != nil {
			s.recordIPFailure(ip, p.Name+" 绑定账号不存在")
			s.userSSOFail(c, st, "无法使用该账号登录")
			return
		}
	} else {
		if !verified || email == "" {
			s.recordIPFailure(ip, p.Name+" 账号未绑定且邮箱未验证")
			s.audit("sso:"+login, "user.sso_login_failed", login, "unbound, email unverified", ip)
			s.userSSOFail(c, st, "该 "+p.Name+" 账号未绑定任何账号，且未提供已验证的邮箱：请先注册账号，再在绑定管理中绑定此登录方式")
			return
		}
		var matched int64
		s.st.DB.Model(&store.User{}).Where("LOWER(email) = ? AND is_service = ?", strings.ToLower(email), false).Count(&matched)
		if matched > 0 {
			s.recordIPFailure(ip, p.Name+" 邮箱命中已有账号")
			s.audit("sso:"+login, "user.sso_login_failed", login, "verified email matches existing user", ip)
			s.userSSOFail(c, st, "该 "+p.Name+" 账号的邮箱与已有账号相同，请先用原方式登录，在绑定管理中绑定此登录方式")
			return
		}
		if !s.userRegistrationOpen() {
			s.audit("sso:"+login, "user.sso_login_failed", login, "unbound, registration closed", ip)
			s.userSSOFail(c, st, "该 "+p.Name+" 账号未绑定任何账号，请先注册后在账号设置中绑定")
			return
		}
		nu, rerr := s.registerBySSO(p, subject, login, email, ip)
		switch {
		case rerr == nil:
			u = *nu
		case errors.Is(rerr, errSSOBound):
			// Lost a race: the account was bound between the lookup
			// and the create — sign that account in instead.
			if err := s.st.DB.First(&b, "provider_id = ? AND subject = ?", p.ID, subject).Error; err != nil ||
				s.st.DB.First(&u, "id = ? AND is_service = ?", b.UserID, false).Error != nil {
				s.userSSOFail(c, st, "登录失败，请稍后重试")
				return
			}
		case errors.Is(rerr, errSSOEmailMatched):
			s.recordIPFailure(ip, p.Name+" 邮箱命中已有账号")
			s.audit("sso:"+login, "user.sso_login_failed", login, "verified email matches existing user", ip)
			s.userSSOFail(c, st, "该 "+p.Name+" 账号的邮箱与已有账号相同，请先用原方式登录，在绑定管理中绑定此登录方式")
			return
		default:
			s.userSSOFail(c, st, rerr.Error())
			return
		}
	}
	if u.Status == "disabled" {
		s.audit("sso:"+login, "user.sso_login_failed", login, "disabled", ip)
		s.userSSOFail(c, st, "无法使用该账号登录")
		return
	}
	switch st.Target {
	case "portal":
		if !s.portalEnabled() {
			s.userSSOFail(c, st, "用户中心暂未开放")
			return
		}
		tok, err := s.issuePortalSSOToken(&u, p.ID, subject, c)
		if err != nil {
			s.userSSOFail(c, st, "登录失败，请稍后重试")
			return
		}
		s.setPortalCookie(c, tok, int(portalSessionTTL.Seconds()))
		s.audit("sso:"+u.Username, "user.portal_login", u.Username, "sso:"+p.Name, ip)
		c.Redirect(http.StatusFound, portalPath)
	default: // panel
		if !s.memberLoginEnabled() {
			s.userSSOFail(c, st, "注册用户登录面板未开启")
			return
		}
		tok, err := s.issueMemberToken(&u, c)
		if err != nil {
			s.userSSOFail(c, st, "登录失败，请稍后重试")
			return
		}
		s.audit(memberActor(&u), "member.login", u.Username, "sso:"+p.Name, ip)
		c.Redirect(http.StatusFound, s.adminPageURL(c.Request, "", "token="+url.QueryEscape(tok)+"&user="+url.QueryEscape(memberDisplayName(&u))))
	}
}

// ssoBindTicket is what a bind callback hands back to the face that
// started the flow — the SSO twin of githubBindTicket, for the same
// reason: the callback request carries no usable session on either
// face, so the signed-in face redeems the bind and that redemption is
// where the state's user and the session's user are compared.
type ssoBindTicket struct {
	UserID     string
	ProviderID string
	Subject    string
	Login      string
	Target     string
}

// ssoUserBindCallback handles the bind intent: occupancy is checked
// up front (another user's binding refuses outright), then the bind
// is handed to the originating face as a single-use ticket for the
// signed-in redemption (see ssoBindTicket).
func (s *Server) ssoUserBindCallback(c *gin.Context, st oauthState, p *store.SSOProvider, subject, login string) {
	ip := clientIP(c.Request)
	var other store.UserSSOBinding
	if err := s.st.DB.First(&other, "provider_id = ? AND subject = ?", p.ID, subject).Error; err == nil {
		if other.UserID != st.UserID {
			s.recordIPFailure(ip, p.Name+" 绑定冲突")
			s.audit("sso:"+login, "user.sso_bind_failed", login, "bound to another user", ip)
			s.userSSOFail(c, st, errSSOBound.Error())
			return
		}
		// Already bound to the initiator: nothing to redeem.
		if st.Target == "portal" {
			c.Redirect(http.StatusFound, portalPath+"#account")
			return
		}
		c.Redirect(http.StatusFound, s.adminPageURL(c.Request, "account", "sso_bound="+url.QueryEscape(p.Name+" · "+login)))
		return
	}
	key := pki.Token(24)
	ticket := ssoBindTicket{UserID: st.UserID, ProviderID: p.ID, Subject: subject, Login: login, Target: st.Target}
	if err := s.eph.put("ssobind:"+key, ticket, oauthTTL); err != nil {
		s.userSSOFail(c, st, "绑定失败，请稍后重试")
		return
	}
	if st.Target == "portal" {
		c.Redirect(http.StatusFound, portalPath+"?ssobind="+key+"#account")
		return
	}
	c.Redirect(http.StatusFound, s.adminPageURL(c.Request, "account", "sso_bind_ticket="+url.QueryEscape(key)))
}

// completeSSOBind redeems a bind ticket for the signed-in user. The
// ticket is spent even when the session user does not match the user
// who started the flow: a mismatched redemption voids the whole
// attempt (design D8 red line 4). Occupancy is re-checked under the
// binding lock — the callback's early check is only a shortcut. A
// user holds at most one binding per provider: rebinding replaces
// their own previous row for that provider, as the admin flow does.
func (s *Server) completeSSOBind(u *store.User, key string) (providerName, login string, err error) {
	var ticket ssoBindTicket
	if !s.eph.take("ssobind:"+key, &ticket) {
		return "", "", errors.New("绑定请求已过期，请重新发起")
	}
	if ticket.UserID != u.ID {
		return "", "", errBindingIdentity
	}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockAccountBindings(tx); err != nil {
			return err
		}
		if _, err := lockBindingUsers(tx, u.ID); err != nil {
			return err
		}
		var other store.UserSSOBinding
		if err := tx.First(&other, "provider_id = ? AND subject = ? AND user_id <> ?", ticket.ProviderID, ticket.Subject, u.ID).Error; err == nil {
			return errSSOBound
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var mine store.UserSSOBinding
		if err := tx.First(&mine, "provider_id = ? AND subject = ? AND user_id = ?", ticket.ProviderID, ticket.Subject, u.ID).Error; err == nil {
			login = mine.Login
			return nil // re-bind of the same account: no-op
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Where("provider_id = ? AND user_id = ?", ticket.ProviderID, u.ID).Delete(&store.UserSSOBinding{}).Error; err != nil {
			return err
		}
		row := store.UserSSOBinding{ProviderID: ticket.ProviderID, Subject: ticket.Subject, UserID: u.ID, Login: ticket.Login, CreatedAt: time.Now().UTC()}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		login = ticket.Login
		cur := *u
		return s.queueUserEvent(tx, "user.updated", &cur, map[string]any{"via": "sso_binding"})
	})
	if err != nil {
		return "", "", err
	}
	return s.ssoProviderName(ticket.ProviderID), login, nil
}

// ssoBindError answers a failed bind action on a JSON face.
func ssoBindError(c *gin.Context, err error) {
	if errors.Is(err, errSSOBound) || errors.Is(err, errBindingIdentity) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "account_binding_conflict"})
		return
	}
	badRequest(c, err)
}

type bindCompleteReq struct {
	Ticket string `json:"ticket" binding:"required"`
}

// memberSSOBindComplete: POST /auth/bindings/sso/complete {ticket} —
// the panel redemption of an SSO bind ticket, under the member
// session the flow started from.
func (s *Server) memberSSOBindComplete(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可绑定单点登录"})
		return
	}
	var req bindCompleteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	name, login, err := s.completeSSOBind(u, req.Ticket)
	if err != nil {
		ssoBindError(c, err)
		return
	}
	s.audit(memberActor(u), "user.sso_bound", u.Username, name+" · "+login, clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "provider": name, "login": login})
}

// portalSSOBindComplete: POST /u/bindings/sso/complete {ticket} — the
// portal redemption of an SSO bind ticket.
func (s *Server) portalSSOBindComplete(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req bindCompleteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	name, login, err := s.completeSSOBind(u, req.Ticket)
	if err != nil {
		ssoBindError(c, err)
		return
	}
	s.audit(s.portalActorFor(c, u), "user.sso_bound", u.Username, name+" · "+login, clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "provider": name, "login": login})
}

// --- unbind ---

// unbindUserSSO removes one provider binding and ends every session
// on both faces: the epoch bump + revocation pair every credential
// removal fires (design D7.6), with the portal fingerprint recheck
// ending SSO sessions even apart from it.
func (s *Server) unbindUserSSO(u *store.User, providerID string) error {
	var b store.UserSSOBinding
	if err := s.st.DB.First(&b, "provider_id = ? AND user_id = ?", providerID, u.ID).Error; err != nil {
		return errors.New("尚未绑定该登录方式")
	}
	if s.userLoginMethodCount(u) <= 1 {
		return errLastLoginMethod
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := bumpUserEpochTx(tx, u.ID); err != nil {
			return err
		}
		return tx.Where("provider_id = ? AND user_id = ?", providerID, u.ID).Delete(&store.UserSSOBinding{}).Error
	})
	if err != nil {
		return err
	}
	s.revokeMemberSessions(u.ID)
	return nil
}

type ssoUnbindReq struct {
	ProviderID      string `json:"providerId" binding:"required"`
	CurrentPassword string `json:"currentPassword"`
}

// memberSSOUnbind: POST /auth/bindings/sso/unbind.
func (s *Server) memberSSOUnbind(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可管理登录方式"})
		return
	}
	var req ssoUnbindReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := checkBindingSecondFactor(u, req.CurrentPassword); err != nil {
		s.recordIPFailure(clientIP(c.Request), "面板解绑单点登录时密码错误")
		badRequest(c, err)
		return
	}
	name := s.ssoProviderName(req.ProviderID)
	if err := s.unbindUserSSO(u, req.ProviderID); err != nil {
		bindingRemovalError(c, err)
		return
	}
	s.audit(memberActor(u), "user.sso_unbound", u.Username, name, clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "relogin": true})
}

// portalSSOUnbind: POST /u/bindings/sso/unbind.
func (s *Server) portalSSOUnbind(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req ssoUnbindReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := checkBindingSecondFactor(u, req.CurrentPassword); err != nil {
		s.recordIPFailure(clientIP(c.Request), "解绑单点登录时密码错误")
		badRequest(c, err)
		return
	}
	name := s.ssoProviderName(req.ProviderID)
	if err := s.unbindUserSSO(u, req.ProviderID); err != nil {
		bindingRemovalError(c, err)
		return
	}
	s.audit(s.portalActorFor(c, u), "user.sso_unbound", u.Username, name, clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "relogin": true})
}

// --- registration ---

// registerBySSO creates the account of a new SSO user whose verified
// e-mail belongs to nobody yet, isomorphic to registerByGitHub: same
// per-IP daily cap (the same "regip:" counter), same license cap,
// same trial plan, SelfService from birth, the verified e-mail kept
// as the account's address — and the binding row lands in the same
// transaction, so the account is never observable unbound.
func (s *Server) registerBySSO(p *store.SSOProvider, subject, login, email, ip string) (*store.User, error) {
	perIP := atoiDefault(s.st.GetSetting("register_ip_daily", "3"), 3)
	if perIP > 0 && !s.countDaily("regip:"+ip, perIP) {
		return nil, errors.New("这个网络今天注册的账号太多了，请明天再试")
	}
	if max := s.license().Limit(license.LimitUsers); max > 0 {
		var cur int64
		s.st.DB.Model(&store.User{}).Where("is_service = ?", false).Count(&cur)
		if int(cur) >= max {
			return nil, errors.New("暂时无法注册，请联系管理员")
		}
	}
	if s.userCapFull() {
		return nil, errUserCap
	}
	base := sanitizeGitHubName(login)
	if at := strings.Index(email, "@"); base == "user" && at > 0 {
		base = sanitizeGitHubName(email[:at])
	}
	trialID := strings.TrimSpace(s.st.GetSetting("register_trial_plan", ""))
	var u store.User
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockAccountBindings(tx); err != nil {
			return err
		}
		var n int64
		if err := tx.Model(&store.UserSSOBinding{}).Where("provider_id = ? AND subject = ?", p.ID, subject).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return errSSOBound
		}
		if err := tx.Model(&store.User{}).Where("LOWER(email) = ? AND is_service = ?", strings.ToLower(email), false).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return errSSOEmailMatched
		}
		name := base
		for i := 0; ; i++ {
			var taken int64
			if err := tx.Model(&store.User{}).Where("LOWER(username) = ?", name).Count(&taken).Error; err != nil {
				return err
			}
			if taken == 0 {
				break
			}
			if i >= 5 {
				return errors.New("暂时无法注册，请联系管理员")
			}
			name = base + "_" + strings.ToLower(pki.Token(4))
		}
		u = newUser(&userReq{Username: name, Remark: "SSO 注册"})
		u.Email, u.SelfService = email, true
		if err := s.createRegisteredUser(tx, &u, "sso", trialID); err != nil {
			return err
		}
		return tx.Create(&store.UserSSOBinding{ProviderID: p.ID, Subject: subject, UserID: u.ID, Login: login, CreatedAt: time.Now().UTC()}).Error
	})
	if err != nil {
		return nil, err
	}
	s.MarkDirty("user registered", s.nodesForUsers(u.ID)...)
	s.audit("sso:"+u.Username, "user.register", u.Username, login, ip)
	s.wakeBusinessEvents()
	s.notify(Event{Kind: "user_registered", Text: fmt.Sprintf("🆕 新用户通过 %s 注册：%s（%s）", p.Name, u.Username, login)})
	return &u, nil
}
