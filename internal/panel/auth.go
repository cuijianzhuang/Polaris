package panel

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

const (
	maxLoginFailures = 5
	lockDuration     = 15 * time.Minute
	tokenTTL         = 24 * time.Hour
)

type claims struct {
	jwt.RegisteredClaims
	Username string `json:"usr"`
	Version  int    `json:"ver"`
	TG       string `json:"tg,omitempty"`  // Telegram admin id for bot sign-ins
	SID      string `json:"sid,omitempty"` // admin session (store.AdminSession)
	// User center sessions (portal.go): never valid for the admin API.
	User  string `json:"u,omitempty"`
	Chat  int64  `json:"ch,omitempty"`
	SubFP string `json:"sfp,omitempty"`
	// MailFP: a session from an e-mail sign-in (Chat is 0), valid while
	// the user keeps that address.
	MailFP string `json:"mfp,omitempty"`
}

// issueToken signs a session for an admin who just signed in (via:
// password / github), recording the session for the request's client.
func (s *Server) issueToken(a *store.Admin, gc *gin.Context, via string) (string, error) {
	c := claims{
		SID: s.newSession(gc, a.ID, a.Username, via, tokenTTL),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   a.ID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(tokenTTL)),
		},
		Username: a.Username,
		Version:  a.TokenVersion,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.jwtKey)
}

func (s *Server) parseToken(tok string) (*claims, error) {
	var c claims
	_, err := jwt.ParseWithClaims(tok, &c, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("unexpected signing method")
		}
		return s.jwtKey, nil
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// authRequired validates the bearer token. Browsers cannot set headers on
// WebSocket upgrades, so only the /ws endpoint also accepts ?token=.
func (s *Server) authRequired(c *gin.Context) {
	tok := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if tok == "" && strings.HasSuffix(c.Request.URL.Path, "/ws") {
		tok = c.Query("token")
	}
	if strings.HasPrefix(tok, apiTokenPrefix) {
		s.authToken(c, tok)
		return
	}
	cl, err := s.parseToken(tok)
	if err != nil || cl.User != "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	// Every session lives server-side too: revoked or expired ends it.
	if _, ok := s.sessionAlive(cl.SID); !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.Set("sid", cl.SID)
	if cl.TG != "" {
		// Telegram sign-in: valid while the Telegram admin exists. Operators
		// are confined to their nodes; the owner gets full access.
		var a store.TGAdmin
		if s.st.DB.First(&a, "id = ?", cl.TG).Error != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		s.noteBase(c.Request)
		c.Set("admin", "tg:"+a.Name)
		c.Set("tgAdmin", &a)
		c.Set("adminRole", a.Role)
		if a.Role != "owner" {
			sc := map[string]bool{}
			for _, id := range a.NodeIDs.V {
				sc[id] = true
			}
			c.Set("nodeScope", sc)
		}
		c.Next()
		return
	}
	// Sessions die with the admin account and on password change.
	var a store.Admin
	if s.st.DB.Select("id", "username", "token_version", "role", "node_ids").First(&a, "id = ?", cl.Subject).Error != nil || a.TokenVersion != cl.Version {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	s.noteBase(c.Request)
	c.Set("admin", a.Username)
	c.Set("staffAdmin", &a)
	c.Set("adminRole", normalizedAdminRole(a.Role))
	if a.Role == "operator" {
		sc := map[string]bool{}
		for _, id := range a.NodeIDs.V {
			sc[id] = true
		}
		c.Set("nodeScope", sc)
	}
	c.Set("adminID", cl.Subject)
	c.Set("tokenVersion", cl.Version)
	c.Next()
}

type loginReq struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
	TOTP     string `json:"totp"`
}

// login implements bcrypt + optional TOTP with a 5-strike, 15-minute lockout.
func (s *Server) login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	// Copy-pasted credentials often carry stray whitespace, and phone
	// keyboards capitalise the first letter: match the username without
	// case and accept a password whose only difference is outer whitespace.
	req.Username = strings.TrimSpace(req.Username)
	var a store.Admin
	if s.st.DB.First(&a, "username = ?", req.Username).Error != nil {
		s.st.DB.First(&a, "LOWER(username) = LOWER(?)", req.Username)
	}
	ip := clientIP(c.Request)
	if a.ID == "" {
		// Same cost as a real check to avoid username probing by timing.
		_ = passwordMatches(string(dummyHash), req.Password)
		s.recordIPFailure(ip, "用户名不存在")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
		return
	}
	now := time.Now()
	if a.LockedUntil != nil && now.Before(*a.LockedUntil) {
		// Only the right password learns of the lock: a 429 for any guess
		// would tell which usernames exist.
		if !passwordMatches(a.PasswordHash, req.Password) {
			s.recordIPFailure(ip, "用户名或密码错误")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
			return
		}
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登录失败次数过多，请 15 分钟后再试", "lockedUntil": a.LockedUntil})
		return
	}
	fail := func(msg string) {
		s.countLoginFailure(&a, now)
		s.recordIPFailure(ip, msg)
		s.audit(a.Username, "admin.login_failed", a.Username, msg, ip)
		c.JSON(http.StatusUnauthorized, gin.H{"error": msg})
	}
	if !passwordMatches(a.PasswordHash, req.Password) {
		fail("用户名或密码错误")
		return
	}
	if a.TOTPEnabled {
		if req.TOTP == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "需要二次验证码", "totpRequired": true})
			return
		}
		if !s.secondFactor(&a, req.TOTP, ip) {
			fail("二次验证码错误")
			return
		}
	}
	s.st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Updates(map[string]any{"failed_attempts": 0, "locked_until": nil, "last_login_at": now})
	tok, err := s.issueToken(&a, c, "password")
	if err != nil {
		serverError(c, err)
		return
	}
	s.audit(a.Username, "admin.login", a.Username, "", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"token": tok, "username": a.Username, "expiresIn": int(tokenTTL.Seconds())})
}

