package panel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const passkeyTTL = 2 * time.Minute

type passkeyUser struct {
	admin       store.Admin
	credentials []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                         { return []byte(u.admin.ID) }
func (u *passkeyUser) WebAuthnName() string                       { return u.admin.Username }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.admin.Username }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

type passkeyChallenge struct {
	Kind      string
	AdminID   string
	SID       string
	Version   int
	Name      string
	Origin    string
	Session   webauthn.SessionData
	CreatedAt time.Time
}

func passkeyHash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func reservePasskeys(tx *gorm.DB) error {
	if tx.Dialector.Name() == "postgres" {
		if e := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "polaris-passkeys").Error; e != nil {
			return e
		}
	}
	r := store.Setting{Key: "passkey_guard", Value: "1"}
	if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&r).Error; e != nil {
		return e
	}
	return tx.Model(&store.Setting{}).Where("key = ?", r.Key).UpdateColumn("value", gorm.Expr("value")).Error
}

// Trust only the configured URL, never Host / X-Forwarded-Host headers.
func (s *Server) webAuthn() (*webauthn.WebAuthn, string, error) {
	u, e := url.Parse(s.cfg.PublicURL)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, "", fmt.Errorf("请先配置公开 HTTPS 地址以使用通行密钥")
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if ip != nil {
		return nil, "", fmt.Errorf("通行密钥请使用域名访问；本机开发使用 localhost，不支持 IP 地址")
	}
	local := host == "localhost"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return nil, "", fmt.Errorf("通行密钥需要 HTTPS；本机开发可使用 localhost")
	}
	origin := strings.ToLower(u.Scheme + "://" + u.Host)
	w, e := webauthn.New(&webauthn.Config{RPID: host, RPDisplayName: "Polaris", RPOrigins: []string{origin}, AttestationPreference: protocol.PreferNoAttestation, AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationRequired}, Timeouts: webauthn.TimeoutsConfig{Login: webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyTTL, TimeoutUVD: passkeyTTL}, Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyTTL, TimeoutUVD: passkeyTTL}}})
	return w, origin, e
}
func (s *Server) readPasskey(row store.Passkey) (webauthn.Credential, error) {
	var cred webauthn.Credential
	raw, e := s.st.Box.Open(row.Data)
	if e != nil {
		return cred, e
	}
	e = json.Unmarshal([]byte(raw), &cred)
	if e == nil && passkeyHash(cred.ID) != row.CredentialHash {
		e = errors.New("credential lookup mismatch")
	}
	return cred, e
}
func (s *Server) passkeyUserOn(db *gorm.DB, id, rpid string) (*passkeyUser, error) {
	u := &passkeyUser{}
	if e := db.First(&u.admin, "id = ?", id).Error; e != nil {
		return nil, e
	}
	var rows []store.Passkey
	if e := db.Where("admin_id = ? AND rp_id = ?", id, rpid).Order("created_at").Find(&rows).Error; e != nil {
		return nil, e
	}
	for _, r := range rows {
		c, e := s.readPasskey(r)
		if e != nil {
			return nil, e
		}
		u.credentials = append(u.credentials, c)
	}
	return u, nil
}
func (s *Server) passkeyAccount(c *gin.Context) (*store.Admin, bool) {
	if c.GetString("adminID") == "" || apiTokenOf(c) != nil {
		c.AbortWithStatusJSON(403, gin.H{"error": "请使用管理员账号登录后管理通行密钥"})
		return nil, false
	}
	var a store.Admin
	if e := s.st.DB.First(&a, "id = ?", c.GetString("adminID")).Error; e != nil {
		dbError(c, e)
		return nil, false
	}
	if a.TokenVersion != c.GetInt("tokenVersion") {
		c.AbortWithStatusJSON(401, gin.H{"error": "账号已变化，请重新登录"})
		return nil, false
	}
	return &a, true
}

