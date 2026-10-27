package panel

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Traffic reports over any period, from the per-day tables (TrafficDaily,
// TagTrafficDaily) that settleTraffic keeps next to the hourly rows and that
// are never pruned. Also: the one-off backfill of those tables from the
// hourly rows, and alerts on sudden traffic spikes.

const dayLayout = "2006-01-02"

// backfillDaily fills the per-day table from the hourly rows once, for
// panels that have hourly data from before the table existed.
func (s *Server) backfillDaily() {
	if s.st.GetSetting("traffic_daily_backfilled", "") == "true" {
		return
	}
	var n int64
	s.st.DB.Model(&store.TrafficDaily{}).Count(&n)
	if n == 0 {
		today := time.Now()
		start := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
		for i := 0; i <= 90; i++ {
			from := start.AddDate(0, 0, -i)
			var rows []store.TrafficDaily
			s.st.DB.Model(&store.TrafficHourly{}).Select("user_id, node_id, SUM(up) AS up, SUM(down) AS down").
				Where("hour >= ? AND hour < ?", from.UTC(), from.AddDate(0, 0, 1).UTC()).Group("user_id, node_id").Scan(&rows)
			for j := range rows {
				rows[j].Day = from.Format(dayLayout)
			}
			if len(rows) > 0 {
				s.st.DB.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 200)
			}
		}
	}
	s.st.SetSetting("traffic_daily_backfilled", "true")
}

// reportRange reads ?from=&to= (YYYY-MM-DD, inclusive); default: last 7 days.
func reportRange(c *gin.Context) (from, to string, err error) {
	now := time.Now()
	to = now.Format(dayLayout)
	from = now.AddDate(0, 0, -6).Format(dayLayout)
	if v := c.Query("from"); v != "" {
		from = v
	}
	if v := c.Query("to"); v != "" {
		to = v
	}
	f, err1 := time.ParseInLocation(dayLayout, from, time.Local)
	t, err2 := time.ParseInLocation(dayLayout, to, time.Local)
	if err1 != nil || err2 != nil || t.Before(f) {
		return "", "", fmt.Errorf("日期格式应为 YYYY-MM-DD，且开始不晚于结束")
	}
	if t.Sub(f) > 3*366*24*time.Hour {
		return "", "", fmt.Errorf("时间跨度最多 3 年")
	}
	return from, to, nil
}

