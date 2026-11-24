package panel

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/mailer"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Username + password as a registered user's second sign-in method —
// see docs/design/user-identity.md (batch 1). The password lives in
// User.PasswordHash and is strictly separate from User.Password, the
// subscription/core credential: nothing here reads or writes the core
// credential, and the admin password endpoints never see a User.
//
// The two sign-in surfaces share one verification kernel and never
// cross: /auth/login keeps reading only the Admin table, the endpoints
// here read only the User table (design red line 1).

const (
	// mailPurposeReset is the mail-code purpose for password resets,
	// shared by the portal and panel faces: a code asked on one face
	// spends on the other, for the same account and the same action.
	mailPurposeReset = "resetpw"
	// mailPurposeSetPW prefixes the per-user purpose ("setpw:<userID>")
	// for the code that proves mailbox ownership when a signed-in user
	// sets a first sign-in password.
	mailPurposeSetPW = "setpw"
)

// errBadCredentials is the one answer every password sign-in failure
// gives — unknown user, no password set, wrong password, disabled
// account and the case-duplicate anomaly are indistinguishable.
var errBadCredentials = errors.New("用户名或密码错误")

var errRegistrationClosed = errors.New("注册暂未开放")

// reservedUsernames can never be self-registered (design D2): they
// read as the service itself in subscriptions, audits and node lists.
var reservedUsernames = map[string]bool{
	"admin": true, "root": true, "support": true, "system": true,
	"official": true, "moderator": true, "service": true, "api": true,
	"polaris": true, "null": true, "undefined": true,
}

// passwordRegisterOn reports the register_password switch. It gates
// only account creation; an existing password keeps signing in with
// the switch off, exactly like portal_email_register on the mail face.
func (s *Server) passwordRegisterOn() bool {
	return s.st.GetSetting("register_password", "false") == "true"
}

// validateUsername applies the self-registration username rules
// (design D2): 3–32 chars of [a-zA-Z0-9_.-], never starting or ending
// with '.' or '-'. adminUsernameRE carries the charset and length.
func validateUsername(name string) error {
	if !adminUsernameRE.MatchString(name) || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, "-") {
		return errors.New("用户名需为 3–32 位字母、数字或 _ . -，且不能以 . 或 - 开头结尾")
	}
	return nil
}

// validateUserPassword applies the user password rules (design D3):
// 8–72 bytes (the bcrypt limit), no leading or trailing space. The
// floor is lower than the admins' 12 on purpose — user-facing
// friction, backstopped by the shared IP ban pool.
func validateUserPassword(pw string) error {
	if len(pw) < 8 || len(pw) > 72 || strings.TrimSpace(pw) != pw {
		return errors.New("密码需为 8–72 字节，首尾不能有空格")
	}
	return nil
}

func hashUserPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// checkUsernameTaken runs inside lockAccountBindings: a self-service
// username must be free case-insensitively among users, must not
// collide with an admin's name (the two tables are separate namespaces
// but a registered user must never read as an admin), and must not be
// reserved.
func checkUsernameTaken(tx *gorm.DB, username string) error {
	lower := strings.ToLower(username)
	if reservedUsernames[lower] {
		return errors.New("该用户名不可用，请换一个")
	}
	var n int64
	if err := tx.Model(&store.User{}).Where("LOWER(username) = ?", lower).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return errors.New("该用户名已被注册，请换一个")
	}
	if err := tx.Model(&store.Admin{}).Where("LOWER(username) = ?", lower).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return errors.New("该用户名不可用，请换一个")
	}
	return nil
}

// registerByPassword creates the account of a new username+password
// user, isomorphic to registerByMail: same per-IP daily cap (the same
// "regip:" counter, so the two registration ways share one budget),
// same license cap, same trial plan, SelfService from birth. The
// optional e-mail is stored as contact information only: it is not
// verified and not checked for uniqueness — ownership of an address
// is only ever established through the mail-code bind flow (design
// D4 / red line 8), so nothing here may treat it as proven.
func (s *Server) registerByPassword(username, password, email, ip string) (*store.User, error) {
	if !s.passwordRegisterOn() {
		return nil, errRegistrationClosed
	}
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if err := validateUserPassword(password); err != nil {
		return nil, err
	}
	email = strings.TrimSpace(email)
	if email != "" {
		email = mailer.Normalize(email)
		if email == "" {
			return nil, errors.New("请填写正确的邮箱地址")
		}
	}
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
	hash, err := hashUserPassword(password)
	if err != nil {
		return nil, err
	}
	trialID := strings.TrimSpace(s.st.GetSetting("register_trial_plan", ""))
	var u store.User
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockAccountBindings(tx); err != nil {
			return err
		}
		if err := checkUsernameTaken(tx, username); err != nil {
			return err
		}
		u = newUser(&userReq{Username: username, Remark: "密码注册"})
		u.PasswordHash, u.Email, u.SelfService = hash, email, true
		return s.createRegisteredUser(tx, &u, "password", trialID)
	})
	if err != nil {
		return nil, err
	}
	s.MarkDirty("user registered", s.nodesForUsers(u.ID)...)
	s.audit("pw:"+u.Username, "user.register", u.Username, username, ip)
	s.wakeBusinessEvents()
	s.notify(Event{Kind: "user_registered", Text: fmt.Sprintf("🆕 新用户通过密码注册：%s", u.Username)})
	return &u, nil
}

