package panel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Server probe: every node measures the TCP connect time to a few targets
// (by default one China Telecom, Unicom and Mobile endpoint) every 30
// seconds, through the agent's existing "probe" command, so older agents
// take part too. Results are folded into 5-minute slots and kept 7 days.

const (
	latencySlot   = 5 * time.Minute
	latencyKeep   = 7 * 24 * time.Hour
	probeParallel = 32
)

// defaultProbeTargets are seeded once. The zstatic endpoints are province
// level carrier addresses meant for TCP pings ({prov}-{ct|cu|cm}-v4…:80).
var defaultProbeTargets = []store.ProbeTarget{
	{Name: "上海电信", Carrier: "ct", Addr: "sh-ct-v4.ip.zstaticcdn.com:80"},
	{Name: "上海联通", Carrier: "cu", Addr: "sh-cu-v4.ip.zstaticcdn.com:80"},
	{Name: "上海移动", Carrier: "cm", Addr: "sh-cm-v4.ip.zstaticcdn.com:80"},
}

var probeCarriers = map[string]bool{"ct": true, "cu": true, "cm": true, "": true}

func (s *Server) probeEnabled() bool { return s.st.GetSetting("probe_enabled", "true") == "true" }

// seedProbeTargets adds the defaults the first time (not again after the
// admin deletes them).
func (s *Server) seedProbeTargets() {
	if s.st.GetSetting("probe_seeded", "") != "" {
		return
	}
	var n int64
	s.st.DB.Model(&store.ProbeTarget{}).Count(&n)
	if n == 0 {
		for i, t := range defaultProbeTargets {
			t.ID, t.SortOrder, t.Enabled = store.NewID(), i, true
			s.st.DB.Create(&t)
		}
	}
	_ = s.st.SetSetting("probe_seeded", "1")
}

func (s *Server) latencyTargets(enabledOnly bool) []store.ProbeTarget {
	var ts []store.ProbeTarget
	q := s.st.DB.Order("sort_order, created_at")
	if enabledOnly {
		q = q.Where("enabled = ?", true)
	}
	q.Find(&ts)
	return nonNil(ts)
}

// runProbes is the 30-second job (leader).
func (s *Server) runLatencyProbes() {
	if !s.probeEnabled() || !s.probeBusy.CompareAndSwap(false, true) {
		return
	}
	defer s.probeBusy.Store(false)
	s.seedProbeTargets()
	ts := s.latencyTargets(true)
	if len(ts) == 0 {
		return
	}
	var ids []string
	s.st.DB.Model(&store.Node{}).Where("status IN ?", []string{"online", "syncing", "error"}).Pluck("id", &ids)
	ctx, cancel := context.WithTimeout(s.ctxBG(), 25*time.Second)
	defer cancel()
	sem := make(chan struct{}, probeParallel)
	var wg sync.WaitGroup
	for _, id := range ids {
		if !s.hub.Online(id) {
			continue
		}
		for _, t := range ts {
			wg.Add(1)
			sem <- struct{}{}
			go func(nodeID string, t store.ProbeTarget) {
				defer func() { <-sem; wg.Done() }()
				ms, ok := s.probeOnce(ctx, nodeID, t.Addr)
				if ctx.Err() != nil && !ok {
					return // the job ran out of time: not the target's fault
				}
				s.recordLatency(nodeID, t.ID, ms, ok, time.Now())
			}(id, t)
		}
	}
	wg.Wait()
}

func (s *Server) probeOnce(ctx context.Context, nodeID, addr string) (float64, bool) {
	res, err := s.hub.Command(ctx, nodeID, "probe", map[string]string{"tcp": addr}, 15*time.Second)
	if err != nil || !res.Ok {
		return 0, false
	}
	ms, err := strconv.ParseFloat(res.Output, 64)
	if err != nil {
		return 0, false
	}
	return max(ms, 0.1), true // the agent reports whole milliseconds; 0 means < 1 ms
}

