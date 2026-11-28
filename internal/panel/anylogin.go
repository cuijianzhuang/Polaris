package panel

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Unified sign-in dispatch for the panel login page: one button per
// method, no "admin or registered user?" question up front. Each
// method gets an "any" entry point whose callback resolves the
// identity in a fixed order — the admin namespace first, then the
// registered-user namespace — and lands on the session that matched.
//
// The unified entries are additions, never replacements: the
// admin-only and user-only entry points (settings binding, the user
// center, the member account page) keep their own states (Kind "" and
// Kind "user") and their callbacks never see a Kind "any" state, so
// their behaviour is unchanged. When the same third-party account is
// bound on both sides, the admin binding wins: the panel is the
// admin's console first, and a member session can always be reached
// through the member-only entries instead.

// --- GitHub ---

// githubAnyLoginURL: POST /auth/github/any/login-url — the login
// page's single GitHub button. No admission gate at mint time: an
// admin must be able to start the round trip even when member login
// is off; the callback applies each side's gates when it resolves.
func (s *Server) githubAnyLoginURL(c *gin.Context) {
	id, _ := s.githubCreds()
	if !s.githubEnabled() {
		badRequest(c, errors.New("尚未配置 GitHub 登录：请在「设置」填写 GitHub OAuth App 的 Client ID 与 Secret"))
		return
	}
	state := s.ghAuth.put(oauthState{Mode: "login", Kind: "any"})
	q := url.Values{"client_id": {id}, "redirect_uri": {s.githubCallbackURL(c.Request)}, "state": {state}, "scope": {"read:user"}, "allow_signup": {"false"}}
	c.JSON(http.StatusOK, gin.H{"url": ghOAuthWeb + "/login/oauth/authorize?" + q.Encode()})
}

// githubAnyCallback is the unified half of the shared GitHub
// callback: admin binding → the admin sign-in semantics (lockout,
// TOTP ticket, session); otherwise the registered-user resolution of
// githubUserLoginCallback, unchanged (bound user → member session,
// unbound → registration when open, refusal otherwise).
func (s *Server) githubAnyCallback(c *gin.Context, st oauthState) {
	back := func(fragment string) { c.Redirect(http.StatusFound, s.adminPageURL(nil, "", fragment)) }
	fail := func(msg string) { back("gh_error=" + url.QueryEscape(msg)) }
	if e := c.Query("error"); e != "" {
		fail("已取消 GitHub 授权")
		return
	}
	gu, err := s.githubIdentity(c.Request.Context(), c.Query("code"), s.githubCallbackURL(c.Request))
	if err != nil {
		fail(err.Error())
		return
	}
	var a store.Admin
	if err := s.st.DB.First(&a, "git_hub_id = ?", gu.ID).Error; err == nil {
		if lockedOut(&a, time.Now()) {
			fail("登录失败次数过多，请 15 分钟后再试")
			return
		}
		if a.TOTPEnabled {
			back("gh_ticket=" + s.ghAuth.ticket(a.ID))
			return
		}
		s.finishLogin(c, &a, "github", back)
		return
	}
	s.githubUserLoginCallback(c, st, gu)
}

// --- SSO ---

