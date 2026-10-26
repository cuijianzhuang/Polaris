package panel

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Support tickets: users open them in the user center (/u/tickets) or the
// bot (/ticket), admins answer in the panel. Each side is told about the
// other's messages (Telegram, mail, webhooks). A ticket waiting for the user
// closes by itself after ticket_autoclose_days.

const (
	ticketOpen     = "open"     // waiting for an admin
	ticketAnswered = "answered" // waiting for the user
	ticketClosed   = "closed"

	ticketSubjectMax = 120
	ticketBodyMax    = 4000
)

func (s *Server) ticketsOn() bool { return s.st.GetSetting("tickets_enabled", "false") == "true" }

func cleanTicketText(subject, body string) (string, string, error) {
	subject, body = strings.TrimSpace(subject), strings.TrimSpace(body)
	if subject == "" {
		subject, _, _ = strings.Cut(body, "\n")
		subject = strings.TrimSpace(subject)
	}
	if body == "" {
		return "", "", errors.New("请填写问题描述")
	}
	if utf8.RuneCountInString(body) > ticketBodyMax {
		return "", "", fmt.Errorf("内容最长 %d 个字", ticketBodyMax)
	}
	if r := []rune(subject); len(r) > ticketSubjectMax {
		subject = string(r[:ticketSubjectMax-1]) + "…"
	}
	return subject, body, nil
}

func excerpt(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// openTicket creates a ticket for u.
func (s *Server) openTicket(u *store.User, subject, body string) (*store.Ticket, error) {
	if !s.ticketsOn() {
		return nil, errors.New("工单暂未开放")
	}
	subject, body, err := cleanTicketText(subject, body)
	if err != nil {
		return nil, err
	}
	maxOpen := atoiDefault(s.st.GetSetting("ticket_max_open", "3"), 3)
	var open int64
	s.st.DB.Model(&store.Ticket{}).Where("user_id = ? AND status <> ?", u.ID, ticketClosed).Count(&open)
	if maxOpen > 0 && open >= int64(maxOpen) {
		return nil, fmt.Errorf("最多同时有 %d 个未关闭的工单，请先在已有工单里继续沟通", maxOpen)
	}
	now := time.Now()
	t := &store.Ticket{ID: store.NewID(), UserID: u.ID, Subject: subject, Status: ticketOpen, AdminUnread: true, CreatedAt: now, UpdatedAt: now}
	var err2 error
	for range 5 { // the number is unique; two at once retry
		err2 = s.st.DB.Transaction(func(tx *gorm.DB) error {
			var last int64
			tx.Model(&store.Ticket{}).Select("COALESCE(MAX(no), 0)").Scan(&last)
			t.No = last + 1
			if err := tx.Create(t).Error; err != nil {
				return err
			}
			return tx.Create(&store.TicketMessage{ID: store.NewID(), TicketID: t.ID, Author: u.Username, Body: body, CreatedAt: now}).Error
		})
		if err2 == nil {
			break
		}
	}
	if err2 != nil {
		return nil, err2
	}
	s.audit("user:"+u.Username, "ticket.create", fmt.Sprintf("#%d", t.No), subject, "")
	s.notify(Event{Kind: "ticket_new", Type: "ticket.created",
		Text: fmt.Sprintf("🎫 新工单 #%d（%s）：%s\n%s", t.No, u.Username, subject, excerpt(body, 300)),
		Data: map[string]any{"ticket": ticketBrief(t), "user": userBrief(u)}})
	return t, nil
}

func ticketBrief(t *store.Ticket) map[string]any {
	return map[string]any{"id": t.ID, "no": t.No, "subject": t.Subject, "status": t.Status, "user_id": t.UserID}
}

// userTicket loads ticket no of user u.
func (s *Server) userTicket(u *store.User, no int64) (*store.Ticket, error) {
	var t store.Ticket
	if s.st.DB.First(&t, "no = ? AND user_id = ?", no, u.ID).Error != nil {
		return nil, errors.New("工单不存在")
	}
	return &t, nil
}

// userReply adds the user's message to their ticket (reopening it).
func (s *Server) userReply(u *store.User, t *store.Ticket, body string) error {
	if !s.ticketsOn() {
		return errors.New("工单暂未开放")
	}
	_, body, err := cleanTicketText("-", body)
	if err != nil {
		return err
	}
	now := time.Now()
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&store.TicketMessage{ID: store.NewID(), TicketID: t.ID, Author: u.Username, Body: body, CreatedAt: now}).Error; err != nil {
			return err
		}
		return tx.Model(t).Updates(map[string]any{"status": ticketOpen, "admin_unread": true, "updated_at": now, "closed_at": nil}).Error
	})
	if err != nil {
		return err
	}
	t.Status = ticketOpen
	s.notify(Event{Kind: "ticket_user_reply", Type: "ticket.replied",
		Text: fmt.Sprintf("🎫 工单 #%d 有新消息（%s）：%s\n%s", t.No, u.Username, t.Subject, excerpt(body, 300)),
		Data: map[string]any{"ticket": ticketBrief(t), "user": userBrief(u), "by": "user"}})
	return nil
}

