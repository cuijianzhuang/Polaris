package panel

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Binding management for registered users — see
// docs/design/user-identity.md (batch 2, D7): one view of the four
// sign-in methods (e-mail code, password, Telegram, GitHub), the
// same semantics on the user-center and member-panel faces. Binding
// actions reuse each method's own flow; this file owns the status
// view and the two removals that had no self-service endpoint yet —
// GitHub unbind and sign-in password clear — under one red line:
// an account must always keep at least one sign-in method
// (design D8 red line 5).

var errLastLoginMethod = errors.New("至少保留一种登录方式，无法解绑最后一个")

// loginMethodCount counts the ways u can still sign in.
func loginMethodCount(u *store.User) int {
	n := 0
	if u.Email != "" {
		n++
	}
	if u.PasswordHash != "" {
		n++
	}
	if u.TelegramID != 0 {
		n++
	}
	if u.GitHubID != 0 {
		n++
	}
	return n
}

// loginMethodsView is the status payload both faces render.
func (s *Server) loginMethodsView(u *store.User) gin.H {
	return gin.H{
		"email": u.Email != "", "emailAddress": u.Email,
		"password": u.PasswordHash != "",
		"telegram": u.TelegramID != 0,
		"github":   u.GitHubID != 0, "githubLogin": u.GitHubLogin,
		"githubAvailable": s.githubEnabled(),
		"count":           loginMethodCount(u),
	}
}

// checkBindingSecondFactor re-proves the account password for
// sensitive binding changes when the account has one (design D7).
// Without a password there is nothing stronger to ask for than the
// live session itself, which a verified method issued.
func checkBindingSecondFactor(u *store.User, currentPassword string) error {
	if u.PasswordHash == "" {
		return nil
	}
	if !passwordMatches(u.PasswordHash, currentPassword) {
		return errors.New("当前密码不正确")
	}
	return nil
}

// bumpUserEpochTx advances the user's portal session generation
// inside the caller's transaction — half of the invalidation pair
// every credential change fires (design D8 red line 6); the other
// half is revokeMemberSessions after commit.
func bumpUserEpochTx(tx *gorm.DB, userID string) error {
	epoch, err := lockPortalEpoch(tx, userID)
	if err != nil {
		return err
	}
	return tx.Model(&store.PortalSessionEpoch{}).Where("user_id = ?", userID).Update("version", epoch.Version+1).Error
}

// unbindUserGitHub clears the GitHub binding and ends every session
// on both faces: a session the method issued must not outlive it
// (design D8 red line 6), and the epoch bump + revocation pair is
// the same one password changes use.
func (s *Server) unbindUserGitHub(u *store.User) error {
	if u.GitHubID == 0 {
		return errors.New("尚未绑定 GitHub")
	}
	if loginMethodCount(u) <= 1 {
		return errLastLoginMethod
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := bumpUserEpochTx(tx, u.ID); err != nil {
			return err
		}
		return tx.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"git_hub_id": 0, "git_hub_login": ""}).Error
	})
	if err != nil {
		return err
	}
	s.revokeMemberSessions(u.ID)
	return nil
}

// clearUserLoginPassword removes the sign-in password (the core
// credential in User.Password is a different world and is never
// touched here). The current password is the second factor; success
// ends every session on both faces, like a password change.
func (s *Server) clearUserLoginPassword(u *store.User, currentPassword string) error {
	if u.PasswordHash == "" {
		return errors.New("尚未设置登录密码")
	}
	if !passwordMatches(u.PasswordHash, currentPassword) {
		return errors.New("当前密码不正确")
	}
	if loginMethodCount(u) <= 1 {
		return errLastLoginMethod
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := bumpUserEpochTx(tx, u.ID); err != nil {
			return err
		}
		return tx.Model(&store.User{}).Where("id = ?", u.ID).Update("password_hash", "").Error
	})
	if err != nil {
		return err
	}
	s.revokeMemberSessions(u.ID)
	return nil
}

