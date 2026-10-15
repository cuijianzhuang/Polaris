package sub

import "strings"

// SchemePreset is a rule scheme offered in the UI. Built-in presets carry
// their ini text; remote ones (the ACL4SSR CM_Online family) a URL.
type SchemePreset struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"`
	Desc  string `json:"desc"`
	URL   string `json:"url,omitempty"`
	Text  string `json:"-"`
}

const acl = "https://raw.githubusercontent.com/ACL4SSR/ACL4SSR/master/Clash/"
const cmACL = "https://raw.githubusercontent.com/cmliu/ACL4SSR/main/Clash/"

// Shared group lines. []@regions expands to one url-test group per region.
const (
	gAuto   = "custom_proxy_group=♻️ 自动选择`url-test`.*`http://www.gstatic.com/generate_204`300,,50\n"
	gManual = "custom_proxy_group=☑️ 手动切换`select`.*\n"
	gDirect = "custom_proxy_group=🎯 全球直连`select`[]DIRECT`[]🚀 节点选择`[]♻️ 自动选择\n"
	gBlock  = "custom_proxy_group=🛑 广告拦截`select`[]REJECT`[]DIRECT\n"
	gFinal  = "custom_proxy_group=🐟 漏网之鱼`select`[]🚀 节点选择`[]🎯 全球直连`[]♻️ 自动选择`[]@regions\n"
	gMain   = "custom_proxy_group=🚀 节点选择`select`[]♻️ 自动选择`[]@regions`[]☑️ 手动切换`[]DIRECT\n"
)

func pick(name string) string {
	return "custom_proxy_group=" + name + "`select`[]🚀 节点选择`[]♻️ 自动选择`[]@regions`[]☑️ 手动切换`[]DIRECT\n"
}

// MinimalScheme reproduces the classic default: one selector, auto / fallback
// / load-balance over every node, region groups, China direct.
const MinimalScheme = `[custom]
ruleset=DIRECT,[]GEOIP,LAN
ruleset=DIRECT,[]GEOSITE,CN
ruleset=DIRECT,[]GEOIP,CN
ruleset=节点选择,[]FINAL
custom_proxy_group=节点选择` + "`select`[]自动选择`[]故障转移`[]负载均衡`[]@regions`[]DIRECT`.*" + `
custom_proxy_group=自动选择` + "`url-test`.*`http://www.gstatic.com/generate_204`300,,50" + `
custom_proxy_group=故障转移` + "`fallback`.*`http://www.gstatic.com/generate_204`300" + `
custom_proxy_group=负载均衡` + "`load-balance`.*`http://www.gstatic.com/generate_204`300" + `
`

var liteScheme = "[custom]\n" +
	"ruleset=🎯 全球直连," + acl + "LocalAreaNetwork.list\n" +
	"ruleset=🎯 全球直连," + acl + "UnBan.list\n" +
	"ruleset=🛑 广告拦截," + acl + "BanAD.list\n" +
	"ruleset=🛑 广告拦截," + acl + "BanProgramAD.list\n" +
	"ruleset=🚀 节点选择," + acl + "GoogleCN.list\n" +
	"ruleset=🎯 全球直连," + acl + "Ruleset/SteamCN.list\n" +
	"ruleset=Ⓜ️ 微软服务," + acl + "Microsoft.list\n" +
	"ruleset=🍎 苹果服务," + acl + "Apple.list\n" +
	"ruleset=📲 电报消息," + acl + "Telegram.list\n" +
	"ruleset=🚀 节点选择," + acl + "ProxyLite.list\n" +
	"ruleset=🎯 全球直连," + acl + "ChinaDomain.list\n" +
	"ruleset=🎯 全球直连," + acl + "ChinaCompanyIp.list\n" +
	"ruleset=🎯 全球直连,[]GEOIP,CN\n" +
	"ruleset=🐟 漏网之鱼,[]FINAL\n" +
	gMain + gManual + gAuto + pick("📲 电报消息") +
	"custom_proxy_group=Ⓜ️ 微软服务`select`[]🎯 全球直连`[]🚀 节点选择`.*\n" +
	"custom_proxy_group=🍎 苹果服务`select`[]🎯 全球直连`[]🚀 节点选择`.*\n" +
	gDirect + gBlock + gFinal

