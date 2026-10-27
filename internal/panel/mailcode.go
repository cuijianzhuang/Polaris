package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/mailer"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Sign-in by e-mail: with SMTP configured, /u offers "e-mail me a code".
// A 6-digit code (kept hashed in the shared ephemeral store, 10 minutes, 5
// tries) signs the user in; with portal_email_register on, an address with
// no account gets a new one (a trial plan, or nothing until they buy).
// Signed-in users can bind or change their address the same way. Answers
// never say whether an address has an account.

const (
	mailCodeTTL      = 10 * time.Minute
	mailCodeTries    = 5
	mailCodeCooldown = 60 * time.Second
	mailCodesPerDay  = 10
)

type mailCode struct {
	Hash   string    `json:"h"`
	Tries  int       `json:"t"`
	Sent   time.Time `json:"s"`
	UserID string    `json:"u,omitempty"` // bind: the signed-in user
}

func mailFP(addr string) string {
	if addr == "" {
		return ""
	}
	h := sha256.Sum256([]byte("mail:" + addr))
	return hex.EncodeToString(h[:8])
}

func mailKey(purpose, addr string) string {
	h := sha256.Sum256([]byte(addr))
	return "mailcode:" + purpose + ":" + hex.EncodeToString(h[:16])
}

func codeHash(addr, code string) string {
	h := sha256.Sum256([]byte(addr + "\x00" + code))
	return hex.EncodeToString(h[:])
}

func (s *Server) mailLoginOn() bool { return s.mailOn() }

func (s *Server) mailRegisterOn() bool {
	return s.mailOn() && s.st.GetSetting("portal_email_register", "false") == "true"
}

// registerDomainOK applies register_email_domains (comma separated, empty =
// any).
func (s *Server) registerDomainOK(addr string) bool {
	list := strings.TrimSpace(s.st.GetSetting("register_email_domains", ""))
	if list == "" {
		return true
	}
	domain := addr[strings.LastIndex(addr, "@")+1:]
	return slices.ContainsFunc(strings.FieldsFunc(strings.ToLower(list), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }), func(d string) bool {
		d = strings.TrimPrefix(d, "@")
		return domain == d
	})
}

// countDaily counts one use of key within 24 hours and reports whether it
// stays within limit.
func (s *Server) countDaily(key string, limit int) bool {
	return s.countWindow(key, limit, 24*time.Hour)
}

// countWindow counts one use of key in a fixed window that starts at its
// first use; false once limit is reached.
func (s *Server) countWindow(key string, limit int, window time.Duration) bool {
	var n int
	ok := false
	if s.eph.update(key, &n, func() ephAction {
		if n >= limit {
			return ephKeep
		}
		n++
		ok = true
		return ephSave
	}) {
		return ok
	}
	return limit > 0 && s.eph.put(key, 1, window) == nil
}

func randomCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

