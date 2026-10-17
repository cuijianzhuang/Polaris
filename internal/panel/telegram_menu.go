package panel

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Bot menu: /start, /menu and /help open a main menu whose buttons edit the
// same message in place ("v:<view>[:arg]" callbacks), so a chat stays one
// tidy card instead of a scroll of replies. Every view ends with a row back
// to the menu.

const tgRule = "┄┄┄┄┄┄┄┄┄┄┄┄┄┄"

var (
	btnHome = button{Text: "🏠 主菜单", Data: "v:home"}
)

func backRow(refresh string) []button {
	if refresh == "" {
		return []button{btnHome}
	}
	return []button{{Text: "🔄 刷新", Data: refresh}, btnHome}
}

// tgBar draws a 10-cell usage bar.
func tgBar(pct float64) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	n := int(pct/10 + 0.5)
	return strings.Repeat("▰", n) + strings.Repeat("▱", 10-n)
}

var userStatusText = map[string]string{"active": "✅ 正常", "limited": "⛔ 流量用尽", "expired": "⌛ 已到期", "disabled": "🚫 已禁用"}

// view renders one menu screen.
func (b *tgBot) view(name, arg string, id tgIdentity) (string, [][]button) {
	switch name {
	case "me":
		if id.user == nil {
			return b.view("bindhelp", "", id)
		}
		return b.userCard(id.user), userButtons()
	case "nodes":
		if id.user == nil {
			return b.view("bindhelp", "", id)
		}
		return b.userNodesText(id.user), [][]button{backRow("v:nodes")}
	case "bindhelp":
		return "🔗 <b>绑定账号</b>\n" + tgRule + "\n把面板发给你的<b>订阅链接</b>发给我：\n<code>/bind https://…/sub/…</code>\n\n也可以在订阅页点「Telegram 绑定」一键完成。", [][]button{backRow("")}
	case "status":
		if id.admin == nil {
			return "仅管理员可用", [][]button{backRow("")}
		}
		return b.statusText(id), [][]button{{{Text: "🖥 节点列表", Data: "n:l"}}, backRow("v:status")}
	case "users":
		if !id.owner() {
			return "仅超级管理员可用", [][]button{backRow("")}
		}
		page, _ := strconv.Atoi(arg)
		return b.userList(page)
	case "top":
		if !id.owner() {
			return "仅超级管理员可用", [][]button{backRow("")}
		}
		return b.trafficText(), [][]button{backRow("v:top")}
	case "help":
		return b.helpText(id), [][]button{backRow("")}
	case "pro":
		return tgProText, [][]button{backRow("")}
	case "update":
		if !id.owner() {
			return "仅超级管理员可用", [][]button{backRow("")}
		}
		return b.updateView()
	}
	return b.homeText(id), b.homeButtons(id)
}

func (b *tgBot) homeText(id tgIdentity) string {
	var sb strings.Builder
	sb.WriteString("✦ <b>" + esc(b.s.st.GetSetting("site_title", "Polaris")) + "</b>\n" + tgRule + "\n")
	switch {
	case id.owner():
		sb.WriteString("👑 超级管理员 · " + esc(id.admin.Name) + "\n")
	case id.admin != nil:
		fmt.Fprintf(&sb, "🛠 节点管理员 · %s（%d 个节点）\n", esc(id.admin.Name), len(id.admin.NodeIDs.V))
	}
	if u := id.user; u != nil {
		used := u.Used()
		line := "📊 " + sub.HumanBytes(used)
		if u.TrafficLimit > 0 {
			line += " / " + sub.HumanBytes(u.TrafficLimit)
		}
		fmt.Fprintf(&sb, "👤 %s  %s\n%s\n", esc(u.Username), userStatusText[u.Status], line)
	}
	if id.admin == nil && id.user == nil {
		sb.WriteString("你还没有绑定账号。\n点下方「绑定账号」查看方法。\n")
	}
	sb.WriteString("\n请选择：")
	return sb.String()
}