func (s *Server) me(c *gin.Context) {
	if v, ok := c.Get("tgAdmin"); ok {
		a := v.(*store.TGAdmin)
		out := gin.H{"username": "tg:" + a.Name, "telegram": true, "role": a.Role}
		if a.Role != "owner" {
			out["nodeIds"] = a.NodeIDs.V
		}
		c.JSON(http.StatusOK, out)
		return
	}
	var a store.Admin
	if err := s.st.DB.First(&a, "id = ?", c.GetString("adminID")).Error; err != nil {
		notFound(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": a.ID, "username": a.Username, "role": normalizedAdminRole(a.Role), "totpEnabled": a.TOTPEnabled, "githubLogin": a.GitHubLogin,
		"lastLoginAt": a.LastLoginAt, "createdAt": a.CreatedAt, "recoveryCodesLeft": len(a.RecoveryCodes.V), "sudo": s.sudoOK(c), "assignedNodeIds": a.NodeIDs.V})
}

func (s *Server) changePassword(c *gin.Context) {
	var req struct {
		Old string `json:"old" binding:"required"`
		New string `json:"new" binding:"required,min=8"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var a store.Admin
	if err := s.st.DB.First(&a, "id = ?", c.GetString("adminID")).Error; err != nil {
		notFound(c)
		return
	}
	if !passwordMatches(a.PasswordHash, req.Old) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "原密码错误"})
		return
	}
	if strings.TrimSpace(req.New) != req.New {
		c.JSON(http.StatusBadRequest, gin.H{"error": "新密码首尾不能有空格"})
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err != nil { // bcrypt takes at most 72 bytes; never store an empty hash
		c.JSON(http.StatusBadRequest, gin.H{"error": "新密码太长（最多 72 字节，约 24 个汉字）"})
		return
	}
	a.TokenVersion++
	// The new password and the end of the other sessions land together.
	if err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&a).Updates(map[string]any{"password_hash": string(h), "token_version": a.TokenVersion}).Error; err != nil {
			return err
		}
		return tx.Model(&store.AdminSession{}).Where("admin_id = ?", a.ID).Update("revoked", true).Error
	}); err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "admin.password", a.Username, "other sessions revoked")
	tok, err := s.issueToken(&a, c, "password") // a fresh session for this browser; the rest are gone
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": tok})
}

// totpSetup generates a pending secret. The current secret (if any) stays
// active until a code from the new one is verified.
func (s *Server) totpSetup(c *gin.Context) {
	var a store.Admin
	if err := s.st.DB.First(&a, "id = ?", c.GetString("adminID")).Error; err != nil {
		notFound(c)
		return
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: s.st.GetSetting("site_title", "Polaris"), AccountName: a.Username})
	if err != nil {
		serverError(c, err)
		return
	}
	if err := s.st.DB.Model(&a).Update("totp_pending", s.st.Box.Seal(key.Secret())).Error; err != nil {
		dbError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"secret": key.Secret(), "url": key.URL()})
}

// totpEnable verifies a code and turns TOTP on (or off with enable=false).
func (s *Server) totpEnable(c *gin.Context) {
	var req struct {
		Code   string `json:"code" binding:"required"`
		Enable bool   `json:"enable"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var a store.Admin
	if err := s.st.DB.First(&a, "id = ?", c.GetString("adminID")).Error; err != nil {
		notFound(c)
		return
	}
	if req.Enable {
		// Turning on (or re-binding) requires a code from the pending secret.
		if a.TOTPPending == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请先生成新的二次验证密钥"})
			return
		}
		if !totp.Validate(req.Code, s.st.Box.MustOpen(a.TOTPPending)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "验证码错误"})
			return
		}
		if err := s.st.DB.Model(&a).Updates(map[string]any{"totp_secret": a.TOTPPending, "totp_pending": "", "totp_enabled": true}).Error; err != nil {
			dbError(c, err)
			return
		}
		codes := s.issueRecoveryCodes(&a)
		s.auditCtx(c, "admin.totp", a.Username, "enabled")
		c.JSON(http.StatusOK, gin.H{"recoveryCodes": codes})
		return
	} else {
		if !a.TOTPEnabled || !totp.Validate(req.Code, s.st.Box.MustOpen(a.TOTPSecret)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "验证码错误"})
			return
		}
		if err := s.st.DB.Model(&a).Updates(map[string]any{"totp_enabled": false, "totp_pending": "", "recovery_codes": store.NewJSON([]string{})}).Error; err != nil {
			dbError(c, err)
			return
		}
	}
	s.auditCtx(c, "admin.totp", a.Username, map[bool]string{true: "enabled", false: "disabled"}[req.Enable])
	c.Status(http.StatusNoContent)
}

