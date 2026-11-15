package sub

import (
	"encoding/json"
	"sort"
	"strings"
)

// String table for the user portal's own JavaScript. The template renders the
// static chrome through PortalText; anything built at runtime — view titles,
// node state labels, check results, error messages — reads from this table so
// an English visitor does not get a half-translated page.
//
// Keep the keys in sync with the usages in assets/portal.js and
// assets/portal-theme.js. Values are plain text; callers pass the argument
// through s() which substitutes {n} and similar placeholders.
var portalJSChinese = map[string]string{
	// page titles
	"viewOverview": "仪表盘", "viewSubscription": "我的订阅", "viewNodes": "可用节点",
	"viewPreferences": "高级订阅设置", "viewOrders": "我的订单", "viewAccount": "个人中心", "viewHelp": "使用教程",
	"userCenterTitle": "用户中心",
	// node states and proof labels
	"nodeOnline": "在线", "nodeOffline": "离线", "nodeUnknown": "未上报",
	"proofPassed": "检查通过", "proofFailed": "检查失败", "proofStale": "待复检", "proofUnknown": "尚未检查",
	"bizPassed": "认证与传输通过", "bizFailed": "最近验证失败", "bizStale": "验证已过期", "bizUnknown": "尚未验证",
	// node status list
	"lastCheck": "最近检查：{time}", "noRecentCheck": "暂无近期检查记录",
	"statusUpdatedAt":    "状态更新于 {time} · 后台检查记录，不是本地测速",
	"statusUpdateFailed": "节点状态更新失败，已保留上次结果，请重试",
	"viewReason":         "查看原因", "viewDetail": "查看详情", "viewAbout": "查看{name}",
	// entry check
	"checking": "检查中…", "checkConnection": "检查连接",
	"checkProbing":     "正在从面板检查订阅入口的 TCP／TLS 连接…",
	"autoCheckRunning": "自动检查中：{name}",
	"autoCheckLast":    "最近自动检查：{name} · {state}",
	"checkPassed":      "连接通过", "checkUnsupported": "检查不适用", "checkFailed": "连接失败",
	"checkTimeout":          "检查超时，请稍后重试",
	"invalidStatusResponse": "状态响应无效",
	"autoCheckOn":           "自动检查已开启，每 12 秒检查一个入口，5 分钟内不重复检查。",
	"autoCheckOff":          "自动检查已暂停，仍可在节点详情中手动检查。",
	"countBiz":              "业务已验证", "countReachable": "连接通过", "countUnknown": "待检查", "countUnavailable": "暂不可用",
	"nodeTotal": "共 {n} 个节点", "countJoin": " · ",
	"pleaseSignIn": "请重新登录", "cannotUpdateStatus": "无法更新状态", "checkFailedRetry": "检查失败，请重试",
	// devices
	"currentDevice": "当前设备：{device}。请选择已安装、且支持订阅中协议的客户端。",
	"deviceGuide":   "{device} 使用指引",
	// theme toggle
	"themeDark": "深色模式", "themeLight": "浅色模式", "switchTo": "切换到{mode}",
}

var portalJSEnglish = map[string]string{
	"viewOverview": "Dashboard", "viewSubscription": "Subscription", "viewNodes": "Nodes",
	"viewPreferences": "Advanced subscription settings", "viewOrders": "Orders", "viewAccount": "Account", "viewHelp": "Help",
	"userCenterTitle": "User center",
	"nodeOnline":      "Online", "nodeOffline": "Offline", "nodeUnknown": "Not reported",
	"proofPassed": "Check passed", "proofFailed": "Check failed", "proofStale": "Recheck pending", "proofUnknown": "Not checked yet",
	"bizPassed": "Credentials and transport OK", "bizFailed": "Last verification failed", "bizStale": "Verification expired", "bizUnknown": "Not verified yet",
	"lastCheck": "Last checked: {time}", "noRecentCheck": "No recent check",
	"statusUpdatedAt":    "Status updated {time} · checked by the panel, not measured locally",
	"statusUpdateFailed": "Could not update node status. Showing the previous result — please retry.",
	"viewReason":         "Why", "viewDetail": "Details", "viewAbout": "View {name}",
	"checking": "Checking…", "checkConnection": "Check connection",
	"checkProbing":     "Checking the subscription entry's TCP/TLS connection from the panel…",
	"autoCheckRunning": "Auto-checking: {name}",
	"autoCheckLast":    "Last auto-check: {name} · {state}",
	"checkPassed":      "Connected", "checkUnsupported": "Check not applicable", "checkFailed": "Connection failed",
	"checkTimeout":          "Check timed out, please retry",
	"invalidStatusResponse": "Invalid status response",
	"autoCheckOn":           "Auto-check on: one entry every 12 seconds, none repeated within 5 minutes.",
	"autoCheckOff":          "Auto-check paused. You can still check a node manually from its details.",
	"countBiz":              "Verified", "countReachable": "Connected", "countUnknown": "Pending", "countUnavailable": "Unavailable",
	"nodeTotal": "{n} nodes", "countJoin": " · ",
	"pleaseSignIn": "Please sign in again", "cannotUpdateStatus": "Could not update status", "checkFailedRetry": "Check failed, please retry",
	"currentDevice": "This device: {device}. Pick a client you have installed that supports the protocols in your subscription.",
	"deviceGuide":   "{device} guide",
	"themeDark":     "Dark mode", "themeLight": "Light mode", "switchTo": "Switch to {mode}",
}

// portalJSStrings renders the table as a JSON object literal assigned to
// window.__portalI18n. Keys are emitted in sorted order so the output is stable
// and can be compared in tests.
func portalJSStrings(lang string) string {
	table := portalJSChinese
	if PortalLocale(lang) == "en" {
		table = portalJSEnglish
	}
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("window.__portalI18n={")
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		val, _ := json.Marshal(table[k])
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteString("};")
	return b.String()
}