// adminReply answers a ticket and tells the user.
func (s *Server) adminReply(t *store.Ticket, admin, body string, closeIt bool) error {
	_, body, err := cleanTicketText("-", body)
	if err != nil {
		return err
	}
	now := time.Now()
	status := ticketAnswered
	var closed *time.Time
	if closeIt {
		status, closed = ticketClosed, &now
	}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&store.TicketMessage{ID: store.NewID(), TicketID: t.ID, Admin: true, Author: admin, Body: body, CreatedAt: now}).Error; err != nil {
			return err
		}
		return tx.Model(t).Updates(map[string]any{"status": status, "user_unread": true, "admin_unread": false, "updated_at": now, "closed_at": closed}).Error
	})
	if err != nil {
		return err
	}
	t.Status = status
	var u store.User
	if s.st.DB.First(&u, "id = ?", t.UserID).Error == nil {
		text := fmt.Sprintf("📨 <b>工单 #%d 有新回复</b>\n%s\n%s\n\n%s", t.No, tgRule, esc(t.Subject), esc(excerpt(body, 1500)))
		if closeIt {
			text += "\n\n工单已关闭；如还有问题可以继续回复重新打开。"
		}
		text += fmt.Sprintf("\n\n回复：<code>/reply %d 内容</code>", t.No)
		if s.portalEnabled() {
			text += "\n或在用户中心查看：" + esc(s.portalURL()+"/tickets/"+strconv.FormatInt(t.No, 10))
		}
		s.notify(Event{Kind: "ticket_reply", UserID: u.ID, UserOnly: true, Type: "ticket.replied", Text: text,
			Data: map[string]any{"ticket": ticketBrief(t), "user": userBrief(&u), "by": "admin"}})
	}
	return nil
}

// closeTicket closes a ticket (by its user or an admin).
func (s *Server) closeTicket(t *store.Ticket, by string) error {
	now := time.Now()
	if err := s.st.DB.Model(t).Updates(map[string]any{"status": ticketClosed, "closed_at": now, "updated_at": now}).Error; err != nil {
		return err
	}
	t.Status = ticketClosed
	s.userEventless("ticket.closed", map[string]any{"ticket": ticketBrief(t), "by": by})
	return nil
}

// userEventless emits a webhook-only event that is not about one user.
func (s *Server) userEventless(typ string, data map[string]any) {
	s.notify(Event{Kind: typ, Type: typ, Data: data})
}

// autoCloseTickets closes tickets that waited for the user too long (daily).
func (s *Server) autoCloseTickets() {
	days := atoiDefault(s.st.GetSetting("ticket_autoclose_days", "7"), 7)
	if days <= 0 {
		return
	}
	now := time.Now()
	s.st.DB.Model(&store.Ticket{}).Where("status = ? AND updated_at < ?", ticketAnswered, now.AddDate(0, 0, -days)).
		Updates(map[string]any{"status": ticketClosed, "closed_at": now})
}

// --- admin API ---

