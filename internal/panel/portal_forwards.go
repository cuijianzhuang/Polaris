package panel

import (
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// "My forwards" in the user center: list, add and delete self-service port
// forwards (see forward_shop.go).

type portalFwdRow struct {
	ID, Name, Node, Entry, Target, Network string
}

type portalFwdNode struct{ ID, Name string }

// portalForwards is GET /u/forwards.
func (s *Server) portalForwards(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalUser(c)
	if u == nil {
		c.Redirect(http.StatusFound, portalPath)
		return
	}
	var rs []store.ForwardRule
	s.st.DB.Where("user_id = ?", u.ID).Order("created_at, id").Find(&rs)
	nodes := map[string]*store.Node{}
	var ns []store.Node
	s.st.DB.Find(&ns)
	for i := range ns {
		nodes[ns[i].ID] = &ns[i]
	}
	rows := make([]portalFwdRow, 0, len(rs))
	for _, r := range rs {
		row := portalFwdRow{ID: r.ID, Name: r.Name, Network: r.Network}
		if n := nodes[r.NodeID]; n != nil {
			row.Node, row.Entry = n.Name, net.JoinHostPort(nodeHost(n), strconv.Itoa(r.ListenPort))
		}
		if len(r.Remotes.V) > 0 {
			row.Target = r.Remotes.V[0].Addr
		}
		rows = append(rows, row)
	}
	var opts []portalFwdNode
	for _, n := range s.fwdNodes(u.ID) {
		opts = append(opts, portalFwdNode{ID: n.ID, Name: n.Name})
	}
	lo, hi := fwdPorts(u.ForwardEntitlement)
	used, quota := bytesText(u.FwdUsed), "不限"
	if u.FwdTraffic > 0 {
		quota = bytesText(u.FwdTraffic)
	}
	state := ""
	switch {
	case u.FwdMax == 0:
		state = "你的套餐不包含端口转发。"
	case !s.fwdServes(u) && u.Status != "active":
		state = "账号未处于正常状态，转发已暂停。"
	case !s.fwdServes(u):
		state = "本周期的转发流量已用完，转发已暂停，下个周期自动恢复。"
	}
	data := map[string]any{"Title": s.subTitle(s.st.GetSetting("site_title", "Polaris")), "Rows": rows, "Nodes": opts,
		"Max": u.FwdMax, "Count": len(rs), "Lo": lo, "Hi": hi, "Used": used, "Quota": quota, "State": state,
		"Speed": u.FwdSpeedMbps, "CanAdd": u.FwdMax > len(rs) && len(opts) > 0 && s.fwdServes(u)}
	var buf []byte
	w := &bytesWriter{b: &buf}
	if err := portalFwdTpl.Execute(w, data); err != nil {
		serverError(c, err)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", buf)
}

type bytesWriter struct{ b *[]byte }

func (w *bytesWriter) Write(p []byte) (int, error) { *w.b = append(*w.b, p...); return len(p), nil }

func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n>>10)
	}
}

func (s *Server) portalFwdWriter(c *gin.Context) *store.User {
	if !portalWrite(c) {
		return nil
	}
	u := s.portalUser(c)
	if u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请重新登录"})
		return nil
	}
	if !s.ticketLimit.Allow(clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "操作太频繁，请稍后再试"})
		return nil
	}
	return u
}

// portalForwardCreate is POST /u/forwards.
func (s *Server) portalForwardCreate(c *gin.Context) {
	u := s.portalFwdWriter(c)
	if u == nil {
		return
	}
	var req userForwardReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	row, err := s.createUserForward(c.Request.Context(), u, req)
	if err != nil {
		badRequest(c, err)
		return
	}
	s.audit("user:"+u.Username, "forward.user_create", row.Name, fmt.Sprintf(":%d → %s", row.ListenPort, row.Remotes.V[0].Addr), clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": row.ID, "listenPort": row.ListenPort})
}

// portalForwardDelete is POST /u/forwards/:id/delete.
func (s *Server) portalForwardDelete(c *gin.Context) {
	u := s.portalFwdWriter(c)
	if u == nil {
		return
	}
	if err := s.deleteUserForward(u, c.Param("id")); err != nil {
		badRequest(c, err)
		return
	}
	s.audit("user:"+u.Username, "forward.user_delete", c.Param("id"), "", clientIP(c.Request))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

var portalFwdTpl = template.Must(template.New("forwards").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex">
<title>{{.Title}} · 我的转发</title>` + ticketPageStyle + `</head><body><main>
<header><h1>我的转发</h1><a href="/u">← 用户中心</a></header>
<div class="card"><p class="mute">已用 {{.Count}} / {{.Max}} 条 · 本周期转发流量 {{.Used}} / {{.Quota}}{{if .Speed}} · 每条限速 {{.Speed}} Mbps{{end}} · 端口范围 {{.Lo}}–{{.Hi}}</p>
{{if .State}}<p class="err">{{.State}}</p>{{end}}
{{if .CanAdd}}<h2>添加转发</h2>
<div class="row" style="flex-wrap:wrap;gap:8px">
<select id="node">{{range .Nodes}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select>
<select id="net"><option value="tcp,udp">TCP + UDP</option><option value="tcp">TCP</option><option value="udp">UDP</option></select>
<input id="target" placeholder="目标 地址:端口，如 example.com:443" style="flex:1;min-width:200px">
<input id="port" type="number" min="{{.Lo}}" max="{{.Hi}}" placeholder="入口端口（留空自动）" style="width:150px">
<input id="name" maxlength="64" placeholder="备注（可选）" style="width:140px">
<button class="btn" id="add" type="button">添加</button></div>
<p class="mute">目标必须是公网地址。域名在添加时解析一次并固定为 IP；目标 IP 变了请删除后重新添加。</p>
<p class="err" id="err" hidden></p>{{end}}</div>
<div class="card"><h2>转发列表</h2>
{{if .Rows}}<table><tr><th>入口</th><th>目标</th><th>协议</th><th></th></tr>{{range .Rows}}<tr><td><code>{{.Entry}}</code><div class="mute">{{.Node}} · {{.Name}}</div></td><td><code>{{.Target}}</code></td><td class="mute">{{.Network}}</td><td><button class="btn alt del" data-id="{{.ID}}" type="button">删除</button></td></tr>{{end}}</table>
{{else}}<p class="mute">还没有转发。</p>{{end}}</div>
</main><script>
function fpost(p,b){return fetch('/u/forwards'+p,{method:'POST',headers:{'Content-Type':'application/json','X-Polaris-Portal':'1'},body:JSON.stringify(b||{}),credentials:'same-origin'}).then(function(r){return r.json().then(function(j){if(!r.ok)throw new Error(j.error||r.statusText);return j})})}
function ferr(m){var e=document.getElementById('err');if(e){e.textContent=m;e.hidden=!m}else if(m){alert(m)}}
var add=document.getElementById('add');if(add)add.addEventListener('click',function(){var b=this;b.disabled=true;ferr('');
var port=parseInt(document.getElementById('port').value||'0',10)||0;
fpost('',{nodeId:document.getElementById('node').value,network:document.getElementById('net').value,target:document.getElementById('target').value,listenPort:port,name:document.getElementById('name').value}).then(function(){location.reload()}).catch(function(e){b.disabled=false;ferr(e.message)})});
document.querySelectorAll('.del').forEach(function(b){b.addEventListener('click',function(){if(!confirm('删除这条转发？'))return;b.disabled=true;fpost('/'+b.dataset.id+'/delete').then(function(){location.reload()}).catch(function(e){b.disabled=false;ferr(e.message)})})});
</script></body></html>`))