// recordLatency folds one probe into its slot.
func (s *Server) recordLatency(nodeID, targetID string, ms float64, ok bool, now time.Time) {
	at := now.UTC().Truncate(latencySlot)
	var m store.NodeLatency
	if s.st.DB.First(&m, "node_id = ? AND target_id = ? AND at = ?", nodeID, targetID, at).Error != nil {
		m = store.NodeLatency{NodeID: nodeID, TargetID: targetID, At: at, Sent: 1, LastAt: now}
		if ok {
			m.SumMs, m.MinMs, m.MaxMs, m.LastMs = ms, ms, ms, ms
		} else {
			m.Lost = 1
		}
		if s.st.DB.Create(&m).Error == nil {
			return
		}
		if s.st.DB.First(&m, "node_id = ? AND target_id = ? AND at = ?", nodeID, targetID, at).Error != nil {
			return
		}
	}
	upd := map[string]any{"sent": m.Sent + 1, "last_at": now, "last_ms": 0.0}
	if ok {
		upd["sum_ms"], upd["last_ms"] = m.SumMs+ms, ms
		if m.Sent == m.Lost || ms < m.MinMs { // first success in the slot
			upd["min_ms"] = ms
		}
		if ms > m.MaxMs {
			upd["max_ms"] = ms
		}
	} else {
		upd["lost"] = m.Lost + 1
	}
	s.st.DB.Model(&store.NodeLatency{}).Where("node_id = ? AND target_id = ? AND at = ?", nodeID, targetID, at).Updates(upd)
}

// latencyPoint is a slot as the UI draws it.
type latencyPoint struct {
	T      time.Time `json:"t"`
	Target string    `json:"target"`
	Avg    *float64  `json:"avg"` // nil: every probe failed
	Min    float64   `json:"min"`
	Max    float64   `json:"max"`
	Loss   float64   `json:"loss"` // percent
}

func pointOf(m *store.NodeLatency) latencyPoint {
	p := latencyPoint{T: m.At, Target: m.TargetID, Min: m.MinMs, Max: m.MaxMs}
	if ok := m.Sent - m.Lost; ok > 0 {
		v := m.SumMs / float64(ok)
		p.Avg = &v
	}
	if m.Sent > 0 {
		p.Loss = 100 * float64(m.Lost) / float64(m.Sent)
	}
	return p
}

// latencyNow is a node's current figure for one target.
type latencyNow struct {
	Ms    float64    `json:"ms"`   // last probe, 0 = it failed
	Avg   *float64   `json:"avg"`  // last hour
	Loss  float64    `json:"loss"` // last hour, percent
	At    time.Time  `json:"at"`
	Spark []*float64 `json:"spark"` // 5-minute averages, oldest first, last 2 hours
}

// latencySummary: node id -> target id -> current figures. Probes older
// than 3 minutes are dropped (the node went away).
func (s *Server) latencySummary(nodeIDs []string, now time.Time) map[string]map[string]*latencyNow {
	out := map[string]map[string]*latencyNow{}
	if len(nodeIDs) == 0 {
		return out
	}
	from := now.UTC().Add(-2 * time.Hour).Truncate(latencySlot)
	var ms []store.NodeLatency
	s.st.DB.Where("node_id IN ? AND at >= ?", nodeIDs, from).Order("at").Find(&ms)
	const slots = int(2*time.Hour/latencySlot) + 1
	type acc struct {
		sent, lost int
		sum        float64
	}
	hour := map[string]*acc{}
	for i := range ms {
		m := &ms[i]
		if out[m.NodeID] == nil {
			out[m.NodeID] = map[string]*latencyNow{}
		}
		cur := out[m.NodeID][m.TargetID]
		if cur == nil {
			cur = &latencyNow{Spark: make([]*float64, slots)}
			out[m.NodeID][m.TargetID] = cur
		}
		if i := int(m.At.Sub(from) / latencySlot); i >= 0 && i < slots {
			cur.Spark[i] = pointOf(m).Avg
		}
		if now.Sub(m.LastAt) < 3*time.Minute && m.LastAt.After(cur.At) {
			cur.Ms, cur.At = m.LastMs, m.LastAt
		}
		if m.At.After(now.Add(-time.Hour)) {
			k := m.NodeID + "|" + m.TargetID
			if hour[k] == nil {
				hour[k] = &acc{}
			}
			hour[k].sent += m.Sent
			hour[k].lost += m.Lost
			hour[k].sum += m.SumMs
		}
	}
	for k, a := range hour {
		node, target, _ := strings.Cut(k, "|")
		cur := out[node][target]
		if ok := a.sent - a.lost; ok > 0 {
			v := a.sum / float64(ok)
			cur.Avg = &v
		}
		if a.sent > 0 {
			cur.Loss = 100 * float64(a.lost) / float64(a.sent)
		}
	}
	return out
}

