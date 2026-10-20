package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	qrcode "github.com/skip2/go-qrcode"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// The Telegram bot talks to the Bot API by long polling (no public webhook
// needed). Roles:
//   - owner: every node and user, all notifications
//   - operator: only bound nodes (query, sync, restart cores, rollback)
//   - user: bound via /bind <subscription link>; self-service and reminders

type tgBot struct {
	s    *Server
	http *http.Client

	mu      sync.Mutex
	starsMu sync.Mutex // serializes native checkout link creation
	token   string
	base    string
	me      string // bot username
	selfID  int64  // stable bot identity, even when the username changes
	meToken string // token me was learnt with (instances that do not poll)
	lastErr string
	running bool
}

type bindCode struct {
	Role    string
	Name    string
	NodeIDs []string
	Expires time.Time
}

func newTGBot(s *Server) *tgBot {
	return &tgBot{s: s, http: &http.Client{Timeout: 70 * time.Second}}
}

// putBindCode stores a one-time admin bind code in the shared store: the
// admin UI of any instance issues it, the leader's bot redeems it.
func (b *tgBot) putBindCode(code string, c bindCode) {
	_ = b.s.eph.put("tgbind:"+strings.ToUpper(code), c, time.Until(c.Expires))
}

// --- Bot API ---

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
	Callback *struct {
		ID      string     `json:"id"`
		From    tgUser     `json:"from"`
		Message *tgMessage `json:"message"`
		Data    string     `json:"data"`
	} `json:"callback_query"`
	PreCheckout *tgPreCheckout `json:"pre_checkout_query"`
}

type tgMessage struct {
	MessageID int64  `json:"message_id"`
	From      tgUser `json:"from"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text              string     `json:"text"`
	SuccessfulPayment *tgPayment `json:"successful_payment"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type button struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
}

func (b *tgBot) creds() (string, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.token, b.base
}

func (b *tgBot) call(ctx context.Context, method string, params any, out any) error {
	token, base := b.creds()
	if token == "" {
		return errors.New("bot token not configured")
	}
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return b.do(req, out)
}