func (b *tgBot) homeButtons(id tgIdentity) [][]button {
	var rows [][]button
	if id.user != nil {
		rows = append(rows,
			[]button{{Text: "📊 我的流量", Data: "v:me"}, {Text: "🔗 订阅链接", Data: "me:sub"}},
			[]button{{Text: "🌐 可用节点", Data: "v:nodes"}})
	} else if id.admin == nil {
		rows = append(rows, []button{{Text: "🔗 绑定账号", Data: "v:bindhelp"}})
	}
	if id.admin != nil {
		if !b.s.license().Allow(license.FeatureTGMenu) {
			rows = append(rows, []button{{Text: "🔒 管理菜单（专业版）", Data: "v:pro"}})
		} else {
			rows = append(rows, []button{{Text: "📈 概览", Data: "v:status"}, {Text: "🖥 节点管理", Data: "n:l"}})
			if id.owner() {
				rows = append(rows, []button{{Text: "👥 用户", Data: "v:users:0"}, {Text: "🏆 今日排行", Data: "v:top"}},
					[]button{{Text: "⬆️ 版本更新", Data: "v:update"}})
			}
		}
		notify := "🔔 告警：开"
		if !id.admin.Notify {
			notify = "🔕 告警：关"
		}
		rows = append(rows, []button{{Text: "🔑 登录面板", Data: "m:login"}, {Text: notify, Data: "m:notify"}})
	}
	rows = append(rows, []button{{Text: "❓ 帮助", Data: "v:help"}})
	return rows
}

func (b *tgBot) helpText(id tgIdentity) string {
	var sb strings.Builder
	sb.WriteString("❓ <b>帮助</b>\n" + tgRule + "\n" + b.roleLine(id) + "\n\n<b>账号</b>\n")
	sb.WriteString("/menu 主菜单\n/me 流量与到期\n/sub 订阅链接与二维码\n/nodes 可用节点\n/bind 订阅链接 — 绑定\n/unbind 解除绑定\n/whoami 查看 Chat ID\n")
	if id.admin != nil {
		sb.WriteString("\n<b>管理员</b>\n/status 概览\n/nodes 节点管理\n/node 名称 — 节点详情\n/login 登录网页面板\n/notify on|off 告警\n")
	}
	if id.owner() {
		sb.WriteString("/users 关键词 — 搜索用户\n/traffic 今日流量排行\n/update 版本更新（升级面板与节点、自动更新）\n")
	}
	return sb.String()
}

// --- user views ---

func (b *tgBot) userCard(u *store.User) string {
	used := u.Used()
	var sb strings.Builder
	fmt.Fprintf(&sb, "👤 <b>%s</b>  %s\n%s\n", esc(u.Username), userStatusText[u.Status], tgRule)
	if u.TrafficLimit > 0 {
		pct := float64(used) * 100 / float64(u.TrafficLimit)
		fmt.Fprintf(&sb, "📊 流量  <b>%s</b> / %s\n<code>%s</code> %.1f%%\n", sub.HumanBytes(used), sub.HumanBytes(u.TrafficLimit), tgBar(pct), pct)
	} else {
		fmt.Fprintf(&sb, "📊 流量  <b>%s</b> / 不限\n", sub.HumanBytes(used))
	}
	fmt.Fprintf(&sb, "      ↑ %s   ↓ %s\n", sub.HumanBytes(u.UsedUp), sub.HumanBytes(u.UsedDown))
	switch {
	case u.ExpireAt == nil && u.ExpireAfterDays > 0:
		fmt.Fprintf(&sb, "📅 到期  首次使用后 %d 天\n", u.ExpireAfterDays)
	case u.ExpireAt == nil:
		sb.WriteString("📅 到期  永不到期\n")
	default:
		days := int(time.Until(*u.ExpireAt).Hours() / 24)
		left := fmt.Sprintf("剩 %d 天", days)
		if days < 0 {
			left = "已过期"
		}
		fmt.Fprintf(&sb, "📅 到期  %s（%s）\n", u.ExpireAt.Local().Format("2006-01-02"), left)
	}
	if u.OnlineAt != nil && time.Since(*u.OnlineAt) < 2*time.Minute {
		fmt.Fprintf(&sb, "🟢 在线 · %d 个 IP", len(u.OnlineIPs.V))
	} else if u.OnlineAt != nil {
		sb.WriteString("⚪ 最近在线 " + u.OnlineAt.Local().Format("01-02 15:04"))
	} else {
		sb.WriteString("⚪ 尚未使用")
	}
	return sb.String()
}

func userButtons() [][]button {
	return [][]button{
		{{Text: "🔗 订阅链接", Data: "me:sub"}, {Text: "🌐 可用节点", Data: "v:nodes"}},
		backRow("v:me"),
	}
}

