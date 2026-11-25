package panel

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/mailer"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// E-mail: with a mail provider configured, users who have an address get
// the same notices as on Telegram (mail_notify_users), plus a receipt
// when an order is delivered; mail_admin_to receives admin notices when
// mail_notify_admin is on. Mail goes out in order, best effort. The
// provider is a single active channel chosen by mail_provider — smtp
// (default), resend or cloudflare — with no automatic fallback.

// mailConfigFrom builds the SMTP account from settings values.
func mailConfigFrom(get func(string, string) string, pass string) *mailer.Config {
	port, _ := strconv.Atoi(strings.TrimSpace(get("smtp_port", "")))
	return &mailer.Config{
		Host: strings.TrimSpace(get("smtp_host", "")), Port: port, Security: get("smtp_security", mailer.StartTLS),
		User: strings.TrimSpace(get("smtp_user", "")), Pass: pass,
		From: strings.TrimSpace(get("smtp_from", "")), FromName: strings.TrimSpace(get("smtp_from_name", "")),
	}
}

// The selectable sending channels (mail_provider values).
const (
	mailProviderSMTP       = "smtp"
	mailProviderResend     = "resend"
	mailProviderCloudflare = "cloudflare"
)

// mailProviderOf is the selected channel; anything unknown means smtp.
func mailProviderOf(get func(string, string) string) string {
	switch strings.TrimSpace(get("mail_provider", "")) {
	case mailProviderResend:
		return mailProviderResend
	case mailProviderCloudflare:
		return mailProviderCloudflare
	}
	return mailProviderSMTP
}

// mailProviderName is the channel's display name, used in messages.
func mailProviderName(provider string) string {
	switch provider {
	case mailProviderResend:
		return "Resend"
	case mailProviderCloudflare:
		return "Cloudflare"
	}
	return "SMTP"
}

// mailSecrets holds the opened (decrypted) credential of each channel.
type mailSecrets struct {
	smtpPass  string
	resendKey string
	cfToken   string
}

// mailFrom resolves a channel's From: the channel's own from / from_name
// settings, each falling back to the global smtp_from / smtp_from_name.
func mailFrom(get func(string, string) string, addrKey, nameKey string) (string, string) {
	addr := strings.TrimSpace(get(addrKey, ""))
	if addr == "" {
		addr = strings.TrimSpace(get("smtp_from", ""))
	}
	name := get(nameKey, "")
	if name == "" {
		name = get("smtp_from_name", "")
	}
	return addr, name
}

// buildMailSender returns the Sender for the selected channel, or the
// channel configuration's Check error when it is incomplete.
func buildMailSender(get func(string, string) string, sec mailSecrets) (mailer.Sender, error) {
	switch mailProviderOf(get) {
	case mailProviderResend:
		from, name := mailFrom(get, "resend_from", "resend_from_name")
		cfg := &mailer.ResendConfig{APIKey: sec.resendKey, From: from, FromName: name}
		if err := cfg.Check(); err != nil {
			return nil, err
		}
		return cfg, nil
	case mailProviderCloudflare:
		from, name := mailFrom(get, "cf_mail_from", "cf_mail_from_name")
		cfg := &mailer.CloudflareConfig{AccountID: strings.TrimSpace(get("cf_mail_account_id", "")), APIToken: sec.cfToken, From: from, FromName: name}
		if err := cfg.Check(); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	cfg := mailConfigFrom(get, sec.smtpPass)
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// mailSender is the configured sender for the selected channel, or nil
// when that channel is not fully configured. Mail availability (mailOn,
// and everything built on it: code sign-in, registration, notices) is
// judged on the selected channel alone.
func (s *Server) mailSender() mailer.Sender {
	sec := mailSecrets{
		smtpPass:  s.st.Box.MustOpen(s.st.GetSetting("smtp_password", "")),
		resendKey: s.st.Box.MustOpen(s.st.GetSetting("resend_api_key", "")),
		cfToken:   s.st.Box.MustOpen(s.st.GetSetting("cf_mail_api_token", "")),
	}
	snd, err := buildMailSender(s.st.GetSetting, sec)
	if err != nil {
		return nil
	}
	return snd
}

func (s *Server) mailOn() bool { return s.mailSender() != nil }

// mailSubjects names the notices users get.
var mailSubjects = map[string]string{
	"traffic_warn": "流量提醒", "expire_warn": "到期提醒", "user_limited": "流量已用尽", "user_expired": "账号已到期",
	"user_active": "服务已恢复", "ip_limit": "设备超限提醒", "order_delivered": "订单已开通", "ticket_reply": "工单回复",
}

var tagRe = regexp.MustCompile(`<[^>]+>`)

// plainText turns Telegram HTML into plain text.
func plainText(s string) string {
	return strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(strings.ReplaceAll(s, tgRule, ""), "")))
}

// sendMail sends one message through the selected channel.
func (s *Server) sendMail(ctx context.Context, to, subject, text, htmlBody string) error {
	snd := s.mailSender()
	if snd == nil {
		return fmt.Errorf("未配置 %s", mailProviderName(mailProviderOf(s.st.GetSetting)))
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	site := s.st.GetSetting("site_title", "Polaris")
	return snd.Send(ctx, mailer.Message{To: to, Subject: "[" + site + "] " + subject, Text: text, HTML: htmlBody})
}

// runMail delivers events by mail.
func (s *Server) runMail(ctx context.Context, events <-chan Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-events:
			if e.Text == "" {
				continue
			}
			s.mailEvent(ctx, e)
		}
	}
}

