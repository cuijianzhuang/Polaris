package panel

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Public status page (/status, off by default): node state, resources and
// carrier latency for anyone, without addresses, versions or hidden nodes.

const statusPath = "/status"

type statusSnap struct {
	at   time.Time
	body []byte
}

func (s *Server) statusPageEnabled() bool { return s.st.GetSetting("status_page", "false") == "true" }

func (s *Server) statusGate(decoy, h gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.statusPageEnabled() {
			decoy(c)
			return
		}
		h(c)
	}
}

func (s *Server) statusTitle() string {
	if t := strings.TrimSpace(s.st.GetSetting("status_title", "")); t != "" {
		return t
	}
	return s.st.GetSetting("site_title", "Polaris") + " · 服务器状态"
}

// statusData: GET /status/data, cached for 10 seconds.
func (s *Server) statusData(c *gin.Context) {
	if sn := s.statusCache.Load(); sn != nil && time.Since(sn.at) < 10*time.Second {
		c.Data(http.StatusOK, "application/json; charset=utf-8", sn.body)
		return
	}
	out := s.probeOverview(true)
	out["title"] = s.statusTitle()
	out["note"] = s.st.GetSetting("status_note", "")
	body, err := json.Marshal(out)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	s.statusCache.Store(&statusSnap{at: time.Now(), body: body})
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// statusPageHTML: GET /status.
func (s *Server) statusPageHTML(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = statusTpl.Execute(c.Writer, map[string]any{"Title": s.statusTitle(), "Data": statusPath + "/data"})
}

// statusCSS is the probe card design shared with the admin probe page
// (web/src/styles.css, "Server probe v2").
const statusCSS = `.pv{--pv-card:#fff;--pv-card-2:#f8fafc;--pv-line:#e3e8ef;--pv-ink:#18202b;--pv-mute:#667085;--pv-track:#edf1f6;--pv-green:#059669;--pv-blue:#2563eb;--pv-yellow:#d97706;--pv-red:#dc2626;--pv-cyan:#0891b2;--pv-purple:#7c3aed;--pv-glow:0 1px 2px rgba(16,24,40,.05);--pv-mono:"JetBrains Mono","IBM Plex Mono",ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;color:var(--pv-ink);font-variant-numeric:tabular-nums}
@media (prefers-color-scheme:dark){.pv{--pv-card:rgba(21,27,38,.72);--pv-card-2:rgba(26,34,48,.6);--pv-line:#1e2a3a;--pv-ink:#d3dae3;--pv-mute:#8999af;--pv-track:#1a2230;--pv-green:#00d4aa;--pv-blue:#4da6ff;--pv-yellow:#ffb870;--pv-red:#f85149;--pv-cyan:#39d2c0;--pv-purple:#b392f0;--pv-glow:0 0 0 1px rgba(57,210,192,.04),0 8px 24px -12px rgba(0,0,0,.6)}body{background:#0a0e14;background-image:radial-gradient(1200px 600px at 10% -10%,rgba(57,210,192,.08),transparent),radial-gradient(900px 500px at 110% 0,rgba(77,166,255,.07),transparent)}}
.pv-sum{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px;margin-bottom:14px}
.pv-tile{background:var(--pv-card);border:1px solid var(--pv-line);border-radius:12px;padding:12px 14px;box-shadow:var(--pv-glow);min-width:0}
.pv-tile .k{font:500 11px var(--pv-mono);letter-spacing:.08em;text-transform:uppercase;color:var(--pv-mute)}
.pv-tile .v{font:600 20px/1.3 var(--pv-mono);margin-top:4px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.pv-tile .v small{font-size:12px;color:var(--pv-mute);font-weight:500}
.pv-tile .v.g{color:var(--pv-green)}.pv-tile .v.b{color:var(--pv-blue)}.pv-tile .v.y{color:var(--pv-yellow)}.pv-tile .v.c{color:var(--pv-cyan)}
.pv-bar-row{display:flex;flex-wrap:wrap;align-items:center;gap:8px;margin-bottom:12px}
.pv-chips{display:flex;flex-wrap:wrap;gap:6px;flex:1;min-width:0}
.pv-chip{font:500 12px var(--pv-mono);padding:4px 10px;border-radius:999px;border:1px solid var(--pv-line);background:var(--pv-card);color:var(--pv-mute);cursor:pointer}
.pv-chip.on{border-color:var(--pv-cyan);color:var(--pv-cyan);background:color-mix(in srgb,var(--pv-cyan) 10%,var(--pv-card))}
.pv-seg{display:inline-flex;border:1px solid var(--pv-line);border-radius:9px;overflow:hidden;background:var(--pv-card)}
.pv-seg button{font:500 12px var(--pv-mono);padding:5px 12px;border:0;background:none;color:var(--pv-mute);cursor:pointer}
.pv-seg button.on{background:color-mix(in srgb,var(--pv-cyan) 14%,transparent);color:var(--pv-cyan)}
.pv-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(300px,1fr));gap:12px}
.pv-card{position:relative;background:var(--pv-card);border:1px solid var(--pv-line);border-radius:14px;padding:14px 14px 12px;display:flex;flex-direction:column;gap:10px;box-shadow:var(--pv-glow);text-align:left;font:inherit;color:inherit;width:100%;transition:border-color .15s,transform .15s}
button.pv-card{cursor:pointer}button.pv-card:hover{border-color:var(--pv-cyan);transform:translateY(-1px)}
.pv-card.off{opacity:.6}
.pv-card::before{content:"";position:absolute;left:14px;right:14px;top:0;height:2px;border-radius:0 0 2px 2px;background:linear-gradient(90deg,var(--pv-green),var(--pv-cyan),var(--pv-blue));opacity:.8}
.pv-card.off::before{background:var(--pv-red)}
.pv-hd{display:flex;align-items:center;gap:8px;min-width:0}
.pv-flag{font-size:18px;line-height:1}
.pv-name{flex:1;min-width:0;font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.pv-st{font:600 11px var(--pv-mono);padding:2px 8px;border-radius:6px;border:1px solid currentColor;white-space:nowrap}
.pv-st.on{color:var(--pv-green)}.pv-st.off{color:var(--pv-red)}
.pv-meta{display:flex;flex-wrap:wrap;gap:5px;align-items:center}
.pv-badge{font:500 10.5px var(--pv-mono);padding:1px 7px;border-radius:5px;background:var(--pv-card-2);border:1px solid var(--pv-line);color:var(--pv-mute);white-space:nowrap}
.pv-badge.v4{color:var(--pv-blue);border-color:color-mix(in srgb,var(--pv-blue) 35%,transparent)}
.pv-badge.v6{color:var(--pv-purple);border-color:color-mix(in srgb,var(--pv-purple) 35%,transparent)}
.pv-badge.cost{color:var(--pv-yellow)}.pv-badge.exp{color:var(--pv-cyan)}.pv-badge.exp.late{color:var(--pv-red)}
.pv-rows{display:flex;flex-direction:column;gap:6px}
.pv-row{display:grid;grid-template-columns:44px 1fr auto;align-items:center;gap:8px;font:12px var(--pv-mono)}
.pv-k{font-weight:600;font-size:11px;letter-spacing:.05em;color:var(--pv-mute)}
.pv-track{height:6px;border-radius:4px;background:var(--pv-track);overflow:hidden}
.pv-track i{display:block;height:100%;border-radius:4px;transition:width .4s}
.pv-val{min-width:58px;text-align:right;white-space:nowrap}
.pv-pair{display:flex;gap:12px;justify-content:flex-start;grid-column:2/4;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.pv-dn{color:var(--pv-green)}.pv-up{color:var(--pv-blue)}
.pv-lat{display:flex;flex-direction:column;gap:6px;border-top:1px dashed var(--pv-line);padding-top:9px}
.pv-lr{display:grid;grid-template-columns:minmax(52px,auto) 1fr 52px 34px;align-items:end;gap:8px;font:12px var(--pv-mono)}
.pv-ln{display:flex;align-items:center;gap:5px;color:var(--pv-mute);white-space:nowrap;overflow:hidden;text-overflow:ellipsis;align-self:center}
.pv-ln i{width:7px;height:7px;border-radius:2px;flex:none}
.pv-bk{display:flex;align-items:flex-end;gap:2px;height:18px}
.pv-bk span{flex:1;min-width:2px;border-radius:2px 2px 0 0;opacity:.85}
.pv-lv{text-align:right;font-weight:600;align-self:center}
.pv-ls{text-align:right;color:var(--pv-mute);font-size:11px;align-self:center}
.pv-off{font:12px var(--pv-mono);color:var(--pv-mute)}
.pv-table{width:100%;border-collapse:collapse;font:12px var(--pv-mono);background:var(--pv-card);border:1px solid var(--pv-line);border-radius:12px;overflow:hidden}
.pv-table th{font-size:11px;font-weight:600;letter-spacing:.05em;text-transform:uppercase;color:var(--pv-mute);text-align:left;padding:9px 10px;border-bottom:1px solid var(--pv-line);white-space:nowrap}
.pv-table td{padding:8px 10px;border-bottom:1px solid var(--pv-line);white-space:nowrap}
.pv-table tr:last-child td{border-bottom:0}
.pv-table tr.click{cursor:pointer}.pv-table tr.click:hover td{background:var(--pv-card-2)}
.pv-mini{display:inline-flex;align-items:center;gap:6px}.pv-mini .pv-track{width:54px}
.pv-scroll{overflow-x:auto;border-radius:12px}
@media (max-width:640px){.pv-grid{grid-template-columns:minmax(0,1fr)}.pv-sum{grid-template-columns:repeat(2,minmax(0,1fr))}.pv-tile .v{font-size:17px}}
`

var statusTpl = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" type="image/png" href="/favicon.ico"><meta name="referrer" content="no-referrer"><meta name="color-scheme" content="light dark">
<title>{{.Title}}</title>
<style>
*{box-sizing:border-box}
body{margin:0;background:#f3f5f8;font:14px/1.5 "Inter","Noto Sans SC",system-ui,sans-serif}
main{max-width:1240px;margin:0 auto;padding:26px 16px 48px}
header{display:flex;flex-wrap:wrap;align-items:flex-end;justify-content:space-between;gap:10px;margin-bottom:16px}
h1{font:700 22px var(--pv-mono);margin:0;letter-spacing:-.01em}
.prompt{font:12px var(--pv-mono);color:var(--pv-mute);margin-top:4px}
.prompt b{color:var(--pv-green);font-weight:600}
.cursor{display:inline-block;width:7px;height:13px;margin-left:3px;vertical-align:-2px;background:var(--pv-cyan);animation:blink 1.1s steps(1) infinite}
@keyframes blink{50%{opacity:0}}
@media (prefers-reduced-motion:reduce){.cursor{animation:none}}
.at{font:12px var(--pv-mono);color:var(--pv-mute)}
.note{background:var(--pv-card);border:1px solid var(--pv-line);border-left:3px solid var(--pv-cyan);border-radius:10px;padding:10px 14px;margin-bottom:14px;white-space:pre-wrap}
footer{margin-top:22px;text-align:center;font:12px var(--pv-mono);color:var(--pv-mute)}
` + statusCSS + `
</style></head><body><main class="pv">
<header><div><h1>{{.Title}}</h1><div class="prompt"><b>$</b> status --live<span class="cursor"></span></div></div><span class="at" id="at"></span></header>
<div id="note" class="note" hidden></div>
<div id="sum"></div>
<div class="pv-bar-row"><div class="pv-chips" id="chips"></div><span class="pv-seg"><button type="button" id="v-card" class="on">卡片</button><button type="button" id="v-table">表格</button></span></div>
<div id="list"><div class="pv-off">加载中…</div></div>
<footer>延迟为节点到各运营商的 TCP 连接耗时 · 每 30 秒测一次 · 柱状为近 2 小时每 5 分钟平均</footer>
</main>
<script>
(function(){
var DATA={{.Data}},last=null,region='',view='card';
try{if(localStorage.getItem('status-view')==='table')view='table'}catch(e){}
function esc(s){return String(s==null?'':s).replace(/[&<>"']/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]})}
function flag(cc){if(!/^[A-Za-z]{2}$/.test(cc||''))return '';cc=cc.toUpperCase();return String.fromCodePoint(127397+cc.charCodeAt(0),127397+cc.charCodeAt(1))}
function bps(n){var u=['bps','Kbps','Mbps','Gbps'],i=0;while(n>=1000&&i<3){n/=1000;i++}return (i?n.toFixed(1):n.toFixed(0))+' '+u[i]}
function bytes(n){var u=['B','KB','MB','GB','TB'],i=0;while(n>=1024&&i<4){n/=1024;i++}return n.toFixed(i?1:0)+' '+u[i]}
function up(s){var d=Math.floor(s/86400),h=Math.floor(s%86400/3600);return d?d+'天 '+h+'时':h+'时 '+Math.floor(s%3600/60)+'分'}
function uc(p){return p>=85?'var(--pv-red)':p>=60?'var(--pv-yellow)':'var(--pv-green)'}
function pc(ms){return ms==null||ms<=0?'var(--pv-red)':ms<=80?'var(--pv-green)':ms<=160?'var(--pv-cyan)':ms<=260?'var(--pv-yellow)':'var(--pv-red)'}
function bar(k,p,v,title){p=Math.min(100,Math.max(0,p||0));return '<div class="pv-row"'+(title?' title="'+esc(title)+'"':'')+'><span class="pv-k">'+k+'</span><div class="pv-track"><i style="width:'+p.toFixed(0)+'%;background:'+uc(p)+'"></i></div><span class="pv-val">'+v+'</span></div>'}
function buckets(vs){var max=1;vs.forEach(function(v){if(v!=null&&v>max)max=v});return '<span class="pv-bk">'+vs.map(function(v){return '<span style="height:'+(v==null?18:Math.max(15,v/max*100)).toFixed(0)+'%;background:'+pc(v)+';opacity:'+(v==null?.5:.85)+'"></span>'}).join('')+'</span>'}
var col={ct:'#3b82f6',cu:'#ef4444',cm:'#10b981'};
function badges(n){var h='<div class="pv-meta">';if(n.ipv4)h+='<span class="pv-badge v4">IPv4</span>';if(n.ipv6)h+='<span class="pv-badge v6">IPv6</span>';
 if(n.os)h+='<span class="pv-badge">'+esc(n.os.length>18?n.os.slice(0,18)+'…':n.os)+'</span>';if(n.arch)h+='<span class="pv-badge">'+esc(n.arch)+'</span>';
 var sz=[n.cores?n.cores+'C':'',n.memTotal?bytes(n.memTotal).replace(' ',''):'',n.diskTotal?bytes(n.diskTotal).replace(' ',''):''].filter(Boolean).join(' / ');if(sz)h+='<span class="pv-badge">'+sz+'</span>';
 if(n.price)h+='<span class="pv-badge cost">'+esc(n.price)+'</span>';
 if(n.expireAt){var d=Math.ceil((new Date(n.expireAt)-Date.now())/864e5);h+='<span class="pv-badge exp'+(d<=7?' late':'')+'">'+(d>=0?'剩 '+d+' 天':'已过期')+'</span>'}
 return h+'</div>'}
function lat(n,ts){if(!ts.length)return '';return '<div class="pv-lat">'+ts.map(function(t){var l=n.latency[t.id],live=l&&n.online,c=col[t.carrier]||'#8b5cf6';
 var v=!live?'—':l.ms?l.ms.toFixed(0)+'ms':'超时',lc=live&&l.loss>=5?'var(--pv-red)':live&&l.loss>=1?'var(--pv-yellow)':'';
 return '<div class="pv-lr"'+(l&&l.avg!=null?' title="近 1 小时平均 '+l.avg.toFixed(0)+' ms · 丢包 '+l.loss.toFixed(1)+'%"':'')+'><span class="pv-ln"><i style="background:'+c+'"></i>'+esc(t.name)+'</span>'+buckets(l?l.spark:[])+'<b class="pv-lv" style="color:'+(live?pc(l.ms):'var(--pv-mute)')+'">'+v+'</b><span class="pv-ls"'+(lc?' style="color:'+lc+'"':'')+'>'+(live?l.loss.toFixed(0)+'%':'')+'</span></div>'}).join('')+'</div>'}
function card(n,ts){var h='<div class="pv-card'+(n.online?'':' off')+'"><div class="pv-hd"><span class="pv-flag">'+flag(n.flag)+'</span><span class="pv-name">'+esc(n.name)+'</span><span class="pv-st '+(n.online?'on">在线':'off">离线')+'</span></div>'+badges(n);
 if(n.online){h+='<div class="pv-rows">'+bar('CPU',n.cpu,n.cpu.toFixed(1)+'%')+bar('RAM',n.mem,n.mem.toFixed(1)+'%',n.memTotal?bytes(n.memTotal*n.mem/100)+' / '+bytes(n.memTotal):'')+bar('DISK',n.disk,n.disk?n.disk.toFixed(1)+'%':'—',n.diskTotal?bytes(n.diskTotal):'');
  if(n.netLimit>0)h+=bar('TRF',100*n.netUsed/n.netLimit,bytes(n.netUsed),bytes(n.netUsed)+' / '+bytes(n.netLimit));
  h+='<div class="pv-row"><span class="pv-k">NET</span><span class="pv-pair"><span class="pv-dn">▼ '+bps(n.rxBps)+'</span><span class="pv-up">▲ '+bps(n.txBps)+'</span></span></div>';
  h+='<div class="pv-row"><span class="pv-k">UP</span><span class="pv-pair"><span>'+up(n.uptime)+'</span><span style="color:var(--pv-mute)">load '+n.load1.toFixed(2)+'</span></span></div></div>'}
 else h+='<div class="pv-off">离线 · '+(n.lastSeen?new Date(n.lastSeen).toLocaleString():'—')+'</div>';
 return h+lat(n,ts)+'</div>'}
function mini(p,v){return '<span class="pv-mini"><span class="pv-track"><i style="width:'+Math.min(100,p).toFixed(0)+'%;background:'+uc(p)+'"></i></span>'+v+'</span>'}
function table(ns,ts){return '<div class="pv-scroll"><table class="pv-table"><thead><tr><th>节点</th><th>状态</th><th>CPU</th><th>RAM</th><th>DISK</th><th>NET ▼/▲</th><th>运行</th>'+ts.map(function(t){return '<th>'+esc(t.name)+'</th>'}).join('')+'</tr></thead><tbody>'+
 ns.map(function(n){return '<tr><td>'+flag(n.flag)+' <b>'+esc(n.name)+'</b></td><td><span class="pv-st '+(n.online?'on">在线':'off">离线')+'</span></td><td>'+(n.online?mini(n.cpu,n.cpu.toFixed(0)+'%'):'—')+'</td><td>'+(n.online?mini(n.mem,n.mem.toFixed(0)+'%'):'—')+'</td><td>'+(n.online&&n.disk?mini(n.disk,n.disk.toFixed(0)+'%'):'—')+'</td><td>'+(n.online?'<span class="pv-dn">'+bps(n.rxBps)+'</span> / <span class="pv-up">'+bps(n.txBps)+'</span>':'—')+'</td><td>'+(n.online?up(n.uptime):'—')+'</td>'+
  ts.map(function(t){var l=n.latency[t.id];return '<td style="font-weight:600;color:'+(l&&n.online?pc(l.ms):'var(--pv-mute)')+'">'+(l&&n.online?(l.ms?l.ms.toFixed(0)+'ms':'超时'):'—')+'</td>'}).join('')+'</tr>'}).join('')+'</tbody></table></div>'}
function summary(ns){var on=ns.filter(function(n){return n.online}),rx=0,tx=0,used=0,cpu=0,regs={};on.forEach(function(n){rx+=n.rxBps;tx+=n.txBps;cpu+=n.cpu});ns.forEach(function(n){used+=n.netUsed||0;if(n.flag)regs[n.flag]=1});
 var t=function(k,v,c){return '<div class="pv-tile"><div class="k">'+k+'</div><div class="v '+(c||'')+'">'+v+'</div></div>'};
 return '<div class="pv-sum">'+t('在线',on.length+'<small> / '+ns.length+'</small>','g')+t('总下行',bps(rx))+t('总上行',bps(tx),'b')+(used>0?t('本月流量',bytes(used),'y'):'')+t('平均 CPU',(on.length?cpu/on.length:0).toFixed(1)+'%','c')+t('地区',Object.keys(regs).length)+'</div>'}
function draw(){var d=last;if(!d)return;
 var counts={};d.nodes.forEach(function(n){if(n.flag)counts[n.flag]=(counts[n.flag]||0)+1});
 document.getElementById('chips').innerHTML='<button type="button" class="pv-chip'+(region?'':' on')+'" data-r="">全部 '+d.nodes.length+'</button>'+Object.keys(counts).map(function(f){return '<button type="button" class="pv-chip'+(region===f?' on':'')+'" data-r="'+esc(f)+'">'+flag(f)+' '+counts[f]+'</button>'}).join('');
 var ns=d.nodes.filter(function(n){return !region||n.flag===region});
 document.getElementById('v-card').className=view==='card'?'on':'';document.getElementById('v-table').className=view==='table'?'on':'';
 document.getElementById('list').innerHTML=!d.nodes.length?'<div class="pv-off">暂无节点</div>':view==='table'?table(ns,d.targets):'<div class="pv-grid">'+ns.map(function(n){return card(n,d.targets)}).join('')+'</div>'}
function render(d){last=d;document.getElementById('at').textContent='更新于 '+new Date(d.at).toLocaleTimeString();
 var note=document.getElementById('note');if(d.note){note.textContent=d.note;note.hidden=false}else note.hidden=true;
 document.getElementById('sum').innerHTML=summary(d.nodes);draw()}
document.getElementById('chips').addEventListener('click',function(e){var b=e.target.closest('[data-r]');if(!b)return;var r=b.getAttribute('data-r');region=region===r?'':r;draw()});
function setView(v){view=v;try{localStorage.setItem('status-view',v)}catch(e){}draw()}
document.getElementById('v-card').onclick=function(){setView('card')};document.getElementById('v-table').onclick=function(){setView('table')};
function load(){fetch(DATA,{cache:'no-store'}).then(function(r){return r.json()}).then(render).catch(function(){})}
load();setInterval(load,15000);
})();
</script></body></html>`))