// portalMailCode: POST /u/email/code {email, purpose: login|bind}.
func (s *Server) portalMailCode(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if !s.mailLoginOn() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "邮箱登录暂未开放"})
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
	purpose, userID := "login", ""
	if req.Purpose == "bind" {
		u := s.portalUser(c)
		if u == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
			return
		}
		if _, err := checkUserEmail(s.st.DB, addr, u.ID); err != nil {
			badRequest(c, err)
			return
		}
		purpose, userID = "bind:"+u.ID, u.ID
	}
	key := mailKey(purpose, addr)
	var prev mailCode
	if s.eph.get(key, &prev) && time.Since(prev.Sent) < mailCodeCooldown {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": fmt.Sprintf("验证码已发送，请 %d 秒后再试", int((mailCodeCooldown-time.Since(prev.Sent)).Seconds())+1)})
		return
	}
	// Who may get a login code: an existing account, or anyone when sign-up
	// is open (and the domain allowed). Others get the same answer and no
	// mail, so the form does not reveal who has an account.
	send := userID != ""
	if !send {
		var n int64
		s.st.DB.Model(&store.User{}).Where("email = ? AND is_service = ?", addr, false).Count(&n)
		send = n > 0 || (s.mailRegisterOn() && s.registerDomainOK(addr))
	}
	res := gin.H{"ok": true, "ttl": int(mailCodeTTL.Seconds()), "cooldown": int(mailCodeCooldown.Seconds())}
	// The cooldown and the daily cap apply whether or not a mail goes out:
	// a 429 only for real accounts would tell them apart.
	h := sha256.Sum256([]byte(addr))
	if !s.countDaily("maildaily:"+hex.EncodeToString(h[:16]), mailCodesPerDay) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "这个邮箱今天收到的验证码太多了，请明天再试"})
		return
	}
	if !send {
		// A placeholder no code matches, so verifying answers alike too.
		if err := s.eph.put(key, mailCode{Hash: "none", Sent: time.Now()}, mailCodeTTL); err != nil {
			serverError(c, err)
			return
		}
		c.JSON(http.StatusOK, res)
		return
	}
	code := randomCode()
	if err := s.eph.put(key, mailCode{Hash: codeHash(addr, code), Sent: time.Now(), UserID: userID}, mailCodeTTL); err != nil {
		serverError(c, err)
		return
	}
	title := "登录验证码"
	if userID != "" {
		title = "绑定邮箱验证码"
	}
	plain, body := s.mailCodeBody(title, code)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if err := s.sendMail(ctx, addr, title+" "+code, plain, body); err != nil {
		var gone mailCode
		s.eph.take(key, &gone)
		s.log.Warn("login code mail", "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "邮件发送失败，请稍后再试或联系管理员"})
		return
	}
	c.JSON(http.StatusOK, res)
}

// checkMailCode spends one try of the code for addr; true when it matches
// (the code is then used up).
func (s *Server) checkMailCode(purpose, addr, code string) error {
	code = strings.TrimSpace(code)
	key := mailKey(purpose, addr)
	var mc mailCode
	ok := false
	found := s.eph.update(key, &mc, func() ephAction {
		if subtle.ConstantTimeCompare([]byte(mc.Hash), []byte(codeHash(addr, code))) == 1 {
			ok = true
			return ephDelete
		}
		mc.Tries++
		if mc.Tries >= mailCodeTries {
			return ephDelete
		}
		return ephSave
	})
	switch {
	case !found:
		return errors.New("验证码已失效，请重新获取")
	case !ok && mc.Tries >= mailCodeTries:
		return errors.New("验证码错误次数太多，请重新获取")
	case !ok:
		return fmt.Errorf("验证码不正确（还能试 %d 次）", mailCodeTries-mc.Tries)
	}
	return nil
}

// portalMailVerify: POST /u/email/verify {email, code, purpose}. Login
// signs in (or registers); bind sets the signed-in user's address.
func (s *Server) portalMailVerify(c *gin.Context) {
	if !portalWrite(c) {
		return
	}
	if !s.mailLoginOn() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "邮箱登录暂未开放"})
		return
	}
	var req struct {
		Email   string `json:"email"`
		Code    string `json:"code"`
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
	if req.Purpose == "bind" {
		s.portalMailBind(c, addr, req.Code)
		return
	}
	if err := s.checkMailCode("login", addr, req.Code); err != nil {
		badRequest(c, err)
		return
	}
	var u store.User
	created := false
	if s.st.DB.First(&u, "email = ? AND is_service = ?", addr, false).Error != nil {
		if !s.mailRegisterOn() || !s.registerDomainOK(addr) {
			badRequest(c, errors.New("这个邮箱没有绑定账号"))
			return
		}
		nu, err := s.registerByMail(addr, clientIP(c.Request))
		if err != nil {
			badRequest(c, err)
			return
		}
		u, created = *nu, true
	}
	tok, err := s.issuePortalMailToken(&u)
	if err != nil {
		serverError(c, err)
		return
	}
	s.setPortalCookie(c, tok, int(portalSessionTTL.Seconds()))
	s.audit("mail:"+u.Username, "user.portal_login", u.Username, "email", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"ok": true, "created": created})
}

