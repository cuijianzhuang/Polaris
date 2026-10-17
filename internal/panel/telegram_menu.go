package panel

import (
	"fmt"
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
	case "status", "users", "top", "update":
		return b.adminView(name, arg, id)
	case "help":
		return b.helpText(id), [][]button{backRow("")}
	case "pro":
		return tgProText, [][]button{backRow("")}
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
		rows = append(rows, []button{{Text: "🛠 登录后台", Data: "m:login"}, {Text: notify, Data: "m:notify"}})
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
