package sub

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

var pageTpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"bytes": HumanBytes,
}).Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="icon" href="data:image/svg+xml,%3Csvg%20xmlns='http://www.w3.org/2000/svg'%20viewBox='0%200%20512%20512'%20role='img'%20aria-label='Polaris'%3E%3Cdefs%3E%3ClinearGradient%20id='bg'%20x1='0'%20y1='0'%20x2='1'%20y2='1'%3E%3Cstop%20offset='0'%20stop-color='%2313899F'/%3E%3Cstop%20offset='1'%20stop-color='%230A4557'/%3E%3C/linearGradient%3E%3CradialGradient%20id='glow'%20cx='0.5'%20cy='0.5'%20r='0.5'%3E%3Cstop%20offset='0'%20stop-color='%239FF3E8'%20stop-opacity='.5'/%3E%3Cstop%20offset='1'%20stop-color='%239FF3E8'%20stop-opacity='0'/%3E%3C/radialGradient%3E%3C/defs%3E%3Crect%20width='512'%20height='512'%20rx='116'%20fill='url(%23bg)'/%3E%3Ccircle%20cx='256'%20cy='262'%20r='168'%20fill='none'%20stroke='%23FFFFFF'%20stroke-opacity='.16'%20stroke-width='10'/%3E%3Ccircle%20cx='256'%20cy='262'%20r='130'%20fill='url(%23glow)'/%3E%3Cpath%20d='M256%2058%20Q271%20247%20384%20262%20Q271%20277%20256%20476%20Q241%20277%20128%20262%20Q241%20247%20256%2058%20Z'%20fill='%23FFFFFF'/%3E%3Cpath%20d='M375%20120%20Q380%20162%20420%20167%20Q380%20172%20375%20214%20Q370%20172%20330%20167%20Q370%20162%20375%20120%20Z'%20fill='%239FF3E8'/%3E%3Ccircle%20cx='137'%20cy='381'%20r='13'%20fill='%239FF3E8'%20fill-opacity='.85'/%3E%3C/svg%3E">
<meta name="robots" content="noindex"><meta name="referrer" content="no-referrer">
<title>{{.Title}} · 订阅</title>
<style>
:root{--bg:#F4F5F2;--card:#fff;--ink:#15181C;--mute:#5B6470;--line:#E2E5E0;--pri:#0E7490;--up:#7DD3E3;--ok:#15803D;--warn:#B45309;--bad:#B91C1C;--note:#ECF7F9}
@media (prefers-color-scheme:dark){:root{--bg:#101316;--card:#181C20;--ink:#E6E8EA;--mute:#9AA2AB;--line:#2A3036;--pri:#22A3BF;--up:#155E6E;--ok:#4ADE80;--warn:#F59E0B;--bad:#F87171;--note:#14262B}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.6 "IBM Plex Sans","Noto Sans SC",system-ui,sans-serif}
main{max-width:600px;margin:0 auto;padding:28px 16px 48px;display:flex;flex-direction:column;gap:14px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:18px}
h1{font-size:22px;margin:0}h2{font-size:15px;margin:0 0 10px}
.mute{color:var(--mute);font-size:13px}
.row{display:flex;align-items:center;gap:8px;flex-wrap:wrap}.sp{flex:1}
.st{font-size:12px;padding:2px 8px;border-radius:6px;border:1px solid currentColor}
.st.active{color:var(--ok)}.st.limited,.st.expired,.st.disabled{color:var(--bad)}
.bar{height:8px;border-radius:4px;background:var(--line);overflow:hidden;margin-top:8px}.bar>div{height:100%;background:var(--pri)}
.kv{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:8px;margin-top:12px}
.kv div{background:var(--bg);border-radius:8px;padding:8px 10px}.kv b{display:block;font-size:15px}
.usage{margin-top:14px;display:flex;align-items:baseline;gap:2px;flex-wrap:wrap}.usage b{font-size:28px;font-weight:600;letter-spacing:-.01em}.usage .pct{margin-left:auto;font-weight:600;font-size:15px;color:var(--pri)}
.pct.warn{color:var(--warn)}.pct.bad{color:var(--bad)}.bar>div.warn{background:var(--warn)}.bar>div.bad{background:var(--bad)}
.plan{margin-top:12px;border-top:1px solid var(--line);padding-top:10px;display:flex;flex-direction:column;gap:6px}
.plan div{display:flex;justify-content:space-between;gap:12px;font-size:13px}.plan span{color:var(--mute)}.plan b{font-weight:500;text-align:right}.plan b.warn{color:var(--warn)}.plan b.bad{color:var(--bad)}
.note{background:var(--note);border:1px solid var(--line);border-radius:10px;padding:12px 14px;white-space:pre-wrap}
.grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px}
a.btn,button.btn{display:flex;align-items:center;justify-content:center;min-height:44px;border-radius:8px;border:1px solid var(--line);background:var(--card);color:var(--ink);text-decoration:none;font:inherit;font-weight:500;cursor:pointer;padding:0 10px;text-align:center}
a.btn.pri{background:var(--pri);border-color:var(--pri);color:#fff}
button.sm{min-height:32px;font-size:12px;padding:0 10px}
code{font-family:"IBM Plex Mono",ui-monospace,monospace;font-size:12px;word-break:break-all}
img.qr{display:block;margin:0 auto;width:200px;height:200px;background:#fff;border-radius:8px;padding:6px}
svg{display:block;width:100%;height:auto}
.legend{display:flex;gap:14px;font-size:12px;color:var(--mute);margin-top:6px}.legend i{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:4px;vertical-align:-1px}
.node{border-top:1px solid var(--line);padding:10px 0}.node:first-of-type{border-top:0}
details summary{cursor:pointer;list-style:none}details summary::-webkit-details-marker{display:none}
.nw{flex-wrap:nowrap}.nm{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.tag{white-space:nowrap;font-size:11px;padding:1px 6px;border-radius:5px;background:var(--bg);color:var(--mute)}
.who{display:flex;align-items:center;gap:8px;flex-wrap:wrap;font-size:13px;color:var(--mute)}
</style></head><body><main>
{{if .Portal}}<div class="who"><span>👤 已通过 Telegram 登录</span><span class="sp"></span><button class="btn sm" type="button" data-portal="reset">重置订阅链接</button><button class="btn sm" type="button" data-portal="logout">退出</button></div>{{end}}
{{if .Announce}}<section class="note" role="note">📢 {{.Announce}}</section>{{end}}
<section class="card">
<div class="row"><h1>{{.Title}}</h1><span class="sp"></span>{{if .Status}}<span class="st {{.Status}}">{{.StatusLabel}}</span>{{end}}</div>
{{if .Username}}<div class="mute">{{.Username}}</div>{{end}}
<div class="usage"><b>{{bytes .Used}}</b><span class="mute"> / {{if .Total}}{{bytes .Total}}{{else}}不限流量{{end}}</span>{{if .Total}}<span class="pct {{.Level}}">{{.Percent}}%</span>{{end}}</div>
{{if .Total}}<div class="bar" role="progressbar" aria-valuenow="{{.Percent}}" aria-valuemin="0" aria-valuemax="100"><div class="{{.Level}}" style="width:{{.Percent}}%"></div></div>{{end}}
<div class="kv">
<div><span class="mute">剩余流量</span><b>{{if .Total}}{{bytes .Left}}{{else}}不限{{end}}</b></div>
<div><span class="mute">上传</span><b>{{bytes .Upload}}</b></div>
<div><span class="mute">下载</span><b>{{bytes .Download}}</b></div>
</div>
<div class="plan">
<div><span>📦 套餐</span><b>{{if .Total}}{{bytes .Total}}{{else}}不限流量{{end}}</b></div>
<div><span>⏳ 到期</span><b class="{{.ExpireLevel}}">{{.ExpiryText}}</b></div>
{{if .Expire}}<div><span>🕒 到期时间</span><b>{{.Expire}}</b></div>{{end}}
{{if .ResetNote}}<div><span>♻️ 流量重置</span><b>{{.ResetNote}}</b></div>{{end}}
{{range .NodeQuotas}}<div><span>📍 {{.Node}}</span><b{{if .Bad}} class="bad"{{end}}>{{.Text}}</b></div>{{end}}
</div>
</section>
{{if .Chart}}<section class="card"><h2>近 {{.ChartDays}} 天流量</h2>{{.Chart}}
<div class="legend"><span><i style="background:var(--pri)"></i>下载</span><span><i style="background:var(--up)"></i>上传</span><span class="sp"></span><span>峰值 {{bytes .ChartMax}}</span></div></section>{{end}}
<section class="card"><img class="qr" src="{{.QR}}" alt="订阅二维码"><p class="mute" style="text-align:center;margin:8px 0">用客户端扫码导入，或复制链接</p>
<div class="row"><code style="flex:1">{{.URL}}</code><button class="btn sm" type="button" data-copy="{{.URL}}">复制</button></div></section>
<section class="card"><h2>一键导入</h2><div class="grid">
{{range .Clients}}<a class="btn{{if .Pri}} pri{{end}}" href="{{.Href}}">{{.Name}}</a>{{end}}
</div><p class="mute" style="margin:10px 0 0">按钮无反应时，复制上方链接到客户端手动添加订阅。</p></section>
{{if .Links}}<section class="card"><h2>节点（{{len .Links}}）</h2>
{{range .Links}}<div class="node"><details><summary class="row nw"><span class="nm" title="{{.Name}}">{{.Name}}</span><span class="tag">{{.Protocol}}</span><button class="btn sm" type="button" data-copy="{{.Link}}">复制</button></summary>
{{if .QR}}<img class="qr" style="margin-top:10px" src="{{.QR}}" alt="{{.Name}} 二维码">{{end}}</details></div>{{end}}
</section>{{end}}
{{if or .SupportURL .BotURL}}<section class="card"><div class="grid">
{{if .BotURL}}<a class="btn" href="{{.BotURL}}">Telegram 机器人绑定</a>{{end}}
{{if .SupportURL}}<a class="btn" href="{{.SupportURL}}">联系客服</a>{{end}}
</div></section>{{end}}
</main>
<script>
function legacyCopy(t){var a=document.createElement('textarea');a.value=t;a.setAttribute('readonly','');a.style.position='fixed';a.style.top='-1000px';a.style.opacity='0';document.body.appendChild(a);a.select();a.setSelectionRange(0,t.length);var ok=false;try{ok=document.execCommand('copy')}catch(x){}document.body.removeChild(a);return ok}
function copyText(t){if(navigator.clipboard&&window.isSecureContext){return navigator.clipboard.writeText(t).catch(function(){if(!legacyCopy(t))throw 0})}return legacyCopy(t)?Promise.resolve():Promise.reject()}
document.addEventListener('click',function(e){var b=e.target.closest('[data-copy]');if(!b)return;e.preventDefault();var t=b.getAttribute('data-copy'),l=b.textContent;
copyText(t).then(function(){b.textContent='已复制';setTimeout(function(){b.textContent=l},1500)},function(){prompt('复制以下内容',t)})});
{{if .Portal}}document.addEventListener('click',function(e){var b=e.target.closest('[data-portal]');if(!b)return;var a=b.getAttribute('data-portal');
if(a==='reset'&&!confirm('重置后旧订阅链接和已导入的节点立即失效，所有客户端都需要重新导入。确定重置？'))return;b.disabled=true;
fetch('/u/'+a,{method:'POST',headers:{'X-Polaris-Portal':'1'},credentials:'same-origin'}).then(function(r){if(!r.ok)throw 0;location.replace('/u')}).catch(function(){b.disabled=false;alert('操作失败，请刷新后重试')})});{{end}}
</script></body></html>`))

type pageClient struct {
	Name string
	Href template.URL
	Pri  bool
}

type pageLink struct {
	PageLink
	QR template.URL
}

var statusLabel = map[string]string{"active": "正常", "limited": "流量已用尽", "expired": "已到期", "disabled": "已停用"}

// RenderPage renders the browser info page with usage, QR codes and import buttons.
func RenderPage(info Info) ([]byte, error) {
	png, err := qrcode.Encode(info.URL, qrcode.Medium, 400)
	if err != nil {
		return nil, err
	}
	used := info.Upload + info.Download
	pct := 0
	left := int64(0)
	if info.Total > 0 {
		pct = int(used * 100 / info.Total)
		if pct > 100 {
			pct = 100
		}
		if left = info.Total - used; left < 0 {
			left = 0
		}
	}
	exp, expLevel := "", ""
	if info.Expire != nil {
		exp = info.Expire.Local().Format("2006-01-02 15:04")
		switch d := time.Until(*info.Expire); {
		case d <= 0:
			expLevel = "bad"
		case d < 7*24*time.Hour:
			expLevel = "warn"
		}
	}
	level := ""
	switch {
	case pct >= 95:
		level = "bad"
	case pct >= 80:
		level = "warn"
	}
	links := make([]pageLink, 0, len(info.Links))
	for i, l := range info.Links {
		pl := pageLink{PageLink: l}
		if i < 80 { // keep the page light for huge lists
			if q, err := qrcode.Encode(l.Link, qrcode.Low, 320); err == nil {
				pl.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(q))
			}
		}
		links = append(links, pl)
	}
	chart, max := dailyChart(info.Daily)
	data := map[string]any{
		"Title": info.Title, "Username": info.Username, "Used": used, "Upload": info.Upload, "Download": info.Download,
		"Total": info.Total, "Left": left, "Percent": pct, "Expire": exp, "ExpiryText": ExpiryLeft(info), "ExpireLevel": expLevel, "Level": level, "URL": info.URL,
		"Status": info.Status, "StatusLabel": statusLabel[info.Status], "Announce": info.Announce, "ResetNote": info.ResetNote,
		"SupportURL": safeURL(info.SupportURL), "BotURL": safeURL(info.BotURL), "Portal": info.Portal, "NodeQuotas": info.NodeQuotas,
		"QR":      template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png)),
		"Clients": clientLinks(info), "Links": links, "Chart": chart, "ChartMax": max, "ChartDays": len(info.Daily),
	}
	var buf bytes.Buffer
	if err := pageTpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// safeURL only lets http(s) and tg links through.
func safeURL(s string) template.URL {
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "tg://") {
		return template.URL(s)
	}
	return ""
}

func withFormat(u, f string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "format=" + f
}

// clientLinks builds one-tap import deep links for common clients.
func clientLinks(info Info) []pageClient {
	q := url.QueryEscape
	name := q(info.Title)
	clash := withFormat(info.URL, "clash")
	sb := withFormat(info.URL, "singbox")
	links := info.URL
	return []pageClient{
		{"Clash Verge / mihomo", template.URL("clash://install-config?url=" + q(clash) + "&name=" + name), true},
		{"sing-box (SFA / SFI)", template.URL("sing-box://import-remote-profile?url=" + q(sb) + "#" + url.PathEscape(info.Title)), false},
		{"Shadowrocket", template.URL("shadowrocket://add/sub://" + base64.RawURLEncoding.EncodeToString([]byte(links)) + "?remark=" + name), false},
		{"Stash", template.URL("stash://install-config?url=" + q(clash) + "&name=" + name), false},
		{"Hiddify", template.URL("hiddify://import/" + links + "#" + url.PathEscape(info.Title)), false},
		{"Happ", template.URL("happ://add/" + links), false},
		{"v2rayNG", template.URL("v2rayng://install-config?url=" + q(links)), false},
		{"Streisand", template.URL("streisand://import/" + links + "#" + url.PathEscape(info.Title)), false},
		{"Surge", template.URL("surge:///install-config?url=" + q(withFormat(info.URL, "surge"))), false},
		{"Loon", template.URL("loon://import?sub=" + q(withFormat(info.URL, "loon"))), false},
		{"Quantumult X", template.URL("quantumult-x:///add-resource?remote-resource=" + q(`{"server_remote":["`+withFormat(info.URL, "quanx")+`, tag=`+info.Title+`"]}`)), false},
		{"Karing", template.URL("karing://install-config?url=" + q(links) + "&name=" + name), false},
	}
}

// dailyChart draws stacked up/down bars as inline SVG (no JS, works in any theme).
func dailyChart(days []DayUsage) (template.HTML, int64) {
	if len(days) == 0 {
		return "", 0
	}
	var max int64
	var sum int64
	for _, d := range days {
		if t := d.Up + d.Down; t > max {
			max = t
		}
		sum += d.Up + d.Down
	}
	if sum == 0 {
		return "", 0
	}
	const w, h, pad = 560.0, 140.0, 18.0
	bw := w / float64(len(days))
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="每日流量柱状图">`, w, h+pad)
	for i, d := range days {
		x := float64(i)*bw + bw*0.15
		total := float64(d.Up + d.Down)
		th := total / float64(max) * h
		uh := float64(d.Up) / float64(max) * h
		fmt.Fprintf(&b, `<g><title>%s 下载 %s · 上传 %s</title>`, d.Day.Format("01-02"), HumanBytes(d.Down), HumanBytes(d.Up))
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" style="fill:var(--pri)"/>`, x, h-th, bw*0.7, th-uh)
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" style="fill:var(--up)"/></g>`, x, h-uh, bw*0.7, uh)
		if i == 0 || i == len(days)-1 || (len(days) > 10 && i == len(days)/2) {
			anchor := "middle"
			if i == 0 {
				anchor = "start"
			} else if i == len(days)-1 {
				anchor = "end"
			}
			tx := x + bw*0.35
			if anchor == "start" {
				tx = x
			} else if anchor == "end" {
				tx = x + bw*0.7
			}
			fmt.Fprintf(&b, `<text x="%.1f" y="%.0f" font-size="11" style="fill:var(--mute)" text-anchor="%s">%s</text>`, tx, h+pad-3, anchor, d.Day.Format("01-02"))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()), max
}

// HumanBytes formats a byte count.
//
// Units read like the panel ("1.5 GB", "70 GB", "512 MB"): 1024-based, at
// most three significant digits, trailing zeros dropped.
func HumanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", max(n, 0))
	}
	v, exp := float64(n), -1
	for v >= 1024 && exp < 5 {
		v /= 1024
		exp++
	}
	prec := 2
	switch {
	case v >= 100:
		prec = 0
	case v >= 10:
		prec = 1
	}
	s := strconv.FormatFloat(v, 'f', prec, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s + " " + string("KMGTPE"[exp]) + "B"
}
