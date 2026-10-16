package panel

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Bot side of panel updates (super admins only): /update shows the running
// and latest versions with buttons to update the panel, the panel and then
// every agent, or only the agents; and how auto update is set.

var autoUpdateText = map[string]string{
	"off": "关闭", "notify": "只通知新版本", "panel": "自动升级面板", "all": "自动升级面板和节点",
}

func (b *tgBot) updateView() (string, [][]button) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var sb strings.Builder
	sb.WriteString("⬆️ <b>版本更新</b>\n" + tgRule + "\n")
	fmt.Fprintf(&sb, "当前版本  <code>%s</code>\n", esc(Version))
	latest, err := b.s.latestPanelVersion(ctx, true)
	switch {
	case err != nil:
		sb.WriteString("最新版本  获取失败：" + esc(truncate(err.Error(), 120)) + "\n")
	case newerThanRunning(latest):
		fmt.Fprintf(&sb, "最新版本  <code>%s</code> 🆕\n", esc(latest))
	default:
		fmt.Fprintf(&sb, "最新版本  <code>%s</code> ✅ 已是最新\n", esc(latest))
	}
	outdated := 0
	online, _ := b.s.onlineAgentVersions()
	for _, v := range online {
		if v != Version {
			outdated++
		}
	}
	fmt.Fprintf(&sb, "在线节点  %d 个，%d 个 Agent 版本与面板不同\n", len(online), outdated)
	mode := b.s.st.GetSetting("auto_update", "notify")
	fmt.Fprintf(&sb, "自动更新  %s", autoUpdateText[mode])
	if mode == "panel" || mode == "all" {
		fmt.Fprintf(&sb, "（每天 %s 点）", b.s.st.GetSetting("auto_update_hour", "4"))
	}
	sb.WriteString("\n")
	if !b.s.updaterInstalled() {
		sb.WriteString("\n⚠️ " + esc(errUpdaterMissing.Error()) + "\n")
	}
	var rows [][]button
	if err == nil && newerThanRunning(latest) {
		rows = append(rows, []button{{Text: "⬆️ 升级面板", Data: "up:panel"}, {Text: "⬆️ 面板+全部节点", Data: "up:all"}})
	}
	if outdated > 0 {
		rows = append(rows, []button{{Text: fmt.Sprintf("🖥 升级节点 Agent（%d）", outdated), Data: "up:agents"}})
	}
	rows = append(rows, []button{{Text: "⚙️ 自动更新：" + autoUpdateText[mode], Data: "up:mode"}})
	rows = append(rows, backRow("v:update"))
	return sb.String(), rows
}

// onUpdateCallback handles up:<panel|all|agents|mode>.
func (b *tgBot) onUpdateCallback(chat, msg int64, action string, id tgIdentity, answer func(string)) {
	if !id.owner() {
		answer("仅超级管理员可用")
		return
	}
	who := "tg:" + id.admin.Name
	switch action {
	case "panel", "all":
		if err := b.s.requestPanelUpdate("", who, action == "all"); err != nil {
			answer("无法升级")
			b.send(chat, "❌ "+esc(err.Error()), backRow(""))
			return
		}
		answer("已开始升级")
		text := "⏳ 已开始升级面板，约 1 分钟后完成，结果会发到这里（面板重启期间网页会短暂断开）。"
		if action == "all" {
			text += "\n面板升级完成后会接着升级所有在线节点的 Agent。"
		}
		b.edit(chat, msg, text, backRow(""))
	case "agents":
		answer("正在升级节点…")
		b.edit(chat, msg, "⏳ 正在升级节点 Agent…", backRow(""))
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			res := b.s.upgradeOutdatedAgents(ctx)
			b.s.audit(who, "node.upgrade_agent_all", fmt.Sprintf("%d", len(res)), Version, "")
			text := "✅ 所有在线节点的 Agent 已是 " + Version
			if len(res) > 0 {
				text = agentUpgradeSummary(res)
			}
			b.send(chat, esc(text), backRow("v:update"))
		}()
	case "mode": // cycle through the modes
		next := map[string]string{"off": "notify", "notify": "panel", "panel": "all", "all": "off"}
		mode := next[b.s.st.GetSetting("auto_update", "notify")]
		if mode == "" {
			mode = "notify"
		}
		b.s.st.SetSetting("auto_update", mode)
		b.s.audit(who, "settings.update", "auto_update", mode, "")
		answer("自动更新：" + autoUpdateText[mode])
		text, rows := b.updateView()
		b.edit(chat, msg, text, rows...)
	default:
		answer("")
	}
}

// onlineAgentVersions maps online node names to their agent versions.
func (s *Server) onlineAgentVersions() (map[string]string, error) {
	var ns []struct {
		ID, Name, AgentVersion string
	}
	if err := s.st.DB.Table("nodes").Select("id, name, agent_version").Scan(&ns).Error; err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, n := range ns {
		if s.hub.Online(n.ID) {
			out[n.Name] = n.AgentVersion
		}
	}
	return out, nil
}
