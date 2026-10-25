package panel

import (
	"crypto/subtle"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// "Sign in with Telegram" from the login page: the page starts a request and
// shows a two-digit number, the admin opens t.me/<bot>?start=weblogin_<id>
// and taps the same number among four choices, and the page (polling with a
// secret only it holds) receives a session. Number matching keeps a link
// forwarded by an attacker from signing the attacker in.

const (
	tgWebLoginTTL     = 3 * time.Minute
	tgWebLoginMax     = 64 // pending requests, bounds the public endpoint
	tgWebLoginPerIP   = 5  // requests per client within tgWebLoginTTL
	tgWebLoginPrefix  = "weblogin_"
	tgWebLoginChoices = 4
)

// tgWebLogins keeps sign-in requests in the shared store: the page polls
// whichever panel instance it reaches, while the leader's bot approves.
type tgWebLogins struct {
	eph  *ephStore
	name string // key prefix
}

func (w *tgWebLogins) key(id string) string { return w.name + ":" + id }

type tgWebLogin struct {
	Secret  string `json:"s"`
	Code    int    `json:"c"`
	IP      string `json:"ip"`
	UA      string `json:"ua"`
	Chat    int64  `json:"ch,omitempty"` // chat that opened it; 0 = not yet
	Subject string `json:"su,omitempty"` // who signed in (Telegram admin or user id), set once approved
	Denied  bool   `json:"d,omitempty"`
}

func (s *Server) tgWebLoginEnabled() bool {
	s.tg.mu.Lock()
	defer s.tg.mu.Unlock()
	// Another instance (the leader) may be the one polling the bot.
	return s.tg.me != "" && (s.tg.running || !s.isLeader())
}

// tgWebLoginStart: POST /auth/tg/start (public).
func (s *Server) tgWebLoginStart(c *gin.Context) {
	if !s.tgWebLoginEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Telegram 机器人未运行"})
		return
	}
	s.startWebLogin(c, &s.tgWeb, tgWebLoginPrefix)
}

// startWebLogin opens a sign-in request in w; the bot is reached with
// /start <prefix><id>. Shared by the admin login page and the user center.
func (s *Server) startWebLogin(c *gin.Context, w *tgWebLogins, prefix string) {
	// Per IP first: one client alone must not fill the shared cap and lock
	// everyone else out of Telegram sign-in.
	if w.eph.count(w.name+":") >= tgWebLoginMax || !s.countWindow(w.name+"-ip:"+clientIP(c.Request), tgWebLoginPerIP, tgWebLoginTTL) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登录请求过多，请稍后再试"})
		return
	}
	id := pki.Token(12)
	req := &tgWebLogin{Secret: pki.Token(24), Code: 10 + rand.IntN(90), IP: clientIP(c.Request), UA: c.Request.UserAgent()}
	if err := w.eph.put(w.key(id), req, tgWebLoginTTL); err != nil {
		serverError(c, err)
		return
	}
	s.tg.mu.Lock()
	bot := s.tg.me
	s.tg.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"id": id, "secret": req.Secret, "code": req.Code,
		"url":       "https://t.me/" + bot + "?start=" + prefix + id,
		"bot":       bot,
		"expiresIn": int(tgWebLoginTTL.Seconds()),
	})
}

// tgWebLoginPoll: POST /auth/tg/poll {id, secret} (public).
// status: pending | ok (with token) | denied | expired.
func (s *Server) tgWebLoginPoll(c *gin.Context) {
	adminID, _, ok := s.pollWebLogin(c, &s.tgWeb)
	if !ok {
		return
	}
	var a store.TGAdmin
	if s.st.DB.First(&a, "id = ?", adminID).Error != nil {
		c.JSON(http.StatusOK, gin.H{"status": "denied"})
		return
	}
	tok, err := s.issueTGToken(&a, c)
	if err != nil {
		serverError(c, err)
		return
	}
	s.audit("tg:"+a.Name, "admin.login", "tg:"+a.Name, "telegram web ("+a.Role+")", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "token": tok, "username": "tg:" + a.Name, "expiresIn": int(tgSessionTTL.Seconds())})
}

// pollWebLogin answers a poll for a request in w. Until the request is
// approved it writes the response itself and returns ok=false; once approved
// (single use) it returns the subject and the chat that approved it.
func (s *Server) pollWebLogin(c *gin.Context, w *tgWebLogins) (subject string, chat int64, ok bool) {
	var r struct {
		ID     string `json:"id" binding:"required"`
		Secret string `json:"secret" binding:"required"`
	}
	if err := c.ShouldBindJSON(&r); err != nil {
		badRequest(c, err)
		return "", 0, false
	}
	var req tgWebLogin
	status := ""
	found := w.eph.update(w.key(r.ID), &req, func() ephAction {
		switch {
		case subtle.ConstantTimeCompare([]byte(req.Secret), []byte(r.Secret)) != 1:
			status = "expired"
			return ephKeep
		case req.Denied:
			status = "denied"
			return ephDelete
		case req.Subject != "":
			return ephDelete // single use
		default:
			status = "pending"
			return ephKeep
		}
	})
	switch {
	case !found || status == "expired":
		c.JSON(http.StatusOK, gin.H{"status": "expired"})
		return "", 0, false
	case status == "denied":
		c.JSON(http.StatusOK, gin.H{"status": "denied"})
		return "", 0, false
	case status == "pending":
		c.JSON(http.StatusOK, gin.H{"status": "pending", "opened": req.Chat != 0})
		return "", 0, false
	}
	return req.Subject, req.Chat, true
}