// listTickets is GET /tickets?status=&q=.
func (s *Server) listTickets(c *gin.Context) {
	q := s.st.DB.Model(&store.Ticket{})
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	if v := strings.TrimSpace(c.Query("q")); v != "" {
		like := "%" + v + "%"
		sub := s.st.DB.Model(&store.User{}).Select("id").Where(s.st.Like("username LIKE ?"), like)
		if no, err := strconv.ParseInt(strings.TrimPrefix(v, "#"), 10, 64); err == nil {
			q = q.Where("no = ?", no)
		} else {
			q = q.Where(s.st.Like("subject LIKE ?")+" OR user_id IN (?)", like, sub)
		}
	}
	var ts []store.Ticket
	q.Order("CASE status WHEN 'open' THEN 0 WHEN 'answered' THEN 1 ELSE 2 END, updated_at desc").Limit(500).Find(&ts)
	s.withTicketUsers(ts)
	counts := map[string]int64{}
	for _, st := range []string{ticketOpen, ticketAnswered, ticketClosed} {
		var n int64
		s.st.DB.Model(&store.Ticket{}).Where("status = ?", st).Count(&n)
		counts[st] = n
	}
	c.JSON(http.StatusOK, gin.H{"items": ts, "counts": counts, "enabled": s.ticketsOn()})
}

func (s *Server) withTicketUsers(ts []store.Ticket) {
	var ids []string
	for _, t := range ts {
		ids = append(ids, t.UserID)
	}
	if len(ids) == 0 {
		return
	}
	var us []store.User
	s.st.DB.Select("id, username").Where("id IN ?", ids).Find(&us)
	names := map[string]string{}
	for _, u := range us {
		names[u.ID] = u.Username
	}
	for i := range ts {
		ts[i].Username = orDefault(names[ts[i].UserID], "（已删除）")
	}
}

func (s *Server) adminTicket(c *gin.Context) (*store.Ticket, bool) {
	var t store.Ticket
	if s.st.DB.First(&t, "id = ?", c.Param("id")).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return nil, false
	}
	return &t, true
}

// getTicket is GET /tickets/:id: the thread; reading it clears the admin's
// unread mark.
func (s *Server) getTicket(c *gin.Context) {
	t, ok := s.adminTicket(c)
	if !ok {
		return
	}
	var msgs []store.TicketMessage
	s.st.DB.Where("ticket_id = ?", t.ID).Order("created_at").Find(&msgs)
	if t.AdminUnread {
		s.st.DB.Model(t).UpdateColumn("admin_unread", false)
		t.AdminUnread = false
	}
	ts := []store.Ticket{*t}
	s.withTicketUsers(ts)
	c.JSON(http.StatusOK, gin.H{"ticket": ts[0], "messages": msgs})
}

