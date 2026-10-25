package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// GitHub sign-in for admins. An admin binds their GitHub account once while
// signed in; afterwards "使用 GitHub 登录" signs them in without a password.
// Admins with two-factor authentication still have to enter their code.

// OAuth endpoints (variables so tests can use a fake GitHub).
var (
	ghOAuthWeb = "https://github.com"
	ghOAuthAPI = "https://api.github.com"
)

const oauthTTL = 10 * time.Minute

type oauthState struct {
	Mode     string `json:"m"`           // login / bind
	AdminID  string `json:"a,omitempty"` // bind: the signed-in admin
	Provider string `json:"p,omitempty"` // SSO provider id ("" = GitHub)
	Verifier string `json:"v,omitempty"` // SSO: PKCE code verifier
}

type oauthTicket struct {
	AdminID string `json:"a"`
}

// githubAuth keeps OAuth states and TOTP tickets in the shared store: the
// provider may send the browser back to another panel instance.
type githubAuth struct{ eph *ephStore }

func (g *githubAuth) put(st oauthState) string {
	key := pki.Token(24)
	_ = g.eph.put("oauth:"+key, st, oauthTTL)
	return key
}

// take consumes a state (single use).
func (g *githubAuth) take(key string) (oauthState, bool) {
	var st oauthState
	ok := key != "" && g.eph.take("oauth:"+key, &st)
	return st, ok
}

func (g *githubAuth) ticket(adminID string) string {
	key := pki.Token(24)
	_ = g.eph.put("ticket:"+key, oauthTicket{AdminID: adminID}, 5*time.Minute)
	return key
}

func (g *githubAuth) takeTicket(key string) (string, bool) {
	var t oauthTicket
	ok := key != "" && g.eph.take("ticket:"+key, &t)
	return t.AdminID, ok
}

func (s *Server) githubCreds() (string, string) {
	return s.st.GetSetting("github_client_id", ""), s.st.Box.MustOpen(s.st.GetSetting("github_client_secret", ""))
}

func (s *Server) githubEnabled() bool {
	id, secret := s.githubCreds()
	return id != "" && secret != ""
}

func (s *Server) githubCallbackURL(r *http.Request) string {
	return s.requestBase(r) + s.cfg.APIBase() + "/auth/github/callback"
}

// adminPageURL is adminURL for a page of the admin UI (e.g. "settings").
func (s *Server) adminPageURL(r *http.Request, page, fragment string) string {
	return s.requestBase(r) + strings.TrimRight(s.cfg.AdminPath, "/") + "/" + page + "#" + fragment
}

// githubAuthURL starts a round trip: POST /auth/github/url {mode}. Binding
// requires a signed-in admin (the route sits behind auth for mode=bind).
func (s *Server) githubAuthURL(c *gin.Context, mode, adminID string) {
	id, _ := s.githubCreds()
	if !s.githubEnabled() {
		badRequest(c, errors.New("尚未配置 GitHub 登录：请在「设置」填写 GitHub OAuth App 的 Client ID 与 Secret"))
		return
	}
	state := s.ghAuth.put(oauthState{Mode: mode, AdminID: adminID})
	q := url.Values{"client_id": {id}, "redirect_uri": {s.githubCallbackURL(c.Request)}, "state": {state}, "scope": {"read:user"}, "allow_signup": {"false"}}
	c.JSON(http.StatusOK, gin.H{"url": ghOAuthWeb + "/login/oauth/authorize?" + q.Encode()})
}

func (s *Server) githubLoginURL(c *gin.Context) { s.githubAuthURL(c, "login", "") }
func (s *Server) githubBindURL(c *gin.Context)  { s.githubAuthURL(c, "bind", c.GetString("adminID")) }

type githubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// githubIdentity exchanges the code and reads the GitHub user.
func (s *Server) githubIdentity(ctx context.Context, code, redirect string) (*githubUser, error) {
	id, secret := s.githubCreds()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	form := url.Values{"client_id": {id}, "client_secret": {secret}, "code": {code}, "redirect_uri": {redirect}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ghOAuthWeb+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	err = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("GitHub 授权失败：%s", orDefault(tok.Error, fmt.Sprint(err)))
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, ghOAuthAPI+"/user", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var u githubUser
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&u) != nil || u.ID == 0 {
		return nil, fmt.Errorf("读取 GitHub 用户失败：%s", resp.Status)
	}
	return &u, nil
}