// webLoginPrompt answers /start weblogin_<id> with the number choices.
func (b *tgBot) webLoginPrompt(chat int64, reqID string, id tgIdentity) {
	if id.admin == nil {
		b.send(chat, "🚫 只有已绑定的管理员可以用 Telegram 登录管理后台（普通用户请使用用户中心）。", backRow(""))
		return
	}
	scope := "完整权限"
	if !id.owner() {
		scope = fmt.Sprintf("仅分配给你的 %d 个节点", len(id.admin.NodeIDs.V))
	}
	b.loginPrompt(&b.s.tgWeb, chat, reqID, "wl", "网页登录请求", "🛡 权限："+scope, "⌛ 登录请求已失效，请在网页上重新点击「使用 Telegram 登录」。")
}

// loginPrompt claims a sign-in request for chat and sends the number choices
// (callback <cb>:<id>:<n|x>). A request belongs to the first chat that opens
// it, so a forwarded link cannot be answered by someone else.
func (b *tgBot) loginPrompt(w *tgWebLogins, chat int64, reqID, cb, title, extra, stale string) {
	var req tgWebLogin
	claimed := false
	w.eph.update(w.key(reqID), &req, func() ephAction {
		if req.Denied || req.Subject != "" || (req.Chat != 0 && req.Chat != chat) {
			return ephKeep
		}
		req.Chat, claimed = chat, true
		return ephSave
	})
	if !claimed {
		b.send(chat, stale, backRow(""))
		return
	}
	code, ip, ua := req.Code, req.IP, req.UA

	choices := []int{code}
	for len(choices) < tgWebLoginChoices {
		n := 10 + rand.IntN(90)
		dup := false
		for _, c := range choices {
			dup = dup || c == n
		}
		if !dup {
			choices = append(choices, n)
		}
	}
	rand.Shuffle(len(choices), func(i, j int) { choices[i], choices[j] = choices[j], choices[i] })
	nums := make([]button, len(choices))
	for i, n := range choices {
		nums[i] = button{Text: fmt.Sprint(n), Data: fmt.Sprintf("%s:%s:%d", cb, reqID, n)}
	}
	text := fmt.Sprintf("🔐 <b>%s</b>\n%s\n🌐 IP：<code>%s</code>\n💻 设备：%s\n%s\n\n请点击<b>网页上显示的数字</b>完成登录。\n不是你本人操作？点「拒绝」。",
		title, tgRule, esc(orDefault(ip, "未知")), esc(shortUA(ua)), extra)
	b.send(chat, text, nums, []button{{Text: "❌ 拒绝", Data: cb + ":" + reqID + ":x"}})
}

// loginPick records the number chat tapped for a request: subject signs in
// when it matches. The result is "ok", "denied" (rejected or wrong number:
// "wrong") or "stale".
func (b *tgBot) loginPick(w *tgWebLogins, chat int64, reqID, pick, subject string) (result, ip string) {
	var req tgWebLogin
	result = "stale"
	w.eph.update(w.key(reqID), &req, func() ephAction {
		if req.Chat != chat || req.Denied || req.Subject != "" || subject == "" {
			return ephKeep
		}
		ip = req.IP
		switch {
		case pick == fmt.Sprint(req.Code):
			req.Subject, result = subject, "ok"
		case pick == "x":
			req.Denied, result = true, "denied"
		default:
			req.Denied, result = true, "wrong"
		}
		return ephSave
	})
	return result, ip
}

// webLoginAnswer handles the wl:<id>:<n|x> callback.
func (b *tgBot) webLoginAnswer(chat, msg int64, reqID, pick string, id tgIdentity, answer func(string)) {
	subject := ""
	if id.admin != nil {
		subject = id.admin.ID
	}
	res, ip := b.loginPick(&b.s.tgWeb, chat, reqID, pick, subject)
	b.loginResult(chat, msg, res, ip, "网页将自动进入面板。", answer)
	if res == "denied" || res == "wrong" {
		b.s.audit("tg:"+id.admin.Name, "admin.login_denied", "tg:"+id.admin.Name, map[string]string{"denied": "telegram web", "wrong": "telegram web: wrong number"}[res], ip)
	}
}

// loginResult edits the prompt to show what the tap did.
func (b *tgBot) loginResult(chat, msg int64, res, ip, next string, answer func(string)) {
	switch res {
	case "ok":
		answer("已确认")
		b.edit(chat, msg, fmt.Sprintf("✅ <b>已确认登录</b>\n%s\n🌐 IP：<code>%s</code>\n%s", tgRule, esc(ip), next), backRow(""))
	case "denied":
		answer("已拒绝")
		b.edit(chat, msg, "🚫 已拒绝本次登录。", backRow(""))
	case "wrong":
		answer("数字不匹配")
		b.edit(chat, msg, "⚠️ 数字与网页不一致，已拒绝本次登录。\n如果不是你本人发起的请求，可能有人试图冒用你的账号。", backRow(""))
	default:
		answer("请求已失效")
		b.edit(chat, msg, "⌛ 登录请求已失效。", backRow(""))
	}
}

// shortUA reduces a user agent to "Browser · OS".
func shortUA(ua string) string {
	if ua == "" {
		return "未知"
	}
	pick := func(pairs ...string) string {
		for i := 0; i < len(pairs); i += 2 {
			if strings.Contains(ua, pairs[i]) {
				return pairs[i+1]
			}
		}
		return ""
	}
	browser := pick("Edg/", "Edge", "OPR/", "Opera", "Firefox/", "Firefox", "Chrome/", "Chrome", "Safari/", "Safari")
	os := pick("Windows", "Windows", "Android", "Android", "iPhone", "iOS", "iPad", "iPadOS", "Mac OS X", "macOS", "Linux", "Linux")
	if browser == "" && os == "" {
		if len(ua) > 40 {
			ua = ua[:40] + "…"
		}
		return ua
	}
	return strings.Trim(browser+" · "+os, " ·")
}