// probeNode is one node on the probe pages.
type probeNode struct {
	ID       string                 `json:"id"`
	Name     string                 `json:"name"`
	Region   string                 `json:"region"`
	Flag     string                 `json:"flag"`
	Online   bool                   `json:"online"`
	Status   string                 `json:"status"`
	CPU      float64                `json:"cpu"`
	Mem      float64                `json:"mem"`
	RxBps    uint64                 `json:"rxBps"`
	TxBps    uint64                 `json:"txBps"`
	Uptime   uint64                 `json:"uptime"`
	Load1    float64                `json:"load1"`
	NetUsed  int64                  `json:"netUsed"`
	NetLimit int64                  `json:"netLimit"`
	LastSeen *time.Time             `json:"lastSeen"`
	Hidden   bool                   `json:"hidden,omitempty"`
	Address  string                 `json:"address,omitempty"`
	Version  string                 `json:"agentVersion,omitempty"`
	Latency  map[string]*latencyNow `json:"latency"`
}

type probeTargetView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Carrier string `json:"carrier"`
}

// probeOverview builds the page data; public leaves out addresses, versions
// and hidden nodes.
func (s *Server) probeOverview(public bool) gin.H {
	now := time.Now()
	var ns []store.Node
	q := s.st.DB.Order("sort_order, name")
	if public {
		// Nodes upgraded from the first probe release can have NULL here.
		// Match the admin switch: only an explicit true hides the node.
		q = q.Where("status_hidden = ? OR status_hidden IS NULL", false)
	}
	q.Find(&ns)
	ids := make([]string, len(ns))
	for i := range ns {
		ids[i] = ns[i].ID
	}
	lat := s.latencySummary(ids, now)
	showTraffic := s.st.GetSetting("status_show_traffic", "false") == "true"
	nodes := make([]probeNode, 0, len(ns))
	for i := range ns {
		n := &ns[i]
		sys := n.System.V
		online := n.Status == "online" || n.Status == "syncing" || n.Status == "error"
		p := probeNode{ID: n.ID, Name: n.Name, Region: n.Region, Flag: n.Flag, Online: online, Status: n.Status, LastSeen: n.LastSeen,
			NetUsed: netUsed(n), NetLimit: n.NetLimit, Latency: lat[n.ID]}
		if online {
			p.CPU, p.Mem, p.RxBps, p.TxBps, p.Uptime, p.Load1 = sys.CPU, sys.Mem, sys.RxBps, sys.TxBps, sys.Uptime, sys.Load1
		}
		if p.Latency == nil {
			p.Latency = map[string]*latencyNow{}
		}
		if public && !showTraffic {
			p.NetUsed, p.NetLimit = 0, 0
		}
		if !public {
			p.Hidden, p.Address, p.Version = n.StatusHidden, nodeHost(n), n.AgentVersion
		}
		nodes = append(nodes, p)
	}
	ts := s.latencyTargets(true)
	tv := make([]probeTargetView, len(ts))
	for i, t := range ts {
		tv[i] = probeTargetView{ID: t.ID, Name: t.Name, Carrier: t.Carrier}
	}
	return gin.H{"nodes": nodes, "targets": tv, "at": now, "probeEnabled": s.probeEnabled()}
}

// --- admin API ---

// probePage: GET /probe.
func (s *Server) probePage(c *gin.Context) {
	s.seedProbeTargets()
	out := s.probeOverview(false)
	out["statusPage"] = s.statusPageEnabled()
	out["statusUrl"] = s.subBase() + statusPath
	c.JSON(http.StatusOK, out)
}

// nodeLatency: GET /nodes/:id/latency?hours=24 (up to 7 days).
func (s *Server) nodeLatency(c *gin.Context) {
	hours, _ := strconv.Atoi(c.DefaultQuery("hours", "24"))
	hours = min(max(hours, 1), 168)
	var ms []store.NodeLatency
	s.st.DB.Where("node_id = ? AND at >= ?", c.Param("id"), time.Now().Add(-time.Duration(hours)*time.Hour).UTC()).Order("at").Find(&ms)
	pts := make([]latencyPoint, len(ms))
	for i := range ms {
		pts[i] = pointOf(&ms[i])
	}
	c.JSON(http.StatusOK, gin.H{"slotSeconds": int(latencySlot.Seconds()), "targets": s.latencyTargets(false), "points": pts})
}

type probeTargetReq struct {
	Name      string `json:"name"`
	Carrier   string `json:"carrier"`
	Addr      string `json:"addr"`
	SortOrder int    `json:"sortOrder"`
	Enabled   *bool  `json:"enabled"`
}

func (r *probeTargetReq) check() error {
	r.Name, r.Addr, r.Carrier = strings.TrimSpace(r.Name), strings.TrimSpace(r.Addr), strings.ToLower(strings.TrimSpace(r.Carrier))
	if r.Name == "" || len(r.Name) > 64 {
		return errors.New("请填写名称（不超过 64 个字符）")
	}
	if !probeCarriers[r.Carrier] {
		return errors.New("运营商只能是 ct（电信）、cu（联通）、cm（移动）或留空")
	}
	host, port, err := net.SplitHostPort(r.Addr)
	if err != nil || host == "" || len(r.Addr) > 255 {
		return errors.New("地址格式为 主机:端口，例如 sh-ct-v4.ip.zstaticcdn.com:80")
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return errors.New("端口应在 1–65535 之间")
	}
	return nil
}