// replyTicket is POST /tickets/:id/reply {body, close}.
func (s *Server) replyTicket(c *gin.Context) {
	t, ok := s.adminTicket(c)
	if !ok {
		return
	}
	var req struct {
		Body  string `json:"body"`
		Close bool   `json:"close"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if err := s.adminReply(t, c.GetString("admin"), req.Body, req.Close); err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "ticket.reply", fmt.Sprintf("#%d", t.No), map[bool]string{true: "closed"}[req.Close])
	c.JSON(http.StatusOK, t)
}

// setTicketStatus is POST /tickets/:id/close and /reopen.
func (s *Server) setTicketStatus(closeIt bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		t, ok := s.adminTicket(c)
		if !ok {
			return
		}
		if closeIt {
			if err := s.closeTicket(t, "admin"); err != nil {
				dbError(c, err)
				return
			}
		} else {
			if err := s.st.DB.Model(t).Updates(map[string]any{"status": ticketOpen, "closed_at": nil, "updated_at": time.Now()}).Error; err != nil {
				dbError(c, err)
				return
			}
			t.Status = ticketOpen
		}
		s.auditCtx(c, map[bool]string{true: "ticket.close", false: "ticket.reopen"}[closeIt], fmt.Sprintf("#%d", t.No), "")
		c.JSON(http.StatusOK, t)
	}
}

// deleteTicket is DELETE /tickets/:id.
func (s *Server) deleteTicket(c *gin.Context) {
	t, ok := s.adminTicket(c)
	if !ok {
		return
	}
	s.st.DB.Where("ticket_id = ?", t.ID).Delete(&store.TicketMessage{})
	s.st.DB.Delete(t)
	s.auditCtx(c, "ticket.delete", fmt.Sprintf("#%d", t.No), t.Subject)
	c.Status(http.StatusNoContent)
}

// --- user center ---

func (s *Server) portalTicketUser(c *gin.Context) *store.User {
	if !s.ticketsOn() {
		c.Redirect(http.StatusFound, portalPath)
		return nil
	}
	u := s.portalUser(c)
	if u == nil {
		c.Redirect(http.StatusFound, portalPath)
	}
	return u
}

type ticketRow struct {
	No                    int64
	Subject, Status, When string
	Unread                bool
}

var ticketStatusName = map[string]string{ticketOpen: "等待客服回复", ticketAnswered: "客服已回复", ticketClosed: "已关闭"}

// portalTickets is GET /u/tickets: the user's tickets and a form for a new one.
func (s *Server) portalTickets(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalTicketUser(c)
	if u == nil {
		return
	}
	var ts []store.Ticket
	s.st.DB.Where("user_id = ?", u.ID).Order("updated_at desc").Limit(50).Find(&ts)
	rows := make([]ticketRow, 0, len(ts))
	for _, t := range ts {
		rows = append(rows, ticketRow{No: t.No, Subject: t.Subject, Status: ticketStatusName[t.Status], When: t.UpdatedAt.Local().Format("01-02 15:04"), Unread: t.UserUnread})
	}
	s.renderTicketPage(c, ticketListTpl, map[string]any{"Title": s.subTitle(s.st.GetSetting("site_title", "Polaris")), "Tickets": rows, "User": u.Username})
}

type ticketMsgView struct {
	Admin        bool
	Author, When string
	Body         string
}

// portalTicket is GET /u/tickets/:no.
func (s *Server) portalTicket(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalTicketUser(c)
	if u == nil {
		return
	}
	no, _ := strconv.ParseInt(c.Param("no"), 10, 64)
	t, err := s.userTicket(u, no)
	if err != nil {
		c.Redirect(http.StatusFound, portalPath+"/tickets")
		return
	}
	if t.UserUnread {
		s.st.DB.Model(t).UpdateColumn("user_unread", false)
	}
	var msgs []store.TicketMessage
	s.st.DB.Where("ticket_id = ?", t.ID).Order("created_at").Find(&msgs)
	views := make([]ticketMsgView, 0, len(msgs))
	for _, m := range msgs {
		author := "我"
		if m.Admin {
			author = "客服"
		}
		views = append(views, ticketMsgView{Admin: m.Admin, Author: author, When: m.CreatedAt.Local().Format("2006-01-02 15:04"), Body: m.Body})
	}
	s.renderTicketPage(c, ticketViewTpl, map[string]any{"Title": s.subTitle(s.st.GetSetting("site_title", "Polaris")), "T": t,
		"Status": ticketStatusName[t.Status], "Closed": t.Status == ticketClosed, "Messages": views})
}

func (s *Server) renderTicketPage(c *gin.Context, t *template.Template, data map[string]any) {
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		serverError(c, err)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(buf.String()))
}

func (s *Server) ticketWriteUser(c *gin.Context) *store.User {
	if !portalWrite(c) {
		return nil
	}
	if !s.ticketsOn() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "工单暂未开放"})
		return nil
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return nil
	}
	if !s.ticketLimit.Allow(clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "操作太频繁，请稍后再试"})
		return nil
	}
	return u
}

// portalTicketNew is POST /u/tickets {subject, body}.
func (s *Server) portalTicketNew(c *gin.Context) {
	u := s.ticketWriteUser(c)
	if u == nil {
		return
	}
	var req struct{ Subject, Body string }
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	t, err := s.openTicket(u, req.Subject, req.Body)
	if err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"no": t.No, "url": portalPath + "/tickets/" + strconv.FormatInt(t.No, 10)})
}

// portalTicketReply is POST /u/tickets/:no/reply {body} and
// /u/tickets/:no/close.
func (s *Server) portalTicketAction(closeIt bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := s.ticketWriteUser(c)
		if u == nil {
			return
		}
		no, _ := strconv.ParseInt(c.Param("no"), 10, 64)
		t, err := s.userTicket(u, no)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		if closeIt {
			err = s.closeTicket(t, "user")
		} else {
			var req struct{ Body string }
			if err := c.ShouldBindJSON(&req); err != nil {
				badRequest(c, err)
				return
			}
			err = s.userReply(u, t, req.Body)
		}
		if err != nil {
			badRequest(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// --- bot ---

func ticketHelp() string {
	return "🎫 <b>工单</b>\n" + tgRule + "\n提交问题：\n<code>/ticket 问题描述</code>\n\n回复工单：\n<code>/reply 工单号 内容</code>\n\n客服回复后会在这里通知你。"
}

// ticketCommand handles /ticket <text>.
func (b *tgBot) ticketCommand(chat int64, arg string, id tgIdentity) {
	back := []button{{Text: "🎫 我的工单", Data: "v:tickets"}, btnHome}
	switch {
	case !b.s.ticketsOn():
		b.send(chat, "工单暂未开放，请联系管理员。", backRow(""))
		return
	case id.user == nil:
		b.send(chat, "请先绑定账号再提交工单。", []button{{Text: "🔗 绑定账号", Data: "v:bindhelp"}, btnHome})
		return
	case strings.TrimSpace(arg) == "":
		b.send(chat, ticketHelp(), back)
		return
	}
	t, err := b.s.openTicket(id.user, "", arg)
	if err != nil {
		b.send(chat, "❌ "+esc(err.Error()), back)
		return
	}
	b.send(chat, fmt.Sprintf("✅ 已提交工单 <b>#%d</b>，客服回复后会通知你。", t.No), back)
}

// replyCommand handles /reply <no> <text>.
func (b *tgBot) replyCommand(chat int64, arg string, id tgIdentity) {
	back := []button{{Text: "🎫 我的工单", Data: "v:tickets"}, btnHome}
	if !b.s.ticketsOn() || id.user == nil {
		b.ticketCommand(chat, "", id)
		return
	}
	noText, body, _ := strings.Cut(strings.TrimSpace(arg), " ")
	no, err := strconv.ParseInt(strings.TrimPrefix(noText, "#"), 10, 64)
	if err != nil || strings.TrimSpace(body) == "" {
		b.send(chat, ticketHelp(), back)
		return
	}
	t, err := b.s.userTicket(id.user, no)
	if err == nil {
		err = b.s.userReply(id.user, t, body)
	}
	if err != nil {
		b.send(chat, "❌ "+esc(err.Error()), back)
		return
	}
	b.send(chat, fmt.Sprintf("✅ 已回复工单 <b>#%d</b>。", no), back)
}

// ticketsView lists the user's recent tickets in the bot.
func (b *tgBot) ticketsView(id tgIdentity) (string, [][]button) {
	if !b.s.ticketsOn() {
		return "工单暂未开放，请联系管理员。", [][]button{backRow("")}
	}
	if id.user == nil {
		return "请先绑定账号再使用工单。", [][]button{{{Text: "🔗 绑定账号", Data: "v:bindhelp"}, btnHome}}
	}
	var ts []store.Ticket
	b.s.st.DB.Where("user_id = ?", id.user.ID).Order("updated_at desc").Limit(10).Find(&ts)
	var sb strings.Builder
	sb.WriteString(ticketHelp() + "\n")
	if len(ts) > 0 {
		sb.WriteString("\n<b>最近的工单</b>\n")
	}
	for _, t := range ts {
		fmt.Fprintf(&sb, "#%d %s · %s\n", t.No, esc(excerpt(t.Subject, 30)), ticketStatusName[t.Status])
	}
	return sb.String(), [][]button{backRow("")}
}

var ticketPageStyle = `<style>
:root{--bg:#F4F5F2;--card:#fff;--ink:#15181C;--mute:#5B6470;--line:#E2E5E0;--pri:#0E7490;--bad:#B91C1C;--me:#E6F3F6}
@media (prefers-color-scheme:dark){:root{--bg:#101316;--card:#181C20;--ink:#E6E8EA;--mute:#9AA2AB;--line:#2A3036;--pri:#22A3BF;--bad:#F87171;--me:#14313A}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.6 "IBM Plex Sans","Noto Sans SC",system-ui,sans-serif}
main{max-width:640px;margin:0 auto;padding:24px 16px 48px;display:flex;flex-direction:column;gap:14px}
header{display:flex;justify-content:space-between;align-items:center;gap:8px}h1{font-size:20px;margin:0}h2{font-size:16px;margin:0 0 8px}
a{color:var(--pri)}.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:16px}
.mute{color:var(--mute);font-size:13px}.err{color:var(--bad);font-size:13px}
input,textarea{width:100%;border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--ink);font:inherit;padding:8px 10px;margin:4px 0}
textarea{min-height:110px;resize:vertical}
.btn{display:inline-flex;align-items:center;justify-content:center;min-height:38px;border-radius:8px;border:1px solid var(--pri);background:var(--pri);color:#fff;font:inherit;padding:0 14px;cursor:pointer}
.btn.alt{background:var(--card);color:var(--ink);border-color:var(--line)}.btn:disabled{opacity:.6}
.row{display:flex;gap:8px;flex-wrap:wrap;align-items:center}
table{width:100%;border-collapse:collapse}td{padding:8px 4px;border-top:1px solid var(--line);vertical-align:top}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:var(--bad);margin-right:6px}
.msg{border:1px solid var(--line);border-radius:10px;padding:10px 12px;white-space:pre-wrap;word-break:break-word}.msg.me{background:var(--me)}
.msg b{font-weight:600}[hidden]{display:none!important}
</style>`

var ticketPageScript = `<script>
function tpost(p,b){return fetch('/u/tickets'+p,{method:'POST',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},body:JSON.stringify(b||{}),credentials:'same-origin'}).then(function(r){return r.json().then(function(j){if(!r.ok)throw new Error(j.error||r.statusText);return j})})}
function terr(m){var e=document.getElementById('err');e.textContent=m;e.hidden=!m}
</script>`

var ticketListTpl = template.Must(template.New("tickets").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex">
<title>{{.Title}} · 工单</title>` + ticketPageStyle + `</head><body><main>
<header><h1>工单</h1><a href="/u">← 用户中心</a></header>
<div class="card"><h2>提交新工单</h2>
<input id="subject" maxlength="120" placeholder="标题（可留空，取内容第一行）">
<textarea id="body" maxlength="4000" placeholder="请描述遇到的问题：使用的客户端、节点、出现的提示等。不要填写密码。"></textarea>
<div class="row"><button class="btn" id="send" type="button">提交</button><span class="mute">客服回复后会通过 Telegram 或邮件通知你。</span></div>
<p class="err" id="err" hidden></p></div>
<div class="card"><h2>我的工单</h2>
{{if .Tickets}}<table>{{range .Tickets}}<tr><td>{{if .Unread}}<span class="dot" title="有新回复"></span>{{end}}<a href="/u/tickets/{{.No}}">#{{.No}} {{.Subject}}</a></td><td class="mute">{{.Status}}</td><td class="mute">{{.When}}</td></tr>{{end}}</table>
{{else}}<p class="mute">还没有工单。</p>{{end}}</div>
</main>` + ticketPageScript + `<script>
document.getElementById('send').addEventListener('click',function(){var b=this;b.disabled=true;terr('');
tpost('',{subject:document.getElementById('subject').value,body:document.getElementById('body').value}).then(function(j){location.href=j.url}).catch(function(e){b.disabled=false;terr(e.message)})});
</script></body></html>`))

