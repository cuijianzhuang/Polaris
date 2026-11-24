package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/mailer"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Registered users (store.User) as a third panel principal — see
// docs/design/panel-member-login.md. A member session is issued by
// issueMemberToken after an e-mail code check, carries the Mem claim (and
// never the portal User claim), lives in AdminSession like every panel
// session, and is confined by memberRoutes plus a node scope recomputed
// on every request: approval, rejection and account state take effect on
// the next request, not at the next sign-in.

const (
	memberRole       = "member"
	memberSessionVia = "mail-member"
	mailPurposePanel = "panel"
)

// memberRoutes is the member role's route whitelist (batch 1: read +
// operate own nodes, plus the session/self routes the scoped workspace
// needs; batch 2: manage own nodes and their inbounds, §7/§9). It is
// deliberately a separate table from scopedRoutes: widening the operator
// surface must never widen the member surface by accident. Members are
// confined to this table alone — the selfAccountRoutes spillover the
// operator branch allows does not apply to them (batch 2 tightening).
var memberRoutes = map[string]bool{
	"GET /auth/me":                      true,
	"POST /auth/logout":                 true,
	"GET /auth/sessions":                true,
	"DELETE /auth/sessions/:id":         true,
	"POST /auth/sessions/revoke-others": true,
	"GET /ws":                           true,
	"GET /nodes":                        true,
	"GET /nodes/:id":                    true,
	"GET /nodes/:id/revisions":          true,
	"GET /nodes/:id/snell-readiness":    true,
	"GET /nodes/:id/cores":              true,
	"GET /nodes/:id/metrics":            true,
	"GET /nodes/:id/net-daily":          true,
	"POST /nodes/:id/sync":              true,
	"POST /nodes/:id/rollback":          true,
	"POST /cores/:id/restart":           true,
	"GET /cores/:id/logs":               true,
	"GET /capabilities":                 true,
	"GET /core-latest":                  true,
	"GET /stats/nodes":                  true,
	"GET /stats/traffic":                true, // only with ?node= in scope, see scopeGuard
	// Batch 2: the write surface. Every one of these resolves its target
	// through the member ownership checks before doing anything (see
	// memberScopeGuard and member_nodes.go); node publication, relay,
	// CDN and template routes are deliberately absent.
	"POST /nodes":                   true,
	"PUT /nodes/:id":                true,
	"DELETE /nodes/:id":             true,
	"POST /nodes/:id/install-token": true,
	"GET /inbounds":                 true,
	"POST /inbounds":                true,
	"GET /inbounds/:id":             true,
	"PUT /inbounds/:id":             true,
	"DELETE /inbounds/:id":          true,
	// Identity batch 2: the member's own sign-in methods (user-identity
	// D7's named exception to the batch-2 self-route narrowing — this
	// group only, every handler resolves the member principal itself).
	"POST /auth/github/user/bind-url":      true,
	"POST /auth/github/user/bind-complete": true,
	"GET /auth/bindings":                   true,
	"POST /auth/bindings/github/unbind":    true,
	"POST /auth/bindings/password/clear":   true,
}

// memberActor names a member in the audit log, distinct from admin actors
// and from the portal's mail:/tg: actors.
func memberActor(u *store.User) string { return "member:" + u.ID }

// memberDisplayName is how a member is shown in the panel: their e-mail
// address (the credential they sign in with), else their username.
func memberDisplayName(u *store.User) string {
	if u.Email != "" {
		return u.Email
	}
	return u.Username
}

// memberLoginEnabled is the admission gate (design D6): the admin switch
// is on and user nodes are not switched off entirely.
func (s *Server) memberLoginEnabled() bool {
	return s.st.GetSetting("panel_member_login", "false") == "true" && s.effectiveUserNodesMode() != userNodesOff
}

// memberMailLoginAvailable is what /info reports: admission plus a
// working mail setup, since the code has to reach the user somehow.
func (s *Server) memberMailLoginAvailable() bool {
	return s.memberLoginEnabled() && s.mailOn()
}

// memberNodeScope recomputes the operation scope: the user's own nodes
// that are approved. Pending/rejected nodes are absent on purpose — the
// node list is the one read path that shows them (see listNodes).
func (s *Server) memberNodeScope(userID string) map[string]bool {
	var ids []string
	s.st.DB.Model(&store.Node{}).Where("owner_id = ? AND review_state = ?", userID, "approved").Pluck("id", &ids)
	sc := make(map[string]bool, len(ids))
	for _, id := range ids {
		sc[id] = true
	}
	return sc
}