func (b *tgBot) do(req *http.Request, out any) error {
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&env); err != nil {
		return fmt.Errorf("telegram: %s", resp.Status)
	}
	if !env.OK {
		return fmt.Errorf("telegram: %s", env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (b *tgBot) send(chat int64, text string, rows ...[]button) {
	p := map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}
	if len(rows) > 0 {
		p["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.call(ctx, "sendMessage", p, nil); err != nil {
		b.s.log.Warn("telegram send", "chat", chat, "err", err)
	}
}

func (b *tgBot) edit(chat, msg int64, text string, rows ...[]button) {
	p := map[string]any{"chat_id": chat, "message_id": msg, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}
	if len(rows) > 0 {
		p["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.call(ctx, "editMessageText", p, nil); err != nil && !strings.Contains(err.Error(), "not modified") {
		b.send(chat, text, rows...)
	}
}

func (b *tgBot) sendPhoto(chat int64, png []byte, caption string) {
	token, base := b.creds()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", strconv.FormatInt(chat, 10))
	_ = w.WriteField("caption", caption)
	_ = w.WriteField("parse_mode", "HTML")
	fw, _ := w.CreateFormFile("photo", "qr.png")
	_, _ = fw.Write(png)
	w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/sendPhoto", &buf)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if err := b.do(req, nil); err != nil {
		b.s.log.Warn("telegram photo", "err", err)
	}
}

// sendBackup sends a backup archive to every owner (Bot API limit: 50 MB).
func (b *tgBot) sendBackup(path string) {
	if !b.isRunning() {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var owners []store.TGAdmin
	b.s.st.DB.Where("role = ?", "owner").Find(&owners)
	name := filepath.Base(path)
	note := "包含数据库与内部 CA，未加密：恢复需使用同一 secret_key 的面板。请妥善保管。"
	if strings.HasSuffix(name, ".enc") {
		note = "已用备份口令加密，附带密钥：可恢复到任何服务器（需要口令）。"
	}
	caption := fmt.Sprintf("🗄 <b>面板备份</b>\n%s\n📄 %s\n📦 %s\n\n%s", tgRule, esc(name), sub.HumanBytes(int64(len(data))), note)
	for _, a := range owners {
		if len(data) > 49<<20 {
			b.send(a.ChatID, "⚠️ 备份 "+esc(name)+" 超过 Telegram 50 MB 限制，未发送；请在面板「设置 → 备份与恢复」下载。")
			continue
		}
		b.sendDocument(a.ChatID, name, data, caption)
	}
}

func (b *tgBot) sendDocument(chat int64, name string, data []byte, caption string) {
	token, base := b.creds()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", strconv.FormatInt(chat, 10))
	_ = w.WriteField("caption", caption)
	_ = w.WriteField("parse_mode", "HTML")
	fw, _ := w.CreateFormFile("document", name)
	_, _ = fw.Write(data)
	w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/sendDocument", &buf)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if err := b.do(req, nil); err != nil {
		b.s.log.Warn("telegram document", "err", err)
	}
}

// --- lifecycle ---

// run keeps a poller alive while the bot is enabled; settings changes are
// picked up within a few seconds.
func (b *tgBot) run(ctx context.Context) {
	for ctx.Err() == nil {
		enabled := b.s.st.GetSetting("tg_enabled", "false") == "true"
		token := b.s.st.Box.MustOpen(b.s.st.GetSetting("tg_token", ""))
		base := strings.TrimRight(orDefault(b.s.st.GetSetting("tg_api_base", ""), "https://api.telegram.org"), "/")
		if !enabled || token == "" {
			b.setRunning(false, "")
			sleep(ctx, 5*time.Second)
			continue
		}
		b.mu.Lock()
		b.token, b.base = token, base // notifications go out from every instance
		b.mu.Unlock()
		if !b.s.isLeader() {
			// Only one instance may poll a bot (Telegram answers the others
			// with a conflict); the leader does. This one still needs the
			// bot's name for sign-in links.
			b.setRunning(false, "")
			b.learnName(ctx, token)
			sleep(ctx, 5*time.Second)
			continue
		}
		b.poll(ctx, token, base)
		sleep(ctx, 3*time.Second)
	}
}

// learnName fills in the bot's username on an instance that does not poll.
func (b *tgBot) learnName(ctx context.Context, token string) {
	b.mu.Lock()
	known := b.me != "" && b.meToken == token
	b.mu.Unlock()
	if known {
		return
	}
	var me tgUser
	if b.call(ctx, "getMe", map[string]any{}, &me) == nil {
		b.mu.Lock()
		b.me, b.selfID, b.meToken = me.Username, me.ID, token
		b.mu.Unlock()
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (b *tgBot) setRunning(on bool, errMsg string) {
	b.mu.Lock()
	b.running, b.lastErr = on, errMsg
	b.mu.Unlock()
}

// poll runs until the token/base change, the bot is disabled or an error occurs.
func (b *tgBot) poll(ctx context.Context, token, base string) {
	var me tgUser
	if err := b.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		b.setRunning(false, err.Error())
		sleep(ctx, 20*time.Second)
		return
	}
	b.mu.Lock()
	b.me, b.selfID = me.Username, me.ID
	b.mu.Unlock()
	b.setRunning(true, "")
	b.registerCommands(ctx)
	var offset int64
	for ctx.Err() == nil {
		if !b.s.isLeader() || b.s.st.GetSetting("tg_enabled", "false") != "true" || b.s.st.Box.MustOpen(b.s.st.GetSetting("tg_token", "")) != token ||
			strings.TrimRight(orDefault(b.s.st.GetSetting("tg_api_base", ""), "https://api.telegram.org"), "/") != base {
			return
		}
		var ups []tgUpdate
		pctx, cancel := context.WithTimeout(ctx, 65*time.Second)
		err := b.call(pctx, "getUpdates", map[string]any{"offset": offset, "timeout": b.pollTimeout(), "allowed_updates": []string{"message", "callback_query", "pre_checkout_query"}}, &ups)
		cancel()
		if err != nil {
			msg := err.Error()
			if strings.Contains(msg, "Conflict") || strings.Contains(msg, "terminated by other getUpdates") {
				// Another process polls the same bot: messages go to whichever
				// asks first, e.g. an old panel still running after a reinstall,
				// which then answers with its own (different) accounts.
				msg = "另一个程序正在使用同一个 Bot Token 接收消息（常见于重装后旧面板仍在运行，或另一台服务器配置了同一个机器人）。同一个机器人只能由一个面板使用：请停掉另一处，或为本面板创建新的机器人。"
			}
			b.setRunning(false, msg)
			return
		}
		b.setRunning(true, "")
		for _, u := range ups {
			offset = u.UpdateID + 1
			b.handle(u)
		}
	}
}

// pollTimeout is shortened in tests via the tg_poll_timeout setting.
func (b *tgBot) pollTimeout() int {
	n, err := strconv.Atoi(b.s.st.GetSetting("tg_poll_timeout", "50"))
	if err != nil || n < 0 {
		return 50
	}
	return n
}

func (b *tgBot) registerCommands(ctx context.Context) {
	cmds := []map[string]string{
		{"command": "menu", "description": "🏠 主菜单"}, {"command": "me", "description": "📊 我的流量与到期"},
		{"command": "buy", "description": "🛒 购买套餐"}, {"command": "redeem", "description": "🎟 兑换码 /redeem 码"},
		{"command": "orders", "description": "📋 我的订单"}, {"command": "terms", "description": "📄 购买条款"}, {"command": "paysupport", "description": "💬 支付与售后"},
		{"command": "sub", "description": "🔗 订阅链接与二维码"}, {"command": "nodes", "description": "🌐 节点"},
		{"command": "rename", "description": "✏️ 修改用户名 /rename 新用户名"},
		{"command": "status", "description": "📈 概览（管理员）"}, {"command": "users", "description": "👥 用户（管理员）"},
		{"command": "traffic", "description": "🏆 今日排行（管理员）"}, {"command": "login", "description": "🛠 登录管理后台（管理员）"},
		{"command": "update", "description": "⬆️ 版本更新（管理员）"},
		{"command": "bind", "description": "绑定账号 /bind 订阅链接"}, {"command": "help", "description": "❓ 帮助"},
	}
	_ = b.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// --- identities ---

type tgIdentity struct {
	admin *store.TGAdmin
	user  *store.User
	chat  int64
}

func (b *tgBot) identify(chat int64) tgIdentity {
	id := tgIdentity{chat: chat}
	var a store.TGAdmin
	if b.s.st.DB.First(&a, "chat_id = ?", chat).Error == nil {
		id.admin = &a
	}
	var u store.User
	if b.s.st.DB.First(&u, "telegram_id = ? AND is_service = ?", chat, false).Error == nil {
		id.user = &u
	}
	return id
}

func (id tgIdentity) owner() bool { return id.admin != nil && id.admin.Role == "owner" }

// canNode reports whether the admin may see/operate a node.
func (id tgIdentity) canNode(nodeID string) bool {
	if id.admin == nil {
		return false
	}
	if id.admin.Role == "owner" {
		return true
	}
	for _, n := range id.admin.NodeIDs.V {
		if n == nodeID {
			return true
		}
	}
	return false
}

// --- dispatch ---

func (b *tgBot) handle(u tgUpdate) {
	defer func() {
		if r := recover(); r != nil {
			b.s.log.Error("telegram handler panic", "panic", r)
		}
	}()
	// Identity is the chat ID, so only private chats (where chat ID == the
	// person's user ID) are served: in a group every member would otherwise
	// act with the group's binding.
	if u.Callback != nil && u.Callback.Message != nil {
		m := u.Callback.Message
		if m.Chat.Type != "private" || u.Callback.From.ID != m.Chat.ID {
			return
		}
		b.onCallback(u.Callback.ID, m.Chat.ID, m.MessageID, u.Callback.Data, u.Callback.From)
		return
	}
	if u.PreCheckout != nil {
		b.onPreCheckout(u.PreCheckout)
		return
	}
	if u.Message != nil && u.Message.SuccessfulPayment != nil && u.Message.Chat.Type == "private" {
		b.onStarsPaid(u.Message.Chat.ID, u.Message.SuccessfulPayment)
		return
	}
	if u.Message == nil || u.Message.Text == "" {
		return
	}
	chat := u.Message.Chat.ID
	if u.Message.Chat.Type != "private" {
		if strings.HasPrefix(u.Message.Text, "/") {
			b.send(chat, "出于安全考虑，请私聊机器人使用。")
		}
		return
	}
	cmd, arg, _ := strings.Cut(strings.TrimSpace(u.Message.Text), " ")
	cmd = strings.ToLower(strings.SplitN(cmd, "@", 2)[0])
	arg = strings.TrimSpace(arg)
	id := b.identify(chat)
	switch cmd {
	case "/start", "/menu":
		if cmd == "/start" && strings.HasPrefix(arg, portalLoginPrefix) { // user center
			b.portalLoginPrompt(chat, strings.TrimPrefix(arg, portalLoginPrefix), id)
			return
		}
		if cmd == "/start" && strings.HasPrefix(arg, tgWebLoginPrefix) { // login page
			b.webLoginPrompt(chat, strings.TrimPrefix(arg, tgWebLoginPrefix), id)
			return
		}
		if cmd == "/start" && arg != "" { // deep link t.me/bot?start=<sub token>
			b.bind(chat, u.Message.From, arg, id)
			return
		}
		text, rows := b.view("home", "", id)
		b.send(chat, text, rows...)
	case "/buy", "/shop":
		text, rows := b.view("shop", "", id)
		b.send(chat, text, rows...)
	case "/redeem":
		b.redeemCommand(chat, u.Message.From, arg, id)
	case "/terms":
		b.send(chat, b.termsView(), backRow(""))
	case "/paysupport", "/support":
		b.send(chat, b.supportView(), backRow(""))
	case "/orders":
		text, rows := b.ordersView(chat)
		b.send(chat, text, rows...)
	case "/help":
		text, rows := b.view("help", "", id)
		b.send(chat, text, rows...)
	case "/whoami":
		b.send(chat, fmt.Sprintf("Chat ID：<code>%d</code>\n%s", chat, b.roleLine(id)))
	case "/bind":
		b.bind(chat, u.Message.From, arg, id)
	case "/rename":
		if id.user == nil {
			text, rows := b.view("bindhelp", "", id)
			b.send(chat, text, rows...)
			return
		}
		if arg == "" {
			b.send(chat, b.renameView(id.user), backRow("v:me"))
			return
		}
		if err := b.s.renameSelf(id.user, arg, "tg"); err != nil {
			b.send(chat, "❌ "+esc(err.Error()), []button{{Text: "✏️ 修改说明", Data: "v:rename"}}, backRow("v:me"))
			return
		}
		b.send(chat, "✅ 用户名已改为 <b>"+esc(id.user.Username)+"</b>\n订阅链接和节点不受影响，无需重新导入。\n\n"+b.userCard(id.user), b.userButtons(id.user)...)
	case "/unbind":
		if id.user != nil {
			b.s.st.DB.Model(&store.User{}).Where("id = ?", id.user.ID).Update("telegram_id", 0)
			b.send(chat, "已解除与账号 <b>"+esc(id.user.Username)+"</b> 的绑定")
		} else {
			b.send(chat, "当前没有绑定账号")
		}
	case "/me":
		if id.user == nil {
			text, rows := b.view("bindhelp", "", id)
			b.send(chat, text, rows...)
			return
		}
		b.send(chat, b.userCard(id.user), b.userButtons(id.user)...)
	case "/sub":
		if id.user == nil {
			text, rows := b.view("bindhelp", "", id)
			b.send(chat, text, rows...)
			return
		}
		b.sendSub(chat, id.user)
	case "/nodes":
		if id.admin == nil {
			if id.user != nil {
				b.userNodes(chat, id.user)
			} else {
				b.send(chat, "无权限")
			}
			return
		}
		b.adminCommand(cmd, arg, chat, id)
	case "/node", "/status", "/users", "/user", "/traffic", "/update", "/upgrade":
		b.adminCommand(cmd, arg, chat, id)
	case "/login":
		if id.admin == nil {
			b.send(chat, "仅管理员可用。普通用户请使用 /me、/sub、/nodes 查看自己的账号与节点。")
			return
		}
		b.sendLoginLink(chat, id)
	case "/notify":
		if id.admin == nil {
			return
		}
		on := arg != "off"
		b.s.st.DB.Model(&store.TGAdmin{}).Where("id = ?", id.admin.ID).Update("notify", on)
		b.send(chat, map[bool]string{true: "已开启告警通知", false: "已关闭告警通知"}[on])
	default:
		if strings.HasPrefix(cmd, "/") {
			b.send(chat, "未知命令，打开 /menu 主菜单或发送 /help 查看命令", backRow(""))
		}
	}
}

func (b *tgBot) roleLine(id tgIdentity) string {
	var parts []string
	if id.admin != nil {
		if id.admin.Role == "owner" {
			parts = append(parts, "身份：超级管理员")
		} else {
			parts = append(parts, fmt.Sprintf("身份：节点管理员（%d 个节点）", len(id.admin.NodeIDs.V)))
		}
	}
	if id.user != nil {
		parts = append(parts, "绑定账号："+esc(id.user.Username))
	}
	if len(parts) == 0 {
		return "未绑定"
	}
	return strings.Join(parts, "\n")
}

// bind accepts an admin bind code or a subscription link/token.
func (b *tgBot) bind(chat int64, from tgUser, arg string, id tgIdentity) {
	if arg == "" {
		b.send(chat, "用法：<code>/bind 订阅链接</code>（管理员使用后台生成的绑定码）")
		return
	}
	var code bindCode
	if isCode := b.s.eph.take("tgbind:"+strings.ToUpper(arg), &code); isCode {
		name := orDefault(code.Name, orDefault(from.Username, from.FirstName))
		a := store.TGAdmin{ID: store.NewID(), ChatID: chat, Name: name, Role: code.Role, NodeIDs: store.NewJSON(nonNilStrs(code.NodeIDs)), Notify: true}
		if id.admin != nil {
			a.ID = id.admin.ID
		}
		if err := b.s.st.DB.Save(&a).Error; err != nil {
			b.send(chat, "绑定失败："+esc(err.Error()))
			return
		}
		b.s.audit("telegram", "tg.admin_bind", name, fmt.Sprintf("chat %d role %s", chat, code.Role), "")
		b.send(chat, "✅ 已绑定为"+map[string]string{"owner": "超级管理员", "operator": "节点管理员"}[code.Role]+"。", backRow(""))
		return
	}
	u, problem := b.s.userFromSubLink(arg)
	if u == nil {
		b.send(chat, problem)
		return
	}
	b.s.st.DB.Model(&store.User{}).Where("telegram_id = ?", chat).Update("telegram_id", 0)
	b.s.st.DB.Model(u).Update("telegram_id", chat)
	b.s.audit("telegram", "tg.user_bind", u.Username, fmt.Sprintf("chat %d", chat), "")
	u.TelegramID = chat
	b.send(chat, "✅ 已绑定账号 <b>"+esc(u.Username)+"</b>\n\n"+b.userCard(u), b.userButtons(u)...)
}

// --- user views ---

func (b *tgBot) sendSub(chat int64, u *store.User) {
	link := b.s.subURL(u.SubToken)
	if png, err := qrcode.Encode(link, qrcode.Medium, 512); err == nil {
		b.sendPhoto(chat, png, "📷 用客户端扫码导入订阅")
	}
	center := ""
	if b.s.portalEnabled() {
		center = "\n\n👤 <b>用户中心</b>（查看流量、到期，用 Telegram 登录）：\n" + esc(b.s.portalURL())
	}
	b.send(chat, "🔗 <b>订阅链接</b>\n"+tgRule+"\n<code>"+esc(link)+"</code>\n\n点按链接即可复制，客户端类型自动识别（Clash / sing-box / v2rayN / Shadowrocket 等）。"+center,
		[]button{{Text: "♻️ 重置链接", Data: "me:reset"}, btnHome})
}

// --- admin views ---

// --- callbacks ---

// tgProText explains locked admin menus.
const tgProText = "🔒 <b>管理菜单需要专业版</b>\n" + tgRule + "\n节点管理、用户列表、流量排行和版本更新菜单需要专业版许可证（面板 → 许可证）。\n\n告警通知、绑定账号和用户自助功能照常可用。"

// tgAdvanced reports callbacks of the admin menus (a paid feature); user
// self-service, sign-in, alerts and the home screen stay free.
func tgAdvanced(parts []string) bool {
	switch parts[0] {
	case "me", "ul", "wl", "m", "s": // self-service, user center and panel sign-in, alert toggle, shop
		return false
	case "v":
		if len(parts) < 2 {
			return false
		}
		switch parts[1] {
		case "", "me", "rename", "nodes", "bindhelp", "help", "pro", "home", "shop", "redeem", "terms", "paysupport", "orders", "packs", "unpaid":
			return false
		}
	}
	return true
}

func (b *tgBot) onCallback(cbID string, chat, msg int64, data string, from tgUser) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	answer := func(text string) {
		_ = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": cbID, "text": text}, nil)
	}
	id := b.identify(chat)
	parts := strings.Split(data, ":")
	if tgAdvanced(parts) && !b.s.license().Allow(license.FeatureTGMenu) {
		answer("管理菜单需要专业版许可证")
		b.edit(chat, msg, tgProText, backRow(""))
		return
	}
	if tgLicenseManagementWrite(parts) && b.s.license().ManagementReadOnly {
		answer(licenseReadOnlyMessage)
		return
	}
	switch parts[0] {
	case "me":
		if id.user == nil {
			answer("尚未绑定账号")
			return
		}
		switch parts[1] {
		case "card":
			b.edit(chat, msg, b.userCard(id.user), b.userButtons(id.user)...)
		case "sub":
			b.sendSub(chat, id.user)
		case "nodes":
			b.edit(chat, msg, b.userNodesText(id.user), backRow("v:nodes"))
		case "reset":
			b.send(chat, "⚠️ 确定重置订阅链接？\n旧链接和已导入的节点将立即失效，所有客户端都需要重新导入。", []button{{Text: "确认重置", Data: "me:reset!"}, btnHome})
		case "reset!":
			if err := b.s.rotateUserCredentials(id.user); err != nil {
				b.send(chat, "重置失败："+esc(err.Error()))
				break
			}
			b.s.audit("telegram", "user.reset_token", id.user.Username, "self-service", "")
			b.sendSub(chat, id.user)
		}
		answer("")
	case "s": // shop
		b.shopCallback(ctx, parts, chat, msg, from, id, answer)
	case "up", "n", "u": // admin menus: version update, nodes, users
		b.adminCallback(ctx, parts, chat, msg, id, answer)
	case "ul": // user center sign-in: ul:<request>:<number|x>
		if len(parts) == 3 {
			b.portalLoginAnswer(chat, msg, parts[1], parts[2], id, answer)
		}
	case "wl": // login page sign-in: wl:<request>:<number|x>
		if len(parts) == 3 {
			b.webLoginAnswer(chat, msg, parts[1], parts[2], id, answer)
		}
	case "v": // menu views edit the message in place
		view, arg := "home", ""
		if len(parts) > 1 {
			view = parts[1]
		}
		if len(parts) > 2 {
			arg = parts[2]
		}
		text, rows := b.view(view, arg, id)
		switch view {
		case "orders":
			text, rows = b.ordersView(chat)
		case "unpaid":
			text, rows = b.unpaidView(chat)
		}
		b.edit(chat, msg, text, rows...)
		answer("")
	case "m":
		if id.admin == nil || len(parts) < 2 {
			answer("无权限")
			return
		}
		switch parts[1] {
		case "login":
			b.sendLoginLink(chat, id)
			answer("")
		case "notify":
			on := !id.admin.Notify
			b.s.st.DB.Model(&store.TGAdmin{}).Where("id = ?", id.admin.ID).Update("notify", on)
			id.admin.Notify = on
			text, rows := b.view("home", "", id)
			b.edit(chat, msg, text, rows...)
			answer(map[bool]string{true: "已开启告警通知", false: "已关闭告警通知"}[on])
		}
	default:
		answer("")
	}
}

// --- notifications ---

// runNotifications delivers events to admins (scoped by node) and users.
func (b *tgBot) runNotifications(ctx context.Context, events <-chan Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-events:
			if !b.isRunning() || e.Text == "" { // no text: webhook-only events
				continue
			}
			var admins []store.TGAdmin
			if !e.UserOnly {
				b.s.st.DB.Where("notify = ?", true).Find(&admins)
			}
			for _, a := range admins {
				if a.Role == "owner" || (e.NodeID != "" && tgIdentity{admin: &a}.canNode(e.NodeID)) {
					b.send(a.ChatID, esc(e.Text))
				}
			}
			if e.UserID != "" && b.s.st.GetSetting("tg_notify_users", "true") == "true" {
				var u store.User
				if b.s.st.DB.First(&u, "id = ?", e.UserID).Error == nil && u.TelegramID != 0 {
					if e.UserOnly { // reminders are pre-formatted HTML
						b.send(u.TelegramID, e.Text)
					} else if custom := b.s.customReminder(e.Kind, &u); custom != "" {
						b.send(u.TelegramID, custom)
					} else {
						b.send(u.TelegramID, esc(userFacing(e)))
					}
				}
			}
		}
	}
}

func userFacing(e Event) string {
	switch e.Kind {
	case "user_limited":
		return "⛔ 你的流量已用尽，服务已暂停。续费或重置后自动恢复。"
	case "user_expired":
		return "⌛ 你的账号已到期，服务已暂停。"
	case "user_active":
		return "✅ 你的服务已恢复。"
	case "ip_limit":
		return "🚨 检测到同时在线设备超过限制，请检查订阅是否外泄。"
	}
	return e.Text
}

func (b *tgBot) isRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// remindUsers sends usage and expiry reminders to bound users (hourly):
// once when usage crosses remind_usage_pct (default 80, 0 = off) and daily
// within remind_expire_days of expiry (default 3, 0 = off).
func (s *Server) remindUsers() {
	pctAt := atoiDefault(s.st.GetSetting("remind_usage_pct", "80"), 80)
	expDays := atoiDefault(s.st.GetSetting("remind_expire_days", "3"), 3)
	var us []store.User
	s.st.DB.Where("(telegram_id <> 0 OR email <> '') AND is_service = ? AND status = ?", false, "active").Find(&us)
	now := time.Now()
	for _, u := range us {
		if u.TrafficLimit > 0 && pctAt > 0 {
			pct := float64(u.Used()) * 100 / float64(u.TrafficLimit)
			if pct >= float64(pctAt) && !u.WarnedUsage {
				s.st.DB.Model(&u).Update("warned_usage", true)
				text := s.customReminder("traffic_warn", &u)
				if text == "" {
					text = fmt.Sprintf("📊 <b>流量提醒</b>\n%s\n已使用 <b>%.0f%%</b>（%s / %s）\n剩余 <b>%s</b>\n%s",
						tgRule, pct, sub.HumanBytes(u.Used()), sub.HumanBytes(u.TrafficLimit), sub.HumanBytes(max(u.TrafficLimit-u.Used(), 0)), resetHint(&u))
				}
				s.notify(Event{Kind: "traffic_warn", UserID: u.ID, UserOnly: true, Text: text})
			} else if pct < float64(pctAt) && u.WarnedUsage {
				s.st.DB.Model(&u).Update("warned_usage", false)
			}
		}
		if expDays > 0 && u.ExpireAt != nil && u.ExpireAt.Sub(now) < time.Duration(expDays)*24*time.Hour && u.ExpireAt.After(now) &&
			(u.WarnedExpire == nil || now.Sub(*u.WarnedExpire) > 23*time.Hour) {
			s.st.DB.Model(&u).Update("warned_expire", now)
			text := s.customReminder("expire_warn", &u)
			if text == "" {
				text = fmt.Sprintf("⏰ <b>到期提醒</b>\n%s\n账号将于 <b>%s</b> 到期（%s）\n到期后服务会自动暂停，请及时续费。",
					tgRule, u.ExpireAt.Local().Format("2006-01-02 15:04"), sub.ExpiryLeft(sub.Info{Expire: u.ExpireAt}))
			}
			s.notify(Event{Kind: "expire_warn", UserID: u.ID, UserOnly: true, Text: text})
		}
	}
}

func resetHint(u *store.User) string {
	var i sub.Info
	planInfo(&i, u)
	if i.ResetNote != "" {
		return "♻️ " + i.ResetNote
	}
	return "用完后服务会自动暂停。"
}

// dailyReport sends owners a daily summary at tg_daily_report_hour (default
// 9, local time) unless tg_daily_report is "false".
func (s *Server) dailyReport() {
	if s.st.GetSetting("tg_daily_report", "true") != "true" || !s.tg.isRunning() {
		return
	}
	now := time.Now()
	if now.Hour() != min(max(atoiDefault(s.st.GetSetting("tg_daily_report_hour", "9"), 9), 0), 23) {
		return
	}
	today := now.Format("2006-01-02")
	if s.st.GetSetting("tg_daily_report_last", "") == today {
		return
	}
	_ = s.st.SetSetting("tg_daily_report_last", today)
	text := s.dailyReportText(now)
	var owners []store.TGAdmin
	s.st.DB.Where("role = ? AND notify = ?", "owner", true).Find(&owners)
	for _, a := range owners {
		s.tg.send(a.ChatID, text, []button{{Text: "📈 概览", Data: "v:status"}, btnHome})
	}
}

func (s *Server) dailyReportText(now time.Time) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 <b>每日报告</b> · %s\n%s\n", now.Format("2006-01-02"), tgRule)
	nodes := s.nodeViews()
	var off []string
	for _, n := range nodes {
		if n.Status == "offline" || n.Status == "error" {
			off = append(off, n.Name)
		}
	}
	fmt.Fprintf(&sb, "🖥 节点 %d/%d 在线", len(nodes)-len(off), len(nodes))
	if len(off) > 0 {
		fmt.Fprintf(&sb, "（异常：%s）", esc(joinCap(off, 5)))
	}
	sb.WriteString("\n")
	var active int64
	s.st.DB.Model(&store.User{}).Where("is_service = ? AND status = ?", false, "active").Count(&active)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	sum := func(from, to time.Time) (up, down int64) {
		var r struct{ Up, Down int64 }
		realUsers(s.st.DB.Model(&store.TrafficHourly{})).Select("COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").
			Where("hour >= ? AND hour < ?", from.UTC(), to.UTC()).Scan(&r)
		return r.Up, r.Down
	}
	yu, yd := sum(dayStart.AddDate(0, 0, -1), dayStart)
	mu, md := sum(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()), now)
	fmt.Fprintf(&sb, "👥 有效用户 %d\n📈 昨日 %s（↑ %s ↓ %s）\n🗓 本月 %s\n", active,
		sub.HumanBytes(yu+yd), sub.HumanBytes(yu), sub.HumanBytes(yd), sub.HumanBytes(mu+md))

	warn := time.Duration(max(atoiDefault(s.st.GetSetting("expire_warn_days", "7"), 7), 1)) * 24 * time.Hour
	list := func(filter string, label func(u store.User) string) (int64, string) {
		q := userFilter(s.st.DB.Model(&store.User{}).Where("is_service = ?", false), filter, now, warn)
		var n int64
		q.Session(&gorm.Session{}).Count(&n)
		var us []store.User
		q.Order("expire_at").Limit(8).Find(&us)
		names := make([]string, len(us))
		for i, u := range us {
			names[i] = label(u)
		}
		return n, joinCap(names, 8)
	}
	plain := func(u store.User) string { return u.Username }
	var items []string
	if n, l := list("limited", plain); n > 0 {
		items = append(items, fmt.Sprintf("⛔ 已超额 %d：%s", n, esc(l)))
	}
	if n, l := list("expired", plain); n > 0 {
		items = append(items, fmt.Sprintf("⌛ 已过期 %d：%s", n, esc(l)))
	}
	if n, l := list("expiring", func(u store.User) string { return u.Username + "（" + u.ExpireAt.Local().Format("01-02") + "）" }); n > 0 {
		items = append(items, fmt.Sprintf("⏰ %d 天内到期 %d：%s", int(warn.Hours()/24), n, esc(l)))
	}
	if n, l := list("near", func(u store.User) string {
		return fmt.Sprintf("%s（%d%%）", u.Username, u.Used()*100/max(u.TrafficLimit, 1))
	}); n > 0 {
		items = append(items, fmt.Sprintf("📊 流量将满 %d：%s", n, esc(l)))
	}
	sb.WriteString(tgRule + "\n")
	if len(items) == 0 {
		sb.WriteString("✅ 一切正常，没有超额或即将到期的用户")
	} else {
		sb.WriteString("⚠️ <b>需要关注</b>\n" + strings.Join(items, "\n"))
	}
	return sb.String()
}

