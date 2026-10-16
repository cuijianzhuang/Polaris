package panel

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Single sign-on: any OpenID Connect provider (Google, Microsoft, GitLab,
// Keycloak, Authentik, Okta, Auth0…, endpoints discovered from the issuer)
// or plain OAuth 2.0 server (GitHub, Gitea…) gets a "使用 … 登录" button.
// Like GitHub sign-in, an admin binds the external account once while signed
// in; the authorization code flow uses PKCE and a single-use state, and the
// account is identified through the userinfo endpoint. Admins with 2FA
// still enter their code.

func (s *Server) ssoCallbackURL(r *http.Request) string {
	return s.requestBase(r) + s.cfg.APIBase() + "/auth/sso/callback"
}

type oidcDiscovery struct {
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	UserInfo      string `json:"userinfo_endpoint"`
}

func discoverOIDC(ctx context.Context, issuer string) (*oidcDiscovery, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", u, err)
	}
	defer resp.Body.Close()
	var d oidcDiscovery
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&d) != nil || d.Authorization == "" || d.Token == "" {
		return nil, fmt.Errorf("%s 不是有效的 OpenID Connect 配置（%s）", u, resp.Status)
	}
	return &d, nil
}

func validEndpoint(u string) bool {
	p, err := url.Parse(u)
	return err == nil && p.Host != "" && (p.Scheme == "https" || p.Scheme == "http")
}

type ssoProviderReq struct {
	Name         string `json:"name" binding:"required"`
	Kind         string `json:"kind" binding:"required"`
	Issuer       string `json:"issuer"`
	AuthURL      string `json:"authUrl"`
	TokenURL     string `json:"tokenUrl"`
	UserInfoURL  string `json:"userInfoUrl"`
	ClientID     string `json:"clientId" binding:"required"`
	ClientSecret string `json:"clientSecret"`
	Scopes       string `json:"scopes"`
	SubjectField string `json:"subjectField"`
	LoginField   string `json:"loginField"`
	Enabled      *bool  `json:"enabled"`
}

// fill validates the request into p, discovering OIDC endpoints.
func (s *Server) fillProvider(ctx context.Context, p *store.SSOProvider, req ssoProviderReq) error {
	p.Name, p.Kind, p.ClientID = strings.TrimSpace(req.Name), req.Kind, strings.TrimSpace(req.ClientID)
	p.Scopes, p.SubjectField, p.LoginField = strings.TrimSpace(req.Scopes), strings.TrimSpace(req.SubjectField), strings.TrimSpace(req.LoginField)
	if req.ClientSecret != "" {
		p.ClientSecret = s.st.Box.Seal(req.ClientSecret)
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}
	switch p.Kind {
	case "oidc":
		p.Issuer = strings.TrimRight(strings.TrimSpace(req.Issuer), "/")
		if !validEndpoint(p.Issuer) {
			return errors.New("请填写 Issuer 地址（https://…）")
		}
		d, err := discoverOIDC(ctx, p.Issuer)
		if err != nil {
			return err
		}
		p.AuthURL, p.TokenURL, p.UserInfoURL = d.Authorization, d.Token, d.UserInfo
		if p.UserInfoURL == "" {
			return errors.New("该提供方没有 userinfo 端点")
		}
		p.Scopes = orDefault(p.Scopes, "openid profile email")
		p.SubjectField = orDefault(p.SubjectField, "sub")
		p.LoginField = orDefault(p.LoginField, "preferred_username")
	case "oauth2":
		p.Issuer = ""
		p.AuthURL, p.TokenURL, p.UserInfoURL = strings.TrimSpace(req.AuthURL), strings.TrimSpace(req.TokenURL), strings.TrimSpace(req.UserInfoURL)
		if !validEndpoint(p.AuthURL) || !validEndpoint(p.TokenURL) || !validEndpoint(p.UserInfoURL) {
			return errors.New("请填写授权、令牌与用户信息三个地址（https://…）")
		}
		p.SubjectField = orDefault(p.SubjectField, "id")
		p.LoginField = orDefault(p.LoginField, "login")
	default:
		return errors.New("类型应为 oidc 或 oauth2")
	}
	if p.Name == "" || p.ClientID == "" || p.ClientSecret == "" {
		return errors.New("请填写名称、Client ID 与 Client Secret")
	}
	return nil
}

// --- admin API ---

func (s *Server) listSSOProviders(c *gin.Context) {
	var ps []store.SSOProvider
	s.st.DB.Order("created_at").Find(&ps)
	c.JSON(http.StatusOK, gin.H{"providers": nonNil(ps), "callbackUrl": s.ssoCallbackURL(c.Request)})
}

func (s *Server) createSSOProvider(c *gin.Context) {
	var req ssoProviderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	p := store.SSOProvider{ID: store.NewID(), Enabled: true}
	if err := s.fillProvider(c.Request.Context(), &p, req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.st.DB.Create(&p).Error; err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "sso.create", p.Name, p.Kind)
	c.JSON(http.StatusCreated, p)
}