type usage struct {
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

func (s *Server) names() (nodes, users map[string]string) {
	nodes, users = map[string]string{}, map[string]string{}
	var ns []store.Node
	s.st.DB.Select("id", "name").Find(&ns)
	for _, n := range ns {
		nodes[n.ID] = n.Name
	}
	var us []store.User
	s.st.DB.Select("id", "username").Find(&us)
	for _, u := range us {
		users[u.ID] = u.Username
	}
	return nodes, users
}

// statsReport: GET /stats/report?from=&to=&limit= — totals, daily trend,
// traffic by node, and rankings of users, nodes and inbounds.
func (s *Server) statsReport(c *gin.Context) {
	from, to, err := reportRange(c)
	if err != nil {
		badRequest(c, err)
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	limit = min(max(limit, 1), 200)
	nodeNames, userNames := s.names()
	nodeF, userF := c.Query("node"), c.Query("user")
	scoped := func(q *gorm.DB) *gorm.DB {
		if nodeF != "" {
			q = q.Where("node_id = ?", nodeF)
		} else {
			q = realUsers(q)
		}
		if userF != "" {
			q = q.Where("user_id = ?", userF)
		}
		return q
	}
	q := scoped(s.st.DB.Model(&store.TrafficDaily{}).Where("day >= ? AND day <= ?", from, to))

	type dayRow struct {
		Day  string `json:"date"`
		Up   int64  `json:"up"`
		Down int64  `json:"down"`
	}
	var trend []dayRow
	q.Session(&gorm.Session{}).Select("day, SUM(up) AS up, SUM(down) AS down").Group("day").Order("day").Scan(&trend)
	// Every day in the range, zeros included.
	byDay := map[string]usage{}
	var total usage
	for _, r := range trend {
		byDay[r.Day] = usage{r.Up, r.Down}
		total.Up += r.Up
		total.Down += r.Down
	}
	f, _ := time.ParseInLocation(dayLayout, from, time.Local)
	t, _ := time.ParseInLocation(dayLayout, to, time.Local)
	// The period of the same length just before, for "vs previous" deltas.
	span := int(t.Sub(f).Hours()/24+0.5) + 1
	pf, pt := f.AddDate(0, 0, -span).Format(dayLayout), f.AddDate(0, 0, -1).Format(dayLayout)
	var prev usage
	scoped(s.st.DB.Model(&store.TrafficDaily{}).Where("day >= ? AND day <= ?", pf, pt)).
		Select("COALESCE(SUM(up),0) AS up, COALESCE(SUM(down),0) AS down").Scan(&prev)
	var prevUsers int64
	scoped(s.st.DB.Model(&store.TrafficDaily{}).Where("day >= ? AND day <= ?", pf, pt)).Distinct("user_id").Count(&prevUsers)
	var activeUsers int64
	q.Session(&gorm.Session{}).Distinct("user_id").Count(&activeUsers)
	trend = trend[:0]
	for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
		k := d.Format(dayLayout)
		trend = append(trend, dayRow{Day: k, Up: byDay[k].Up, Down: byDay[k].Down})
	}

	type ranked struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Up   int64  `json:"up"`
		Down int64  `json:"down"`
	}
	var nodes []ranked
	q.Session(&gorm.Session{}).Select("node_id AS id, SUM(up) AS up, SUM(down) AS down").Group("node_id").Order("SUM(up+down) DESC").Scan(&nodes)
	for i := range nodes {
		nodes[i].Name = orDefault(nodeNames[nodes[i].ID], "已删除节点")
	}
	var users []ranked
	q.Session(&gorm.Session{}).Select("user_id AS id, SUM(up) AS up, SUM(down) AS down").Group("user_id").Order("SUM(up+down) DESC").Limit(limit).Scan(&users)
	for i := range users {
		users[i].Name = orDefault(userNames[users[i].ID], "已删除用户")
	}
	type inRow struct {
		NodeID   string `json:"nodeId"`
		NodeName string `json:"nodeName"`
		Tag      string `json:"tag"`
		Up       int64  `json:"up"`
		Down     int64  `json:"down"`
	}
	var inbounds []inRow
	iq := s.st.DB.Model(&store.TagTrafficDaily{}).Where("kind = ? AND day >= ? AND day <= ?", "inbound", from, to)
	if nodeF != "" {
		iq = iq.Where("node_id = ?", nodeF)
	}
	iq.
		Select("node_id, tag, SUM(up) AS up, SUM(down) AS down").Group("node_id, tag").Order("SUM(up+down) DESC").Limit(limit).Scan(&inbounds)
	for i := range inbounds {
		inbounds[i].NodeName = orDefault(nodeNames[inbounds[i].NodeID], "已删除节点")
	}
	c.JSON(http.StatusOK, gin.H{"from": from, "to": to, "total": total, "trend": trend,
		"prev": gin.H{"from": pf, "to": pt, "total": prev, "activeUsers": prevUsers}, "activeUsers": activeUsers,
		"filter": gin.H{"node": nodeF, "nodeName": nodeNames[nodeF], "user": userF, "userName": userNames[userF]},
		"nodes":  nonNil(nodes), "users": nonNil(users), "inbounds": nonNil(inbounds)})
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// statsExport: GET /stats/export?from=&to= — CSV of each user's traffic on
// each node over the period (for billing / reconciliation).
func (s *Server) statsExport(c *gin.Context) {
	from, to, err := reportRange(c)
	if err != nil {
		badRequest(c, err)
		return
	}
	nodeNames, userNames := s.names()
	var rows []store.TrafficDaily
	eq := s.st.DB.Model(&store.TrafficDaily{}).Where("day >= ? AND day <= ?", from, to)
	if v := c.Query("node"); v != "" {
		eq = eq.Where("node_id = ?", v)
	} else {
		eq = realUsers(eq)
	}
	if v := c.Query("user"); v != "" {
		eq = eq.Where("user_id = ?", v)
	}
	eq.Select("user_id, node_id, SUM(up) AS up, SUM(down) AS down").Group("user_id, node_id").Scan(&rows)
	sort.Slice(rows, func(i, j int) bool {
		a, b := userNames[rows[i].UserID], userNames[rows[j].UserID]
		if a != b {
			return a < b
		}
		return nodeNames[rows[i].NodeID] < nodeNames[rows[j].NodeID]
	})
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=traffic-%s_%s.csv", from, to))
	c.Writer.WriteString("\xEF\xBB\xBF") // Excel reads UTF-8 with a BOM
	w := csv.NewWriter(c.Writer)
	w.Write([]string{"用户", "节点", "上传(字节)", "下载(字节)", "合计(字节)", "合计"})
	totals := map[string]int64{}
	for _, r := range rows {
		sum := r.Up + r.Down
		totals[r.UserID] += sum
		w.Write([]string{orDefault(userNames[r.UserID], r.UserID), orDefault(nodeNames[r.NodeID], r.NodeID),
			strconv.FormatInt(r.Up, 10), strconv.FormatInt(r.Down, 10), strconv.FormatInt(sum, 10), sub.HumanBytes(sum)})
	}
	ids := make([]string, 0, len(totals))
	for id := range totals {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return userNames[ids[i]] < userNames[ids[j]] })
	for _, id := range ids {
		w.Write([]string{orDefault(userNames[id], id), "（合计）", "", "", strconv.FormatInt(totals[id], 10), sub.HumanBytes(totals[id])})
	}
	w.Flush()
	s.auditCtx(c, "stats.export", from+" ~ "+to, "")
}