func joinCap(xs []string, n int) string {
	if len(xs) > n {
		return strings.Join(xs[:n], "、") + " 等"
	}
	return strings.Join(xs, "、")
}

func esc(s string) string { return html.EscapeString(s) }

// --- API ---

func (s *Server) tgStatus(c *gin.Context) {
	s.tg.mu.Lock()
	defer s.tg.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"running": s.tg.running, "username": s.tg.me, "error": s.tg.lastErr})
}

func (s *Server) listTGAdmins(c *gin.Context) {
	var as []store.TGAdmin
	s.st.DB.Order("role desc, name").Find(&as)
	var bound int64
	s.st.DB.Model(&store.User{}).Where("telegram_id <> 0").Count(&bound)
	c.JSON(http.StatusOK, gin.H{"admins": as, "boundUsers": bound})
}

// tgBindCode issues a one-time code an admin sends to the bot as /bind CODE.
func (s *Server) tgBindCode(c *gin.Context) {
	var req struct {
		Role    string   `json:"role" binding:"required"`
		Name    string   `json:"name"`
		NodeIDs []string `json:"nodeIds"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Role != "owner" && req.Role != "operator" {
		badRequest(c, fmt.Errorf("role must be owner or operator"))
		return
	}
	code := strings.ToUpper(pki.Password(10))
	s.tg.putBindCode(code, bindCode{Role: req.Role, Name: req.Name, NodeIDs: req.NodeIDs, Expires: time.Now().Add(10 * time.Minute)})
	s.tg.mu.Lock()
	me := s.tg.me
	s.tg.mu.Unlock()
	s.auditCtx(c, "tg.bind_code", req.Role, req.Name)
	link := ""
	if me != "" {
		link = "https://t.me/" + me
	}
	c.JSON(http.StatusOK, gin.H{"code": code, "command": "/bind " + code, "expiresIn": 600, "bot": link})
}

func (s *Server) updateTGAdmin(c *gin.Context) {
	var req struct {
		Name    string   `json:"name"`
		Role    string   `json:"role"`
		NodeIDs []string `json:"nodeIds"`
		Notify  *bool    `json:"notify"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	var a store.TGAdmin
	if err := s.st.DB.First(&a, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	if req.Name != "" {
		a.Name = req.Name
	}
	if req.Role == "owner" || req.Role == "operator" {
		a.Role = req.Role
	}
	if req.NodeIDs != nil {
		a.NodeIDs = store.NewJSON(req.NodeIDs)
	}
	if req.Notify != nil {
		a.Notify = *req.Notify
	}
	s.st.DB.Save(&a)
	s.auditCtx(c, "tg.admin_update", a.Name, a.Role)
	c.JSON(http.StatusOK, a)
}

func (s *Server) deleteTGAdmin(c *gin.Context) {
	s.st.DB.Delete(&store.TGAdmin{}, "id = ?", c.Param("id"))
	s.auditCtx(c, "tg.admin_delete", c.Param("id"), "")
	c.Status(http.StatusNoContent)
}

func (s *Server) tgTest(c *gin.Context) {
	if !s.tg.isRunning() {
		badRequest(c, fmt.Errorf("机器人未运行：请检查 Token 与网络"))
		return
	}
	var as []store.TGAdmin
	s.st.DB.Find(&as)
	for _, a := range as {
		s.tg.send(a.ChatID, "🔔 Polaris 测试消息")
	}
	c.JSON(http.StatusOK, gin.H{"sent": len(as)})
}

// sendLoginLink sends a one-time panel sign-in link to a Telegram admin.
func (b *tgBot) sendLoginLink(chat int64, id tgIdentity) {
	link := b.s.tgLoginURL(b.s.newTGLoginCode(id.admin.ID))
	scope := "完整权限"
	if !id.owner() {
		scope = fmt.Sprintf("仅可查看与同步分配给你的 %d 个节点", len(id.admin.NodeIDs.V))
	}
	b.send(chat, fmt.Sprintf("🛠 <b>登录管理后台</b>\n%s\n<a href=\"%s\">👉 点此登录后台</a>\n地址：%s\n权限：%s\n\n⏱ 5 分钟内有效，只能使用一次，请勿转发。\n（这是管理员后台，不是用户中心）", tgRule, esc(link), esc(b.s.panelBase()), scope))
	b.s.audit("tg:"+id.admin.Name, "admin.login_link", "tg:"+id.admin.Name, "", "")
}
