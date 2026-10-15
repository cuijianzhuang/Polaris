package panel

import (
	"net/http"
	"sort"
	"strconv"
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
	s.st.DB.Model(&store.TrafficHourly{}).Select("COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").
		Where("hour >= ?", dayStart).Scan(&today)

	// 7-day trend, bucketed by local day.
	from := dayStart.AddDate(0, 0, -6)
	var rows []store.TrafficHourly
	s.st.DB.Where("hour >= ?", from).Find(&rows)
	type day struct {
		Date string `json:"date"`
		Up   int64  `json:"up"`
		Down int64  `json:"down"`
	}
	days := make([]day, 7)
	for i := range days {
		days[i].Date = from.AddDate(0, 0, i).Local().Format("2006-01-02")
	}
	for _, r := range rows {
		idx := int(r.Hour.Local().Sub(from.Local()).Hours() / 24)
		if idx >= 0 && idx < 7 {
			days[idx].Up += r.Up
			days[idx].Down += r.Down
		}
	}
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