var ticketViewTpl = template.Must(template.New("ticket").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex">
<title>{{.Title}} · 工单 #{{.T.No}}</title>` + ticketPageStyle + `</head><body><main>
<header><h1>#{{.T.No}} {{.T.Subject}}</h1><a href="/u/tickets">← 全部工单</a></header>
<p class="mute">{{.Status}}</p>
{{range .Messages}}<div class="msg{{if not .Admin}} me{{end}}"><div class="mute"><b>{{.Author}}</b> · {{.When}}</div>{{.Body}}</div>{{end}}
<div class="card"><h2>{{if .Closed}}继续回复（会重新打开工单）{{else}}回复{{end}}</h2>
<textarea id="body" maxlength="4000"></textarea>
<div class="row"><button class="btn" id="send" type="button">发送</button>{{if not .Closed}}<button class="btn alt" id="close" type="button">问题已解决，关闭工单</button>{{end}}</div>
<p class="err" id="err" hidden></p></div>
</main>` + ticketPageScript + `<script>
var no='{{.T.No}}';
document.getElementById('send').addEventListener('click',function(){var b=this;b.disabled=true;terr('');
tpost('/'+no+'/reply',{body:document.getElementById('body').value}).then(function(){location.reload()}).catch(function(e){b.disabled=false;terr(e.message)})});
var c=document.getElementById('close');if(c)c.addEventListener('click',function(){if(!confirm('确定关闭这个工单？'))return;c.disabled=true;
tpost('/'+no+'/close').then(function(){location.reload()}).catch(function(e){c.disabled=false;terr(e.message)})});
</script></body></html>`))