// Re-check security state under row locks: a request authenticated before a
// concurrent logout/recovery must not register keys or mint new recovery codes.
func (s *Server) lockPasskeySession(tx *gorm.DB, c *gin.Context) error {
	var a store.Admin
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&a, "id = ?", c.GetString("adminID")).Error; e != nil {
		return e
	}
	if a.TokenVersion != c.GetInt("tokenVersion") {
		return errors.New("账号已变化，请重新登录")
	}
	var sess store.AdminSession
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&sess, "id = ? AND admin_id = ? AND revoked = ?", c.GetString("sid"), a.ID, false).Error; e != nil {
		return errors.New("登录会话已失效")
	}
	if sess.ExpiresAt.Before(time.Now()) || sess.SudoUntil == nil || !sess.SudoUntil.After(time.Now()) {
		return errors.New("身份验证已过期，请重新验证")
	}
	return nil
}
func (s *Server) listPasskeys(c *gin.Context) {
	a, ok := s.passkeyAccount(c)
	if !ok {
		return
	}
	rows := []store.Passkey{}
	if e := s.st.DB.Where("admin_id = ?", a.ID).Order("created_at").Find(&rows).Error; e != nil {
		dbError(c, e)
		return
	}
	_, origin, e := s.webAuthn()
	var count int64
	s.st.DB.Model(&store.AccountRecoveryCode{}).Where("admin_id = ?", a.ID).Count(&count)
	out := gin.H{"items": rows, "available": e == nil, "origin": origin, "recoveryCodesLeft": count}
	if e != nil {
		out["reason"] = e.Error()
	}
	c.JSON(200, out)
}
func (s *Server) newPasskeyChallenge(ch passkeyChallenge) (string, error) {
	id := pki.Token(32)
	ch.CreatedAt = time.Now()
	return id, s.eph.put("passkey:"+ch.Kind+":"+id, ch, passkeyTTL)
}
func (s *Server) beginPasskeyRegistration(c *gin.Context) {
	if !s.shared.allow("passkey-register", 10, c.GetString("adminID")) {
		c.JSON(429, gin.H{"error": "请求过于频繁，请稍后重试"})
		return
	}
	a, ok := s.passkeyAccount(c)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if e := c.ShouldBindJSON(&req); e != nil {
		badRequest(c, e)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 64 {
		badRequest(c, errors.New("请输入通行密钥名称（最多 64 字节）"))
		return
	}
	w, origin, e := s.webAuthn()
	if e != nil {
		badRequest(c, e)
		return
	}
	u, e := s.passkeyUserOn(s.st.DB, a.ID, w.Config.RPID)
	if e != nil {
		serverError(c, e)
		return
	}
	if len(u.credentials) >= 20 {
		badRequest(c, errors.New("每个账号最多注册 20 个通行密钥"))
		return
	}
	opts, session, e := w.BeginRegistration(u)
	if e != nil {
		serverError(c, e)
		return
	}
	id, e := s.newPasskeyChallenge(passkeyChallenge{Kind: "register", AdminID: a.ID, SID: c.GetString("sid"), Version: a.TokenVersion, Name: req.Name, Origin: origin, Session: *session})
	if e != nil {
		serverError(c, e)
		return
	}
	c.JSON(200, gin.H{"challengeId": id, "options": opts})
}

type finishPasskeyRequest struct {
	ChallengeID string          `json:"challengeId" binding:"required"`
	Credential  json.RawMessage `json:"credential" binding:"required"`
}

func (s *Server) takePasskeyChallenge(c *gin.Context, kind string) (finishPasskeyRequest, passkeyChallenge, bool) {
	var req finishPasskeyRequest
	var ch passkeyChallenge
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if e := c.ShouldBindJSON(&req); e != nil {
		badRequest(c, e)
		return req, ch, false
	}
	if len(req.ChallengeID) > 128 || !s.eph.take("passkey:"+kind+":"+req.ChallengeID, &ch) || ch.Kind != kind || time.Since(ch.CreatedAt) > passkeyTTL {
		c.JSON(400, gin.H{"error": "挑战已使用或已过期，请重试"})
		return req, ch, false
	}
	return req, ch, true
}
func (s *Server) finishPasskeyRegistration(c *gin.Context) {
	a, ok := s.passkeyAccount(c)
	if !ok {
		return
	}
	req, ch, ok := s.takePasskeyChallenge(c, "register")
	if !ok {
		return
	}
	w, origin, e := s.webAuthn()
	if e != nil || ch.Origin != origin || ch.AdminID != a.ID || ch.SID != c.GetString("sid") || ch.Version != a.TokenVersion {
		badRequest(c, errors.New("注册会话已变化，请重新注册"))
		return
	}
	parsed, e := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(req.Credential))
	if e != nil {
		badRequest(c, errors.New("无效的通行密钥响应"))
		return
	}
	row := store.Passkey{ID: store.NewID(), AdminID: a.ID, Name: ch.Name, RPID: w.Config.RPID}
	codes := []string{}
	e = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := reservePasskeys(tx); e != nil {
			return e
		}
		if e := s.lockPasskeySession(tx, c); e != nil {
			return e
		}
		u, e := s.passkeyUserOn(tx, a.ID, w.Config.RPID)
		if e != nil {
			return e
		}
		if u.admin.TokenVersion != ch.Version || len(u.credentials) >= 20 {
			return errors.New("账号已变化或密钥数量已达上限")
		}
		cred, e := w.CreateCredential(u, ch.Session, parsed)
		if e != nil {
			return errors.New("通行密钥验证失败，请重新注册")
		}
		raw, e := json.Marshal(cred)
		if e != nil {
			return e
		}
		row.CredentialHash, row.Data = passkeyHash(cred.ID), s.st.Box.Seal(string(raw))
		if e = tx.Create(&row).Error; e != nil {
			return e
		}
		var n int64
		if e = tx.Model(&store.AccountRecoveryCode{}).Where("admin_id = ?", a.ID).Count(&n).Error; e != nil {
			return e
		}
		if n == 0 {
			codes, e = s.makeAccountRecoveryCodes(tx, a.ID)
		}
		return e
	})
	if e != nil {
		badRequest(c, e)
		return
	}
	s.auditCtx(c, "passkey.register", row.ID, row.Name)
	c.JSON(201, gin.H{"passkey": row, "recoveryCodes": codes})
}
func (s *Server) beginPasskeyLogin(c *gin.Context) {
	w, origin, e := s.webAuthn()
	if e != nil {
		badRequest(c, e)
		return
	}
	if !s.shared.allow("passkey-login", 10, clientIP(c.Request)) {
		c.JSON(429, gin.H{"error": "请求过于频繁，请稍后重试"})
		return
	}
	opts, session, e := w.BeginDiscoverableLogin()
	if e != nil {
		serverError(c, e)
		return
	}
	id, e := s.newPasskeyChallenge(passkeyChallenge{Kind: "login", Origin: origin, Session: *session})
	if e != nil {
		serverError(c, e)
		return
	}
	c.JSON(200, gin.H{"challengeId": id, "options": opts})
}
func (s *Server) finishPasskeyLogin(c *gin.Context) {
	if !s.shared.allow("passkey-finish", 20, clientIP(c.Request)) {
		c.JSON(429, gin.H{"error": "请求过于频繁，请稍后重试"})
		return
	}
	req, ch, ok := s.takePasskeyChallenge(c, "login")
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
	var a store.Admin
	var keyID string
	e = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := reservePasskeys(tx); e != nil {
			return e
		}
		handler := func(rawID, handle []byte) (webauthn.User, error) {
			var row store.Passkey
			if e := tx.First(&row, "credential_hash = ? AND rp_id = ?", passkeyHash(rawID), w.Config.RPID).Error; e != nil {
				return nil, e
			}
			if !bytes.Equal(handle, []byte(row.AdminID)) {
				return nil, errors.New("user handle mismatch")
			}
			u, e := s.passkeyUserOn(tx, row.AdminID, w.Config.RPID)
			if e != nil {
				return nil, e
			}
			if u.admin.LockedUntil != nil && u.admin.LockedUntil.After(time.Now()) {
				return nil, errors.New("account locked")
			}
			a, keyID = u.admin, row.ID
			return u, nil
		}
		_, cred, e := w.ValidatePasskeyLogin(handler, ch.Session, parsed)
		if e != nil {
			return errors.New("通行密钥验证失败")
		}
		if cred.Authenticator.CloneWarning {
			return errors.New("通行密钥计数异常，请使用其他方式登录并检查此密钥")
		}
		raw, e := json.Marshal(cred)
		if e != nil {
			return e
		}
		now := time.Now()
		if e = tx.Model(&store.Passkey{}).Where("id = ?", keyID).Updates(map[string]any{"data": s.st.Box.Seal(string(raw)), "last_used_at": now}).Error; e != nil {
			return e
		}
		return tx.Model(&store.Admin{}).Where("id = ?", a.ID).Updates(map[string]any{"failed_attempts": 0, "locked_until": nil, "last_login_at": now}).Error
	})
	if e != nil {
		s.recordIPFailure(clientIP(c.Request), "通行密钥登录失败")
		c.JSON(401, gin.H{"error": "通行密钥验证失败，请重试或使用其他登录方式"})
		return
	}
	tok, e := s.issueToken(&a, c, "passkey")
	if e != nil {
		serverError(c, e)
		return
	}
	claims, e := s.parseToken(tok)
	if e != nil {
		serverError(c, e)
		return
	}
	e = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := reservePasskeys(tx); e != nil {
			return e
		}
		var current store.Admin
		var key store.Passkey
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", a.ID).Error; e != nil {
			return e
		}
		if current.TokenVersion != a.TokenVersion {
			return errors.New("account changed")
		}
		if e := tx.First(&key, "id = ? AND admin_id = ?", keyID, a.ID).Error; e != nil {
			return e
		}
		r := tx.Model(&store.AdminSession{}).Where("id = ? AND revoked = ?", claims.SID, false).Update("passkey_id", keyID)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return errors.New("session revoked")
		}
		return nil
	})
	if e != nil {
		s.st.DB.Model(&store.AdminSession{}).Where("id = ?", claims.SID).Update("revoked", true)
		c.JSON(401, gin.H{"error": "账号或密钥已撤销，请重新登录"})
		return
	}
	s.audit(a.Username, "admin.login", a.Username, "passkey", clientIP(c.Request))
	c.JSON(200, gin.H{"token": tok, "username": a.Username, "expiresIn": int(tokenTTL.Seconds())})
}
func (s *Server) revokePasskey(c *gin.Context) {
	a, ok := s.passkeyAccount(c)
	if !ok {
		return
	}
	e := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := reservePasskeys(tx); e != nil {
			return e
		}
		if e := s.lockPasskeySession(tx, c); e != nil {
			return e
		}
		var row store.Passkey
		if e := tx.First(&row, "id = ? AND admin_id = ?", c.Param("pid"), a.ID).Error; e != nil {
			return e
		}
		if e := tx.Model(&store.AdminSession{}).Where("passkey_id = ?", row.ID).Update("revoked", true).Error; e != nil {
			return e
		}
		return tx.Delete(&row).Error
	})
	if e != nil {
		dbError(c, e)
		return
	}
	s.auditCtx(c, "passkey.revoke", c.Param("pid"), "")
	c.Status(204)
}
func (s *Server) makeAccountRecoveryCodes(tx *gorm.DB, adminID string) ([]string, error) {
	if e := tx.Delete(&store.AccountRecoveryCode{}, "admin_id = ?", adminID).Error; e != nil {
		return nil, e
	}
	codes := []string{}
	for i := 0; i < 10; i++ {
		code := pki.Token(24)
		hash := passkeyHash([]byte(adminID + ":" + code))
		if e := tx.Create(&store.AccountRecoveryCode{Hash: hash, AdminID: adminID}).Error; e != nil {
			return nil, e
		}
		codes = append(codes, code)
	}
	return codes, nil
}
func (s *Server) regenerateAccountRecovery(c *gin.Context) {
	a, ok := s.passkeyAccount(c)
	if !ok {
		return
	}
	var codes []string
	e := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := reservePasskeys(tx); e != nil {
			return e
		}
		if e := s.lockPasskeySession(tx, c); e != nil {
			return e
		}
		var e error
		codes, e = s.makeAccountRecoveryCodes(tx, a.ID)
		return e
	})
	if e != nil {
		serverError(c, e)
		return
	}
	s.auditCtx(c, "account.recovery_codes", a.ID, "")
	c.JSON(200, gin.H{"recoveryCodes": codes})
}
func (s *Server) recoverPasskeyAccount(c *gin.Context) {
	if !s.shared.allow("passkey-recovery", 5, clientIP(c.Request)) {
		c.JSON(429, gin.H{"error": "请求过于频繁，请稍后重试"})
		return
	}
	var req struct {
		Username string `json:"username" binding:"required"`
		Code     string `json:"code" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if e := c.ShouldBindJSON(&req); e != nil {
		badRequest(c, e)
		return
	}
	if len(req.Password) < 8 || len(req.Password) > 72 || strings.TrimSpace(req.Password) != req.Password {
		badRequest(c, errors.New("新密码需为 8–72 字节，首尾不能有空格"))
		return
	}
	if _, _, e := s.webAuthn(); e != nil {
		badRequest(c, e)
		return
	}
	var a store.Admin
	if e := s.st.DB.Where("username = ?", strings.TrimSpace(req.Username)).First(&a).Error; e != nil {
		s.st.DB.Where("LOWER(username) = LOWER(?)", strings.TrimSpace(req.Username)).First(&a)
	}
	hash := passkeyHash([]byte(a.ID + ":" + strings.TrimSpace(req.Code)))
	password, e := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if e != nil {
		serverError(c, e)
		return
	}
	e = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := reservePasskeys(tx); e != nil {
			return e
		}
		var row store.AccountRecoveryCode
		if a.ID == "" {
			return errors.New("invalid recovery")
		}
		if e := tx.First(&row, "hash = ? AND admin_id = ?", hash, a.ID).Error; e != nil {
			return e
		}
		if e := tx.Delete(&store.AccountRecoveryCode{}, "admin_id = ?", a.ID).Error; e != nil {
			return e
		}
		if e := tx.Delete(&store.Passkey{}, "admin_id = ?", a.ID).Error; e != nil {
			return e
		}
		if e := tx.Model(&store.AdminSession{}).Where("admin_id = ?", a.ID).Update("revoked", true).Error; e != nil {
			return e
		}
		return tx.Model(&store.Admin{}).Where("id = ?", a.ID).Updates(map[string]any{"password_hash": string(password), "token_version": gorm.Expr("token_version + 1"), "totp_enabled": false, "totp_secret": "", "totp_pending": "", "recovery_codes": store.NewJSON([]string{}), "failed_attempts": 0, "locked_until": nil}).Error
	})
	if e != nil {
		s.recordIPFailure(clientIP(c.Request), "账号恢复失败")
		c.JSON(401, gin.H{"error": "账号或恢复码无效"})
		return
	}
	s.audit(a.Username, "account.recover", a.Username, "passkeys and sessions revoked", clientIP(c.Request))
	c.JSON(200, gin.H{"ok": true})
}

func (s *Server) passkeysAvailable() bool { _, _, e := s.webAuthn(); return e == nil }
