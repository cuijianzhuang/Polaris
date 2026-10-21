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

// E-mail: with an SMTP server configured (smtp_*), users who have an
// address get the same notices as on Telegram (mail_notify_users), plus a
// receipt when an order is delivered; mail_admin_to receives admin notices
// when mail_notify_admin is on. Mail goes out in order, best effort.

// mailConfigFrom builds the SMTP account from settings values.
func mailConfigFrom(get func(string, string) string, pass string) *mailer.Config {
	port, _ := strconv.Atoi(strings.TrimSpace(get("smtp_port", "")))
	return &mailer.Config{
		Host: strings.TrimSpace(get("smtp_host", "")), Port: port, Security: get("smtp_security", mailer.StartTLS),
		User: strings.TrimSpace(get("smtp_user", "")), Pass: pass,
		From: strings.TrimSpace(get("smtp_from", "")), FromName: strings.TrimSpace(get("smtp_from_name", "")),
	}
}

// mailConfig is the configured account, or nil when mail is off.
func (s *Server) mailConfig() *mailer.Config {
	c := mailConfigFrom(s.st.GetSetting, s.st.Box.MustOpen(s.st.GetSetting("smtp_password", "")))
	if c.Check() != nil {
		return nil
	}
	return c
}

func (s *Server) mailOn() bool { return s.mailConfig() != nil }

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

// mailBody wraps text in the site's simple HTML layout.
func (s *Server) mailBody(title, text string, u *store.User) (string, string) {
	site := s.st.GetSetting("site_title", "Polaris")
	plain := text
	var foot []string
	if u != nil && u.SubToken != "" {
		plain += "\n\n订阅链接：" + s.subURL(u.SubToken)
		foot = append(foot, `订阅链接：<a href="`+html.EscapeString(s.subURL(u.SubToken))+`">`+html.EscapeString(s.subURL(u.SubToken))+`</a>`)
	}
	if s.portalEnabled() {
		plain += "\n用户中心：" + s.portalURL()
		foot = append(foot, `用户中心：<a href="`+html.EscapeString(s.portalURL())+`">`+html.EscapeString(s.portalURL())+`</a>`)
	}
	if sup := strings.TrimSpace(s.st.GetSetting("sub_support_url", "")); sup != "" {
		plain += "\n客服：" + sup
		foot = append(foot, "客服："+html.EscapeString(sup))
	}
	plain += "\n\n— " + site
	body := strings.ReplaceAll(html.EscapeString(text), "\n", "<br>")
	h := `<!doctype html><html><body style="margin:0;background:#f4f5f7;font:15px/1.6 -apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;color:#1f2328">` +
		`<div style="max-width:560px;margin:24px auto;background:#fff;border-radius:10px;padding:24px 28px">` +
		`<div style="font-size:13px;color:#6b7280">` + html.EscapeString(site) + `</div>` +
		`<h2 style="margin:6px 0 16px;font-size:20px">` + html.EscapeString(title) + `</h2>` +
		`<div>` + body + `</div>`
	if len(foot) > 0 {
		h += `<div style="margin-top:20px;padding-top:14px;border-top:1px solid #e5e7eb;font-size:13px;color:#6b7280;word-break:break-all">` + strings.Join(foot, "<br>") + `</div>`
	}
	return plain, h + `</div></body></html>`
}

// sendMail sends one message with the configured account.
func (s *Server) sendMail(ctx context.Context, to, subject, text, htmlBody string) error {
	c := s.mailConfig()
	if c == nil {
		return errors.New("未配置 SMTP")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	site := s.st.GetSetting("site_title", "Polaris")
	return c.Send(ctx, mailer.Message{To: to, Subject: "[" + site + "] " + subject, Text: text, HTML: htmlBody})
}

// runMail delivers events by mail.
func (s *Server) runMail(ctx context.Context, events <-chan Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-events:
			if e.Text == "" || s.mailConfig() == nil {
				continue
			}
			s.mailEvent(ctx, e)
		}
	}
}

func (s *Server) mailEvent(ctx context.Context, e Event) {
	if !e.UserOnly && s.st.GetSetting("mail_notify_admin", "true") == "true" {
		if to := mailer.Normalize(s.st.GetSetting("mail_admin_to", "")); to != "" {
			text := plainText(e.Text)
			subject := strings.SplitN(text, "\n", 2)[0]
			if len([]rune(subject)) > 60 {
				subject = string([]rune(subject)[:60]) + "…"
			}
			plain, h := s.mailBody(subject, text, nil)
			if err := s.sendMail(ctx, to, subject, plain, h); err != nil {
				s.log.Warn("admin mail", "kind", e.Kind, "err", err)
			}
		}
	}
	if e.UserID == "" || s.st.GetSetting("mail_notify_users", "true") != "true" {
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
	subject := orDefault(mailSubjects[e.Kind], "通知")
	plain, h := s.mailBody(subject, text, &u)
	if err := s.sendMail(ctx, u.Email, subject, plain, h); err != nil {
		s.log.Warn("user mail", "kind", e.Kind, "user", u.Username, "err", err)
	}
}

// mailOrderDelivered sends the buyer a receipt for a delivered order.
func (s *Server) mailOrderDelivered(o *store.Order, u *store.User, created bool) {
	if u == nil || u.Email == "" || s.mailConfig() == nil || s.st.GetSetting("mail_notify_users", "true") != "true" {
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

// testMail is POST /mail/test {to, host, port, security, user, password,
// from, fromName}: sends a test message with the form's values (an empty
// password means the saved one).
func (s *Server) testMail(c *gin.Context) {
	var req struct {
		To       string `json:"to"`
		Host     string `json:"host"`
		Port     string `json:"port"`
		Security string `json:"security"`
		User     string `json:"user"`
		Password string `json:"password"`
		From     string `json:"from"`
		FromName string `json:"fromName"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	vals := map[string]string{"smtp_host": req.Host, "smtp_port": req.Port, "smtp_security": req.Security, "smtp_user": req.User,
		"smtp_from": req.From, "smtp_from_name": req.FromName}
	pass := req.Password
	if pass == "" {
		pass = s.st.Box.MustOpen(s.st.GetSetting("smtp_password", ""))
	}
	cfg := mailConfigFrom(func(k, def string) string { return orDefault(vals[k], def) }, pass)
	to := mailer.Normalize(req.To)
	if to == "" {
		badRequest(c, errors.New("请填写正确的收件地址"))
		return
	}
	if err := cfg.Check(); err != nil {
		badRequest(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	site := s.st.GetSetting("site_title", "Polaris")
	plain, h := s.mailBody("测试邮件", "这是一封测试邮件：收到它说明 SMTP 配置正确。", nil)
	if err := cfg.Send(ctx, mailer.Message{To: to, Subject: "[" + site + "] 测试邮件", Text: plain, HTML: h}); err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	s.auditCtx(c, "mail.test", to, "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