// currentPortalEpoch reads the user's portal session generation. The row
// always exists for anyone holding a member session (issueMemberToken
// creates it); a missing row is a mismatch, not a zero.
func (s *Server) currentPortalEpoch(userID string) (uint64, bool) {
	var e store.PortalSessionEpoch
	if s.st.DB.First(&e, "user_id = ?", userID).Error != nil {
		return 0, false
	}
	return e.Version, true
}

// issueMemberToken signs a panel session for a registered user, following
// issueTGToken's shape: same session table, same signing, same TTL as an
// admin's. The PortalEpoch stamp ties the session to the user's portal
// generation, so changing the bound e-mail or resetting portal sessions
// ends panel sessions too.
func (s *Server) issueMemberToken(u *store.User, gc *gin.Context) (string, error) {
	var epoch uint64
	if err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		e, err := lockPortalEpoch(tx, u.ID)
		if err != nil {
			return err
		}
		epoch = e.Version
		return nil
	}); err != nil {
		return "", err
	}
	name := memberDisplayName(u)
	c := claims{
		SID: s.newSession(gc, u.ID, truncate(name, 64), memberSessionVia, tokenTTL),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(tokenTTL)),
		},
		Username:    name,
		Mem:         u.ID,
		PortalEpoch: epoch,
	}
	return s.signToken(jwt.NewWithClaims(jwt.SigningMethodHS256, c))
}

