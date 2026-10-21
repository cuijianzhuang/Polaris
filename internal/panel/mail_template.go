package panel

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/store"
)

type mailView struct {
	Site, Title, Code, Support string
	Lines                      []string
	CodeMinutes                int
	Portal, Subscription       string
	SupportLink                string
}

// mailWebLink allows only absolute web links in email action buttons.
// Other support contact formats are still shown as text.
func mailWebLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return ""
	}
	return raw
}

func (s *Server) mailBody(title, text string, u *store.User) (string, string) {
	return s.renderMailBody(title, text, u, "")
}

func (s *Server) mailCodeBody(title, code string) (string, string) {
	text := fmt.Sprintf("你的验证码是：%s\n\n%d 分钟内有效。如果不是你本人操作，请忽略这封邮件。", code, int(mailCodeTTL.Minutes()))
	return s.renderMailBody(title, text, nil, code)
}

// Keep the plain-text alternative and its links while rendering all HTML
// through contextual escaping. No remote images, fonts or scripts are used.
func (s *Server) renderMailBody(title, text string, u *store.User, code string) (string, string) {
	v := mailView{Site: s.st.GetSetting("site_title", "Polaris"), Title: title, Code: code, CodeMinutes: int(mailCodeTTL.Minutes()),
		Lines: strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")}
	plain := text
	if u != nil && u.SubToken != "" {
		link := s.subURL(u.SubToken)
		plain += "\n\n订阅链接：" + link
		v.Subscription = mailWebLink(link)
	}
	if s.portalEnabled() {
		link := s.portalURL()
		plain += "\n用户中心：" + link
		v.Portal = mailWebLink(link)
	}
	if sup := strings.TrimSpace(s.st.GetSetting("sub_support_url", "")); sup != "" {
		plain += "\n客服：" + sup
		v.Support, v.SupportLink = sup, mailWebLink(sup)
	}
	plain += "\n\n— " + v.Site
	var out bytes.Buffer
	if err := mailTemplate.Execute(&out, v); err != nil {
		s.log.Warn("render mail", "err", err)
		return plain, "" // SMTP still sends the complete plain-text alternative.
	}
	return plain, out.String()
}

// Tables and inline styles provide a readable fallback in email clients
// that strip stylesheets; media queries refine mobile and dark appearance.
var mailTemplate = template.Must(template.New("mail").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark"><meta name="supported-color-schemes" content="light dark">
<title>{{.Title}} · {{.Site}}</title>
<style>
@media only screen and (max-width:600px){.mail-shell{padding:20px 12px!important}.mail-header,.mail-content,.mail-links{padding-left:22px!important;padding-right:22px!important}.mail-title{font-size:25px!important}.mail-code{font-size:32px!important;letter-spacing:6px!important}}
@media (prefers-color-scheme:dark){.mail-bg{background-color:#10171c!important}.mail-card{background-color:#19232a!important;border-color:#304049!important}.mail-ink{color:#edf4f6!important}.mail-muted{color:#aabcc5!important}.mail-rule{border-color:#304049!important}.mail-code-box{background-color:#10353d!important;border-color:#245660!important}.mail-code{color:#6cdeed!important}.mail-secondary{background-color:#23333d!important;border-color:#425a67!important;color:#d6f3f7!important}.mail-link{color:#83deed!important}}
</style></head>
<body class="mail-bg" style="margin:0;padding:0;background-color:#f1f5f7;color:#142c39;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Microsoft YaHei',Arial,sans-serif;-webkit-text-size-adjust:100%">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;font-size:1px;line-height:1px;mso-hide:all">{{.Title}} · {{.Site}}</div>
<table role="presentation" class="mail-bg" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="#f1f5f7" style="width:100%;background-color:#f1f5f7"><tr><td class="mail-shell" align="center" style="padding:44px 20px">
<!--[if mso]><table role="presentation" width="600" align="center" cellpadding="0" cellspacing="0"><tr><td><![endif]-->
<table role="presentation" class="mail-card" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="#ffffff" style="width:100%;max-width:600px;background-color:#ffffff;border:1px solid #dfe8ec;border-radius:18px;border-collapse:separate;overflow:hidden">
<tr><td height="5" bgcolor="#0e8198" style="height:5px;background-color:#0e8198;font-size:1px;line-height:1px;border-radius:18px 18px 0 0">&nbsp;</td></tr>
<tr><td class="mail-header" style="padding:30px 36px 26px">
<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="width:100%"><tr>
<td width="42" valign="middle" style="width:42px"><div style="width:36px;height:36px;line-height:36px;border-radius:10px;background-color:#0e8198;color:#ffffff;text-align:center;font-size:25px" aria-hidden="true">✦</div></td>
<td class="mail-ink" valign="middle" style="padding-left:10px;color:#142c39;font-size:17px;font-weight:700;word-break:break-word;overflow-wrap:anywhere">{{.Site}}</td>
<td class="mail-muted" align="right" valign="middle" style="padding-left:10px;color:#718793;font-size:11px;letter-spacing:1px;white-space:nowrap">{{if .Code}}安全验证{{else}}服务通知{{end}}</td>
</tr></table>
</td></tr>
<tr><td class="mail-content" style="padding:0 36px 30px">
<h1 class="mail-title mail-ink" style="margin:0 0 20px;color:#142c39;font-size:28px;font-weight:700;line-height:1.4;word-break:break-word;overflow-wrap:anywhere">{{.Title}}</h1>
{{if .Code}}
<p class="mail-muted" style="margin:0 0 18px;color:#617987;font-size:14px;line-height:1.8">你的验证码是：</p>
<table role="presentation" class="mail-code-box" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="#eff9fb" style="width:100%;background-color:#eff9fb;border:1px solid #cce9ee;border-radius:12px"><tr><td align="center" style="padding:22px 12px">
<span class="mail-code" style="display:inline-block;color:#08758c;font:700 32px/1.4 'SFMono-Regular',Consolas,'Liberation Mono',monospace;letter-spacing:6px;white-space:nowrap">{{.Code}}</span>
</td></tr></table>
<p class="mail-muted" style="margin:18px 0 0;color:#617987;font-size:13px;line-height:1.8">{{.CodeMinutes}} 分钟内有效。如果不是你本人操作，请忽略这封邮件。</p>
{{else}}
<div class="mail-ink" style="color:#304b5a;font-size:15px;line-height:1.9;word-break:break-word;overflow-wrap:anywhere">{{range .Lines}}{{.}}<br>{{end}}</div>
{{end}}
</td></tr>
{{if or .Portal .Subscription}}
<tr><td class="mail-links mail-rule" style="padding:24px 36px 28px;border-top:1px solid #e7eef2">
{{if .Portal}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%"><tr><td align="center" bgcolor="#0e8198" style="background-color:#0e8198;border-radius:9px"><a href="{{.Portal}}" style="display:block;padding:13px 18px;color:#ffffff;font-size:14px;font-weight:600;line-height:1.5;text-decoration:none;text-align:center">打开用户中心</a></td></tr></table>{{end}}
{{if .Subscription}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%;{{if .Portal}}margin-top:12px;{{end}}"><tr><td class="mail-secondary" align="center" bgcolor="#f4f9fa" style="background-color:#f4f9fa;border:1px solid #d7e8ed;border-radius:9px"><a class="mail-secondary" href="{{.Subscription}}" style="display:block;padding:12px 18px;color:#17677c;font-size:14px;font-weight:600;line-height:1.5;text-decoration:none;text-align:center">查看订阅链接</a></td></tr></table>{{end}}
<p class="mail-muted" style="margin:18px 0 6px;color:#718793;font-size:11px;line-height:1.7">如果按钮无法打开，可使用以下链接：</p>
{{if .Portal}}<div style="font-size:11px;line-height:1.8;word-break:break-all"><span class="mail-muted" style="color:#718793">用户中心：</span><a class="mail-link" href="{{.Portal}}" style="color:#287a8e;text-decoration:underline">{{.Portal}}</a></div>{{end}}
{{if .Subscription}}<div style="font-size:11px;line-height:1.8;word-break:break-all"><span class="mail-muted" style="color:#718793">订阅链接：</span><a class="mail-link" href="{{.Subscription}}" style="color:#287a8e;text-decoration:underline">{{.Subscription}}</a></div>{{end}}
</td></tr>
{{end}}
<tr><td class="mail-links mail-rule" style="padding:20px 36px;border-top:1px solid #e7eef2">
<p class="mail-muted" style="margin:0;color:#718793;font-size:12px;line-height:1.8;word-break:break-word;overflow-wrap:anywhere">此邮件由 {{.Site}} 自动发送。{{if .Support}}<br>需要帮助？{{if .SupportLink}}<a class="mail-link" href="{{.SupportLink}}" style="color:#287a8e;text-decoration:underline">联系客服</a>{{else}}客服：{{.Support}}{{end}}{{end}}</p>
</td></tr>
</table>
<!--[if mso]></td></tr></table><![endif]-->
<p class="mail-muted" style="margin:20px 0 0;color:#8297a2;font-size:11px;line-height:1.6;word-break:break-word;overflow-wrap:anywhere">{{.Site}} · 服务与账户通知</p>
</td></tr></table>
</body></html>`))
