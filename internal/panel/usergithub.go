package panel

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// GitHub as a registered user's fourth sign-in method — see
// docs/design/user-identity.md (batch 2, D6). The flow reuses the
// admins' single OAuth app and the single callback GitHub allows:
// entry points are separate per face, the shared callback dispatches
// on oauthState.Kind, and the two account namespaces never cross —
// this file only ever reads and writes store.User rows, the admin
// callback only store.Admin rows.

// errGitHubBound refuses a bind (or a registration race) when the
// GitHub account already belongs to another user. GitHub bindings
// never merge accounts — merging exists only on the e-mail path,
// where mailbox ownership is proven by a code (design D6).
var errGitHubBound = errors.New("该 GitHub 账号已绑定其他账号")

// githubBindTicket is what a bind callback hands back to the face
// that started the flow. The callback request itself carries no
// usable session for either face (panel tokens live in localStorage,
// the portal cookie is scoped to /u), so the bind is completed by a
// redemption call from the signed-in face — and that redemption is
// where the state's user and the session's user are compared
// (design D6 / D8 red line 4).
type githubBindTicket struct {
	UserID      string
	GitHubID    int64
	GitHubLogin string
	Target      string
}

// userRegistrationOpen reports whether this deployment takes new
// self-service accounts at all, by any method (design D6): the
// GitHub login of an unbound account may register when either
// registration switch is on.
func (s *Server) userRegistrationOpen() bool {
	return s.st.GetSetting("portal_email_register", "false") == "true" || s.passwordRegisterOn()
}

// userGitHubAuthURL mints a user-flow state and answers the authorize
// URL, shaped exactly like the admin one (same app, same callback,
// same scope) apart from the state payload.
func (s *Server) userGitHubAuthURL(c *gin.Context, target, mode, userID string) {
	id, _ := s.githubCreds()
	if !s.githubEnabled() {
		badRequest(c, errors.New("尚未配置 GitHub 登录：请联系管理员配置 GitHub OAuth App"))
		return
	}
	state := s.ghAuth.put(oauthState{Mode: mode, Kind: "user", Target: target, UserID: userID})
	q := url.Values{"client_id": {id}, "redirect_uri": {s.githubCallbackURL(c.Request)}, "state": {state}, "scope": {"read:user"}, "allow_signup": {"false"}}
	c.JSON(http.StatusOK, gin.H{"url": ghOAuthWeb + "/login/oauth/authorize?" + q.Encode()})
}

// userGitHubLoginURLPanel: POST /auth/github/user/login-url — the
// panel face's GitHub sign-in entry (public, behind loginGuard).
func (s *Server) userGitHubLoginURLPanel(c *gin.Context) {
	if !s.memberLoginEnabled() {
		c.JSON(http.StatusForbidden, gin.H{"error": "注册用户登录面板未开启"})
		return
	}
	s.userGitHubAuthURL(c, "panel", "login", "")
}

// userGitHubBindURLPanel: POST /auth/github/user/bind-url — a member
// starts binding GitHub to their own account.
func (s *Server) userGitHubBindURLPanel(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可绑定 GitHub 登录"})
		return
	}
	s.userGitHubAuthURL(c, "panel", "bind", u.ID)
}

// userGitHubLoginURLPortal: POST /u/github/login-url — the user
// center's GitHub sign-in entry (no session yet, by definition).
func (s *Server) userGitHubLoginURLPortal(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if until, banned := s.ipBannedUntil(clientIP(c.Request)); banned {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": fmt.Sprintf("尝试过于频繁，请在 %s 后再试", until.Format("15:04"))})
		return
	}
	s.userGitHubAuthURL(c, "portal", "login", "")
}

// userGitHubBindURLPortal: POST /u/github/bind-url — a signed-in
// portal user starts binding GitHub to their own account.
func (s *Server) userGitHubBindURLPortal(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	s.userGitHubAuthURL(c, "portal", "bind", u.ID)
}

// userGitHubFail lands a failed user flow back on the face it came
// from, with the message in the channel that face reads: the panel
// login/account pages read the URL fragment, the portal pages read
// the query string.
func (s *Server) userGitHubFail(c *gin.Context, st oauthState, msg string) {
	if st.Target == "portal" {
		if st.Mode == "bind" {
			c.Redirect(http.StatusFound, portalPath+"?gh_error="+url.QueryEscape(msg)+"#account")
			return
		}
		c.Redirect(http.StatusFound, portalPath+"/login?gh_error="+url.QueryEscape(msg))
		return
	}
	page := ""
	if st.Mode == "bind" {
		page = "account"
	}
	c.Redirect(http.StatusFound, s.adminPageURL(c.Request, page, "gh_error="+url.QueryEscape(msg)))
}