// githubCallback is GET /auth/github/callback (public: GitHub redirects here).
func (s *Server) githubCallback(c *gin.Context) {
	page := ""
	back := func(fragment string) { c.Redirect(http.StatusFound, s.adminPageURL(c.Request, page, fragment)) }
	fail := func(msg string) { back("gh_error=" + url.QueryEscape(msg)) }
	st, ok := s.ghAuth.take(c.Query("state"))
	if ok && st.Mode == "bind" {
		page = "settings" // the bind result is shown where binding started
	}
	if !ok {
		fail("登录请求已过期，请重试")
		return
	}
	if e := c.Query("error"); e != "" {
		fail("已取消 GitHub 授权")
		return
	}
	gu, err := s.githubIdentity(c.Request.Context(), c.Query("code"), s.githubCallbackURL(c.Request))
	if err != nil {
		fail(err.Error())
		return
	}
	switch st.Mode {
	case "bind":
		var other store.Admin
		if s.st.DB.Where("git_hub_id = ? AND id <> ?", gu.ID, st.AdminID).First(&other).Error == nil {
			fail("该 GitHub 账号已绑定管理员 " + other.Username)
			return
		}
		var a store.Admin
		if s.st.DB.First(&a, "id = ?", st.AdminID).Error != nil {
			fail("管理员不存在")
			return
		}
		s.st.DB.Model(&a).Updates(map[string]any{"git_hub_id": gu.ID, "git_hub_login": gu.Login})
		s.audit(a.Username, "admin.github_bind", a.Username, gu.Login, clientIP(c.Request))
		back("gh_bound=" + url.QueryEscape(gu.Login))
	default:
		var a store.Admin
		if s.st.DB.First(&a, "git_hub_id = ?", gu.ID).Error != nil {
			s.audit(gu.Login, "admin.github_login_failed", gu.Login, "not bound", clientIP(c.Request))
			fail("GitHub 账号 " + gu.Login + " 未绑定任何管理员：请先用密码登录，在「设置」中绑定")
			return
		}
		if lockedOut(&a, time.Now()) {
			fail("登录失败次数过多，请 15 分钟后再试")
			return
		}
		if a.TOTPEnabled {
			back("gh_ticket=" + s.ghAuth.ticket(a.ID))
			return
		}
		s.finishLogin(c, &a, "github", back)
	}
}

func (s *Server) finishLogin(c *gin.Context, a *store.Admin, via string, back func(string)) {
	s.st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Updates(map[string]any{"failed_attempts": 0, "locked_until": nil, "last_login_at": time.Now()})
	tok, err := s.issueToken(a, c, via)
	if err != nil {
		back("gh_error=" + url.QueryEscape(err.Error()))
		return
	}
	s.audit(a.Username, "admin.login", a.Username, via, clientIP(c.Request))
	back("token=" + url.QueryEscape(tok) + "&user=" + url.QueryEscape(a.Username))
}

// githubTOTP completes a GitHub sign-in for an admin with 2FA:
// POST /auth/github/totp {ticket, totp}.
func (s *Server) githubTOTP(c *gin.Context) {
	var req struct {
		Ticket string `json:"ticket" binding:"required"`
		TOTP   string `json:"totp" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	id, ok := s.ghAuth.takeTicket(req.Ticket)
	var a store.Admin
	if !ok || s.st.DB.First(&a, "id = ?", id).Error != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "登录已过期，请重新使用 GitHub 登录"})
		return
	}
	if lockedOut(&a, time.Now()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登录失败次数过多，请 15 分钟后再试"})
		return
	}
	if !s.secondFactor(&a, req.TOTP, clientIP(c.Request)) {
		s.countLoginFailure(&a, time.Now())
		s.recordIPFailure(clientIP(c.Request), "GitHub 登录二次验证码错误")
		s.audit(a.Username, "admin.login_failed", a.Username, "github: bad totp", clientIP(c.Request))
		c.JSON(http.StatusUnauthorized, gin.H{"error": "二次验证码错误，请重新使用 GitHub 登录"})
		return
	}
	s.st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Updates(map[string]any{"failed_attempts": 0, "last_login_at": time.Now()})
	tok, err := s.issueToken(&a, c, "github")
	if err != nil {
		serverError(c, err)
		return
	}
	s.audit(a.Username, "admin.login", a.Username, "github", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"token": tok, "username": a.Username, "expiresIn": int(tokenTTL.Seconds())})
}

// githubUnbind removes the signed-in admin's GitHub binding.
func (s *Server) githubUnbind(c *gin.Context) {
	var a store.Admin
	if err := s.st.DB.First(&a, "id = ?", c.GetString("adminID")).Error; err != nil {
		notFound(c)
		return
	}
	s.st.DB.Model(&a).Updates(map[string]any{"git_hub_id": 0, "git_hub_login": ""})
	s.auditCtx(c, "admin.github_unbind", a.Username, a.GitHubLogin)
	c.Status(http.StatusNoContent)
}

// githubInfo tells the settings page what to show.
func (s *Server) githubInfo(c *gin.Context) {
	id, _ := s.githubCreds()
	c.JSON(http.StatusOK, gin.H{"enabled": s.githubEnabled(), "clientId": id, "callbackUrl": s.githubCallbackURL(c.Request)})
}