// verifyUserPassword is the shared sign-in kernel of both faces. It
// returns the user on success; on failure it returns a short reason
// for the audit detail (never for the client, which always gets
// errBadCredentials). Legacy case-duplicate usernames (Alice/alice
// both in the table — the unique index is case-sensitive) fail closed:
// there is no safe way to pick one.
func (s *Server) verifyUserPassword(username, password string) (*store.User, string) {
	var us []store.User
	s.st.DB.Where("LOWER(username) = ? AND is_service = ?", strings.ToLower(username), false).Limit(2).Find(&us)
	if len(us) > 1 {
		return nil, "duplicate"
	}
	if len(us) == 0 {
		// Same cost as a real check to avoid probing by timing.
		_ = passwordMatches(string(dummyHash), password)
		return nil, "bad"
	}
	u := &us[0]
	hash := u.PasswordHash
	if hash == "" {
		hash = string(dummyHash)
	}
	if u.Status == "disabled" || u.PasswordHash == "" || !passwordMatches(hash, password) {
		return nil, "bad"
	}
	return u, ""
}

// failPasswordLogin does the bookkeeping every failed password sign-in
// shares: the IP failure lands in the same ban pool as the admin and
// mail faces, and the attempt is audited under the submitted name.
func (s *Server) failPasswordLogin(c *gin.Context, username, reason string) {
	ip := clientIP(c.Request)
	s.recordIPFailure(ip, "密码登录失败")
	if reason == "duplicate" {
		s.log.Warn("password sign-in blocked: case-duplicate username", "username", username, "ip", ip)
	}
	s.audit("pw:"+username, "user.password_login_failed", username, reason, ip)
	c.JSON(http.StatusUnauthorized, gin.H{"error": errBadCredentials.Error()})
}

// setUserPassword stores a new sign-in password hash and ends every
// session the user holds on both faces (design D3): the portal
// generation bumps inside the transaction, member panel sessions are
// revoked right after. The current session is not exempt — the user
// signs in again with the new password.
func (s *Server) setUserPassword(u *store.User, hash string) error {
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		epoch, err := lockPortalEpoch(tx, u.ID)
		if err != nil {
			return err
		}
		if err := tx.Model(&store.PortalSessionEpoch{}).Where("user_id = ?", u.ID).Update("version", epoch.Version+1).Error; err != nil {
			return err
		}
		return tx.Model(&store.User{}).Where("id = ?", u.ID).Update("password_hash", hash).Error
	})
	if err != nil {
		return err
	}
	s.revokeMemberSessions(u.ID)
	return nil
}

// setPasswordForEmail sets the sign-in password of the account owning
// addr; the caller has already proven mailbox ownership with a code.
// A verified address with no account is the one failure that may say
// so — the code itself proved the address is the caller's.
func (s *Server) setPasswordForEmail(addr, hash string) (*store.User, error) {
	var u store.User
	if err := s.st.DB.First(&u, "email = ? AND is_service = ?", addr, false).Error; err != nil {
		return nil, errors.New("这个邮箱没有绑定账号")
	}
	if err := s.setUserPassword(&u, hash); err != nil {
		return nil, err
	}
	u.PasswordHash = hash
	return &u, nil
}

// memberPasswordLogin: POST /auth/member/password {username, password}
// — the panel face of password sign-in, gated by the same member
// admission as the mail face.
func (s *Server) memberPasswordLogin(c *gin.Context) {
	if !s.memberLoginEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册用户登录面板未开启"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	u, reason := s.verifyUserPassword(req.Username, req.Password)
	if u == nil {
		s.failPasswordLogin(c, req.Username, reason)
		return
	}
	tok, err := s.issueMemberToken(u, c)
	if err != nil {
		serverError(c, err)
		return
	}
	s.audit(memberActor(u), "member.login", u.Username, "password", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"token": tok, "username": memberDisplayName(u), "expiresIn": int(tokenTTL.Seconds())})
}

// memberPasswordRegister: POST /auth/member/register {username,
// password, email?} — password registration on the panel face: member
// admission plus the register_password switch, then straight in.
func (s *Server) memberPasswordRegister(c *gin.Context) {
	if !s.memberLoginEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册用户登录面板未开启"})
		return
	}
	if !s.passwordRegisterOn() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册暂未开放", "code": "registration_closed"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Email    string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	u, err := s.registerByPassword(strings.TrimSpace(req.Username), req.Password, req.Email, clientIP(c.Request))
	if err != nil {
		badRequest(c, err)
		return
	}
	tok, err := s.issueMemberToken(u, c)
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": tok, "username": memberDisplayName(u), "expiresIn": int(tokenTTL.Seconds()), "created": true})
}