// metricSlot is the averaging window of node resource history.
const metricSlot = 5 * time.Minute

// recordMetric folds a heartbeat's resource sample into its 5-minute slot.
func (s *Server) recordMetric(nodeID string, sys store.SystemInfo, now time.Time) {
	at := now.UTC().Truncate(metricSlot)
	var m store.NodeMetric
	if s.st.DB.First(&m, "node_id = ? AND at = ?", nodeID, at).Error != nil {
		s.warnWrite("node metric", s.st.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.NodeMetric{NodeID: nodeID, At: at, CPU: sys.CPU, Mem: sys.Mem,
			RxBps: sys.RxBps, TxBps: sys.TxBps, Load1: sys.Load1, N: 1}).Error)
		return
	}
	n := float64(m.N)
	avg := func(old, v float64) float64 { return (old*n + v) / (n + 1) }
	s.st.DB.Model(&store.NodeMetric{}).Where("node_id = ? AND at = ?", nodeID, at).Updates(map[string]any{
		"cpu": avg(m.CPU, sys.CPU), "mem": avg(m.Mem, sys.Mem), "load1": avg(m.Load1, sys.Load1),
		"rx_bps": uint64(avg(float64(m.RxBps), float64(sys.RxBps))), "tx_bps": uint64(avg(float64(m.TxBps), float64(sys.TxBps))),
		"n": m.N + 1,
	})
}

// nodeMetrics: GET /nodes/:id/metrics?hours=24 (up to 30 days).
func (s *Server) nodeMetrics(c *gin.Context) {
	hours, _ := strconv.Atoi(c.DefaultQuery("hours", "24"))
	hours = min(max(hours, 1), 720)
	var ms []store.NodeMetric
	s.st.DB.Where("node_id = ? AND at >= ?", c.Param("id"), time.Now().Add(-time.Duration(hours)*time.Hour).UTC()).Order("at").Find(&ms)
	c.JSON(http.StatusOK, gin.H{"slotSeconds": int(metricSlot.Seconds()), "points": nonNil(ms)})
}

// checkTrafficSpikes alerts admins when a user's last 24 hours exceed
// traffic_spike_factor times their daily average of the 7 days before (and
// at least traffic_spike_min_gb): a shared or leaked account, usually.
func (s *Server) checkTrafficSpikes() {
	factor, _ := strconv.ParseFloat(s.st.GetSetting("traffic_spike_factor", "5"), 64)
	if factor <= 0 {
		return
	}
	minGB, _ := strconv.ParseFloat(s.st.GetSetting("traffic_spike_min_gb", "5"), 64)
	minBytes := int64(max(minGB, 0) * (1 << 30))
	now := time.Now()
	type row struct {
		UserID string
		Total  int64
	}
	var recent, before []row
	realUsers(s.st.DB.Model(&store.TrafficHourly{})).Select("user_id, SUM(up+down) AS total").
		Where("hour >= ?", now.Add(-24*time.Hour).UTC()).Group("user_id").Scan(&recent)
	realUsers(s.st.DB.Model(&store.TrafficHourly{})).Select("user_id, SUM(up+down) AS total").
		Where("hour >= ? AND hour < ?", now.Add(-8*24*time.Hour).UTC(), now.Add(-24*time.Hour).UTC()).Group("user_id").Scan(&before)
	base := map[string]int64{}
	for _, r := range before {
		base[r.UserID] = r.Total
	}
	_, userNames := s.names()
	for _, r := range recent {
		avg := float64(base[r.UserID]) / 7
		if r.Total < minBytes || avg <= 0 || float64(r.Total) < factor*avg {
			continue // no history yet counts as no baseline, not as a spike
		}
		if !s.alertOnce("spike:"+r.UserID, 24*time.Hour) {
			continue
		}
		s.notify(Event{Kind: "traffic_spike", Text: fmt.Sprintf("📈 用户 %s 近 24 小时用了 %s，是过去 7 天日均（%s）的 %.1f 倍，请留意是否共享或泄露",
			orDefault(userNames[r.UserID], r.UserID), sub.HumanBytes(r.Total), sub.HumanBytes(int64(avg)), float64(r.Total)/avg)})
	}
}