// ssoAnyLoginURL: POST /auth/sso/any/login-url {providerId} — the
// login page's single button per provider. Like the GitHub twin, it
// mints without a user-side gate; the callback decides which side
// the identity belongs to.
func (s *Server) ssoAnyLoginURL(c *gin.Context) {
	var req userSSOURLReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var p store.SSOProvider
	if s.st.DB.First(&p, "id = ? AND enabled = ?", req.ProviderID, true).Error != nil {
		notFound(c)
		return
	}
	verifier, challenge := pkcePair()
	state := s.ghAuth.put(oauthState{Mode: "login", Kind: "any", Provider: p.ID, Verifier: verifier})
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

// ssoAnyCallback is the unified half of the shared SSO callback:
// admin binding → the admin sign-in semantics; otherwise the
// registered-user resolution of ssoUserLoginCallback, unchanged
// (no silent merge, registration when open). With sso_user_login
// off the entry keeps pure admin semantics: an identity bound only
// on the user side is refused exactly like an unbound one.
func (s *Server) ssoAnyCallback(c *gin.Context, st oauthState) {
	back := func(fragment string) { c.Redirect(http.StatusFound, s.adminPageURL(nil, "", fragment)) }
	fail := func(msg string) { back("gh_error=" + url.QueryEscape(msg)) }
	if e := c.Query("error"); e != "" {
		fail("已取消授权：" + orDefault(c.Query("error_description"), e))
		return
	}
	var p store.SSOProvider
	if s.st.DB.First(&p, "id = ?", st.Provider).Error != nil {
		fail("登录方式已被删除")
		return
	}
	// The user flow's identity read is the admins' plus the e-mail
	// claims; the admin branch below consumes subject/login only.
	subject, login, email, verified, err := s.ssoUserIdentity(c.Request.Context(), &p, c.Query("code"), st.Verifier, s.ssoCallbackURL(c.Request))
	if err != nil {
		fail(err.Error())
		return
	}
	ip := clientIP(c.Request)
	var b store.SSOBinding
	var a store.Admin
	if s.st.DB.First(&b, "provider_id = ? AND subject = ?", p.ID, subject).Error == nil && s.st.DB.First(&a, "id = ?", b.AdminID).Error == nil {
		if lockedOut(&a, time.Now()) {
			fail("登录失败次数过多，请 15 分钟后再试")
			return
		}
		if a.TOTPEnabled {
			back("gh_ticket=" + s.ghAuth.ticket(a.ID))
			return
		}
		s.finishLogin(c, &a, "sso", back)
		return
	}
	if !s.ssoUserLoginOn() {
		s.recordIPFailure(ip, p.Name+" 账号未绑定")
		s.audit(login, "admin.login_failed", login, p.Name+": not bound", ip)
		fail(p.Name + " 账号 " + login + " 未绑定任何管理员：请先用密码登录，在「设置 → 账号安全」绑定")
		return
	}
	s.ssoUserLoginCallback(c, st, &p, subject, login, email, verified)
}

// --- Passkeys ---

// beginAnyPasskeyLogin: POST /auth/passkeys/any/login/begin — one
// discoverable ceremony for both tables. On the panel face the two
// sides share one relying party (the configured PublicURL), so a
// single challenge serves whichever table owns the credential; the
// finish decides by credential hash.
func (s *Server) beginAnyPasskeyLogin(c *gin.Context) {
	w, origin, e := s.webAuthn()
	if e != nil {
		badRequest(c, e)
		return
	}
	if !s.shared.allow("passkey-login", 10, clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后重试"})
		return
	}
	opts, session, e := w.BeginDiscoverableLogin()
	if e != nil {
		serverError(c, e)
		return
	}
	id, e := s.newPasskeyChallenge(passkeyChallenge{Kind: "anylogin", Origin: origin, Session: *session})
	if e != nil {
		serverError(c, e)
		return
	}
	c.JSON(http.StatusOK, gin.H{"challengeId": id, "options": opts})
}

// finishAnyPasskeyLogin: POST /auth/passkeys/any/login/finish. The
// credential's hash names its table — admin table first (the same
// priority as the OAuth dispatch above) — and the matching side's
// own validation core verifies the assertion: nothing about the
// signature check is re-implemented here. A credential owned by one
// side never falls through to the other side's validation.
func (s *Server) finishAnyPasskeyLogin(c *gin.Context) {
	if !s.shared.allow("passkey-finish", 20, clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后重试"})
		return
	}
	req, ch, ok := s.takePasskeyChallenge(c, "anylogin")
	if !ok {
		return
	}
	w, origin, e := s.webAuthn()
	if e != nil || ch.Origin != origin {
		badRequest(c, errors.New("登录域名已变化，请重试"))
		return
	}
	parsed, e := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.Credential))
	if e != nil {
		badRequest(c, errors.New("无效的通行密钥响应"))
		return
	}
	ip := clientIP(c.Request)
	hash, rpid := passkeyHash(parsed.RawID), w.Config.RPID
	var adminKey store.Passkey
	if s.st.DB.First(&adminKey, "credential_hash = ? AND rp_id = ?", hash, rpid).Error == nil {
		a, keyID, err := s.validateAdminPasskeyLogin(w, ch, parsed)
		if err != nil {
			s.recordIPFailure(ip, "通行密钥登录失败")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "通行密钥验证失败，请重试或使用其他登录方式"})
			return
		}
		s.completeAdminPasskeyLogin(c, a, keyID)
		return
	}
	var userKey store.UserPasskey
	if s.st.DB.First(&userKey, "credential_hash = ? AND rp_id = ?", hash, rpid).Error == nil {
		if !s.memberLoginEnabled() {
			c.JSON(http.StatusForbidden, gin.H{"error": "注册用户登录面板未开启"})
			return
		}
		uch := userPasskeyChallenge{Kind: "login", Face: passkeyFacePanel, Origin: ch.Origin, Session: ch.Session}
		u, key, err := s.validateUserPasskeyLogin(w, uch, parsed)
		if err != nil {
			if errors.Is(err, errUserPasskeyClone) && u != nil {
				s.audit("pk:"+u.Username, "user.passkey_login_failed", u.Username, "clone warning", clientIP(c.Request))
			}
			s.recordIPFailure(ip, "用户通行密钥登录失败")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "通行密钥验证失败，请重试或使用其他登录方式"})
			return
		}
		s.completeMemberPasskeyLogin(c, u, key)
		return
	}
	s.recordIPFailure(ip, "通行密钥登录失败")
	c.JSON(http.StatusUnauthorized, gin.H{"error": "通行密钥验证失败，请重试或使用其他登录方式"})
}
