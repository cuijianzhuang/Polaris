package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Sign-in hardening:
//   - per-IP failure counting with bans that grow each time (persisted, so a
//     restart does not reset them), never applied to the login allowlist;
//   - server-side sessions: every token names a session that can be listed
//     and revoked, and dies with it;
//   - sudo: sensitive actions need a re-verification (password, or TOTP when
//     enabled) in the last few minutes;
//   - TOTP recovery codes;
//   - an alert when an admin signs in from an address never used before.

const (
	ipFailWindow  = 15 * time.Minute
	ipBanBase     = 15 * time.Minute
	ipBanMax      = 7 * 24 * time.Hour
	sudoTTL       = 10 * time.Minute
	recoveryCount = 10
)

// --- per-IP bans ---

func (s *Server) ipBanThreshold() int {
	n, err := strconv.Atoi(s.st.GetSetting("login_ban_failures", "10"))
	if err != nil || n < 3 {
		return 10
	}
	return n
}

// loginAllowlist parses the login_allowlist setting (IPs / CIDRs).
func (s *Server) loginAllowlist() []*net.IPNet {
	var out []*net.IPNet
	for _, f := range strings.FieldsFunc(s.st.GetSetting("login_allowlist", ""), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == ';' }) {
		if !strings.Contains(f, "/") {
			if ip := net.ParseIP(f); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				f = fmt.Sprintf("%s/%d", f, bits)
			}
		}
		if _, n, err := net.ParseCIDR(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) ipAllowlisted(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range s.loginAllowlist() {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

// ipBannedUntil reports an active ban on ip.
func (s *Server) ipBannedUntil(ip string) (time.Time, bool) {
	if s.ipAllowlisted(ip) {
		return time.Time{}, false
	}
	var b store.LoginBan
	if s.st.DB.First(&b, "ip = ?", ip).Error != nil || b.Until == nil || time.Now().After(*b.Until) {
		return time.Time{}, false
	}
	return *b.Until, true
}

// recordIPFailure counts a failed sign-in from ip and bans it past the
// threshold: 15 minutes, doubling with every further ban, up to 7 days.
func (s *Server) recordIPFailure(ip, reason string) {
	if ip == "" || s.ipAllowlisted(ip) {
		return
	}
	now := time.Now()
	var b store.LoginBan
	if s.st.DB.First(&b, "ip = ?", ip).Error != nil {
		b = store.LoginBan{IP: ip}
	}
	if now.Sub(b.WindowAt) > ipFailWindow {
		b.Failures, b.WindowAt = 0, now
	}
	b.Failures++
	b.Reason, b.UpdatedAt = reason, now
	if b.Failures >= s.ipBanThreshold() {
		d := ipBanBase << min(b.Bans, 10)
		if d > ipBanMax || d <= 0 {
			d = ipBanMax
		}
		until := now.Add(d)
		b.Until, b.Bans, b.Failures = &until, b.Bans+1, 0
		s.audit("system", "security.ip_ban", ip, fmt.Sprintf("%s，封禁 %s（第 %d 次）", reason, d, b.Bans), ip)
		s.notify(Event{Kind: "login_ban", Text: fmt.Sprintf("🚫 IP %s 登录失败次数过多（%s），已封禁 %s（第 %d 次）", ip, reason, humanDuration(d), b.Bans)})
	}
	s.st.DB.Save(&b)
}

// clearIPFailures resets the failure count after a successful sign-in (the
// ban history stays, so the next ban is still longer).
func (s *Server) clearIPFailures(ip string) {
	s.st.DB.Model(&store.LoginBan{}).Where("ip = ?", ip).Update("failures", 0)
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%g 天", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%g 小时", d.Hours())
	default:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
}

// loginGuard refuses the public sign-in endpoints to banned IPs.
func (s *Server) loginGuard(c *gin.Context) {
	if until, banned := s.ipBannedUntil(clientIP(c.Request)); banned {
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"error": "此 IP 登录失败次数过多，已暂时封禁至 " + until.Local().Format("01-02 15:04"), "bannedUntil": until})
		return
	}
	c.Next()
}

// --- sessions ---

// newSession records a signed-in browser and returns its id.
func (s *Server) newSession(c *gin.Context, adminID, username, via string, ttl time.Duration) string {
	now := time.Now()
	ip := clientIP(c.Request)
	sess := store.AdminSession{ID: store.NewID(), AdminID: adminID, Username: username, Via: via, IP: ip,
		UserAgent: truncate(c.Request.UserAgent(), 250), CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(ttl)}
	var seen, known int64
	s.st.DB.Model(&store.AdminSession{}).Where("admin_id = ?", adminID).Count(&seen)
	s.st.DB.Model(&store.AdminSession{}).Where("admin_id = ? AND ip = ?", adminID, ip).Count(&known)
	s.st.DB.Create(&sess)
	s.clearIPFailures(ip)
	if seen > 0 && known == 0 {
		s.notify(Event{Kind: "admin_login_new_ip", Text: fmt.Sprintf("🔐 管理员 %s 从新地址登录：%s · %s（%s）。如果不是你本人，请在「设置 → 账号安全」结束该会话并修改密码。",
			username, ip, shortUA(c.Request.UserAgent()), via)})
	}
	return sess.ID
}