// authMember is authRequired's third branch (design D1/D4/D6). Admission,
// account state and the portal generation are re-checked on every
// request; the node scope is recomputed, never a sign-in snapshot.
func (s *Server) authMember(c *gin.Context, cl *claims) {
	deny := func() { c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"}) }
	if cl.Subject != cl.Mem || !s.memberLoginEnabled() {
		deny()
		return
	}
	var u store.User
	if s.st.DB.First(&u, "id = ? AND is_service = ?", cl.Mem, false).Error != nil || u.Status == "disabled" {
		deny()
		return
	}
	if epoch, ok := s.currentPortalEpoch(u.ID); !ok || epoch != cl.PortalEpoch {
		deny()
		return
	}
	s.noteBase(c.Request)
	c.Set("admin", memberActor(&u))
	c.Set("memberUser", &u)
	c.Set("memberUserID", u.ID)
	c.Set("adminRole", memberRole)
	c.Set("nodeScope", s.memberNodeScope(u.ID))
	c.Next()
}

// memberUserOf returns the member principal of the request, if any.
func memberUserOf(c *gin.Context) *store.User {
	if v, ok := c.Get("memberUser"); ok {
		return v.(*store.User)
	}
	return nil
}

// revokeMemberSessions ends every panel session issued to the given
// users. Member sessions are the AdminSession rows carrying the user's id
// with the member via; called when accounts are disabled or deleted.
func (s *Server) revokeMemberSessions(userIDs ...string) {
	if len(userIDs) == 0 {
		return
	}
	s.st.DB.Model(&store.AdminSession{}).Where("admin_id IN ? AND via = ?", userIDs, memberSessionVia).Update("revoked", true)
}

// panelMailCode: POST /auth/mail/code {email} — sends a sign-in code for
// the panel. Answers never reveal whether the address has an account: an
// address that may not sign in gets the same response and a placeholder
// code that matches nothing, exactly like the portal flow.
func (s *Server) panelMailCode(c *gin.Context) {
	if !s.mailLoginOn() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "邮箱登录暂未开放"})
		return
	}
	if !s.memberLoginEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册用户登录面板未开启"})
		return
	}
	var req struct {
		Email   string `json:"email"`
		Purpose string `json:"purpose"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	addr := mailer.Normalize(req.Email)
	if addr == "" {
		badRequest(c, errors.New("请填写正确的邮箱地址"))
		return
	}
	if !s.mailCodeLimit.Allow(clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "操作太频繁，请稍后再试"})
		return
	}
	purpose := mailPurposePanel
	if req.Purpose == "reset" {
		// Password reset codes share the portal's purpose, so a code
		// asked on either face spends on the other.
		purpose = mailPurposeReset
	} else if req.Purpose != "" {
		badRequest(c, errors.New("无效的验证码用途"))
		return
	}
	key := mailKey(purpose, addr)
	var prev mailCode
	if s.eph.get(key, &prev) && time.Since(prev.Sent) < mailCodeCooldown {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": fmt.Sprintf("验证码已发送，请 %d 秒后再试", int((mailCodeCooldown-time.Since(prev.Sent)).Seconds())+1)})
		return
	}
	// Who may get a panel code: an existing account, or anyone when
	// sign-up is open and the domain allowed (the code then registers).
	// A reset code only ever goes to an existing account.
	var n int64
	s.st.DB.Model(&store.User{}).Where("email = ? AND is_service = ?", addr, false).Count(&n)
	send := n > 0 || (purpose == mailPurposePanel && s.mailRegisterOn() && s.registerDomainOK(addr))
	res := gin.H{"ok": true, "ttl": int(mailCodeTTL.Seconds()), "cooldown": int(mailCodeCooldown.Seconds())}
	// The daily cap applies whether or not a mail goes out: a 429 only
	// for real accounts would tell them apart (same key as the portal's).
	h := sha256.Sum256([]byte(addr))
	if !s.countDaily("maildaily:"+hex.EncodeToString(h[:16]), mailCodesPerDay) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "这个邮箱今天收到的验证码太多了，请明天再试"})
		return
	}
	if !send {
		if err := s.eph.put(key, mailCode{Hash: "none", Sent: time.Now()}, mailCodeTTL); err != nil {
			serverError(c, err)
			return
		}
		c.JSON(http.StatusOK, res)
		return
	}
	code := randomCode()
	if err := s.eph.put(key, mailCode{Hash: codeHash(addr, code), Sent: time.Now()}, mailCodeTTL); err != nil {
		serverError(c, err)
		return
	}
	title := "面板登录验证码"
	if purpose == mailPurposeReset {
		title = "重置密码验证码"
	}
	plain, body := s.mailCodeBody(title, code)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if err := s.sendMail(ctx, addr, title+" "+code, plain, body); err != nil {
		var gone mailCode
		s.eph.take(key, &gone)
		s.log.Warn("panel login code mail", "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "邮件发送失败，请稍后再试或联系管理员"})
		return
	}
	c.JSON(http.StatusOK, res)
}

// panelMailVerify: POST /auth/mail/verify {email, code} — exchanges a
// panel code for a member session. With sign-up open, an unknown address
// registers first (same account, same rules as the portal's register),
// then signs in.
func (s *Server) panelMailVerify(c *gin.Context) {
	if !s.mailLoginOn() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "邮箱登录暂未开放"})
		return
	}
	if !s.memberLoginEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册用户登录面板未开启"})
		return
	}
	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	addr := mailer.Normalize(req.Email)
	if addr == "" {
		badRequest(c, errors.New("请填写正确的邮箱地址"))
		return
	}
	if !s.mailCodeLimit.Allow(clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "操作太频繁，请稍后再试"})
		return
	}
	ip := clientIP(c.Request)
	if err := s.checkMailCode(mailPurposePanel, addr, req.Code); err != nil {
		s.recordIPFailure(ip, "面板邮箱验证码错误")
		s.audit("mail:"+addr, "member.login_failed", addr, err.Error(), ip)
		badRequest(c, err)
		return
	}
	var u store.User
	created := false
	if s.st.DB.First(&u, "email = ? AND is_service = ?", addr, false).Error != nil {
		if !s.mailRegisterOn() || !s.registerDomainOK(addr) {
			badRequest(c, errors.New("无法使用该邮箱登录"))
			return
		}
		nu, err := s.registerByMail(addr, ip)
		if err != nil {
			badRequest(c, err)
			return
		}
		u, created = *nu, true
	}
	if u.Status == "disabled" {
		// Same words as an unknown address: the answer must not confirm
		// that the account exists (design red line 7).
		s.recordIPFailure(ip, "面板邮箱登录被拒")
		s.audit(memberActor(&u), "member.login_failed", u.Username, "disabled", ip)
		badRequest(c, errors.New("无法使用该邮箱登录"))
		return
	}
	tok, err := s.issueMemberToken(&u, c)
	if err != nil {
		serverError(c, err)
		return
	}
	s.audit(memberActor(&u), "member.login", u.Username, "email", ip)
	c.JSON(http.StatusOK, gin.H{"token": tok, "username": memberDisplayName(&u), "expiresIn": int(tokenTTL.Seconds()), "created": created})
}