var mediaScheme = "[custom]\n" +
	"ruleset=🎯 全球直连," + acl + "LocalAreaNetwork.list\n" +
	"ruleset=🎯 全球直连," + acl + "UnBan.list\n" +
	"ruleset=🛑 广告拦截," + acl + "BanAD.list\n" +
	"ruleset=🤖 AI 服务," + acl + "Ruleset/OpenAi.list\n" +
	"ruleset=🤖 AI 服务," + cmACL + "ai.list\n" +
	"ruleset=🤖 AI 服务," + cmACL + "Claude.list\n" +
	"ruleset=🤖 AI 服务," + cmACL + "Copilot.list\n" +
	"ruleset=📹 油管视频," + acl + "Ruleset/YouTube.list\n" +
	"ruleset=🎥 奈飞视频," + acl + "Ruleset/Netflix.list\n" +
	"ruleset=🎬 迪士尼+," + acl + "Ruleset/DisneyPlus.list\n" +
	"ruleset=📺 巴哈姆特," + acl + "Ruleset/Bahamut.list\n" +
	"ruleset=🎵 TikTok," + acl + "Ruleset/TikTok.list\n" +
	"ruleset=🌍 国外媒体," + acl + "ProxyMedia.list\n" +
	"ruleset=🌏 国内媒体," + acl + "ChinaMedia.list\n" +
	"ruleset=📲 电报消息," + acl + "Telegram.list\n" +
	"ruleset=🚀 节点选择," + acl + "ProxyLite.list\n" +
	"ruleset=🎯 全球直连," + acl + "ChinaDomain.list\n" +
	"ruleset=🎯 全球直连," + acl + "ChinaCompanyIp.list\n" +
	"ruleset=🎯 全球直连,[]GEOIP,CN\n" +
	"ruleset=🐟 漏网之鱼,[]FINAL\n" +
	gMain + gManual + gAuto +
	pick("🤖 AI 服务") + pick("📹 油管视频") + pick("🎥 奈飞视频") + pick("🎬 迪士尼+") +
	"custom_proxy_group=📺 巴哈姆特`select`[]@regions`[]🚀 节点选择`[]☑️ 手动切换`[]DIRECT\n" +
	pick("🎵 TikTok") + pick("🌍 国外媒体") +
	"custom_proxy_group=🌏 国内媒体`select`[]DIRECT`[]@regions`[]☑️ 手动切换\n" +
	pick("📲 电报消息") + gDirect + gBlock + gFinal

var fullScheme = "[custom]\n" +
	"ruleset=🎯 全球直连," + acl + "LocalAreaNetwork.list\n" +
	"ruleset=🎯 全球直连," + acl + "UnBan.list\n" +
	"ruleset=🛑 广告拦截," + acl + "BanAD.list\n" +
	"ruleset=🍃 应用净化," + acl + "BanProgramAD.list\n" +
	"ruleset=📢 谷歌FCM," + acl + "Ruleset/GoogleFCM.list\n" +
	"ruleset=🚀 节点选择," + acl + "GoogleCN.list\n" +
	"ruleset=🎯 全球直连," + acl + "Ruleset/SteamCN.list\n" +
	"ruleset=Ⓜ️ 微软Bing," + acl + "Bing.list\n" +
	"ruleset=Ⓜ️ 微软云盘," + acl + "OneDrive.list\n" +
	"ruleset=Ⓜ️ 微软服务," + acl + "Microsoft.list\n" +
	"ruleset=🍎 苹果服务," + acl + "Apple.list\n" +
	"ruleset=📲 电报消息," + acl + "Telegram.list\n" +
	"ruleset=🤖 AI 服务," + acl + "Ruleset/OpenAi.list\n" +
	"ruleset=🤖 AI 服务," + cmACL + "ai.list\n" +
	"ruleset=🤖 AI 服务," + cmACL + "Claude.list\n" +
	"ruleset=🎶 网易音乐," + acl + "Ruleset/NetEaseMusic.list\n" +
	"ruleset=🎮 游戏平台," + acl + "Ruleset/Epic.list\n" +
	"ruleset=🎮 游戏平台," + acl + "Ruleset/Sony.list\n" +
	"ruleset=🎮 游戏平台," + acl + "Ruleset/Steam.list\n" +
	"ruleset=🎮 游戏平台," + acl + "Ruleset/Nintendo.list\n" +
	"ruleset=📹 油管视频," + acl + "Ruleset/YouTube.list\n" +
	"ruleset=🎥 奈飞视频," + acl + "Ruleset/Netflix.list\n" +
	"ruleset=📺 巴哈姆特," + acl + "Ruleset/Bahamut.list\n" +
	"ruleset=📺 哔哩哔哩," + acl + "Ruleset/BilibiliHMT.list\n" +
	"ruleset=📺 哔哩哔哩," + acl + "Ruleset/Bilibili.list\n" +
	"ruleset=🌏 国内媒体," + acl + "ChinaMedia.list\n" +
	"ruleset=🌍 国外媒体," + acl + "ProxyMedia.list\n" +
	"ruleset=🚀 节点选择," + acl + "ProxyGFWlist.list\n" +
	"ruleset=🎯 全球直连," + acl + "ChinaDomain.list\n" +
	"ruleset=🎯 全球直连," + acl + "ChinaCompanyIp.list\n" +
	"ruleset=🎯 全球直连," + acl + "Download.list\n" +
	"ruleset=🎯 全球直连,[]GEOIP,CN\n" +
	"ruleset=🐟 漏网之鱼,[]FINAL\n" +
	gMain + gManual + gAuto +
	pick("📲 电报消息") + pick("🤖 AI 服务") + pick("📹 油管视频") + pick("🎥 奈飞视频") +
	"custom_proxy_group=📺 巴哈姆特`select`[]@regions`[]🚀 节点选择`[]☑️ 手动切换`[]DIRECT\n" +
	"custom_proxy_group=📺 哔哩哔哩`select`[]🎯 全球直连`[]@regions\n" +
	pick("🌍 国外媒体") +
	"custom_proxy_group=🌏 国内媒体`select`[]DIRECT`[]@regions`[]☑️ 手动切换\n" +
	"custom_proxy_group=📢 谷歌FCM`select`[]DIRECT`[]🚀 节点选择`[]@regions\n" +
	"custom_proxy_group=Ⓜ️ 微软Bing`select`[]DIRECT`[]🚀 节点选择`[]@regions\n" +
	"custom_proxy_group=Ⓜ️ 微软云盘`select`[]DIRECT`[]🚀 节点选择`[]@regions\n" +
	"custom_proxy_group=Ⓜ️ 微软服务`select`[]DIRECT`[]🚀 节点选择`[]@regions\n" +
	"custom_proxy_group=🍎 苹果服务`select`[]DIRECT`[]🚀 节点选择`[]@regions\n" +
	"custom_proxy_group=🎮 游戏平台`select`[]DIRECT`[]🚀 节点选择`[]@regions\n" +
	"custom_proxy_group=🎶 网易音乐`select`[]DIRECT`[]🚀 节点选择`(网易|音乐|解锁|Music|NetEase)\n" +
	gDirect + gBlock +
	"custom_proxy_group=🍃 应用净化`select`[]REJECT`[]DIRECT\n" + gFinal