func (s *Server) portalMailBind(c *gin.Context, addr, code string) {
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return
	}
	if err := s.checkMailCode("bind:"+u.ID, addr, code); err != nil {
		badRequest(c, err)
		return
	}
	if _, err := checkUserEmail(s.st.DB, addr, u.ID); err != nil {
		badRequest(c, err)
		return
	}
	viaMail := s.portalViaMail(c)
	if err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(u).Update("email", addr).Error; err != nil {
			return err
		}
		u.Email = addr
		return s.queueUserEvent(tx, "user.updated", u, map[string]any{"via": "portal", "email": addr})
	}); err != nil {
		serverError(c, err)
		return
	}
	u.Email = addr
	s.audit(portalActor(u, viaMail), "user.email_bound", u.Username, addr, clientIP(c.Request))
	s.wakeBusinessEvents()
	// A session made from the old address would end: renew it.
	tok, err := s.issuePortalTokenVia(u, viaMail)
	if err != nil {
		serverError(c, err)
		return
	}
	s.setPortalCookie(c, tok, int(portalSessionTTL.Seconds()))
	c.JSON(http.StatusOK, gin.H{"ok": true, "email": addr})
}

// registerByMail creates the account of a new e-mail user: a trial plan
// when register_trial_plan is set, otherwise expired until they buy.
func (s *Server) registerByMail(addr, ip string) (*store.User, error) {
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
	local := strings.ToLower(addr[:strings.Index(addr, "@")])
	local = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return -1
	}, local)
	if len(local) > 16 {
		local = local[:16]
	}
	if local == "" {
		local = "user"
	}
	if s.userCapFull() {
		return nil, errUserCap
	}
	trialID := strings.TrimSpace(s.st.GetSetting("register_trial_plan", ""))
	now := time.Now()
	var u store.User
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var n int64
		tx.Model(&store.User{}).Where("email = ?", addr).Count(&n)
		if n > 0 {
			return errors.New("这个邮箱已经注册")
		}
		u = newUser(&userReq{Username: local + "_" + strings.ToLower(pki.Token(4)), Remark: "邮箱注册"})
		u.Email, u.SelfService = addr, true
		if trialID != "" {
			p, err := loadPlan(tx, trialID)
			if err != nil {
				return errors.New("试用套餐不存在，请联系管理员")
			}
			if err := tx.Create(&u).Error; err != nil {
				return err
			}
			if _, err := applyPlan(tx, &u, p, false, now); err != nil {
				return err
			}
			u.Status = computeStatus(&u, now)
			if err := tx.Model(&store.User{}).Where("id = ?", u.ID).Update("status", u.Status).Error; err != nil {
				return err
			}
			return s.queueUserEvent(tx, "user.registered", &u, map[string]any{"via": "email", "trial": trialID})
		}
		// No trial: no service until a plan is bought.
		gone := now.Add(-time.Second)
		u.ExpireAt = &gone
		u.Status = computeStatus(&u, now)
		if err := tx.Create(&u).Error; err != nil {
			return err
		}
		return s.queueUserEvent(tx, "user.registered", &u, map[string]any{"via": "email", "trial": trialID})
	})
	if err != nil {
		return nil, err
	}
	s.MarkDirty("user registered", s.nodesForUsers(u.ID)...)
	s.audit("mail:"+u.Username, "user.register", u.Username, addr, ip)
	s.wakeBusinessEvents()
	s.notify(Event{Kind: "user_registered", Text: fmt.Sprintf("🆕 新用户通过邮箱注册：%s（%s）", u.Username, addr)})
	return &u, nil
}
