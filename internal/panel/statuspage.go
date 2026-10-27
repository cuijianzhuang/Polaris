package panel

import (
	_ "embed"
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

const statusCacheTTL = time.Second

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

// statusData: GET /status/data. Share one short-lived snapshot across viewers.
func (s *Server) statusData(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	body, status := s.statusSnapshot()
	if status == http.StatusServiceUnavailable {
		c.JSON(status, gin.H{"error": "暂时无法读取节点状态"})
		return
	}
	if status != http.StatusOK {
		c.Status(status)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func (s *Server) statusSnapshot() ([]byte, int) {
	if sn := s.statusCache.Load(); sn != nil && time.Since(sn.at) < statusCacheTTL {
		return sn.body, http.StatusOK
	}
	// A mutex coalesces cold/expired cache requests instead of making each
	// public viewer repeat the fleet's history queries at the faster cadence.
	// Release it before writing to any viewer's potentially slow connection.
	s.statusBuildMu.Lock()
	defer s.statusBuildMu.Unlock()
	if sn := s.statusCache.Load(); sn != nil && time.Since(sn.at) < statusCacheTTL {
		return sn.body, http.StatusOK
	}
	started := time.Now()
	out := s.probeOverview(true)
	if out == nil {
		return nil, http.StatusServiceUnavailable
	}
	out["title"] = s.statusTitle()
	out["note"] = s.st.GetSetting("status_note", "")
	body, err := json.Marshal(out)
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	s.statusCache.Store(&statusSnap{at: started, body: body})
	return body, http.StatusOK
}

// statusPageHTML: GET /status.
func (s *Server) statusPageHTML(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = statusTpl.Execute(c.Writer, map[string]any{"Title": s.statusTitle(), "Data": statusPath + "/data"})
}

// CSS is also imported by the React monitor, keeping both views consistent.
//
//go:embed probe_assets/monitor.css
var statusCSS string

//go:embed probe_assets/status.js
var statusJS string

var statusTpl = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" type="image/png" href="/favicon.ico"><meta name="referrer" content="no-referrer"><meta name="color-scheme" content="light dark">
<title>{{.Title}}</title><style>
*{box-sizing:border-box}body{margin:0;background:#f3f5f8;font:14px/1.5 "Inter",system-ui,sans-serif}main{max-width:1440px;margin:0 auto;padding:34px 24px 48px}button,input,select{font:inherit}button{cursor:pointer}.note{background:var(--pv-card);border:1px solid var(--pv-line);border-left:3px solid var(--pv-yellow);border-radius:8px;padding:12px 15px;margin-bottom:16px;font-size:12px;white-space:pre-wrap}.note[hidden]{display:none}.public-status [hidden]{display:none!important}.pv-link{border:0;background:none;color:var(--pv-cyan);padding:0 5px;font-size:12px}.pv-clear,.pv-dialog-close{border:1px solid var(--pv-line);border-radius:6px;background:var(--pv-card-2);color:var(--pv-ink);padding:6px 12px}.pv-detail{width:min(720px,calc(100% - 28px));max-height:85dvh;overflow-y:auto;border:1px solid var(--pv-line);border-radius:14px;background:var(--pv-card);color:var(--pv-ink);padding:24px}.pv-detail::backdrop{background:rgba(6,12,22,.65);backdrop-filter:blur(3px)}.pv-detail>header{display:flex;align-items:center;justify-content:space-between;gap:16px;margin-bottom:18px}.pv-detail h2{font-size:20px;margin:0;overflow-wrap:anywhere}.pv-detail h3{font-size:13px;margin-top:24px}.pv-detail .pv-detail-summary{margin-top:18px}.pv-detail .row{display:flex;justify-content:space-between;align-items:center;gap:10px;flex-wrap:wrap}.pv-detail .pv-bk{height:42px}.pv-detail .pv-quality{margin:8px 0}.pv-detail .pv-quality-charts{gap:20px}.pv-detail .pv-quality-title{font-size:12px}.pv-detail .pv-ln{font-size:12px}
main.pv-monitor{padding:34px 24px 48px}
@media(max-width:640px){main.pv-monitor{padding:24px 14px}.pv-detail{padding:18px}}
` + statusCSS + `
@media(prefers-color-scheme:dark){body.public-status{background:#0b1018;background-image:radial-gradient(ellipse at 12% 0,rgba(57,210,192,.035),transparent 55%)}.public-status .pv{--pv-card:#131b27;--pv-card-2:#182230;--pv-line:#253144;--pv-ink:#d3dae3;--pv-mute:#91a0b6;--pv-track:#222f41;--pv-green:#37d4ad;--pv-blue:#75aaff;--pv-yellow:#ffb870;--pv-red:#fb8491;--pv-cyan:#56d3ce;--pv-purple:#b392f0;--pv-glow:0 4px 18px -14px #000}}
</style></head><body class="public-status"><main class="pv pv-monitor">
<header class="pv-dashboard-head"><div><div class="pv-eyebrow">POLARIS / SERVER MONITOR</div><h1>{{.Title}}</h1><div class="pv-subtitle">资源状态与网络质量 <span class="pv-live"><i></i><span id="at">等待更新</span></span></div></div><span class="pv-count">每 2 秒更新</span></header>
<div id="note" class="note" hidden></div><div id="error" class="note" role="alert" hidden><span id="error-message"></span><button type="button" id="retry" class="pv-link">重试</button></div><div id="probe-disabled" class="note" hidden>延迟探测已关闭</div>
<div id="sum"></div>
<div class="pv-toolbar"><div class="pv-toolbar-top"><input id="search" class="pv-search" type="search" placeholder="搜索名称、地区或系统" aria-label="搜索节点"><div class="pv-toolbar-controls"><select id="sort" aria-label="节点排序"><option value="default">默认排序</option><option value="name">按名称</option><option value="cpu">CPU 从高到低</option><option value="network">网速从高到低</option><option value="latency">延迟从低到高</option><option value="expiry">到期时间从近到远</option></select><div class="pv-seg" aria-label="展示方式"><button type="button" id="v-card">条形</button><button type="button" id="v-ring">环形</button><button type="button" id="v-table">表格</button></div></div></div><div class="pv-toolbar-bottom"><div class="pv-chips" id="chips"></div><div class="pv-seg" aria-label="节点状态筛选"><button data-state="all">全部</button><button data-state="online">在线</button><button data-state="offline">离线</button><button data-state="attention">需关注</button></div><span class="pv-count" id="count"></span></div></div>
<div id="list"><div class="pv-loading" role="status">正在读取节点状态…</div></div>
<footer class="pv-footnote">延迟为节点到目标的 TCP 连接耗时；小图展示近 2 小时，每格 5 分钟。灰格表示无样本。失败率不是 ICMP 丢包率。</footer>
<dialog id="detail" class="pv-detail" aria-labelledby="detail-title"><header><h2 id="detail-title"></h2><button type="button" id="detail-close" class="pv-dialog-close" aria-label="关闭节点详情">关闭</button></header><div id="detail-content"></div></dialog>
</main><script>var DATA={{.Data}};
` + statusJS + `</script></body></html>`))
