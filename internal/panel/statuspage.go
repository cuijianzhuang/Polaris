package panel

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/flags"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/uitheme"
	"github.com/gin-gonic/gin"
)

// Public status page (/status, off by default): node state, resources and
// carrier latency for anyone, without addresses, versions or hidden nodes.
// The page carries two views over the same gate: a customer-facing service
// view (default, /status/service) and the detailed monitor view
// (/status/data). See docs/design/public-status-page.md.

const statusPath = "/status"

const statusCacheTTL = time.Second

type statusSnap struct {
	at   time.Time
	body []byte
}

func (s *Server) statusPageEnabled() bool { return s.st.GetSetting("status_page", "false") == "true" }

func (s *Server) statusServiceOnly() bool {
	return s.st.GetSetting("status_service_only", "false") == "true"
}

func (s *Server) statusGate(decoy, h gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.statusPageEnabled() {
			decoy(c)
			return
		}
		h(c)
	}
}

// statusDataGate guards the detailed monitor snapshot: in service-only
// mode the monitor data is as unpublished as when the page is off.
func (s *Server) statusDataGate(decoy, h gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.statusPageEnabled() || s.statusServiceOnly() {
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

// --- service view snapshot (whitelist projection) ---

const serviceCacheTTL = 30 * time.Second

// svcSnap caches one built service snapshot in both its fresh and stale
// serialisations, so a failed rebuild can still answer with the last known
// state (marked stale) instead of an error page.
type svcSnap struct {
	at        time.Time
	body      []byte
	staleBody []byte
}

// serviceLatency is one probe target as the service view may show it: the
// target's display name and carrier only, never its address.
type serviceLatency struct {
	Name    string   `json:"name"`
	Carrier string   `json:"carrier"`
	Ms      *float64 `json:"ms"`   // last probe; nil = no live sample
	Loss    float64  `json:"loss"` // last hour, percent
}

// serviceNode is the whitelist projection of one node for the public
// service view. Fields are added here one by one on purpose: nothing from
// store.Node reaches the public page unless it is listed here.
type serviceNode struct {
	Name    string           `json:"name"` // final display name (public_name, else Name)
	Region  string           `json:"region"`
	Flag    string           `json:"flag"`
	State   string           `json:"state"` // online / degraded / offline
	Latency []serviceLatency `json:"latency"`
	// Availability from NodeMetric heartbeat slots (design D6); nil means
	// too little history to state a number. UptimeDays always has 30
	// entries (oldest first, today last), nil per day likewise.
	Uptime7d   *float64         `json:"uptime7d"`
	Uptime30d  *float64         `json:"uptime30d"`
	UptimeDays []*float64       `json:"uptimeDays"`
	Carriers   *serviceCarriers `json:"carriers,omitempty"` // only when probe_public_carriers is on and data exists
}

type serviceSummary struct {
	Total  int    `json:"total"`
	Online int    `json:"online"`
	State  string `json:"state"` // ok / partial / major
}

type serviceSnapshotData struct {
	Title   string         `json:"title"`
	Note    string         `json:"note"`
	At      time.Time      `json:"at"`
	Stale   bool           `json:"stale"`
	Summary serviceSummary `json:"summary"`
	Nodes   []serviceNode  `json:"nodes"`
}

// buildServiceSnapshot assembles the service view from admin nodes only:
// user-owned nodes are somebody else's machines and stay off the operator's
// public availability page, whatever their review state.
func (s *Server) buildServiceSnapshot() (*serviceSnapshotData, error) {
	now := time.Now()
	var ns []store.Node
	// OwnerID was added additively with no column default, so nodes that
	// predate user-owned nodes carry NULL, not ""; both mean an admin node.
	q := s.st.DB.Where("revoked = ?", false).Where("owner_id = ? OR owner_id IS NULL", "").
		Where("status_hidden = ? OR status_hidden IS NULL", false).
		Order("sort_order, name")
	if err := q.Find(&ns).Error; err != nil {
		return nil, err
	}
	targets := s.latencyTargets(true)
	lat := s.latencySummaryForNodes(ns, targets, now)
	ups, err := s.uptimesForNodes(ns, now)
	if err != nil {
		return nil, err
	}
	showCarriers := s.st.GetSetting("probe_public_carriers", "false") == "true"
	out := &serviceSnapshotData{
		Title: s.statusTitle(),
		Note:  s.st.GetSetting("status_note", ""),
		At:    now,
		Nodes: []serviceNode{},
	}
	bad := 0
	for i := range ns {
		n := &ns[i]
		onlineClass := n.Status == "online" || n.Status == "syncing" || n.Status == "error"
		fresh := onlineClass && n.LastSeen != nil && now.Sub(*n.LastSeen) <= 90*time.Second && !n.LastSeen.After(now.Add(time.Minute))
		sn := serviceNode{Region: n.Region, Flag: n.Flag, Latency: []serviceLatency{}, UptimeDays: make([]*float64, 30)}
		if u := ups[n.ID]; u != nil {
			sn.Uptime7d, sn.Uptime30d, sn.UptimeDays = u.d7, u.d30, u.days
		}
		if showCarriers {
			sn.Carriers = s.publicCarriers(n, now)
		}
		sn.Name = strings.TrimSpace(n.PublicName)
		if sn.Name == "" {
			sn.Name = n.Name
		}
		lossy := false
		for _, t := range nodeProbeTargets(n, targets) {
			e := serviceLatency{Name: t.Name, Carrier: t.Carrier}
			if l := lat[n.ID][t.ID]; l != nil {
				e.Loss = l.Loss
				if !l.At.IsZero() && now.Sub(l.At) <= 3*time.Minute {
					ms := l.Ms
					e.Ms = &ms
				}
				if l.Loss >= 50 {
					lossy = true
				}
			}
			sn.Latency = append(sn.Latency, e)
		}
		switch {
		case fresh && !lossy:
			sn.State = "online"
			out.Summary.Online++
		case fresh || onlineClass:
			// Fresh but a probe target is mostly failing, or an online-class
			// node whose heartbeat has gone stale.
			sn.State = "degraded"
			bad++
		default:
			sn.State = "offline"
			bad++
		}
		out.Nodes = append(out.Nodes, sn)
	}
	out.Summary.Total = len(ns)
	switch {
	case bad == 0:
		out.Summary.State = "ok"
	case 3*bad <= len(ns):
		out.Summary.State = "partial"
	default:
		out.Summary.State = "major"
	}
	return out, nil
}

// --- availability (design D6): NodeMetric heartbeat slots, no new tables ---

// nodeUptime is one node's availability over the rolling 7/30-day windows
// plus the per-day bars of the last 30 UTC days. A nil figure means the
// window has too little history to state a number honestly.
type nodeUptime struct {
	d7   *float64
	d30  *float64
	days []*float64 // 30 entries, oldest first, today last
}

// ceilSlot rounds t up to the next metric slot boundary (t itself when it
// already sits on one): the first slot a node created at t can appear in.
func ceilSlot(t time.Time) time.Time {
	u := t.UTC()
	tr := u.Truncate(metricSlot)
	if tr.Before(u) {
		return tr.Add(metricSlot)
	}
	return tr
}

// uptimePct renders present/expected as a one-decimal percentage, or nil
// when the window is too small to mean anything (design D6: fewer than 12
// expected slots — under an hour of history — is "data insufficient",
// never 0% or 100%).
func uptimePct(present, expected int) *float64 {
	if expected < 12 {
		return nil
	}
	if present > expected {
		present = expected
	}
	v := math.Round(1000*float64(present)/float64(expected)) / 10
	return &v
}

// uptimesForNodes aggregates availability for the whole snapshot fleet in
// three grouped queries: slot presence is the only signal — a missing
// 5-minute slot counts as unavailable, including slots the panel itself
// was down for (the page footnote says so; see design D6). NodeLatency is
// never used here: target reachability is not node availability.
func (s *Server) uptimesForNodes(ns []store.Node, now time.Time) (map[string]*nodeUptime, error) {
	out := map[string]*nodeUptime{}
	if len(ns) == 0 {
		return out, nil
	}
	ids := make([]string, len(ns))
	created := make(map[string]time.Time, len(ns))
	for i := range ns {
		ids[i] = ns[i].ID
		created[ns[i].ID] = ns[i].CreatedAt
	}
	// Windows are half-open [start, slotNow): the current, still-open slot
	// is in neither numerator nor denominator.
	slotNow := now.UTC().Truncate(metricSlot)
	type countRow struct {
		NodeID string
		C      int
	}
	countIn := func(from time.Time) (map[string]int, error) {
		var rows []countRow
		err := s.st.DB.Model(&store.NodeMetric{}).
			Select("node_id, COUNT(*) AS c").
			Where("node_id IN ? AND at >= ? AND at < ?", ids, from, slotNow).
			Group("node_id").Scan(&rows).Error
		m := map[string]int{}
		for _, r := range rows {
			m[r.NodeID] = r.C
		}
		return m, err
	}
	c7, err := countIn(slotNow.AddDate(0, 0, -7))
	if err != nil {
		return nil, err
	}
	c30, err := countIn(slotNow.AddDate(0, 0, -30))
	if err != nil {
		return nil, err
	}
	// Per-day bars over the last 30 UTC days (today last, partial so far).
	dayExpr := "date(at)"
	if s.st.DB.Dialector.Name() == "postgres" {
		dayExpr = "to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD')"
	}
	today := time.Date(slotNow.Year(), slotNow.Month(), slotNow.Day(), 0, 0, 0, 0, time.UTC)
	firstDay := today.AddDate(0, 0, -29)
	type dayRow struct {
		NodeID string
		Day    string
		C      int
	}
	var drows []dayRow
	if err := s.st.DB.Model(&store.NodeMetric{}).
		Select("node_id, "+dayExpr+" AS day, COUNT(*) AS c").
		Where("node_id IN ? AND at >= ? AND at < ?", ids, firstDay, slotNow).
		Group("node_id, " + dayExpr).Scan(&drows).Error; err != nil {
		return nil, err
	}
	perDay := map[string]map[string]int{}
	for _, r := range drows {
		if perDay[r.NodeID] == nil {
			perDay[r.NodeID] = map[string]int{}
		}
		perDay[r.NodeID][r.Day] = r.C
	}
	slotsPerDay := int(24 * time.Hour / metricSlot)
	for i := range ns {
		id := ns[i].ID
		u := &nodeUptime{days: make([]*float64, 30)}
		// Rolling windows: a node younger than 24 hours gets no figure at
		// all (design D6); older nodes are measured from their creation.
		if now.Sub(created[id]) >= 24*time.Hour {
			for _, w := range []struct {
				days int
				cnt  map[string]int
				dst  **float64
			}{{7, c7, &u.d7}, {30, c30, &u.d30}} {
				start := slotNow.AddDate(0, 0, -w.days)
				expected := w.days * slotsPerDay
				if cs := ceilSlot(created[id]); cs.After(start) {
					expected = int(slotNow.Sub(cs) / metricSlot)
				}
				*w.dst = uptimePct(w.cnt[id], expected)
			}
		}
		for d := 0; d < 30; d++ {
			ds := firstDay.AddDate(0, 0, d)
			de := ds.AddDate(0, 0, 1)
			if de.After(slotNow) {
				de = slotNow
			}
			es := ds
			if cs := ceilSlot(created[id]); cs.After(es) {
				es = cs
			}
			expected := 0
			if de.After(es) {
				expected = int(de.Sub(es) / metricSlot)
			}
			u.days[d] = uptimePct(perDay[id][ds.Format("2006-01-02")], expected)
		}
		out[id] = u
	}
	return out, nil
}

// serviceSnapshot returns the cached service snapshot, rebuilding at most
// every serviceCacheTTL. A failed rebuild falls back to the previous
// snapshot with stale set; with nothing built yet it reports 503.
func (s *Server) serviceSnapshot() ([]byte, int) {
	if sn := s.svcCache.Load(); sn != nil && time.Since(sn.at) < serviceCacheTTL {
		return sn.body, http.StatusOK
	}
	s.svcBuildMu.Lock()
	defer s.svcBuildMu.Unlock()
	if sn := s.svcCache.Load(); sn != nil && time.Since(sn.at) < serviceCacheTTL {
		return sn.body, http.StatusOK
	}
	data, err := s.buildServiceSnapshot()
	if err == nil {
		var body, staleBody []byte
		body, err = json.Marshal(data)
		if err == nil {
			data.Stale = true
			staleBody, err = json.Marshal(data)
		}
		if err == nil {
			s.svcCache.Store(&svcSnap{at: data.At, body: body, staleBody: staleBody})
			return body, http.StatusOK
		}
	}
	if sn := s.svcCache.Load(); sn != nil {
		return sn.staleBody, http.StatusOK
	}
	return nil, http.StatusServiceUnavailable
}

// statusService: GET /status/service. Rate limited per IP: the page polls
// every 30 s, so 120/min leaves ample room for tabs and CDN fetches while
// keeping the endpoint from becoming a build-pressure lever.
func (s *Server) statusService(c *gin.Context) {
	if !s.statusLimit.Allow(clientIP(c.Request)) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后再试"})
		return
	}
	body, status := s.serviceSnapshot()
	if status != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "暂时无法读取服务状态"})
		return
	}
	c.Header("Cache-Control", "public, max-age=15")
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// statusPageHTML: GET /status.
func (s *Server) statusPageHTML(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = statusTpl.Execute(c.Writer, map[string]any{
		"Title":       s.statusTitle(),
		"Data":        statusPath + "/data",
		"ServiceOnly": s.statusServiceOnly(),
	})
}

