package sub

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/url"

	qrcode "github.com/skip2/go-qrcode"
)

var pageTpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"bytes": HumanBytes,
}).Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · 订阅</title>
<style>
:root{--bg:#F4F5F2;--card:#fff;--ink:#15181C;--mute:#5B6470;--line:#E2E5E0;--pri:#0E7490}
@media (prefers-color-scheme:dark){:root{--bg:#101316;--card:#181C20;--ink:#E6E8EA;--mute:#9AA2AB;--line:#2A3036;--pri:#22A3BF}}
body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.6 "IBM Plex Sans","Noto Sans SC",system-ui,sans-serif}
main{max-width:560px;margin:0 auto;padding:32px 16px;display:flex;flex-direction:column;gap:16px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:20px}
h1{font-size:22px;margin:0}
.mute{color:var(--mute);font-size:13px}
.bar{height:6px;border-radius:3px;background:var(--line);overflow:hidden;margin-top:8px}.bar>div{height:100%;background:var(--pri)}
.grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px}
a.btn{display:flex;align-items:center;justify-content:center;min-height:44px;border-radius:8px;border:1px solid var(--line);color:var(--ink);text-decoration:none;font-weight:500}
a.btn.pri{background:var(--pri);border-color:var(--pri);color:#fff}
code{font-family:"IBM Plex Mono",monospace;font-size:12px;word-break:break-all}
img{display:block;margin:0 auto;width:220px;height:220px;background:#fff;border-radius:8px}
</style></head><body><main>
<section class="card"><h1>{{.Title}}</h1><div class="mute">{{.Username}}</div>
<div style="margin-top:12px">已用 {{bytes .Used}} / {{if .Total}}{{bytes .Total}}{{else}}不限{{end}}</div>
{{if .Total}}<div class="bar"><div style="width:{{.Percent}}%"></div></div>{{end}}
<div class="mute" style="margin-top:8px">到期：{{if .Expire}}{{.Expire}}{{else}}永不{{end}}</div></section>
<section class="card"><img src="{{.QR}}" alt="订阅二维码"><p class="mute" style="text-align:center">用客户端扫码导入</p><code>{{.URL}}</code></section>
<section class="card"><div class="grid">
<a class="btn pri" href="{{.Clash}}">Clash / mihomo</a>
<a class="btn" href="{{.Singbox}}">sing-box</a>
<a class="btn" href="{{.Shadowrocket}}">Shadowrocket</a>
<a class="btn" href="{{.V2rayNG}}">v2rayNG</a>
</div></section>
</main></body></html>`))

// RenderPage renders the browser info page with QR code and import buttons.
func RenderPage(info Info) ([]byte, error) {
	png, err := qrcode.Encode(info.URL, qrcode.Medium, 440)
	if err != nil {
		return nil, err
	}
	used := info.Upload + info.Download
	pct := 0
	if info.Total > 0 {
		pct = int(used * 100 / info.Total)
		if pct > 100 {
			pct = 100
		}
	}
	exp := ""
	if info.Expire != nil {
		exp = info.Expire.Local().Format("2006-01-02 15:04")
	}
	esc := url.QueryEscape(info.URL)
	data := map[string]any{
		"Title": info.Title, "Username": info.Username, "Used": used, "Total": info.Total, "Percent": pct,
		"Expire": exp, "URL": info.URL,
		"QR":           template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png)),
		"Clash":        template.URL("clash://install-config?url=" + esc + "&name=" + url.QueryEscape(info.Title)),
		"Singbox":      template.URL("sing-box://import-remote-profile?url=" + esc + "#" + url.PathEscape(info.Title)),
		"Shadowrocket": template.URL("shadowrocket://add/sub://" + base64.RawURLEncoding.EncodeToString([]byte(info.URL)) + "?remark=" + url.QueryEscape(info.Title)),
		"V2rayNG":      template.URL("v2rayng://install-config?url=" + esc),
	}
	var buf bytes.Buffer
	if err := pageTpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// HumanBytes formats a byte count.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