func (b *tgBot) userNodes(chat int64, u *store.User) {
	b.send(chat, b.userNodesText(u), backRow("v:nodes"))
}

func (b *tgBot) userNodesText(u *store.User) string {
	nodes, err := b.s.subNodes(u)
	if err != nil || len(nodes) == 0 {
		return "🌐 <b>可用节点</b>\n" + tgRule + "\n暂无可用节点，请联系管理员。"
	}
	var ns []store.Node
	b.s.st.DB.Select("name", "status").Find(&ns)
	var sb strings.Builder
	fmt.Fprintf(&sb, "🌐 <b>可用节点</b>（%d）\n%s\n", len(nodes), tgRule)
	for _, n := range nodes {
		dot := "🟢"
		for _, x := range ns {
			if strings.Contains(n.Name, x.Name) && x.Status != "online" && x.Status != "syncing" {
				dot = "🔴"
			}
		}
		sb.WriteString(dot + " " + esc(n.Name) + "\n")
	}
	sb.WriteString("\n🔴 表示节点暂时离线")
	return sb.String()
}

// --- admin views ---

func (b *tgBot) statusText(id tgIdentity) string {
	ns := b.visibleNodes(id)
	online := 0
	for _, n := range ns {
		if n.Status == "online" || n.Status == "syncing" {
			online++
		}
	}
	traffic := b.s.nodeTrafficStats()
	var sb strings.Builder
	fmt.Fprintf(&sb, "📈 <b>概览</b>\n%s\n🖥 节点  <b>%d</b> / %d 在线\n", tgRule, online, len(ns))
	if id.owner() {
		var active, onlineUsers int64
		b.s.st.DB.Model(&store.User{}).Where("is_service = ? AND status = ?", false, "active").Count(&active)
		b.s.st.DB.Model(&store.User{}).Where("is_service = ? AND online_at > ?", false, time.Now().Add(-2*time.Minute)).Count(&onlineUsers)
		var up, down int64
		for _, t := range traffic {
			up, down = up+t.TodayUp, down+t.TodayDown
		}
		fmt.Fprintf(&sb, "👥 用户  <b>%d</b> 在线 · %d 有效\n📶 今日  <b>%s</b>（↑ %s ↓ %s）\n", onlineUsers, active,
			sub.HumanBytes(up+down), sub.HumanBytes(up), sub.HumanBytes(down))
	}
	if len(ns) > 0 {
		sb.WriteString(tgRule + "\n")
	}
	for _, n := range ns {
		line := statusDot(n.Status) + " " + esc(n.Name)
		if t := traffic[n.ID]; t != nil && t.TodayUp+t.TodayDown > 0 {
			line += "  · 今日 " + sub.HumanBytes(t.TodayUp+t.TodayDown)
		}
		if n.AppliedRev < n.DesiredRev {
			line += fmt.Sprintf("  · 同步中 %d/%d", n.AppliedRev, n.DesiredRev)
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

func (b *tgBot) adminNodesView(id tgIdentity) (string, [][]button) {
	ns := b.visibleNodes(id)
	if len(ns) == 0 {
		return "🖥 <b>节点管理</b>\n" + tgRule + "\n没有可管理的节点", [][]button{backRow("")}
	}
	var rows [][]button
	for i := 0; i < len(ns); i += 2 { // two per row
		row := []button{{Text: statusDot(ns[i].Status) + " " + ns[i].Name, Data: "n:v:" + ns[i].ID}}
		if i+1 < len(ns) {
			row = append(row, button{Text: statusDot(ns[i+1].Status) + " " + ns[i+1].Name, Data: "n:v:" + ns[i+1].ID})
		}
		rows = append(rows, row)
	}
	rows = append(rows, backRow("n:l"))
	return fmt.Sprintf("🖥 <b>节点管理</b>（%d）\n%s\n点按节点查看详情与操作", len(ns), tgRule), rows
}

func (b *tgBot) adminNodes(chat, msg int64, id tgIdentity) {
	text, rows := b.adminNodesView(id)
	if msg > 0 {
		b.edit(chat, msg, text, rows...)
	} else {
		b.send(chat, text, rows...)
	}
}

const tgUsersPerPage = 8

func (b *tgBot) userList(page int) (string, [][]button) {
	var total int64
	b.s.st.DB.Model(&store.User{}).Where("is_service = ?", false).Count(&total)
	pages := int((total + tgUsersPerPage - 1) / tgUsersPerPage)
	if page < 0 || page >= pages {
		page = 0
	}
	var us []store.User
	b.s.st.DB.Where("is_service = ?", false).Order("username").Offset(page * tgUsersPerPage).Limit(tgUsersPerPage).Find(&us)
	rows := userRows(us)
	if pages > 1 {
		var nav []button
		if page > 0 {
			nav = append(nav, button{Text: "‹ 上一页", Data: "v:users:" + strconv.Itoa(page-1)})
		}
		nav = append(nav, button{Text: fmt.Sprintf("%d / %d", page+1, pages), Data: "v:users:" + strconv.Itoa(page)})
		if page < pages-1 {
			nav = append(nav, button{Text: "下一页 ›", Data: "v:users:" + strconv.Itoa(page+1)})
		}
		rows = append(rows, nav)
	}
	rows = append(rows, backRow(""))
	return fmt.Sprintf("👥 <b>用户</b>（%d）\n%s\n点按用户查看与操作；搜索请发送 <code>/users 关键词</code>", total, tgRule), rows
}

func userRows(us []store.User) [][]button {
	icon := map[string]string{"active": "🟢", "limited": "⛔", "expired": "⌛", "disabled": "🚫"}
	var rows [][]button
	for _, u := range us {
		rows = append(rows, []button{{Text: icon[u.Status] + " " + u.Username + " · " + sub.HumanBytes(u.Used()), Data: "u:v:" + u.ID}})
	}
	return rows
}

func (b *tgBot) searchUsers(chat int64, q string) {
	if q == "" {
		text, rows := b.userList(0)
		b.send(chat, text, rows...)
		return
	}
	var us []store.User
	like := "%" + q + "%"
	b.s.st.DB.Where("is_service = ?", false).Where("username LIKE ? OR uuid LIKE ? OR remark LIKE ?", like, like, like).
		Order("username").Limit(20).Find(&us)
	switch len(us) {
	case 0:
		b.send(chat, "🔍 没有匹配「"+esc(q)+"」的用户", backRow(""))
	case 1:
		b.send(chat, b.userCard(&us[0]), adminUserButtons(&us[0])...)
	default:
		rows := append(userRows(us), backRow(""))
		b.send(chat, fmt.Sprintf("🔍 「%s」找到 %d 个用户", esc(q), len(us)), rows...)
	}
}

func adminUserButtons(u *store.User) [][]button {
	toggle := button{Text: "🚫 禁用", Data: "u:d:" + u.ID}
	if u.Status == "disabled" {
		toggle = button{Text: "✅ 启用", Data: "u:e:" + u.ID}
	}
	return [][]button{
		{toggle, {Text: "♻️ 重置流量", Data: "u:t:" + u.ID}},
		{{Text: "📅 续期 30 天", Data: "u:x:" + u.ID}, {Text: "🔑 重置订阅", Data: "u:k:" + u.ID}},
		{{Text: "🔄 刷新", Data: "u:v:" + u.ID}, {Text: "« 用户列表", Data: "v:users:0"}, btnHome},
	}
}

func (b *tgBot) trafficText() string {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UTC()
	type row struct {
		UserID string
		Total  int64
	}
	var rs []row
	realUsers(b.s.st.DB.Model(&store.TrafficHourly{})).Select("user_id, SUM(up+down) AS total").Where("hour >= ?", start).
		Group("user_id").Order("total desc").Limit(10).Scan(&rs)
	if len(rs) == 0 {
		return "🏆 <b>今日流量排行</b>\n" + tgRule + "\n今日暂无流量"
	}
	medal := []string{"🥇", "🥈", "🥉"}
	var sb strings.Builder
	sb.WriteString("🏆 <b>今日流量排行</b>\n" + tgRule + "\n")
	for i, r := range rs {
		var u store.User
		b.s.st.DB.Select("username").First(&u, "id = ?", r.UserID)
		rank := fmt.Sprintf(" %d.", i+1)
		if i < len(medal) {
			rank = medal[i]
		}
		fmt.Fprintf(&sb, "%s %s  <b>%s</b>\n", rank, esc(orDefault(u.Username, r.UserID)), sub.HumanBytes(r.Total))
	}
	return sb.String()
}
