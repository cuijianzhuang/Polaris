package panel

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func init() {}

// statsOverview feeds the dashboard KPIs, trend and core distribution.
func (s *Server) statsOverview(c *gin.Context) {
	now := time.Now()
	nodes := s.nodeViews()
	online, timeout, failed := 0, 0, 0
	var rx, tx uint64
	peak, peakRate := "", uint64(0)
	for _, n := range nodes {
		switch n.Status {
		case "online", "syncing":
			online++
		case "offline":
			if n.LastSeen != nil {
				timeout++
			}
		case "error":
			failed++ // connected, but the last spec failed to apply
		}
		rx += n.System.V.RxBps
		tx += n.System.V.TxBps
		if r := n.System.V.RxBps + n.System.V.TxBps; r > peakRate && n.Connected {
			peak, peakRate = n.Name, r
		}
	}
	var onlineUsers, activeUsers, totalUsers int64
	s.st.DB.Model(&store.User{}).Where("is_service = ? AND online_at > ?", false, now.Add(-2*time.Minute)).Count(&onlineUsers)
	s.st.DB.Model(&store.User{}).Where("is_service = ? AND status = ?", false, "active").Count(&activeUsers)
	s.st.DB.Model(&store.User{}).Where("is_service = ?", false).Count(&totalUsers)

	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UTC()
	var today struct{ Up, Down int64 }
	realUsers(s.st.DB.Model(&store.TrafficHourly{})).Select("COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").
		Where("hour >= ?", dayStart).Scan(&today)

	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).UTC()
	var month struct{ Up, Down int64 }
	realUsers(s.st.DB.Model(&store.TrafficHourly{})).Select("COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").
		Where("hour >= ?", monthStart).Scan(&month)

	// Trend over ?days= (default 7, max 90), bucketed by local day.
	n := min(max(atoiDefault(c.Query("days"), 7), 1), 90)
	local := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	type day struct {
		Date string `json:"date"`
		Up   int64  `json:"up"`
		Down int64  `json:"down"`
	}
	days := make([]day, n)
	idx := map[string]int{}
	for i := range days {
		days[i].Date = local.AddDate(0, 0, i-n+1).Format("2006-01-02")
		idx[days[i].Date] = i
	}
	var rows []struct {
		Hour     time.Time
		Up, Down int64
	}
	realUsers(s.st.DB.Model(&store.TrafficHourly{})).Select("hour, SUM(up) AS up, SUM(down) AS down").
		Where("hour >= ?", local.AddDate(0, 0, -n+1).UTC()).Group("hour").Scan(&rows)
	for _, r := range rows {
		if i, ok := idx[r.Hour.Local().Format("2006-01-02")]; ok {
			days[i].Up += r.Up
			days[i].Down += r.Down
		}
	}

	// Today's heaviest users.
	type topUser struct {
		UserID   string `json:"userId"`
		Username string `json:"username"`
		Up       int64  `json:"up"`
		Down     int64  `json:"down"`
	}
	tops := []topUser{}
	realUsers(s.st.DB.Model(&store.TrafficHourly{})).Select("user_id, SUM(up) AS up, SUM(down) AS down").
		Where("hour >= ? AND user_id <> ''", dayStart).Group("user_id").Order("SUM(up+down) DESC").Limit(5).Scan(&tops)
	for i := range tops {
		var u store.User
		if s.st.DB.Select("username").First(&u, "id = ?", tops[i].UserID).Error == nil {
			tops[i].Username = u.Username
		}
	}

	// Things that need a look, as user-list filter counts.
	warn := time.Duration(max(atoiDefault(s.st.GetSetting("expire_warn_days", "7"), 7), 1)) * 24 * time.Hour
	attention := gin.H{"warnDays": int(warn.Hours() / 24)}
	for _, f := range []string{"expiring", "limited", "expired"} {
		var cnt int64
		userFilter(s.st.DB.Model(&store.User{}).Where("is_service = ?", false), f, now, warn).Count(&cnt)
		attention[f] = cnt
	}
	var near int64
	userFilter(s.st.DB.Model(&store.User{}).Where("is_service = ?", false), "near", now, warn).Count(&near)
	attention["near"] = near
	dist := map[model.CoreType]int{}
	var cores []store.Core
	s.st.DB.Find(&cores)
	for _, cr := range cores {
		dist[cr.Type]++
	}
	c.JSON(http.StatusOK, gin.H{
		"nodes":       gin.H{"online": online, "total": len(nodes), "timeout": timeout, "error": failed},
		"users":       gin.H{"online": onlineUsers, "active": activeUsers, "total": totalUsers},
		"today":       today,
		"month":       month,
		"topUsers":    tops,
		"attention":   attention,
		"rate":        gin.H{"rxBps": rx, "txBps": tx, "peakNode": peak},
		"trend":       days,
		"cores":       dist,
		"nodeList":    nodes,
		"generatedAt": now,
	})
}