func cmPreset(file, name, desc string) SchemePreset {
	return SchemePreset{ID: "acl4ssr:" + file, Name: name, Group: "ACL4SSR CM_Online", Desc: desc,
		URL: "https://raw.githubusercontent.com/cmliu/ACL4SSR/main/Clash/config/" + file + ".ini"}
}

var presets = []SchemePreset{
	{ID: "builtin:minimal", Name: "极简默认分流", Group: "Polaris 内置", Desc: "节点选择 / 自动 / 故障转移 / 负载均衡 + 地区分组，国内直连", Text: MinimalScheme},
	{ID: "builtin:lite", Name: "ACL4SSR 精简分流", Group: "Polaris 内置", Desc: "去广告、微软、苹果、电报、国内直连", Text: liteScheme},
	{ID: "builtin:media", Name: "流媒体与 AI 分流", Group: "Polaris 内置", Desc: "OpenAI/Claude、YouTube、Netflix、Disney+、TikTok 独立分组", Text: mediaScheme},
	{ID: "builtin:full", Name: "ACL4SSR 完整分流", Group: "Polaris 内置", Desc: "完整 ACL4SSR 分组：游戏、哔哩哔哩、网易云、微软细分等", Text: fullScheme},
	cmPreset("ACL4SSR_Online", "默认版", "分组比较全，港/美地区组"),
	cmPreset("ACL4SSR_Online_MultiCountry", "多国分组", "多国家地区分组"),
	cmPreset("ACL4SSR_Online_MultiCountry_CF", "多国分组 + CF", "多国家地区分组，含 Cloudflare 优选"),
	cmPreset("ACL4SSR_Online_Full", "全分组", "全分组，重度用户"),
	cmPreset("ACL4SSR_Online_Full_CF", "全分组 + CF", "全分组，含 Cloudflare 优选"),
	cmPreset("ACL4SSR_Online_Full_MultiMode", "全分组多模式", "全分组，地区组支持自动 / 负载均衡"),
	cmPreset("ACL4SSR_Online_Full_MultiMode_CF", "全分组多模式 + CF", "全分组多模式，含 Cloudflare 优选"),
}

// Presets lists the selectable rule schemes.
func Presets() []SchemePreset { return presets }

// Preset finds a preset by id.
func Preset(id string) (SchemePreset, bool) {
	for _, p := range presets {
		if p.ID == id {
			return p, true
		}
	}
	return SchemePreset{}, false
}

// IsBuiltinPreset reports whether id names a preset served from the panel.
func IsBuiltinPreset(id string) bool { return strings.HasPrefix(id, "builtin:") }