// githubUserCallback is the user half of the shared GitHub callback
// (githuboauth.go dispatches on the state's Kind before the admin
// switch can see it).
func (s *Server) githubUserCallback(c *gin.Context, st oauthState) {
	if e := c.Query("error"); e != "" {
		s.userGitHubFail(c, st, "已取消 GitHub 授权")
		return
	}
	gu, err := s.githubIdentity(c.Request.Context(), c.Query("code"), s.githubCallbackURL(c.Request))
	if err != nil {
		s.userGitHubFail(c, st, err.Error())
		return
	}
	if st.Mode == "bind" {
		s.githubUserBindCallback(c, st, gu)
		return
	}
	s.githubUserLoginCallback(c, st, gu)
}

// githubUserLoginCallback signs the bound user in on the face the
// flow started from, or registers a new account when the deployment
// takes registrations (design D6). GitHub's e-mail is never used to
// find or bind an account: an address GitHub reports is not proof of
// ownership of the account that holds it here.
func (s *Server) githubUserLoginCallback(c *gin.Context, st oauthState, gu *githubUser) {
	ip := clientIP(c.Request)
	var u store.User
	if err := s.st.DB.First(&u, "git_hub_id = ? AND is_service = ?", gu.ID, false).Error; err != nil {
		if !s.userRegistrationOpen() {
			s.audit("gh:"+gu.Login, "user.github_login_failed", gu.Login, "unbound, registration closed", ip)
			s.userGitHubFail(c, st, "该 GitHub 账号未绑定任何账号，请先注册后在账号设置中绑定")
			return
		}
		nu, rerr := s.registerByGitHub(gu, ip)
		switch {
		case rerr == nil:
			u = *nu
		case errors.Is(rerr, errGitHubBound):
			// Lost a race: the account was bound between the lookup
			// and the create — sign that account in instead.
			if err := s.st.DB.First(&u, "git_hub_id = ? AND is_service = ?", gu.ID, false).Error; err != nil {
				s.userGitHubFail(c, st, "登录失败，请稍后重试")
				return
			}
		default:
			s.userGitHubFail(c, st, rerr.Error())
			return
		}
	}
	if u.Status == "disabled" {
		s.audit("gh:"+gu.Login, "user.github_login_failed", gu.Login, "disabled", ip)
		s.userGitHubFail(c, st, "无法使用该 GitHub 账号登录")
		return
	}
	switch st.Target {
	case "portal":
		if !s.portalEnabled() {
			s.userGitHubFail(c, st, "用户中心暂未开放")
			return
		}
		tok, err := s.issuePortalGitHubToken(&u, c)
		if err != nil {
			s.userGitHubFail(c, st, "登录失败，请稍后重试")
			return
		}
		s.setPortalCookie(c, tok, int(portalSessionTTL.Seconds()))
		s.audit("gh:"+u.Username, "user.portal_login", u.Username, "github", ip)
		c.Redirect(http.StatusFound, portalPath)
	default: // panel
		if !s.memberLoginEnabled() {
			s.userGitHubFail(c, st, "注册用户登录面板未开启")
			return
		}
		tok, err := s.issueMemberToken(&u, c)
		if err != nil {
			s.userGitHubFail(c, st, "登录失败，请稍后重试")
			return
		}
		s.audit(memberActor(&u), "member.login", u.Username, "github", ip)
		c.Redirect(http.StatusFound, s.adminPageURL(c.Request, "", "token="+url.QueryEscape(tok)+"&user="+url.QueryEscape(memberDisplayName(&u))))
	}
}

// githubUserBindCallback handles the bind intent: occupancy is
// checked up front (another user's binding refuses outright), then
// the bind is handed to the originating face as a single-use ticket
// for the signed-in redemption (see githubBindTicket).
func (s *Server) githubUserBindCallback(c *gin.Context, st oauthState, gu *githubUser) {
	ip := clientIP(c.Request)
	var other store.User
	if err := s.st.DB.First(&other, "git_hub_id = ? AND is_service = ?", gu.ID, false).Error; err == nil {
		if other.ID != st.UserID {
			s.audit("gh:"+gu.Login, "user.github_bind_failed", gu.Login, "bound to another user", ip)
			s.userGitHubFail(c, st, errGitHubBound.Error())
			return
		}
		// Already bound to the initiator: nothing to redeem.
		if st.Target == "portal" {
			c.Redirect(http.StatusFound, portalPath+"#account")
			return
		}
		c.Redirect(http.StatusFound, s.adminPageURL(c.Request, "account", "gh_bound="+url.QueryEscape(gu.Login)))
		return
	}
	key := pki.Token(24)
	ticket := githubBindTicket{UserID: st.UserID, GitHubID: gu.ID, GitHubLogin: gu.Login, Target: st.Target}
	if err := s.eph.put("ghbind:"+key, ticket, oauthTTL); err != nil {
		s.userGitHubFail(c, st, "绑定失败，请稍后重试")
		return
	}
	if st.Target == "portal" {
		c.Redirect(http.StatusFound, portalPath+"?ghbind="+key+"#account")
		return
	}
	c.Redirect(http.StatusFound, s.adminPageURL(c.Request, "account", "gh_bind_ticket="+url.QueryEscape(key)))
}