// sessionAlive checks the session a token names, refreshing its last-seen
// time at most once a minute.
func (s *Server) sessionAlive(sid string) (*store.AdminSession, bool) {
	var sess store.AdminSession
	if sid == "" || s.st.DB.First(&sess, "id = ?", sid).Error != nil || sess.Revoked || time.Now().After(sess.ExpiresAt) {
		return nil, false
	}
	if sess.Via == "passkey" && sess.PasskeyID == "" {
		return nil, false
	}
	if time.Since(sess.LastSeen) > time.Minute {
		s.st.DB.Model(&store.AdminSession{}).Where("id = ?", sid).Update("last_seen", time.Now())
	}
	return &sess, true
}

// sessionOwner is the AdminSession.AdminID of the caller.
func sessionOwner(c *gin.Context) string {
	if v, ok := c.Get("tgAdmin"); ok {
		return "tg:" + v.(*store.TGAdmin).ID
	}
	return c.GetString("adminID")
}

// seesAllSessions: the panel admin and Telegram owners manage every session;
// operators only their own.
func seesAllSessions(c *gin.Context) bool {
	if v, ok := c.Get("tgAdmin"); ok {
		return v.(*store.TGAdmin).Role == "owner"
	}
	return c.GetString("adminRole") == "owner"
}

type sessionView struct {
	store.AdminSession
	Device  string `json:"device"`
	Current bool   `json:"current"`
}

// listSessions: GET /auth/sessions — active sessions.
func (s *Server) listSessions(c *gin.Context) {
	q := s.st.DB.Where("revoked = ? AND expires_at > ?", false, time.Now()).Order("last_seen DESC")
	if !seesAllSessions(c) {
		q = q.Where("admin_id = ?", sessionOwner(c))
	}
	var ss []store.AdminSession
	q.Find(&ss)
	out := make([]sessionView, 0, len(ss))
	for _, x := range ss {
		out = append(out, sessionView{AdminSession: x, Device: shortUA(x.UserAgent), Current: x.ID == c.GetString("sid")})
	}
	c.JSON(http.StatusOK, out)
}

// revokeSession: DELETE /auth/sessions/:id.
func (s *Server) revokeSession(c *gin.Context) {
	q := s.st.DB.Model(&store.AdminSession{}).Where("id = ?", c.Param("id"))
	if !seesAllSessions(c) {
		q = q.Where("admin_id = ?", sessionOwner(c))
	}
	if q.Update("revoked", true).RowsAffected == 0 {
		notFound(c)
		return
	}
	s.auditCtx(c, "admin.session_revoke", c.Param("id"), "")
	c.Status(http.StatusNoContent)
}

// revokeOtherSessions: POST /auth/sessions/revoke-others — every session but
// the caller's (all admins' for the panel admin / owners).
func (s *Server) revokeOtherSessions(c *gin.Context) {
	q := s.st.DB.Model(&store.AdminSession{}).Where("id <> ? AND revoked = ?", c.GetString("sid"), false)
	if !seesAllSessions(c) {
		q = q.Where("admin_id = ?", sessionOwner(c))
	}
	n := q.Update("revoked", true).RowsAffected
	s.auditCtx(c, "admin.session_revoke", "others", fmt.Sprintf("%d sessions", n))
	c.JSON(http.StatusOK, gin.H{"revoked": n})
}

// logout: POST /auth/logout — ends the caller's session.
func (s *Server) logout(c *gin.Context) {
	s.st.DB.Model(&store.AdminSession{}).Where("id = ?", c.GetString("sid")).Update("revoked", true)
	c.Status(http.StatusNoContent)
}

// --- sudo ---

// sudoOK reports a recent re-verification on the caller's session. Telegram
// sessions are confirmed on a bound device at sign-in and have no password,
// so they pass.
func (s *Server) sudoOK(c *gin.Context) bool {
	if _, ok := c.Get("tgAdmin"); ok {
		return true
	}
	var sess store.AdminSession
	if s.st.DB.Select("sudo_until").First(&sess, "id = ?", c.GetString("sid")).Error != nil {
		return false
	}
	return sess.SudoUntil != nil && time.Now().Before(*sess.SudoUntil)
}

func sudoRequired(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "敏感操作需要重新验证身份", "sudoRequired": true})
}

// requireSudo guards sensitive routes.
func (s *Server) requireSudo(c *gin.Context) {
	if !s.sudoOK(c) {
		sudoRequired(c)
		return
	}
	c.Next()
}