// memberPasswordReset: POST /auth/member/password/reset {email, code,
// newPassword} — the panel face of the mail-code reset. The code comes
// from POST /auth/mail/code with purpose "reset".
func (s *Server) memberPasswordReset(c *gin.Context) {
	if !s.mailLoginOn() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "邮箱登录暂未开放"})
		return
	}
	if !s.memberLoginEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册用户登录面板未开启"})
		return
	}
	var req struct {
		Email       string `json:"email"`
		Code        string `json:"code"`
		NewPassword string `json:"newPassword"`
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
	// Validate before spending the code: a bad new password must not
	// burn the one proof of mailbox ownership.
	if err := validateUserPassword(req.NewPassword); err != nil {
		badRequest(c, err)
		return
	}
	ip := clientIP(c.Request)
	if err := s.checkMailCode(mailPurposeReset, addr, req.Code); err != nil {
		s.recordIPFailure(ip, "面板重置密码验证码错误")
		badRequest(c, err)
		return
	}
	hash, err := hashUserPassword(req.NewPassword)
	if err != nil {
		serverError(c, err)
		return
	}
	u, err := s.setPasswordForEmail(addr, hash)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.audit("mail:"+u.Username, "user.password_reset", u.Username, "panel", ip)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// portalPasswordLogin: POST /u/password/login {username, password} —
// the user-center face of password sign-in.
func (s *Server) portalPasswordLogin(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	u, reason := s.verifyUserPassword(req.Username, req.Password)
	if u == nil {
		s.failPasswordLogin(c, req.Username, reason)
		return
	}
	tok, err := s.issuePortalPasswordToken(u, c)
	if err != nil {
		serverError(c, err)
		return
	}
	s.setPortalCookie(c, tok, int(portalSessionTTL.Seconds()))
	s.audit("pw:"+u.Username, "user.portal_login", u.Username, "password", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// portalPasswordRegister: POST /u/password/register {username,
// password, email?} — password registration on the user-center face.
func (s *Server) portalPasswordRegister(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if !s.passwordRegisterOn() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册暂未开放", "code": "registration_closed"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Email    string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	u, err := s.registerByPassword(strings.TrimSpace(req.Username), req.Password, req.Email, clientIP(c.Request))
	if err != nil {
		badRequest(c, err)
		return
	}
	tok, err := s.issuePortalPasswordToken(u, c)
	if err != nil {
		serverError(c, err)
		return
	}
	s.setPortalCookie(c, tok, int(portalSessionTTL.Seconds()))
	c.JSON(http.StatusOK, gin.H{"ok": true, "created": true})
}

// portalPasswordReset is portalMailVerify's reset branch: the code
// (purpose "reset") plus a new password, in one request — no separate
// reset ticket, no link.
func (s *Server) portalPasswordReset(c *gin.Context, addr, code, newPassword string) {
	if err := validateUserPassword(newPassword); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.checkMailCode(mailPurposeReset, addr, code); err != nil {
		badRequest(c, err)
		return
	}
	hash, err := hashUserPassword(newPassword)
	if err != nil {
		serverError(c, err)
		return
	}
	u, err := s.setPasswordForEmail(addr, hash)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.audit("mail:"+u.Username, "user.password_reset", u.Username, "portal", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// portalPasswordSet: POST /u/password/set {newPassword, currentPassword?,
// code?} — a signed-in user sets or changes their sign-in password.
// Changing an existing password needs the current one; setting a first
// one needs a mail code to the bound address (purpose "setpw"); with
// neither a password nor an address, the live session (Telegram) is
// the strongest proof there is (design D7). Success ends every
// session on both faces, this one included.
func (s *Server) portalPasswordSet(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req struct {
		NewPassword     string `json:"newPassword"`
		CurrentPassword string `json:"currentPassword"`
		Code            string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := validateUserPassword(req.NewPassword); err != nil {
		badRequest(c, err)
		return
	}
	ip := clientIP(c.Request)
	switch {
	case u.PasswordHash != "":
		if !passwordMatches(u.PasswordHash, req.CurrentPassword) {
			s.recordIPFailure(ip, "设置密码时当前密码错误")
			badRequest(c, errors.New("当前密码不正确"))
			return
		}
	case u.Email != "":
		if err := s.checkMailCode(mailPurposeSetPW+":"+u.ID, u.Email, req.Code); err != nil {
			badRequest(c, err)
			return
		}
	}
	hash, err := hashUserPassword(req.NewPassword)
	if err != nil {
		serverError(c, err)
		return
	}
	if err := s.setUserPassword(u, hash); err != nil {
		serverError(c, err)
		return
	}
	s.audit(s.portalActorFor(c, u), "user.password_set", u.Username, "", ip)
	c.JSON(http.StatusOK, gin.H{"ok": true, "relogin": true})
}