// completeGitHubBind redeems a bind ticket for the signed-in user.
// The ticket is spent even when the session user does not match the
// user who started the flow: a mismatched redemption voids the whole
// attempt (design D8 red line 4). Occupancy is re-checked under the
// binding lock — the callback's early check is only a shortcut.
func (s *Server) completeGitHubBind(u *store.User, key string) (*githubUser, error) {
	var ticket githubBindTicket
	if !s.eph.take("ghbind:"+key, &ticket) {
		return nil, errors.New("绑定请求已过期，请重新发起")
	}
	if ticket.UserID != u.ID {
		return nil, errBindingIdentity
	}
	gu := &githubUser{ID: ticket.GitHubID, Login: ticket.GitHubLogin}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockAccountBindings(tx); err != nil {
			return err
		}
		users, err := lockBindingUsers(tx, u.ID)
		if err != nil {
			return err
		}
		cur := users[u.ID]
		var n int64
		if err := tx.Model(&store.User{}).Where("git_hub_id = ? AND id <> ?", ticket.GitHubID, u.ID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return errGitHubBound
		}
		if cur.GitHubID == ticket.GitHubID {
			return nil // re-bind of the same account: no-op
		}
		if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"git_hub_id": ticket.GitHubID, "git_hub_login": ticket.GitHubLogin}).Error; err != nil {
			return err
		}
		cur.GitHubID, cur.GitHubLogin = ticket.GitHubID, ticket.GitHubLogin
		return s.queueUserEvent(tx, "user.updated", &cur, map[string]any{"via": "github_binding"})
	})
	if err != nil {
		return nil, err
	}
	return gu, nil
}

// githubBindError answers a failed bind action on a JSON face.
func githubBindError(c *gin.Context, err error) {
	if errors.Is(err, errGitHubBound) || errors.Is(err, errBindingIdentity) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "account_binding_conflict"})
		return
	}
	badRequest(c, err)
}

// userGitHubBindComplete: POST /auth/github/user/bind-complete
// {ticket} — the panel redemption of a bind ticket, under the member
// session the flow started from.
func (s *Server) userGitHubBindComplete(c *gin.Context) {
	u := memberUserOf(c)
	if u == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅注册用户可绑定 GitHub 登录"})
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
	s.audit(memberActor(u), "user.github_bound", u.Username, gu.Login, clientIP(c.Request))
	s.wakeBusinessEvents()
	c.JSON(http.StatusOK, gin.H{"ok": true, "githubLogin": gu.Login})
}

// sanitizeGitHubName derives a registration username base from a
// GitHub login, with registerByMail's generated-name rules (system
// names are exempt from the reserved list and the admin cross-check:
// the random suffix on conflict keeps them unmistakably machine-made,
// design D2).
func sanitizeGitHubName(login string) string {
	local := strings.ToLower(login)
	local = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return -1
	}, local)
	local = strings.Trim(local, ".-")
	if len(local) > 24 {
		local = strings.Trim(local[:24], ".-")
	}
	if len(local) < 3 {
		local = "user"
	}
	return local
}

// registerByGitHub creates the account of a new GitHub user,
// isomorphic to registerByMail: same per-IP daily cap (the same
// "regip:" counter), same license cap, same trial plan, SelfService
// from birth. The caller has already established that registration
// is open (userRegistrationOpen).
func (s *Server) registerByGitHub(gu *githubUser, ip string) (*store.User, error) {
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
	base := sanitizeGitHubName(gu.Login)
	trialID := strings.TrimSpace(s.st.GetSetting("register_trial_plan", ""))
	var u store.User
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockAccountBindings(tx); err != nil {
			return err
		}
		var n int64
		if err := tx.Model(&store.User{}).Where("git_hub_id = ?", gu.ID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return errGitHubBound
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
		u = newUser(&userReq{Username: name, Remark: "GitHub 注册"})
		u.GitHubID, u.GitHubLogin, u.SelfService = gu.ID, gu.Login, true
		return s.createRegisteredUser(tx, &u, "github", trialID)
	})
	if err != nil {
		return nil, err
	}
	s.MarkDirty("user registered", s.nodesForUsers(u.ID)...)
	s.audit("gh:"+u.Username, "user.register", u.Username, gu.Login, ip)
	s.wakeBusinessEvents()
	s.notify(Event{Kind: "user_registered", Text: fmt.Sprintf("🆕 新用户通过 GitHub 注册：%s（%s）", u.Username, gu.Login)})
	return &u, nil
}