// countLoginFailure records a failed sign-in (password, TOTP, or the TOTP step
// of a GitHub sign-in) and locks the account after maxLoginFailures.
// The count is incremented in the database: concurrent guesses each read the
// same stale value, so writing a.FailedAttempts+1 would never lock.
func (s *Server) countLoginFailure(a *store.Admin, now time.Time) {
	s.st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Update("failed_attempts", gorm.Expr("failed_attempts + 1"))
	var n int
	s.st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Select("failed_attempts").Scan(&n)
	a.FailedAttempts = n
	if n >= maxLoginFailures {
		s.st.DB.Model(&store.Admin{}).Where("id = ? AND failed_attempts >= ?", a.ID, maxLoginFailures).
			Updates(map[string]any{"locked_until": now.Add(lockDuration), "failed_attempts": 0})
		a.FailedAttempts = 0
	}
}

func lockedOut(a *store.Admin, now time.Time) bool {
	return a.LockedUntil != nil && now.Before(*a.LockedUntil)
}

// passwordMatches checks a password, then the same text without outer
// whitespace (passwords never start or end with a space, see changePassword).
func passwordMatches(hash, pw string) bool {
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil {
		return true
	}
	t := strings.TrimSpace(pw)
	return t != pw && t != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(t)) == nil
}