// CSS is also imported by the React monitor, keeping both views consistent.
//
//go:embed probe_assets/monitor.css
var statusCSS string

//go:embed probe_assets/status.js
var statusJS string

//go:embed probe_assets/service.js
var serviceJS string

var statusTpl = template.Must(template.New("status").Funcs(template.FuncMap{"taiwanFlag": flags.Taiwan, "uiTokens": func() template.CSS { return template.CSS(uitheme.CSS) }}).Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" type="image/png" href="/favicon.ico"><meta name="referrer" content="no-referrer"><meta name="color-scheme" content="light dark">
<title>{{.Title}}</title><style>{{uiTokens}}</style><style>
*{box-sizing:border-box}body{margin:0;background:var(--ui-light-bg,#fff);font:14px/1.5 system-ui,-apple-system,"Segoe UI","Noto Sans SC",sans-serif}@media(prefers-color-scheme:dark){body:not([data-theme="light"]){background:var(--ui-dark-bg,#000)}}main{max-width:1440px;margin:0 auto;padding:34px 24px 48px}button,input,select{font:inherit}button{cursor:pointer}.note{background:var(--pv-card);border:1px solid var(--pv-line);border-left:3px solid var(--pv-yellow);border-radius:8px;padding:12px 15px;margin-bottom:16px;font-size:12px;white-space:pre-wrap}.note[hidden]{display:none}.public-status [hidden]{display:none!important}.pv-link{border:0;background:none;color:var(--pv-cyan);padding:0 5px;font-size:12px}.pv-clear,.pv-dialog-close{border:1px solid var(--pv-line);border-radius:6px;background:var(--pv-card-2);color:var(--pv-ink);padding:6px 12px}.pv-detail{width:min(720px,calc(100% - 28px));max-height:85dvh;overflow-y:auto;border:1px solid var(--pv-line);border-radius:14px;background:var(--pv-card);color:var(--pv-ink);padding:24px}.pv-detail::backdrop{background:rgba(6,12,22,.65);backdrop-filter:blur(3px)}.pv-detail>header{display:flex;align-items:center;justify-content:space-between;gap:16px;margin-bottom:18px}.pv-detail h2{font-size:20px;margin:0;overflow-wrap:anywhere}.pv-detail h3{font-size:13px;margin-top:24px}.pv-detail .pv-detail-summary{margin-top:18px}.pv-detail .row{display:flex;justify-content:space-between;align-items:center;gap:10px;flex-wrap:wrap}.pv-detail .pv-bk{height:42px}.pv-detail .pv-quality{margin:8px 0}.pv-detail .pv-quality-charts{gap:20px}.pv-detail .pv-quality-title{font-size:12px}.pv-detail .pv-ln{font-size:12px}
main.pv-monitor{padding:34px 24px 48px}
@media(max-width:640px){main.pv-monitor{padding:24px 14px}.pv-detail{padding:18px}.pv-theme,.pv-dialog-close{min-height:44px;padding:10px 14px}.pv-link{min-height:44px;padding:0 10px;display:inline-flex;align-items:center}}
.pv-viewtabs{display:flex;gap:8px;margin:16px 0 4px}
.pv-viewtabs button{border:1px solid var(--pv-line);background:var(--pv-card);color:var(--pv-ink);border-radius:999px;padding:7px 16px;font-size:13px;min-height:36px}
.pv-viewtabs button.on{background:var(--pv-cyan);border-color:var(--pv-cyan);color:#06251f;font-weight:600}
.svc-banner{display:flex;justify-content:space-between;align-items:center;gap:12px;border:1px solid var(--pv-line);border-radius:10px;padding:16px 18px;margin:14px 0 18px;background:var(--pv-card)}
.svc-banner strong{font-size:17px}
.svc-banner span{color:var(--pv-mute);font-size:13px;white-space:nowrap}
.svc-banner.ok{border-left:4px solid var(--pv-green)}
.svc-banner.partial{border-left:4px solid var(--pv-yellow)}
.svc-banner.major{border-left:4px solid var(--pv-red)}
.svc-group{margin-bottom:22px}
.svc-group h2{display:flex;justify-content:space-between;align-items:baseline;font-size:14px;margin:0 0 8px;color:var(--pv-ink)}
.svc-group h2 small{color:var(--pv-mute);font-weight:400}
.svc-row{display:grid;grid-template-columns:minmax(170px,240px) 1fr auto;grid-template-areas:"id nets up";align-items:center;gap:10px 22px;padding:12px 16px;border:1px solid var(--pv-line);border-radius:10px;background:var(--pv-card);margin-bottom:8px}
.svc-id{grid-area:id;display:flex;align-items:center;gap:8px;min-width:0}
.svc-dot{width:9px;height:9px;border-radius:50%;flex:none}
.svc-dot.online{background:var(--pv-green)}
.svc-dot.degraded{background:var(--pv-yellow)}
.svc-dot.offline{background:var(--pv-red)}
.svc-name{font-weight:600;overflow-wrap:anywhere}
.svc-state{color:var(--pv-mute);font-size:12px;flex:none}
.svc-nets{grid-area:nets;display:flex;gap:8px;flex-wrap:wrap}
.svc-nets.svc-none{color:var(--pv-mute);font-size:12px}
.svc-lat{display:inline-flex;gap:6px;align-items:baseline;font-size:12px;background:var(--pv-card-2);border-radius:6px;padding:5px 10px;min-width:138px}
.svc-lat b{color:var(--pv-ink);font-size:13px;font-weight:600}
.svc-lat-none{opacity:.55}
.svc-lat-none b{color:var(--pv-mute)}
.svc-lat-name{color:var(--pv-mute)}
.svc-loss{font-size:11px}
.svc-line{color:var(--pv-mute);font-size:11px;border:1px solid var(--pv-line);border-radius:4px;padding:0 4px}
.svc-up{grid-area:up;display:inline-flex;gap:10px;align-items:center;font-size:12px;color:var(--pv-mute);white-space:nowrap}
.svc-up b{color:var(--pv-ink)}
.svc-days{display:inline-flex;gap:2px;align-items:flex-end;height:16px}
.svc-days i{display:block;width:5px;border-radius:1px}
@media(max-width:1020px){.svc-row{grid-template-columns:1fr auto;grid-template-areas:"id up" "nets nets"}}
@media(max-width:640px){.svc-row{grid-template-columns:1fr;grid-template-areas:"id" "nets" "up"}.svc-up{white-space:normal}}
` + statusCSS + `
@media(prefers-color-scheme:dark){body.public-status:not([data-theme="light"]){background:#000;color-scheme:dark;background-image:none}.public-status:not([data-theme="light"]) .pv{--pv-card:#0A0A0A;--pv-card-2:#18181B;--pv-line:#2E2E2E;--pv-ink:#FAFAFA;--pv-mute:#A1A1AA;--pv-track:#1C1C1F;--pv-green:#63c78f;--pv-blue:#7aa5e8;--pv-yellow:#e0a93e;--pv-orange:#f08c4a;--pv-red:#f26d6d;--pv-cyan:#2DD4BF;--pv-purple:#a493e8;--pv-glow:0 0 0 1px rgba(255,255,255,.03),0 8px 24px -12px rgba(0,0,0,.8)}}
</style></head><body class="public-status"><template id="taiwan-flag">{{taiwanFlag}}</template><main class="pv pv-monitor">
<header class="pv-dashboard-head"><div><div class="pv-eyebrow">POLARIS / STATUS</div><h1>{{.Title}}</h1></div><div class="pv-head-actions"><button class="pv-theme" type="button" id="theme" aria-pressed="false">切换深色</button></div></header>
{{if not .ServiceOnly}}<nav class="pv-viewtabs" aria-label="视图切换"><button type="button" id="tab-service" class="on" aria-pressed="true">服务状态</button><button type="button" id="tab-monitor" aria-pressed="false">详细监控</button></nav>{{end}}
<div id="view-service">
<div id="svc-note" class="note" hidden></div>
<div id="svc-stale" class="note" hidden>数据可能已过期，正在重试更新。</div>
<div id="svc-error" class="note" role="alert" hidden><span>暂时无法读取服务状态。</span><button type="button" id="svc-retry" class="pv-link">重试</button></div>
<div id="svc-banner" class="svc-banner ok"><strong>正在读取服务状态…</strong><span></span></div>
<div id="svc-groups"><div class="pv-loading" role="status">正在读取服务状态…</div></div>
<footer class="pv-footnote">数据截至 <span id="svc-at">—</span> · 每 30 秒更新。延迟为节点到目标的 TCP 连接耗时，百分比为近 1 小时连接失败率，不是 ICMP 丢包率。可用率按面板观测到的 Agent 心跳计算（5 分钟粒度）；面板自身停机期间全站计为不可用；短于 5 分钟的中断可能不体现。</footer>
</div>
{{if not .ServiceOnly}}<div id="view-monitor" hidden>
<div class="pv-subtitle">资源状态与网络质量 <span class="pv-live"><i></i><span id="at">等待更新</span></span></div>
<div id="note" class="note" hidden></div><div id="error" class="note" role="alert" hidden><span id="error-message"></span><button type="button" id="retry" class="pv-link">重试</button></div><div id="probe-disabled" class="note" hidden>延迟探测已关闭</div>
<div id="sum"></div>
<div class="pv-toolbar"><div class="pv-toolbar-top"><input id="search" class="pv-search" type="search" placeholder="搜索名称、地区或系统" aria-label="搜索节点"><div class="pv-toolbar-controls"><select id="sort" aria-label="节点排序"><option value="default">默认排序</option><option value="name">按名称</option><option value="cpu">CPU 从高到低</option><option value="network">网速从高到低</option><option value="latency">延迟从低到高</option><option value="expiry">到期时间从近到远</option></select><div class="pv-seg" aria-label="展示方式"><button type="button" id="v-card">条形</button><button type="button" id="v-ring">环形</button><button type="button" id="v-table">表格</button></div></div></div><div class="pv-toolbar-bottom"><div class="pv-chips" id="chips"></div><div class="pv-seg" aria-label="节点状态筛选"><button data-state="all">全部</button><button data-state="online">在线</button><button data-state="offline">离线</button><button data-state="attention">需关注</button></div><span class="pv-count" id="count"></span></div></div>
<div id="list"><div class="pv-loading" role="status">正在读取节点状态…</div></div>
<footer class="pv-footnote">延迟为节点到目标的 TCP 连接耗时；小图展示近 2 小时，每格 5 分钟。灰格表示无样本。失败率不是 ICMP 丢包率。</footer>
<dialog id="detail" class="pv-detail" aria-labelledby="detail-title"><header><h2 id="detail-title"></h2><button type="button" id="detail-close" class="pv-dialog-close" aria-label="关闭节点详情">关闭</button></header><div id="detail-content"></div></dialog>
</div>{{end}}
</main><script>var SVC_ONLY={{if .ServiceOnly}}true{{else}}false{{end}};
` + serviceJS + `</script>{{if not .ServiceOnly}}<script>var DATA={{.Data}};
` + statusJS + `</script>{{end}}</body></html>`))