func (s *Server) mailEvent(ctx context.Context, e Event) {
	ready := s.mailOn()
	if !e.UserOnly {
		if to := mailer.Normalize(s.st.GetSetting("mail_admin_to", "")); to != "" {
			notice := e
			notice.noticeRecipient = to
			notice.noticeDisabled = !ready || s.st.GetSetting("mail_notify_admin", "true") != "true"
			notice, allowed := s.operationalNotice("email", notice, time.Now())
			if allowed && !notice.noticeDisabled {
				text := plainText(notice.Text)
				subject := strings.SplitN(text, "\n", 2)[0]
				if len([]rune(subject)) > 60 {
					subject = string([]rune(subject)[:60]) + "…"
				}
				plain, h := s.mailBody(subject, text, nil)
				if err := s.sendMail(ctx, to, subject, plain, h); err != nil {
					s.retryOperationalNotice(notice)
					s.log.Warn("admin mail", "kind", e.Kind, "err", err)
				}
			}
		}
	}
	if !ready || e.UserID == "" || s.st.GetSetting("mail_notify_users", "true") != "true" {
		return
	}
	var u store.User
	if s.st.DB.First(&u, "id = ?", e.UserID).Error != nil || u.Email == "" {
		return
	}
	var text string
	switch {
	case e.UserOnly:
		text = plainText(e.Text)
	default:
		if tpl := s.reminderText(e.Kind, &u); tpl != "" {
			text = tpl
		} else {
			text = userFacing(e)
		}
	}
	text += s.userRenewLines(e.Kind)
	subject := orDefault(mailSubjects[e.Kind], "通知")
	plain, h := s.mailBody(subject, text, &u)
	if err := s.sendMail(ctx, u.Email, subject, plain, h); err != nil {
		s.log.Warn("user mail", "kind", e.Kind, "user", u.Username, "err", err)
	}
}

// mailOrderDelivered sends the buyer a receipt for a delivered order.
func (s *Server) mailOrderDelivered(o *store.Order, u *store.User, created bool) {
	if u == nil || u.Email == "" || !s.mailOn() || s.st.GetSetting("mail_notify_users", "true") != "true" {
		return
	}
	text := fmt.Sprintf("订单 %s 已付款并开通。\n%s · %s\n%s", o.ID, o.PlanName, webAmount(o), o.Detail)
	if created {
		text += "\n\n已为你创建账号 " + u.Username + "。"
	}
	plain, h := s.mailBody(mailSubjects["order_delivered"], text, u)
	if err := s.sendMail(context.Background(), u.Email, mailSubjects["order_delivered"], plain, h); err != nil {
		s.log.Warn("order mail", "order", o.ID, "err", err)
	}
}

// --- users' addresses ---

// checkUserEmail normalises addr ("" clears it) and makes sure no other
// user has it.
func checkUserEmail(db *gorm.DB, addr, userID string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", nil
	}
	n := mailer.Normalize(addr)
	if n == "" {
		return "", fmt.Errorf("邮箱地址无效：%s", addr)
	}
	var cnt int64
	db.Model(&store.User{}).Where("email = ? AND id <> ?", n, userID).Count(&cnt)
	if cnt > 0 {
		return "", fmt.Errorf("邮箱 %s 已被其他用户使用", n)
	}
	return n, nil
}

// testMail is POST /mail/test: sends a test message with the form's
// values through the form's selected provider (an empty secret field
// means the saved one).
func (s *Server) testMail(c *gin.Context) {
	var req struct {
		To       string `json:"to"`
		Provider string `json:"provider"`
		// SMTP channel.
		Host     string `json:"host"`
		Port     string `json:"port"`
		Security string `json:"security"`
		User     string `json:"user"`
		Password string `json:"password"`
		From     string `json:"from"`
		FromName string `json:"fromName"`
		// Resend channel.
		ResendAPIKey   string `json:"resendApiKey"`
		ResendFrom     string `json:"resendFrom"`
		ResendFromName string `json:"resendFromName"`
		// Cloudflare channel.
		CFAccountID string `json:"cfAccountId"`
		CFAPIToken  string `json:"cfApiToken"`
		CFFrom      string `json:"cfFrom"`
		CFFromName  string `json:"cfFromName"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	vals := map[string]string{
		"mail_provider": req.Provider,
		"smtp_host":     req.Host, "smtp_port": req.Port, "smtp_security": req.Security, "smtp_user": req.User,
		"smtp_from": req.From, "smtp_from_name": req.FromName,
		"resend_from": req.ResendFrom, "resend_from_name": req.ResendFromName,
		"cf_mail_account_id": req.CFAccountID, "cf_mail_from": req.CFFrom, "cf_mail_from_name": req.CFFromName,
	}
	get := func(k, def string) string {
		if v, ok := vals[k]; ok {
			return v
		}
		return s.st.GetSetting(k, def)
	}
	saved := func(key, given string) string {
		if given != "" {
			return given
		}
		return s.st.Box.MustOpen(s.st.GetSetting(key, ""))
	}
	sec := mailSecrets{
		smtpPass:  saved("smtp_password", req.Password),
		resendKey: saved("resend_api_key", req.ResendAPIKey),
		cfToken:   saved("cf_mail_api_token", req.CFAPIToken),
	}
	to := mailer.Normalize(req.To)
	if to == "" {
		badRequest(c, errors.New("请填写正确的收件地址"))
		return
	}
	snd, err := buildMailSender(get, sec)
	if err != nil {
		badRequest(c, err)
		return
	}
	name := mailProviderName(mailProviderOf(get))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	site := s.st.GetSetting("site_title", "Polaris")
	plain, h := s.mailBody("测试邮件", "这是一封测试邮件：收到它说明 "+name+" 配置正确。", nil)
	if err := snd.Send(ctx, mailer.Message{To: to, Subject: "[" + site + "] 测试邮件", Text: plain, HTML: h}); err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	s.auditCtx(c, "mail.test", to, "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
