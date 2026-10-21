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

var statusTpl = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" type="image/png" href="/favicon.ico"><meta name="referrer" content="no-referrer">
<title>{{.Title}}</title>
<style>
:root{--bg:#F4F5F2;--card:#fff;--ink:#15181C;--mute:#5B6470;--line:#E2E5E0;--pri:#0E7490;--ok:#16A34A;--warn:#D97706;--bad:#DC2626;--ct:#2563EB;--cu:#DC2626;--cm:#16A34A;--ot:#7C3AED}
@media (prefers-color-scheme:dark){:root{--bg:#101316;--card:#181C20;--ink:#E6E8EA;--mute:#9AA2AB;--line:#2A3036;--pri:#22A3BF;--ok:#4ADE80;--warn:#FBBF24;--bad:#F87171;--ct:#60A5FA;--cu:#F87171;--cm:#4ADE80;--ot:#A78BFA}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.5 "IBM Plex Sans","Noto Sans SC",system-ui,sans-serif}
main{max-width:1180px;margin:0 auto;padding:28px 16px 48px}
header{display:flex;flex-wrap:wrap;align-items:baseline;justify-content:space-between;gap:8px;margin-bottom:16px}
h1{font-size:22px;margin:0}.mute{color:var(--mute);font-size:12px}
.note{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:10px 14px;margin-bottom:14px;white-space:pre-wrap}
.sum{display:flex;gap:14px;flex-wrap:wrap;margin-bottom:14px;font-size:13px}
.sum b{font:600 15px "IBM Plex Mono",ui-monospace,monospace}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(270px,1fr));gap:12px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:14px;display:flex;flex-direction:column;gap:9px}
.card.off{opacity:.62}
.top{display:flex;align-items:center;gap:8px}
.name{font-weight:600;flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.dot{width:8px;height:8px;border-radius:50%;background:var(--ok);box-shadow:0 0 0 3px color-mix(in srgb,var(--ok) 20%,transparent)}
.off .dot{background:var(--bad);box-shadow:none}
.row{display:flex;justify-content:space-between;gap:8px;font-size:12px;color:var(--mute)}
.row b{color:var(--ink);font:500 12px "IBM Plex Mono",ui-monospace,monospace}
.bar{height:5px;border-radius:3px;background:var(--line);overflow:hidden}.bar i{display:block;height:100%;background:var(--pri)}
.bar i.w{background:var(--warn)}.bar i.b{background:var(--bad)}
.lat{display:flex;flex-direction:column;gap:5px;border-top:1px dashed var(--line);padding-top:8px}
.lt{display:grid;grid-template-columns:auto 1fr auto;align-items:center;gap:8px;font-size:12px}
.lt .k{display:flex;align-items:center;gap:5px;color:var(--mute);white-space:nowrap}
.lt .k i{width:7px;height:7px;border-radius:2px}
.lt .v{font:500 12px "IBM Plex Mono",ui-monospace,monospace;text-align:right;white-space:nowrap}
.lt svg{width:100%;height:18px}
.loss{color:var(--warn)}.dead{color:var(--bad)}
footer{margin-top:20px;text-align:center}
</style></head><body><main>
<header><h1>{{.Title}}</h1><span class="mute" id="at"></span></header>
<div id="note" class="note" hidden></div>
<div class="sum" id="sum"></div>
<div class="grid" id="grid"><div class="mute">加载中…</div></div>
<footer class="mute">延迟为节点到各运营商的 TCP 连接耗时，每 30 秒测一次</footer>
</main>
<script>
(function(){
var DATA={{.Data}};
function esc(s){return String(s==null?'':s).replace(/[&<>"']/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]})}
function flag(cc){if(!/^[A-Za-z]{2}$/.test(cc||''))return '';cc=cc.toUpperCase();return String.fromCodePoint(127397+cc.charCodeAt(0),127397+cc.charCodeAt(1))}
function bps(n){var u=['bps','Kbps','Mbps','Gbps'],i=0;while(n>=1000&&i<3){n/=1000;i++}return (i?n.toFixed(1):n.toFixed(0))+' '+u[i]}
function bytes(n){var u=['B','KB','MB','GB','TB'],i=0;while(n>=1024&&i<4){n/=1024;i++}return n.toFixed(i?1:0)+' '+u[i]}
function up(s){var d=Math.floor(s/86400),h=Math.floor(s%86400/3600);return d?d+' 天 '+h+' 时':h+' 时 '+Math.floor(s%3600/60)+' 分'}
function bar(p){var c=p>=90?'b':p>=75?'w':'';return '<div class="bar"><i class="'+c+'" style="width:'+Math.min(100,Math.max(0,p)).toFixed(0)+'%"></i></div>'}
function spark(vs,color){var n=vs.length,max=0,min=1e9;vs.forEach(function(v){if(v!=null){if(v>max)max=v;if(v<min)min=v}});if(!max)return '<svg></svg>';var lo=Math.max(0,min-Math.max(5,(max-min)*0.3)),hi=Math.max(max,lo+10);
 var pts=[],segs=[];vs.forEach(function(v,i){var x=(i/(n-1)*100).toFixed(1);if(v==null){if(pts.length)segs.push(pts),pts=[];return}pts.push(x+','+(17-(v-lo)/(hi-lo)*15).toFixed(1))});if(pts.length)segs.push(pts);
 return '<svg viewBox="0 0 100 18" preserveAspectRatio="none">'+segs.map(function(p){return p.length==1?'<circle cx="'+p[0].split(',')[0]+'" cy="'+p[0].split(',')[1]+'" r="1" fill="'+color+'"/>':'<polyline fill="none" stroke="'+color+'" stroke-width="1.4" vector-effect="non-scaling-stroke" points="'+p.join(' ')+'"/>'}).join('')+'</svg>'}
function render(d){
 document.getElementById('at').textContent='更新于 '+new Date(d.at).toLocaleTimeString();
 var note=document.getElementById('note');if(d.note){note.textContent=d.note;note.hidden=false}else note.hidden=true;
 var on=d.nodes.filter(function(n){return n.online}).length;
 document.getElementById('sum').innerHTML='<span>在线 <b>'+on+'</b> / '+d.nodes.length+'</span>';
 var col={ct:'var(--ct)',cu:'var(--cu)',cm:'var(--cm)'};
 document.getElementById('grid').innerHTML=d.nodes.length?d.nodes.map(function(n){
  var h='<div class="card'+(n.online?'':' off')+'"><div class="top"><span class="dot"></span><span>'+flag(n.flag)+'</span><span class="name">'+esc(n.name)+'</span><span class="mute">'+esc(n.region)+'</span></div>';
  if(n.online){
   h+='<div class="row"><span>CPU</span><b>'+n.cpu.toFixed(0)+'%</b></div>'+bar(n.cpu)+'<div class="row"><span>内存</span><b>'+n.mem.toFixed(0)+'%</b></div>'+bar(n.mem);
   h+='<div class="row"><span>网速</span><b>↓ '+bps(n.rxBps)+' · ↑ '+bps(n.txBps)+'</b></div>';
   h+='<div class="row"><span>运行</span><b>'+up(n.uptime)+' · 负载 '+n.load1.toFixed(2)+'</b></div>';
   if(n.netLimit>0)h+='<div class="row"><span>本月流量</span><b>'+bytes(n.netUsed)+' / '+bytes(n.netLimit)+'</b></div>'+bar(100*n.netUsed/n.netLimit);
  }else h+='<div class="row"><span>离线</span><b>'+(n.lastSeen?new Date(n.lastSeen).toLocaleString():'—')+'</b></div>';
  if(d.targets.length){h+='<div class="lat">'+d.targets.map(function(t){var l=n.latency[t.id],c=col[t.carrier]||'var(--ot)',v;
    if(!l||!n.online)v='<span class="mute">—</span>';else if(!l.ms)v='<span class="dead">超时</span>';else v=l.ms.toFixed(0)+' ms'+(l.loss>=1?' <span class="loss">'+l.loss.toFixed(0)+'%</span>':'');
    return '<div class="lt"><span class="k"><i style="background:'+c+'"></i>'+esc(t.name)+'</span>'+(l?spark(l.spark,c):'<svg></svg>')+'<span class="v">'+v+'</span></div>'}).join('')+'</div>'}
  return h+'</div>'}).join(''):'<div class="mute">暂无节点</div>';
}
function load(){fetch(DATA,{cache:'no-store'}).then(function(r){return r.json()}).then(render).catch(function(){})}
load();setInterval(load,15000);
})();
</script></body></html>`))
