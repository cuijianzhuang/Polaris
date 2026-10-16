package panel

import (
	"crypto/subtle"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
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
	tgWebLoginPrefix  = "weblogin_"
	tgWebLoginChoices = 4
)

type tgWebLogins struct {
	mu sync.Mutex
	m  map[string]*tgWebLogin
}

type tgWebLogin struct {
	Secret  string
	Code    int
	IP      string
	UA      string
	Expires time.Time
	Chat    int64  // admin chat that opened it; 0 = not yet
	AdminID string // set once approved
	Denied  bool
}

func (s *Server) tgWebLoginEnabled() bool {
	s.tg.mu.Lock()
	defer s.tg.mu.Unlock()
	return s.tg.running && s.tg.me != ""
}

// tgWebLoginStart: POST /auth/tg/start (public).
func (s *Server) tgWebLoginStart(c *gin.Context) {
	if !s.tgWebLoginEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Telegram 机器人未运行"})
		return
	}
	w := &s.tgWeb
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[string]*tgWebLogin{}
	}
	now := time.Now()
	for k, v := range w.m {
		if now.After(v.Expires) {
			delete(w.m, k)
		}
	}
	if len(w.m) >= tgWebLoginMax {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登录请求过多，请稍后再试"})
		return
	}
	id := pki.Token(12)
	req := &tgWebLogin{Secret: pki.Token(24), Code: 10 + rand.IntN(90), IP: clientIP(c.Request),
		UA: c.Request.UserAgent(), Expires: now.Add(tgWebLoginTTL)}
	w.m[id] = req
	s.tg.mu.Lock()
	bot := s.tg.me
	s.tg.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"id": id, "secret": req.Secret, "code": req.Code,
		"url":       "https://t.me/" + bot + "?start=" + tgWebLoginPrefix + id,
		"bot":       bot,
		"expiresIn": int(tgWebLoginTTL.Seconds()),
	})
}

// tgWebLoginPoll: POST /auth/tg/poll {id, secret} (public).
// status: pending | ok (with token) | denied | expired.
func (s *Server) tgWebLoginPoll(c *gin.Context) {
	var r struct {
		ID     string `json:"id" binding:"required"`
		Secret string `json:"secret" binding:"required"`
	}
	if err := c.ShouldBindJSON(&r); err != nil {
		badRequest(c, err)
		return
	}
	w := &s.tgWeb
	w.mu.Lock()
	req, ok := w.m[r.ID]
	if !ok || subtle.ConstantTimeCompare([]byte(req.Secret), []byte(r.Secret)) != 1 {
		w.mu.Unlock()
		c.JSON(http.StatusOK, gin.H{"status": "expired"})
		return
	}
	switch {
	case req.Denied:
		delete(w.m, r.ID)
		w.mu.Unlock()
		c.JSON(http.StatusOK, gin.H{"status": "denied"})
		return
	case req.AdminID != "":
		delete(w.m, r.ID) // single use
	case time.Now().After(req.Expires):
		delete(w.m, r.ID)
		w.mu.Unlock()
		c.JSON(http.StatusOK, gin.H{"status": "expired"})
		return
	default:
		w.mu.Unlock()
		c.JSON(http.StatusOK, gin.H{"status": "pending", "opened": req.Chat != 0})
		return
	}
	adminID := req.AdminID
	w.mu.Unlock()
	var a store.TGAdmin
	if s.st.DB.First(&a, "id = ?", adminID).Error != nil {
		c.JSON(http.StatusOK, gin.H{"status": "denied"})
		return
	}
	tok, err := s.issueTGToken(&a)
	if err != nil {
		serverError(c, err)
		return
	}
	s.audit("tg:"+a.Name, "admin.login", "tg:"+a.Name, "telegram web ("+a.Role+")", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"status": "ok", "token": tok, "username": "tg:" + a.Name, "expiresIn": int(tgSessionTTL.Seconds())})
}

// webLoginPrompt answers /start weblogin_<id> with the number choices.
func (b *tgBot) webLoginPrompt(chat int64, reqID string, id tgIdentity) {
	if id.admin == nil {
		b.send(chat, "🚫 只有已绑定的管理员可以用 Telegram 登录面板。", backRow(""))
		return
	}
	w := &b.s.tgWeb
	w.mu.Lock()
	req, ok := w.m[reqID]
	if ok && (time.Now().After(req.Expires) || req.Denied || req.AdminID != "" || (req.Chat != 0 && req.Chat != chat)) {
		ok = false
	}
	if !ok {
		w.mu.Unlock()
		b.send(chat, "⌛ 登录请求已失效，请在网页上重新点击「使用 Telegram 登录」。", backRow(""))
		return
	}
	req.Chat = chat
	code, ip, ua := req.Code, req.IP, req.UA
	w.mu.Unlock()

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
		nums[i] = button{Text: fmt.Sprint(n), Data: fmt.Sprintf("wl:%s:%d", reqID, n)}
	}
	scope := "完整权限"
	if !id.owner() {
		scope = fmt.Sprintf("仅分配给你的 %d 个节点", len(id.admin.NodeIDs.V))
	}
	text := fmt.Sprintf("🔐 <b>网页登录请求</b>\n%s\n🌐 IP：<code>%s</code>\n💻 设备：%s\n🛡 权限：%s\n\n请点击<b>网页上显示的数字</b>完成登录。\n不是你本人操作？点「拒绝」。",
		tgRule, esc(orDefault(ip, "未知")), esc(shortUA(ua)), scope)
	b.send(chat, text, nums, []button{{Text: "❌ 拒绝", Data: "wl:" + reqID + ":x"}})
}

// webLoginAnswer handles the wl:<id>:<n|x> callback.
func (b *tgBot) webLoginAnswer(chat, msg int64, reqID, pick string, id tgIdentity, answer func(string)) {
	w := &b.s.tgWeb
	w.mu.Lock()
	req, ok := w.m[reqID]
	if !ok || req.Chat != chat || req.Denied || req.AdminID != "" || time.Now().After(req.Expires) || id.admin == nil {
		w.mu.Unlock()
		answer("请求已失效")
		b.edit(chat, msg, "⌛ 登录请求已失效。", backRow(""))
		return
	}
	approved := pick == fmt.Sprint(req.Code)
	if approved {
		req.AdminID = id.admin.ID
	} else {
		req.Denied = true
	}
	ip := req.IP
	w.mu.Unlock()
	switch {
	case approved:
		answer("已确认")
		b.edit(chat, msg, fmt.Sprintf("✅ <b>已确认登录</b>\n%s\n🌐 IP：<code>%s</code>\n网页将自动进入面板。", tgRule, esc(ip)), backRow(""))
	case pick == "x":
		answer("已拒绝")
		b.edit(chat, msg, "🚫 已拒绝本次登录。", backRow(""))
		b.s.audit("tg:"+id.admin.Name, "admin.login_denied", "tg:"+id.admin.Name, "telegram web", ip)
	default:
		answer("数字不匹配")
		b.edit(chat, msg, "⚠️ 数字与网页不一致，已拒绝本次登录。\n如果不是你本人发起的请求，可能有人试图冒用你的账号。", backRow(""))
		b.s.audit("tg:"+id.admin.Name, "admin.login_denied", "tg:"+id.admin.Name, "telegram web: wrong number", ip)
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
