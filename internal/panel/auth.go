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
}

func (s *Server) issueToken(a *store.Admin) (string, error) {
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   a.ID,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(tokenTTL)),
		},
		Username: a.Username,
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

// authRequired validates the bearer token (or ?token= for WebSocket).
func (s *Server) authRequired(c *gin.Context) {
	tok := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if tok == "" {
		tok = c.Query("token")
	}
	cl, err := s.parseToken(tok)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.Set("admin", cl.Username)
	c.Set("adminID", cl.Subject)
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
	var a store.Admin
	if err := s.st.DB.First(&a, "username = ?", req.Username).Error; err != nil {
		// Same cost as a real check to avoid username probing by timing.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(req.Password))
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
		return
	}
	now := time.Now()
	if a.LockedUntil != nil && now.Before(*a.LockedUntil) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登录失败次数过多，请 15 分钟后再试", "lockedUntil": a.LockedUntil})
		return
	}
	fail := func(msg string) {
		a.FailedAttempts++
		upd := map[string]any{"failed_attempts": a.FailedAttempts}
		if a.FailedAttempts >= maxLoginFailures {
			t := now.Add(lockDuration)
			upd["locked_until"] = t
			upd["failed_attempts"] = 0
		}
		s.st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Updates(upd)
		s.audit(a.Username, "admin.login_failed", a.Username, msg, clientIP(c.Request))
		c.JSON(http.StatusUnauthorized, gin.H{"error": msg})
	}
	if bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(req.Password)) != nil {
		fail("用户名或密码错误")
		return
	}
	if a.TOTPEnabled {
		if req.TOTP == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "需要二次验证码", "totpRequired": true})
			return
		}
		if !totp.Validate(req.TOTP, s.st.Box.MustOpen(a.TOTPSecret)) {
			fail("二次验证码错误")
			return
		}
	}
	s.st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Updates(map[string]any{"failed_attempts": 0, "locked_until": nil, "last_login_at": now})
	tok, err := s.issueToken(&a)
	if err != nil {
		serverError(c, err)
		return
	}
	s.audit(a.Username, "admin.login", a.Username, "", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"token": tok, "username": a.Username, "expiresIn": int(tokenTTL.Seconds())})
}

func (s *Server) me(c *gin.Context) {
	var a store.Admin
	if err := s.st.DB.First(&a, "id = ?", c.GetString("adminID")).Error; err != nil {
		notFound(c)
		return
	}
	c.JSON(http.StatusOK, a)
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
	if bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(req.Old)) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "原密码错误"})
		return
	}
	h, _ := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	s.st.DB.Model(&a).Update("password_hash", string(h))
	s.auditCtx(c, "admin.password", a.Username, "")
	c.Status(http.StatusNoContent)
}

// totpSetup generates a new secret (not yet enabled).
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
	s.st.DB.Model(&a).Updates(map[string]any{"totp_secret": s.st.Box.Seal(key.Secret()), "totp_enabled": false})
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
	if !totp.Validate(req.Code, s.st.Box.MustOpen(a.TOTPSecret)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "验证码错误"})
		return
	}
	s.st.DB.Model(&a).Update("totp_enabled", req.Enable)
	s.auditCtx(c, "admin.totp", a.Username, map[bool]string{true: "enabled", false: "disabled"}[req.Enable])
	c.Status(http.StatusNoContent)
}