// bindingRemovalError answers a failed unbind/clear on a JSON face.
func bindingRemovalError(c *gin.Context, err error) {
	if errors.Is(err, errLastLoginMethod) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "last_login_method"})
		return
	}
	badRequest(c, err)
}

type bindingRemovalReq struct {
	CurrentPassword string `json:"currentPassword"`
}

// --- Member panel face (memberRoutes) ---

// memberBindings: GET /auth/bindings — the member's own sign-in
// methods, for the panel account page.
func (s *Server) memberBindings(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可管理登录方式"})
		return
	}
	c.JSON(http.StatusOK, s.loginMethodsView(u))
}

// memberGitHubUnbind: POST /auth/bindings/github/unbind.
func (s *Server) memberGitHubUnbind(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可管理登录方式"})
		return
	}
	var req bindingRemovalReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := checkBindingSecondFactor(u, req.CurrentPassword); err != nil {
		s.recordIPFailure(clientIP(c.Request), "面板解绑 GitHub 时密码错误")
		badRequest(c, err)
		return
	}
	login := u.GitHubLogin
	if err := s.unbindUserGitHub(u); err != nil {
		bindingRemovalError(c, err)
		return
	}
	s.audit(memberActor(u), "user.github_unbound", u.Username, login, clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "relogin": true})
}

// memberPasswordClear: POST /auth/bindings/password/clear.
func (s *Server) memberPasswordClear(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可管理登录方式"})
		return
	}
	var req bindingRemovalReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.clearUserLoginPassword(u, req.CurrentPassword); err != nil {
		if err.Error() == "当前密码不正确" {
			s.recordIPFailure(clientIP(c.Request), "面板清除登录密码时密码错误")
		}
		bindingRemovalError(c, err)
		return
	}
	s.audit(memberActor(u), "user.password_clear", u.Username, "", clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "relogin": true})
}

// --- User-center face (portalGate) ---

// portalBindings: GET /u/bindings — the signed-in user's sign-in
// methods, for the account page's bindings card.
func (s *Server) portalBindings(c *gin.Context) {
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, s.loginMethodsView(u))
}

// portalGitHubBindComplete: POST /u/bindings/github/complete
// {ticket} — the portal redemption of a bind ticket, under the
// portal session the flow started from.
func (s *Server) portalGitHubBindComplete(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req struct {
		Ticket string `json:"ticket" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	gu, err := s.completeGitHubBind(u, req.Ticket)
	if err != nil {
		githubBindError(c, err)
		return
	}
	s.audit(s.portalActorFor(c, u), "user.github_bound", u.Username, gu.Login, clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "githubLogin": gu.Login})
}

// portalGitHubUnbind: POST /u/bindings/github/unbind.
func (s *Server) portalGitHubUnbind(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req bindingRemovalReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := checkBindingSecondFactor(u, req.CurrentPassword); err != nil {
		s.recordIPFailure(clientIP(c.Request), "解绑 GitHub 时密码错误")
		badRequest(c, err)
		return
	}
	login := u.GitHubLogin
	if err := s.unbindUserGitHub(u); err != nil {
		bindingRemovalError(c, err)
		return
	}
	s.audit(s.portalActorFor(c, u), "user.github_unbound", u.Username, login, clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "relogin": true})
}

// portalPasswordClear: POST /u/bindings/password/clear.
func (s *Server) portalPasswordClear(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	var req bindingRemovalReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.clearUserLoginPassword(u, req.CurrentPassword); err != nil {
		if err.Error() == "当前密码不正确" {
			s.recordIPFailure(clientIP(c.Request), "清除登录密码时密码错误")
		}
		bindingRemovalError(c, err)
		return
	}
	s.audit(s.portalActorFor(c, u), "user.password_clear", u.Username, "", clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "relogin": true})
}
