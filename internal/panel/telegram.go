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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	qrcode "github.com/skip2/go-qrcode"

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
	token   string
	base    string
	me      string // bot username
	lastErr string
	running bool
	codes   map[string]bindCode // one-time admin bind codes
}

type bindCode struct {
	Role    string
	Name    string
	NodeIDs []string
	Expires time.Time
}

func newTGBot(s *Server) *tgBot {
	return &tgBot{s: s, http: &http.Client{Timeout: 70 * time.Second}, codes: map[string]bindCode{}}
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
}

type tgMessage struct {
	MessageID int64  `json:"message_id"`
	From      tgUser `json:"from"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text string `json:"text"`
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
		b.token, b.base = token, base
		b.mu.Unlock()
		b.poll(ctx, token, base)
		sleep(ctx, 3*time.Second)
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
	b.me = me.Username
	b.mu.Unlock()
	b.setRunning(true, "")
	b.registerCommands(ctx)
	var offset int64
	for ctx.Err() == nil {
		if b.s.st.GetSetting("tg_enabled", "false") != "true" || b.s.st.Box.MustOpen(b.s.st.GetSetting("tg_token", "")) != token ||
			strings.TrimRight(orDefault(b.s.st.GetSetting("tg_api_base", ""), "https://api.telegram.org"), "/") != base {
			return
		}
		var ups []tgUpdate
		pctx, cancel := context.WithTimeout(ctx, 65*time.Second)
		err := b.call(pctx, "getUpdates", map[string]any{"offset": offset, "timeout": b.pollTimeout(), "allowed_updates": []string{"message", "callback_query"}}, &ups)
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
		{"command": "me", "description": "我的流量与到期"}, {"command": "sub", "description": "订阅链接与二维码"},
		{"command": "nodes", "description": "节点状态"}, {"command": "status", "description": "面板概览（管理员）"},
		{"command": "users", "description": "搜索用户（管理员）"}, {"command": "traffic", "description": "今日流量排行（管理员）"},
		{"command": "bind", "description": "绑定账号 /bind 订阅链接"}, {"command": "help", "description": "帮助"},
	}
	_ = b.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// --- identities ---

type tgIdentity struct {
	admin *store.TGAdmin
	user  *store.User
}

func (b *tgBot) identify(chat int64) tgIdentity {
	var id tgIdentity
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
		b.onCallback(u.Callback.ID, m.Chat.ID, m.MessageID, u.Callback.Data)
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
	case "/start", "/help":
		if cmd == "/start" && arg != "" { // deep link t.me/bot?start=<sub token>
			b.bind(chat, u.Message.From, arg, id)
			return
		}
		b.help(chat, id)
	case "/whoami":
		b.send(chat, fmt.Sprintf("Chat ID：<code>%d</code>\n%s", chat, b.roleLine(id)))
	case "/bind":
		b.bind(chat, u.Message.From, arg, id)
	case "/unbind":
		if id.user != nil {
			b.s.st.DB.Model(&store.User{}).Where("id = ?", id.user.ID).Update("telegram_id", 0)
			b.send(chat, "已解除与账号 <b>"+esc(id.user.Username)+"</b> 的绑定")
		} else {
			b.send(chat, "当前没有绑定账号")
		}
	case "/me":
		if id.user == nil {
			b.send(chat, "尚未绑定账号。发送 <code>/bind 你的订阅链接</code> 完成绑定。")
			return
		}
		b.send(chat, b.userCard(id.user), userButtons(id.user)...)
	case "/sub":
		if id.user == nil {
			b.send(chat, "尚未绑定账号。")
			return
		}
		b.sendSub(chat, id.user)
	case "/nodes":
		if id.admin != nil {
			b.adminNodes(chat, 0, id)
		} else if id.user != nil {
			b.userNodes(chat, id.user)
		} else {
			b.send(chat, "无权限")
		}
	case "/node":
		b.findNode(chat, arg, id)
	case "/status":
		if id.admin == nil {
			b.send(chat, "仅管理员可用")
			return
		}
		b.send(chat, b.statusText(id))
	case "/users", "/user":
		if !id.owner() {
			b.send(chat, "仅超级管理员可用")
			return
		}
		b.searchUsers(chat, arg)
	case "/traffic":
		if !id.owner() {
			b.send(chat, "仅超级管理员可用")
			return
		}
		b.send(chat, b.trafficText())
	case "/notify":
		if id.admin == nil {
			return
		}
		on := arg != "off"
		b.s.st.DB.Model(&store.TGAdmin{}).Where("id = ?", id.admin.ID).Update("notify", on)
		b.send(chat, map[bool]string{true: "已开启告警通知", false: "已关闭告警通知"}[on])
	default:
		if strings.HasPrefix(cmd, "/") {
			b.send(chat, "未知命令，发送 /help 查看可用命令")
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

func (b *tgBot) help(chat int64, id tgIdentity) {
	var sb strings.Builder
	sb.WriteString("<b>Polaris 机器人</b>\n" + b.roleLine(id) + "\n\n")
	sb.WriteString("/bind 订阅链接 — 绑定账号\n/me — 流量与到期\n/sub — 订阅链接与二维码\n/nodes — 节点状态\n/unbind — 解除绑定\n/whoami — 查看 Chat ID\n")
	if id.admin != nil {
		sb.WriteString("\n<b>管理员</b>\n/status — 概览\n/nodes — 节点列表与操作\n/node 名称 — 节点详情\n/notify on|off — 告警通知\n")
	}
	if id.owner() {
		sb.WriteString("/users 关键词 — 搜索用户\n/traffic — 今日流量排行\n")
	}
	b.send(chat, sb.String())
}

// bind accepts an admin bind code or a subscription link/token.
func (b *tgBot) bind(chat int64, from tgUser, arg string, id tgIdentity) {
	if arg == "" {
		b.send(chat, "用法：<code>/bind 订阅链接</code>（管理员使用后台生成的绑定码）")
		return
	}
	b.mu.Lock()
	code, isCode := b.codes[strings.ToUpper(arg)]
	if isCode {
		delete(b.codes, strings.ToUpper(arg))
	}
	b.mu.Unlock()
	if isCode {
		if time.Now().After(code.Expires) {
			b.send(chat, "绑定码已过期，请在后台重新生成")
			return
		}
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
		b.send(chat, "✅ 已绑定为"+map[string]string{"owner": "超级管理员", "operator": "节点管理员"}[code.Role]+"。发送 /help 查看命令。")
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
	b.send(chat, "✅ 已绑定账号 <b>"+esc(u.Username)+"</b>\n\n"+b.userCard(u), userButtons(u)...)
}

// --- user views ---

func (b *tgBot) userCard(u *store.User) string {
	st := map[string]string{"active": "✅ 正常", "limited": "⛔ 流量用尽", "expired": "⌛ 已到期", "disabled": "🚫 已禁用"}[u.Status]
	used := u.Used()
	limit := "不限"
	pct := ""
	if u.TrafficLimit > 0 {
		limit = sub.HumanBytes(u.TrafficLimit)
		pct = fmt.Sprintf("（%.1f%%）", float64(used)*100/float64(u.TrafficLimit))
	}
	exp := "永不到期"
	if u.ExpireAt != nil {
		days := int(time.Until(*u.ExpireAt).Hours() / 24)
		exp = fmt.Sprintf("%s（剩 %d 天）", u.ExpireAt.Local().Format("2006-01-02"), days)
	}
	online := "离线"
	if u.OnlineAt != nil && time.Since(*u.OnlineAt) < 2*time.Minute {
		online = fmt.Sprintf("在线（%d 个 IP）", len(u.OnlineIPs.V))
	}
	return fmt.Sprintf("<b>%s</b>  %s\n流量：%s / %s%s\n  ↑ %s  ↓ %s\n到期：%s\n状态：%s",
		esc(u.Username), st, sub.HumanBytes(used), limit, pct, sub.HumanBytes(u.UsedUp), sub.HumanBytes(u.UsedDown), exp, online)
}

func userButtons(u *store.User) [][]button {
	return [][]button{{{Text: "订阅链接", Data: "me:sub"}, {Text: "节点状态", Data: "me:nodes"}, {Text: "刷新", Data: "me:card"}}}
}

func (b *tgBot) sendSub(chat int64, u *store.User) {
	link := b.s.subURL(u.SubToken)
	if png, err := qrcode.Encode(link, qrcode.Medium, 512); err == nil {
		b.sendPhoto(chat, png, "扫码导入订阅")
	}
	b.send(chat, "订阅链接（自动适配客户端）：\n<code>"+esc(link)+"</code>",
		[]button{{Text: "重置订阅链接", Data: "me:reset"}})
}

func (b *tgBot) userNodes(chat int64, u *store.User) {
	nodes, err := b.s.subNodes(u)
	if err != nil || len(nodes) == 0 {
		b.send(chat, "暂无可用节点")
		return
	}
	status := map[string]string{}
	var ns []store.Node
	b.s.st.DB.Select("name", "status").Find(&ns)
	for _, n := range ns {
		status[n.Name] = n.Status
	}
	var sb strings.Builder
	sb.WriteString("<b>可用节点</b>\n")
	for _, n := range nodes {
		dot := "🟢"
		for name, st := range status {
			if strings.Contains(n.Name, name) && st != "online" && st != "syncing" {
				dot = "🔴"
			}
		}
		sb.WriteString(dot + " " + esc(n.Name) + "\n")
	}
	b.send(chat, sb.String())
}

// --- admin views ---

func (b *tgBot) visibleNodes(id tgIdentity) []store.Node {
	var ns []store.Node
	b.s.st.DB.Order("sort_order, name").Find(&ns)
	var out []store.Node
	for _, n := range ns {
		if id.canNode(n.ID) {
			out = append(out, n)
		}
	}
	return out
}

func statusDot(st string) string {
	switch st {
	case "online":
		return "🟢"
	case "syncing":
		return "🟡"
	case "pending":
		return "⚪"
	}
	return "🔴"
}

func (b *tgBot) statusText(id tgIdentity) string {
	ns := b.visibleNodes(id)
	online := 0
	for _, n := range ns {
		if n.Status == "online" || n.Status == "syncing" {
			online++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>概览</b>\n节点：%d / %d 在线\n", online, len(ns))
	if id.owner() {
		var active, onlineUsers int64
		b.s.st.DB.Model(&store.User{}).Where("is_service = ? AND status = ?", false, "active").Count(&active)
		b.s.st.DB.Model(&store.User{}).Where("is_service = ? AND online_at > ?", false, time.Now().Add(-2*time.Minute)).Count(&onlineUsers)
		day := time.Now()
		start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location()).UTC()
		var t struct{ Up, Down int64 }
		b.s.st.DB.Model(&store.TrafficHourly{}).Select("COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").Where("hour >= ?", start).Scan(&t)
		fmt.Fprintf(&sb, "用户：%d 在线 / %d 有效\n今日流量：%s（↑ %s ↓ %s）\n", onlineUsers, active, sub.HumanBytes(t.Up+t.Down), sub.HumanBytes(t.Up), sub.HumanBytes(t.Down))
	}
	for _, n := range ns {
		fmt.Fprintf(&sb, "%s %s  rev %d/%d\n", statusDot(n.Status), esc(n.Name), n.AppliedRev, n.DesiredRev)
	}
	return sb.String()
}

func (b *tgBot) adminNodes(chat, msg int64, id tgIdentity) {
	ns := b.visibleNodes(id)
	if len(ns) == 0 {
		b.send(chat, "没有可管理的节点")
		return
	}
	var rows [][]button
	for _, n := range ns {
		rows = append(rows, []button{{Text: statusDot(n.Status) + " " + n.Name, Data: "n:v:" + n.ID}})
	}
	text := "<b>节点</b>（点按查看与操作）"
	if msg > 0 {
		b.edit(chat, msg, text, rows...)
	} else {
		b.send(chat, text, rows...)
	}
}

func (b *tgBot) findNode(chat int64, name string, id tgIdentity) {
	for _, n := range b.visibleNodes(id) {
		if strings.EqualFold(n.Name, name) || n.ID == name {
			text, rows := b.nodeCard(&n)
			b.send(chat, text, rows...)
			return
		}
	}
	b.send(chat, "未找到节点（或无权限）")
}

func (b *tgBot) nodeCard(n *store.Node) (string, [][]button) {
	cores, _ := b.s.nodeCores(n.ID)
	var cs []string
	for _, c := range cores {
		cs = append(cs, fmt.Sprintf("%s %s %s", c.Type, c.Version, map[string]string{"running": "✅", "installing": "⏳"}[c.Status]))
	}
	var inCount int64
	b.s.st.DB.Model(&store.Inbound{}).Where("node_id = ?", n.ID).Count(&inCount)
	last := "—"
	if n.LastSeen != nil {
		last = n.LastSeen.Local().Format("01-02 15:04:05")
	}
	text := fmt.Sprintf("%s <b>%s</b>  %s\n地址：<code>%s</code>\n配置：rev %d / %d\n内核：%s\n入站：%d 个\nCPU %.1f%%  内存 %.1f%%\n↓ %s ↑ %s\n最后心跳：%s",
		statusDot(n.Status), esc(n.Name), esc(n.Region), esc(nodeHost(n)), n.AppliedRev, n.DesiredRev, esc(strings.Join(cs, "，")), inCount,
		n.System.V.CPU, n.System.V.Mem, bpsText(n.System.V.RxBps), bpsText(n.System.V.TxBps), last)
	if n.LastError != "" {
		text += "\n⚠️ " + esc(truncate(n.LastError, 300))
	}
	rows := [][]button{{{Text: "强制同步", Data: "n:s:" + n.ID}, {Text: "回滚上一版", Data: "n:b:" + n.ID}}}
	var restart []button
	for _, c := range cores {
		if c.Type == "xray" || c.Type == "singbox" || c.Type == "hysteria" {
			restart = append(restart, button{Text: "重启 " + string(c.Type), Data: "n:r:" + n.ID + ":" + string(c.Type)})
		}
	}
	if len(restart) > 0 {
		rows = append(rows, restart)
	}
	rows = append(rows, []button{{Text: "刷新", Data: "n:v:" + n.ID}, {Text: "« 节点列表", Data: "n:l"}})
	return text, rows
}

func bpsText(v uint64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.2f Gbps", float64(v)/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1f Mbps", float64(v)/1e6)
	}
	return fmt.Sprintf("%d Kbps", v/1000)
}

func (b *tgBot) searchUsers(chat int64, q string) {
	var us []store.User
	tx := b.s.st.DB.Where("is_service = ?", false).Order("username").Limit(20)
	if q != "" {
		like := "%" + q + "%"
		tx = tx.Where("username LIKE ? OR uuid LIKE ? OR remark LIKE ?", like, like, like)
	}
	tx.Find(&us)
	if len(us) == 0 {
		b.send(chat, "没有匹配的用户")
		return
	}
	if len(us) == 1 {
		b.send(chat, b.userCard(&us[0]), adminUserButtons(&us[0])...)
		return
	}
	var rows [][]button
	for _, u := range us {
		rows = append(rows, []button{{Text: u.Username + " · " + map[string]string{"active": "正常", "limited": "超额", "expired": "到期", "disabled": "禁用"}[u.Status], Data: "u:v:" + u.ID}})
	}
	b.send(chat, fmt.Sprintf("找到 %d 个用户", len(us)), rows...)
}

func adminUserButtons(u *store.User) [][]button {
	toggle := button{Text: "禁用", Data: "u:d:" + u.ID}
	if u.Status == "disabled" {
		toggle = button{Text: "启用", Data: "u:e:" + u.ID}
	}
	return [][]button{
		{toggle, {Text: "重置流量", Data: "u:t:" + u.ID}, {Text: "续期 30 天", Data: "u:x:" + u.ID}},
		{{Text: "重置订阅", Data: "u:k:" + u.ID}, {Text: "刷新", Data: "u:v:" + u.ID}},
	}
}

func (b *tgBot) trafficText() string {
	start := time.Now().Truncate(24 * time.Hour)
	type row struct {
		UserID string
		Total  int64
	}
	var rs []row
	b.s.st.DB.Model(&store.TrafficHourly{}).Select("user_id, SUM(up+down) AS total").Where("hour >= ?", start).
		Group("user_id").Order("total desc").Limit(10).Scan(&rs)
	if len(rs) == 0 {
		return "今日暂无流量"
	}
	var sb strings.Builder
	sb.WriteString("<b>今日流量 Top 10</b>\n")
	for i, r := range rs {
		var u store.User
		b.s.st.DB.Select("username").First(&u, "id = ?", r.UserID)
		fmt.Fprintf(&sb, "%d. %s — %s\n", i+1, esc(orDefault(u.Username, r.UserID)), sub.HumanBytes(r.Total))
	}
	return sb.String()
}

// --- callbacks ---

func (b *tgBot) onCallback(cbID string, chat, msg int64, data string) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	answer := func(text string) {
		_ = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": cbID, "text": text}, nil)
	}
	id := b.identify(chat)
	parts := strings.Split(data, ":")
	switch parts[0] {
	case "me":
		if id.user == nil {
			answer("尚未绑定账号")
			return
		}
		switch parts[1] {
		case "card":
			b.edit(chat, msg, b.userCard(id.user), userButtons(id.user)...)
		case "sub":
			b.sendSub(chat, id.user)
		case "nodes":
			b.userNodes(chat, id.user)
		case "reset":
			b.send(chat, "确定重置订阅链接？旧链接将立即失效，需要在客户端重新导入。", []button{{Text: "确认重置", Data: "me:reset!"}})
		case "reset!":
			tok := pki.Token(32)
			b.s.st.DB.Model(&store.User{}).Where("id = ?", id.user.ID).Update("sub_token", tok)
			b.s.audit("telegram", "user.reset_token", id.user.Username, "self-service", "")
			id.user.SubToken = tok
			b.sendSub(chat, id.user)
		}
		answer("")
	case "n":
		if len(parts) >= 2 && parts[1] == "l" {
			b.adminNodes(chat, msg, id)
			answer("")
			return
		}
		if len(parts) < 3 || !id.canNode(parts[2]) {
			answer("无权限")
			return
		}
		var n store.Node
		if b.s.st.DB.First(&n, "id = ?", parts[2]).Error != nil {
			answer("节点不存在")
			return
		}
		who := "tg:" + id.admin.Name
		switch parts[1] {
		case "s":
			rev, _, err := b.s.Publish(n.ID, "telegram sync", true)
			if err != nil {
				answer("失败：" + err.Error())
				return
			}
			b.s.audit(who, "node.sync", n.Name, fmt.Sprintf("rev %d", rev), "")
			answer(fmt.Sprintf("已下发 rev %d", rev))
		case "b":
			rev, err := b.s.rollbackTo(&n, 0)
			if err != nil {
				answer("回滚失败：" + err.Error())
				return
			}
			b.s.audit(who, "node.rollback", n.Name, fmt.Sprintf("-> rev %d", rev), "")
			answer(fmt.Sprintf("已回滚，新版本 rev %d", rev))
		case "r":
			if len(parts) < 4 {
				return
			}
			res, err := b.s.hub.Command(ctx, n.ID, "restart_core", map[string]string{"type": parts[3]}, 30*time.Second)
			switch {
			case err != nil:
				answer("失败：" + err.Error())
				return
			case !res.Ok:
				answer("失败：" + res.Error)
				return
			}
			b.s.audit(who, "core.restart", n.Name, parts[3], "")
			answer(parts[3] + " 已重启")
		default:
			answer("")
		}
		b.s.st.DB.First(&n, "id = ?", n.ID)
		text, rows := b.nodeCard(&n)
		b.edit(chat, msg, text, rows...)
	case "u":
		if !id.owner() || len(parts) < 3 {
			answer("无权限")
			return
		}
		var u store.User
		if b.s.st.DB.First(&u, "id = ? AND is_service = ?", parts[2], false).Error != nil {
			answer("用户不存在")
			return
		}
		who := "tg:" + id.admin.Name
		done := ""
		switch parts[1] {
		case "d", "e":
			st := map[string]string{"d": "disabled", "e": "active"}[parts[1]]
			b.s.st.DB.Model(&u).Update("status", st)
			b.s.recomputeStatus(u.ID)
			done = map[string]string{"d": "已禁用", "e": "已启用"}[parts[1]]
		case "t":
			b.s.st.DB.Model(&u).Updates(map[string]any{"used_up": 0, "used_down": 0, "last_reset_at": time.Now(), "warned_usage": false})
			b.s.recomputeStatus(u.ID)
			done = "流量已重置"
		case "x":
			base := time.Now()
			if u.ExpireAt != nil && u.ExpireAt.After(base) {
				base = *u.ExpireAt
			}
			b.s.st.DB.Model(&u).Updates(map[string]any{"expire_at": base.AddDate(0, 0, 30), "warned_expire": nil})
			b.s.recomputeStatus(u.ID)
			done = "已续期 30 天"
		case "k":
			b.s.st.DB.Model(&u).Update("sub_token", pki.Token(32))
			done = "订阅已重置"
		}
		if done != "" {
			b.s.audit(who, "user.telegram", u.Username, done, "")
			b.s.MarkDirty("telegram user action", b.s.nodesForUsers(u.ID)...)
		}
		answer(done)
		b.s.st.DB.First(&u, "id = ?", u.ID)
		b.edit(chat, msg, b.userCard(&u), adminUserButtons(&u)...)
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
			if !b.isRunning() {
				continue
			}
			var admins []store.TGAdmin
			b.s.st.DB.Where("notify = ?", true).Find(&admins)
			for _, a := range admins {
				if a.Role == "owner" || (e.NodeID != "" && tgIdentity{admin: &a}.canNode(e.NodeID)) {
					b.send(a.ChatID, esc(e.Text))
				}
			}
			if e.UserID != "" && b.s.st.GetSetting("tg_notify_users", "true") == "true" {
				var u store.User
				if b.s.st.DB.First(&u, "id = ?", e.UserID).Error == nil && u.TelegramID != 0 {
					b.send(u.TelegramID, esc(userFacing(e)))
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

// remindUsers sends 80% traffic and 3-day expiry reminders (hourly).
func (s *Server) remindUsers() {
	var us []store.User
	s.st.DB.Where("telegram_id <> 0 AND is_service = ? AND status = ?", false, "active").Find(&us)
	now := time.Now()
	for _, u := range us {
		if u.TrafficLimit > 0 {
			pct := float64(u.Used()) / float64(u.TrafficLimit)
			if pct >= 0.8 && !u.WarnedUsage {
				s.st.DB.Model(&u).Update("warned_usage", true)
				s.notify(Event{Kind: "traffic_warn", UserID: u.ID, Text: fmt.Sprintf("📊 流量已使用 %.0f%%（%s / %s）", pct*100, sub.HumanBytes(u.Used()), sub.HumanBytes(u.TrafficLimit))})
			} else if pct < 0.8 && u.WarnedUsage {
				s.st.DB.Model(&u).Update("warned_usage", false)
			}
		}
		if u.ExpireAt != nil && u.ExpireAt.Sub(now) < 72*time.Hour && u.ExpireAt.After(now) &&
			(u.WarnedExpire == nil || now.Sub(*u.WarnedExpire) > 24*time.Hour) {
			s.st.DB.Model(&u).Update("warned_expire", now)
			s.notify(Event{Kind: "expire_warn", UserID: u.ID, Text: fmt.Sprintf("⏰ 账号将于 %s 到期", u.ExpireAt.Local().Format("2006-01-02 15:04"))})
		}
	}
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
	s.tg.mu.Lock()
	s.tg.codes[code] = bindCode{Role: req.Role, Name: req.Name, NodeIDs: req.NodeIDs, Expires: time.Now().Add(10 * time.Minute)}
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