// sudo: POST /auth/sudo {password, code} — the password, or with TOTP on, a
// TOTP / recovery code; unlocks sensitive actions for sudoTTL.
func (s *Server) sudo(c *gin.Context) {
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if _, ok := c.Get("tgAdmin"); ok {
		c.JSON(http.StatusOK, gin.H{"until": time.Now().Add(sudoTTL)})
		return
	}
	ip := clientIP(c.Request)
	if until, banned := s.ipBannedUntil(ip); banned {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "失败次数过多，已暂时封禁至 " + until.Local().Format("01-02 15:04")})
		return
	}
	var a store.Admin
	if err := s.st.DB.First(&a, "id = ?", c.GetString("adminID")).Error; err != nil {
		notFound(c)
		return
	}
	ok := false
	if a.TOTPEnabled {
		ok = s.secondFactor(&a, req.Code, ip)
	} else {
		ok = passwordMatches(a.PasswordHash, req.Password)
	}
	if !ok {
		s.recordIPFailure(ip, "重新验证失败")
		s.audit(a.Username, "admin.sudo_failed", a.Username, "", ip)
		msg := "密码错误"
		if a.TOTPEnabled {
			msg = "验证码错误"
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": msg})
		return
	}
	until := time.Now().Add(sudoTTL)
	s.st.DB.Model(&store.AdminSession{}).Where("id = ?", c.GetString("sid")).Update("sudo_until", until)
	c.JSON(http.StatusOK, gin.H{"until": until})
}

// --- TOTP recovery codes ---

const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

func newRecoveryCode() string {
	b := make([]byte, 10)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(recoveryAlphabet))))
		b[i] = recoveryAlphabet[n.Int64()]
	}
	return string(b[:5]) + "-" + string(b[5:])
}

func recoveryHash(code string) string {
	code = strings.ToLower(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(code)))
	h := sha256.Sum256([]byte("polaris-recovery:" + code))
	return hex.EncodeToString(h[:])
}

// issueRecoveryCodes replaces an admin's recovery codes and returns them.
func (s *Server) issueRecoveryCodes(a *store.Admin) []string {
	codes := make([]string, recoveryCount)
	hashes := make([]string, recoveryCount)
	for i := range codes {
		codes[i] = newRecoveryCode()
		hashes[i] = recoveryHash(codes[i])
	}
	a.RecoveryCodes = store.NewJSON(hashes)
	s.st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Update("recovery_codes", a.RecoveryCodes)
	return codes
}

// secondFactor checks a TOTP code, or consumes a recovery code.
func (s *Server) secondFactor(a *store.Admin, code, ip string) bool {
	code = strings.TrimSpace(code)
	if code == "" {
		return false
	}
	if totp.Validate(code, s.st.Box.MustOpen(a.TOTPSecret)) {
		return true
	}
	h := recoveryHash(code)
	for i, x := range a.RecoveryCodes.V {
		if x == h {
			left := append(append([]string{}, a.RecoveryCodes.V[:i]...), a.RecoveryCodes.V[i+1:]...)
			a.RecoveryCodes = store.NewJSON(left)
			s.st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Update("recovery_codes", a.RecoveryCodes)
			s.audit(a.Username, "admin.recovery_code", a.Username, fmt.Sprintf("剩余 %d 个", len(left)), ip)
			s.notify(Event{Kind: "admin_recovery_code", Text: fmt.Sprintf("🔑 管理员 %s 使用了一个二次验证恢复码（剩余 %d 个），来自 %s", a.Username, len(left), ip)})
			return true
		}
	}
	return false
}

// regenRecoveryCodes: POST /auth/totp/recovery-codes (sudo).
func (s *Server) regenRecoveryCodes(c *gin.Context) {
	var a store.Admin
	if err := s.st.DB.First(&a, "id = ?", c.GetString("adminID")).Error; err != nil || !a.TOTPEnabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先开启二次验证"})
		return
	}
	codes := s.issueRecoveryCodes(&a)
	s.auditCtx(c, "admin.recovery_codes", a.Username, "regenerated")
	c.JSON(http.StatusOK, gin.H{"recoveryCodes": codes})
}

// --- security settings page ---

// securityInfo: GET /security — bans, allowlist and proxy trust.
func (s *Server) securityInfo(c *gin.Context) {
	var bans []store.LoginBan
	s.st.DB.Where("until > ? OR (failures > 0 AND updated_at > ?)", time.Now(), time.Now().Add(-ipFailWindow)).
		Order("updated_at DESC").Limit(200).Find(&bans)
	proxies := []string{}
	for _, n := range trustedProxies {
		proxies = append(proxies, n.String())
	}
	c.JSON(http.StatusOK, gin.H{
		"bans": nonNil(bans), "allowlist": s.st.GetSetting("login_allowlist", ""), "banFailures": s.ipBanThreshold(),
		"trustedProxies": proxies, "yourIp": clientIP(c.Request),
	})
}

// unbanIP: DELETE /security/bans/:ip.
func (s *Server) unbanIP(c *gin.Context) {
	s.st.DB.Where("ip = ?", c.Param("ip")).Delete(&store.LoginBan{})
	s.auditCtx(c, "security.ip_unban", c.Param("ip"), "")
	c.Status(http.StatusNoContent)
}