func (s *Server) updateSSOProvider(c *gin.Context) {
	var p store.SSOProvider
	if err := s.st.DB.First(&p, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req ssoProviderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.fillProvider(c.Request.Context(), &p, req); err != nil {
		badRequest(c, err)
		return
	}
	s.st.DB.Save(&p)
	s.auditCtx(c, "sso.update", p.Name, "")
	c.JSON(http.StatusOK, p)
}

func (s *Server) deleteSSOProvider(c *gin.Context) {
	s.st.DB.Where("provider_id = ?", c.Param("id")).Delete(&store.SSOBinding{})
	s.st.DB.Delete(&store.SSOProvider{}, "id = ?", c.Param("id"))
	s.auditCtx(c, "sso.delete", c.Param("id"), "")
	c.Status(http.StatusNoContent)
}

// mySSOBindings: GET /auth/sso/bindings — the signed-in admin's accounts.
func (s *Server) mySSOBindings(c *gin.Context) {
	var bs []store.SSOBinding
	s.st.DB.Where("admin_id = ?", c.GetString("adminID")).Find(&bs)
	c.JSON(http.StatusOK, nonNil(bs))
}

// ssoUnbind: DELETE /auth/sso/bindings/:id.
func (s *Server) ssoUnbind(c *gin.Context) {
	s.st.DB.Where("provider_id = ? AND admin_id = ?", c.Param("id"), c.GetString("adminID")).Delete(&store.SSOBinding{})
	s.auditCtx(c, "admin.sso_unbind", c.GetString("admin"), c.Param("id"))
	c.Status(http.StatusNoContent)
}

// publicSSOProviders are the login page buttons.
func (s *Server) publicSSOProviders() []gin.H {
	var ps []store.SSOProvider
	s.st.DB.Where("enabled = ?", true).Order("created_at").Find(&ps)
	out := []gin.H{}
	for _, p := range ps {
		out = append(out, gin.H{"id": p.ID, "name": p.Name})
	}
	return out
}

// --- the round trip ---

func pkcePair() (verifier, challenge string) {
	verifier = pki.Token(48)
	h := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(h[:])
}

func (s *Server) ssoAuthURL(c *gin.Context, mode, adminID string) {
	var p store.SSOProvider
	if s.st.DB.First(&p, "id = ? AND enabled = ?", c.Param("id"), true).Error != nil {
		notFound(c)
		return
	}
	verifier, challenge := pkcePair()
	state := s.ghAuth.put(oauthState{mode: mode, adminID: adminID, provider: p.ID, verifier: verifier})
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

func (s *Server) ssoLoginURL(c *gin.Context) { s.ssoAuthURL(c, "login", "") }
func (s *Server) ssoBindURL(c *gin.Context)  { s.ssoAuthURL(c, "bind", c.GetString("adminID")) }

// ssoIdentity exchanges the code and reads the account from userinfo.
func (s *Server) ssoIdentity(ctx context.Context, p *store.SSOProvider, code, verifier, redirect string) (subject, login string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	secret := s.st.Box.MustOpen(p.ClientSecret)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {p.ClientID}, "client_secret": {secret}, "code_verifier": {verifier}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(secret))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
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
			return "", "", fmt.Errorf("%s 授权失败：%s", p.Name, orDefault(tok.Description, orDefault(tok.Error, resp.Status)))
		}
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, p.UserInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var info map[string]any
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info) != nil {
		return "", "", fmt.Errorf("读取 %s 用户信息失败：%s", p.Name, resp.Status)
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
		return "", "", fmt.Errorf("%s 用户信息中没有 %q 字段", p.Name, p.SubjectField)
	}
	login = orDefault(str(p.LoginField), orDefault(str("email"), orDefault(str("name"), subject)))
	return subject, login, nil
}

// ssoCallback: GET /auth/sso/callback (the provider redirects here).
func (s *Server) ssoCallback(c *gin.Context) {
	page := ""
	back := func(fragment string) { c.Redirect(http.StatusFound, s.adminPageURL(c.Request, page, fragment)) }
	fail := func(msg string) { back("gh_error=" + url.QueryEscape(msg)) }
	st, ok := s.ghAuth.take(c.Query("state"))
	if ok && st.mode == "bind" {
		page = "settings"
	}
	if !ok || st.provider == "" {
		fail("登录请求已过期，请重试")
		return
	}
	if e := c.Query("error"); e != "" {
		fail("已取消授权：" + orDefault(c.Query("error_description"), e))
		return
	}
	var p store.SSOProvider
	if s.st.DB.First(&p, "id = ?", st.provider).Error != nil {
		fail("登录方式已被删除")
		return
	}
	subject, login, err := s.ssoIdentity(c.Request.Context(), &p, c.Query("code"), st.verifier, s.ssoCallbackURL(c.Request))
	if err != nil {
		fail(err.Error())
		return
	}
	ip := clientIP(c.Request)
	switch st.mode {
	case "bind":
		var other store.SSOBinding
		if s.st.DB.First(&other, "provider_id = ? AND subject = ? AND admin_id <> ?", p.ID, subject, st.adminID).Error == nil {
			fail("该 " + p.Name + " 账号已绑定其他管理员")
			return
		}
		s.st.DB.Where("provider_id = ? AND admin_id = ?", p.ID, st.adminID).Delete(&store.SSOBinding{})
		s.st.DB.Create(&store.SSOBinding{ProviderID: p.ID, Subject: subject, AdminID: st.adminID, Login: login, CreatedAt: time.Now()})
		s.audit("system", "admin.sso_bind", p.Name, login, ip)
		back("sso_bound=" + url.QueryEscape(p.Name+" · "+login))
	default:
		var b store.SSOBinding
		var a store.Admin
		if s.st.DB.First(&b, "provider_id = ? AND subject = ?", p.ID, subject).Error != nil || s.st.DB.First(&a, "id = ?", b.AdminID).Error != nil {
			s.recordIPFailure(ip, p.Name+" 账号未绑定")
			s.audit(login, "admin.login_failed", login, p.Name+": not bound", ip)
			fail(p.Name + " 账号 " + login + " 未绑定任何管理员：请先用密码登录，在「设置 → 账号安全」绑定")
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
		s.finishLogin(c, &a, "sso", back)
	}
}