// listProbeTargets: GET /probe/targets.
func (s *Server) listProbeTargets(c *gin.Context) {
	s.seedProbeTargets()
	c.JSON(http.StatusOK, s.latencyTargets(false))
}

// createProbeTarget: POST /probe/targets.
func (s *Server) createProbeTarget(c *gin.Context) {
	var r probeTargetReq
	if err := c.ShouldBindJSON(&r); err != nil {
		badRequest(c, err)
		return
	}
	if err := r.check(); err != nil {
		badRequest(c, err)
		return
	}
	var n int64
	s.st.DB.Model(&store.ProbeTarget{}).Count(&n)
	if n >= 20 {
		badRequest(c, errors.New("最多 20 个探测目标"))
		return
	}
	t := store.ProbeTarget{ID: store.NewID(), Name: r.Name, Carrier: r.Carrier, Addr: r.Addr, SortOrder: r.SortOrder, Enabled: r.Enabled == nil || *r.Enabled}
	if err := s.st.DB.Create(&t).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "probe.target.create", t.Name, t.Addr)
	c.JSON(http.StatusOK, t)
}

// updateProbeTarget: PUT /probe/targets/:id.
func (s *Server) updateProbeTarget(c *gin.Context) {
	var t store.ProbeTarget
	if err := s.st.DB.First(&t, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var r probeTargetReq
	if err := c.ShouldBindJSON(&r); err != nil {
		badRequest(c, err)
		return
	}
	if err := r.check(); err != nil {
		badRequest(c, err)
		return
	}
	upd := map[string]any{"name": r.Name, "carrier": r.Carrier, "addr": r.Addr, "sort_order": r.SortOrder}
	if r.Enabled != nil {
		upd["enabled"] = *r.Enabled
	}
	if err := s.st.DB.Model(&t).Updates(upd).Error; err != nil {
		dbError(c, err)
		return
	}
	if t.Addr != r.Addr { // old figures were for another address
		s.st.DB.Where("target_id = ?", t.ID).Delete(&store.NodeLatency{})
	}
	s.auditCtx(c, "probe.target.update", r.Name, r.Addr)
	s.st.DB.First(&t, "id = ?", t.ID)
	c.JSON(http.StatusOK, t)
}

// deleteProbeTarget: DELETE /probe/targets/:id.
func (s *Server) deleteProbeTarget(c *gin.Context) {
	var t store.ProbeTarget
	if err := s.st.DB.First(&t, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	s.st.DB.Delete(&t)
	s.st.DB.Where("target_id = ?", t.ID).Delete(&store.NodeLatency{})
	s.auditCtx(c, "probe.target.delete", t.Name, t.Addr)
	c.Status(http.StatusNoContent)
}

// testProbeTarget: POST /probe/targets/test {addr, nodeId}: one probe now.
func (s *Server) testProbeTarget(c *gin.Context) {
	var r struct {
		Addr   string `json:"addr"`
		NodeID string `json:"nodeId"`
	}
	if err := c.ShouldBindJSON(&r); err != nil {
		badRequest(c, err)
		return
	}
	req := probeTargetReq{Name: "test", Addr: r.Addr}
	if err := req.check(); err != nil {
		badRequest(c, err)
		return
	}
	res, err := s.hub.Command(c.Request.Context(), r.NodeID, "probe", map[string]string{"tcp": req.Addr}, 15*time.Second)
	switch {
	case err != nil:
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
	case !res.Ok:
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": res.Error})
	default:
		ms, _ := strconv.Atoi(res.Output)
		c.JSON(http.StatusOK, gin.H{"ok": true, "ms": ms})
	}
}

// setNodeStatusHidden: PUT /probe/nodes/:id {hidden}.
func (s *Server) setNodeStatusHidden(c *gin.Context) {
	var r struct {
		Hidden bool `json:"hidden"`
	}
	if err := c.ShouldBindJSON(&r); err != nil {
		badRequest(c, err)
		return
	}
	res := s.st.DB.Model(&store.Node{}).Where("id = ?", c.Param("id")).Update("status_hidden", r.Hidden)
	if res.Error != nil {
		dbError(c, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		return
	}
	s.statusCache.Store(nil)
	c.Status(http.StatusNoContent)
}