// statsTraffic returns a series: ?user=&node=&from=&to=&bucket=hour|day.
func (s *Server) statsTraffic(c *gin.Context) {
	now := time.Now()
	from := now.AddDate(0, 0, -7)
	to := now
	if v, err := time.Parse(time.RFC3339, c.Query("from")); err == nil {
		from = v
	}
	if v, err := time.Parse(time.RFC3339, c.Query("to")); err == nil {
		to = v
	}
	q := s.st.DB.Where("hour >= ? AND hour <= ?", from.UTC(), to.UTC())
	if v := c.Query("user"); v != "" {
		q = q.Where("user_id = ?", v)
	}
	if v := c.Query("node"); v != "" {
		q = q.Where("node_id = ?", v)
	} else {
		q = realUsers(q)
	}
	var rows []store.TrafficHourly
	if err := q.Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	bucket := c.DefaultQuery("bucket", "hour")
	type point struct {
		T    time.Time `json:"t"`
		Up   int64     `json:"up"`
		Down int64     `json:"down"`
	}
	m := map[int64]*point{}
	for _, r := range rows {
		t := r.Hour
		if bucket == "day" {
			l := t.Local()
			t = time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, l.Location())
		}
		p := m[t.Unix()]
		if p == nil {
			p = &point{T: t}
			m[t.Unix()] = p
		}
		p.Up += r.Up
		p.Down += r.Down
	}
	out := make([]point, 0, len(m))
	for _, p := range m {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	// Top users in range.
	type top struct {
		UserID   string `json:"userId"`
		Username string `json:"username"`
		Total    int64  `json:"total"`
	}
	agg := map[string]int64{}
	for _, r := range rows {
		agg[r.UserID] += r.Up + r.Down
	}
	var tops []top
	for id, t := range agg {
		tops = append(tops, top{UserID: id, Total: t})
	}
	sort.Slice(tops, func(i, j int) bool { return tops[i].Total > tops[j].Total })
	if n, _ := strconv.Atoi(c.DefaultQuery("top", "10")); len(tops) > n {
		tops = tops[:n]
	}
	for i := range tops {
		var u store.User
		if s.st.DB.Select("username").First(&u, "id = ?", tops[i].UserID).Error == nil {
			tops[i].Username = u.Username
		}
	}
	c.JSON(http.StatusOK, gin.H{"series": out, "top": tops})
}

// tagStats is GET /stats/tags?kind=inbound|outbound: bytes per inbound /
// outbound of every node since its last reset.
func (s *Server) tagStats(c *gin.Context) {
	q := s.st.DB.Order("kind, node_id, tag")
	if k := c.Query("kind"); k != "" {
		q = q.Where("kind = ?", k)
	}
	var rows []store.TagTraffic
	q.Find(&rows)
	names := s.nodeNames()
	type row struct {
		store.TagTraffic
		NodeName string `json:"nodeName"`
	}
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		out = append(out, row{r, names[r.NodeID]})
	}
	c.JSON(http.StatusOK, out)
}

// resetTagStats is POST /stats/tags/reset {kind, nodeId, tag}: empty fields
// match everything (e.g. {kind: "outbound"} resets every outbound).
func (s *Server) resetTagStats(c *gin.Context) {
	var req struct {
		Kind   string `json:"kind"`
		NodeID string `json:"nodeId"`
		Tag    string `json:"tag"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	q := s.st.DB.Where("1 = 1")
	if req.Kind != "" {
		q = q.Where("kind = ?", req.Kind)
	}
	if req.NodeID != "" {
		q = q.Where("node_id = ?", req.NodeID)
	}
	if req.Tag != "" {
		q = q.Where("tag = ?", req.Tag)
	}
	if err := q.Delete(&store.TagTraffic{}).Error; err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "stats.reset", strings.TrimSpace(req.Kind+" "+req.Tag), req.NodeID)
	c.Status(http.StatusNoContent)
}

// nodeTraffic sums traffic per node: billed user traffic (after the node's
// multiplier) for today and the last 30 days, plus the raw inbound byte
// counters, which also cover cores that cannot attribute bytes to users.
type nodeTraffic struct {
	TodayUp   int64 `json:"todayUp"`
	TodayDown int64 `json:"todayDown"`
	MonthUp   int64 `json:"monthUp"`
	MonthDown int64 `json:"monthDown"`
	RawUp     int64 `json:"rawUp"`
	RawDown   int64 `json:"rawDown"`
}

func (s *Server) nodeTrafficStats() map[string]*nodeTraffic {
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UTC()
	out := map[string]*nodeTraffic{}
	get := func(id string) *nodeTraffic {
		if out[id] == nil {
			out[id] = &nodeTraffic{}
		}
		return out[id]
	}
	type row struct {
		NodeID   string
		Up, Down int64
	}
	var rows []row
	s.st.DB.Model(&store.TrafficHourly{}).Select("node_id, COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").
		Where("hour >= ?", now.AddDate(0, 0, -30).UTC()).Group("node_id").Scan(&rows)
	for _, r := range rows {
		t := get(r.NodeID)
		t.MonthUp, t.MonthDown = r.Up, r.Down
	}
	rows = nil
	s.st.DB.Model(&store.TrafficHourly{}).Select("node_id, COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").
		Where("hour >= ?", dayStart).Group("node_id").Scan(&rows)
	for _, r := range rows {
		t := get(r.NodeID)
		t.TodayUp, t.TodayDown = r.Up, r.Down
	}
	rows = nil
	s.st.DB.Model(&store.TagTraffic{}).Select("node_id, COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").
		Where("kind = ?", "inbound").Group("node_id").Scan(&rows)
	for _, r := range rows {
		t := get(r.NodeID)
		t.RawUp, t.RawDown = r.Up, r.Down
	}
	return out
}

func (s *Server) nodeTrafficAPI(c *gin.Context) {
	all := s.nodeTrafficStats()
	if sc := nodeScope(c); sc != nil {
		for id := range all {
			if !sc[id] {
				delete(all, id)
			}
		}
	}
	c.JSON(http.StatusOK, all)
}
