package panel

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func init() {}

// statsOverview feeds the dashboard KPIs, trend and core distribution.
func (s *Server) statsOverview(c *gin.Context) {
	now := time.Now().In(s.cfg.Location())
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
	// Trend over ?days= (default 7, max 90).
	n := min(max(atoiDefault(c.Query("days"), 7), 1), 90)
	agg := s.statsOverviewAggregates(now, n)
	out := gin.H{
		"nodes":       gin.H{"online": online, "total": len(nodes), "timeout": timeout, "error": failed},
		"users":       agg.users,
		"today":       agg.today,
		"month":       agg.month,
		"topUsers":    agg.topUsers,
		"attention":   agg.attention,
		"rate":        gin.H{"rxBps": rx, "txBps": tx, "peakNode": peak},
		"trend":       agg.trend,
		"cores":       agg.cores,
		"nodeList":    nodes,
		"generatedAt": now,
	}
	// Traffic-read tokens already access this route; do not expand that scope
	// to payment, refund or diagnostic information. The dedicated endpoint is
	// administrator-session only by default.
	if apiTokenOf(c) == nil {
		operational, err := s.loadOperationalOverview(now)
		if err != nil {
			s.log.Warn("operational overview unavailable", "error", err)
		}
		out["operational"] = operational
	}
	c.JSON(http.StatusOK, out)
}

// statsOverviewTTL bounds how long dashboard aggregates are trusted: the
// dashboard is polled repeatedly, and re-running a dozen aggregate
// queries per poll is the cost this cache removes.
const statsOverviewTTL = 30 * time.Second

type statsDay struct {
	Date string `json:"date"`
	Up   int64  `json:"up"`
	Down int64  `json:"down"`
}

type statsTopUser struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
}

// statsOverviewAggregates is the database-derived part of the dashboard:
// everything except live node status/rate and the operational overview.
// Callers must not mutate the returned slices and maps: they may be
// shared with concurrent requests serving the same cache entry.
//
// The cache is deliberately TTL-only, with no active invalidation on
// traffic settlement: settle runs every 10s, so invalidating there
// would make the cache useless. Thirty seconds of staleness on dashboard
// KPIs is the accepted trade-off, same class as the 60s Setting cache.
// The key carries the local date so a midnight rollover recomputes
// instead of serving yesterday's buckets.
type statsOverviewAggregates struct {
	users     gin.H
	today     struct{ Up, Down int64 }
	month     struct{ Up, Down int64 }
	trend     []statsDay
	topUsers  []statsTopUser
	attention gin.H
	cores     map[model.CoreType]int
}

type statsOvKey struct {
	days int
	date string // local YYYY-MM-DD of the request
}

type statsOvSnap struct {
	at  time.Time
	key statsOvKey
	agg statsOverviewAggregates
}

func (s *Server) statsOverviewAggregates(now time.Time, days int) statsOverviewAggregates {
	key := statsOvKey{days: days, date: now.Format("2006-01-02")}
	if sn := s.statsOvCache.Load(); sn != nil && sn.key == key && time.Since(sn.at) < statsOverviewTTL {
		return sn.agg
	}
	s.statsOvMu.Lock()
	defer s.statsOvMu.Unlock()
	if sn := s.statsOvCache.Load(); sn != nil && sn.key == key && time.Since(sn.at) < statsOverviewTTL {
		return sn.agg
	}
	agg := s.computeStatsOverviewAggregates(now, days)
	s.statsOvCache.Store(&statsOvSnap{at: time.Now(), key: key, agg: agg})
	return agg
}

func (s *Server) computeStatsOverviewAggregates(now time.Time, n int) statsOverviewAggregates {
	var onlineUsers, activeUsers, totalUsers int64
	s.st.DB.Model(&store.User{}).Where("is_service = ? AND online_at > ?", false, now.Add(-2*time.Minute).UTC()).Count(&onlineUsers)
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

	// Trend over n days, bucketed by local day.
	local := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	days := make([]statsDay, n)
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
		if i, ok := idx[r.Hour.In(s.cfg.Location()).Format("2006-01-02")]; ok {
			days[i].Up += r.Up
			days[i].Down += r.Down
		}
	}

	// Today's heaviest users: usernames resolved with one IN query, not
	// one First per user.
	tops := []statsTopUser{}
	realUsers(s.st.DB.Model(&store.TrafficHourly{})).Select("user_id, SUM(up) AS up, SUM(down) AS down").
		Where("hour >= ? AND user_id <> ''", dayStart).Group("user_id").Order("SUM(up+down) DESC").Limit(5).Scan(&tops)
	if len(tops) > 0 {
		ids := make([]string, 0, len(tops))
		for _, t := range tops {
			ids = append(ids, t.UserID)
		}
		var us []store.User
		s.st.DB.Select("id", "username").Where("id IN ?", ids).Find(&us)
		names := map[string]string{}
		for _, u := range us {
			names[u.ID] = u.Username
		}
		for i := range tops {
			tops[i].Username = names[tops[i].UserID]
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
	return statsOverviewAggregates{
		users:     gin.H{"online": onlineUsers, "active": activeUsers, "total": totalUsers},
		today:     today,
		month:     month,
		trend:     days,
		topUsers:  tops,
		attention: attention,
		cores:     dist,
	}
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
	// Series: aggregate per hour in SQL instead of loading every
	// TrafficHourly row and summing in memory. The day bucket below
	// still runs over the (far fewer) hourly rows, so its output is
	// unchanged. Each query gets its own Session clone of the base
	// conditions: reusing one chain would leak the first query's
	// GROUP BY into the second.
	type hourRow struct {
		Hour     time.Time
		Up, Down int64
	}
	var hours []hourRow
	if err := q.Session(&gorm.Session{}).Model(&store.TrafficHourly{}).Select("hour, SUM(up) AS up, SUM(down) AS down").
		Group("hour").Order("hour").Scan(&hours).Error; err != nil {
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
	for _, r := range hours {
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
	// Top users in range: aggregate and sort in SQL, then resolve the
	// usernames with a single IN query instead of one First per user.
	type top struct {
		UserID   string `json:"userId"`
		Username string `json:"username"`
		Total    int64  `json:"total"`
	}
	n, _ := strconv.Atoi(c.DefaultQuery("top", "10"))
	var tops []top
	if n <= 0 {
		// top=0 asks for no users (the old tops[:0]); a negative
		// value used to panic on the same slice.
		tops = []top{}
	} else if err := q.Session(&gorm.Session{}).Model(&store.TrafficHourly{}).Select("user_id, SUM(up+down) AS total").
		Group("user_id").Order("SUM(up+down) DESC").Limit(n).Scan(&tops).Error; err != nil {
		serverError(c, err)
		return
	}
	if len(tops) > 0 {
		ids := make([]string, 0, len(tops))
		for _, t := range tops {
			ids = append(ids, t.UserID)
		}
		var us []store.User
		s.st.DB.Select("id", "username").Where("id IN ?", ids).Find(&us)
		names := map[string]string{}
		for _, u := range us {
			names[u.ID] = u.Username
		}
		for i := range tops {
			tops[i].Username = names[tops[i].UserID]
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
	now := time.Now().In(s.cfg.Location())
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
