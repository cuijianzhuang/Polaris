package panel

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Server traffic: agents report their NICs' cumulative byte counters with
// every heartbeat; the panel adds the deltas to the node's billing cycle and
// a per-day table, warns once at the threshold and once over the limit, and
// starts a new cycle on the reset day.

// netDelta is how much a counter moved since the last report. A counter
// below the last value means the host rebooted and counts from zero again.
func netDelta(last, cur uint64) int64 {
	switch {
	case last == 0: // first report: only a baseline
		return 0
	case cur >= last:
		return int64(cur - last)
	default:
		return int64(cur)
	}
}

// netUsed is the cycle total as the provider bills it.
func netUsed(n *store.Node) int64 {
	var v int64
	switch n.NetMode {
	case "up":
		v = n.NetTx
	case "down":
		v = n.NetRx
	case "max":
		v = max(n.NetRx, n.NetTx)
	default:
		v = n.NetRx + n.NetTx
	}
	return v + n.NetOffset
}

// accountNodeNet folds a heartbeat's NIC counters into the node and returns
// the column updates for the heartbeat's write.
func (s *Server) accountNodeNet(n *store.Node, rx, tx uint64, now time.Time) map[string]any {
	if rx == 0 && tx == 0 {
		return nil // an agent that doesn't report counters
	}
	drx, dtx := netDelta(n.NetLastRx, rx), netDelta(n.NetLastTx, tx)
	upd := map[string]any{"net_last_rx": rx, "net_last_tx": tx}
	if drx == 0 && dtx == 0 {
		return upd
	}
	upd["net_rx"] = gorm.Expr("net_rx + ?", drx)
	upd["net_tx"] = gorm.Expr("net_tx + ?", dtx)
	s.st.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "node_id"}, {Name: "day"}},
		DoUpdates: clause.Assignments(map[string]any{"rx": gorm.Expr("node_traffic_dailies.rx + ?", drx), "tx": gorm.Expr("node_traffic_dailies.tx + ?", dtx)}),
	}).Create(&store.NodeTrafficDaily{NodeID: n.ID, Day: now.Format("2006-01-02"), Rx: drx, Tx: dtx})
	n.NetRx += drx
	n.NetTx += dtx
	if lvl := netAlertLevel(n); lvl > n.NetWarned {
		upd["net_warned"] = lvl
		n.NetWarned = lvl
		s.notify(Event{Kind: "node_traffic", NodeID: n.ID, Text: netAlertText(n, lvl, now)})
	}
	return upd
}

// netAlertLevel is 2 over the limit, 1 past the warning threshold, else 0.
func netAlertLevel(n *store.Node) int {
	if n.NetLimit <= 0 {
		return 0
	}
	used := netUsed(n)
	pct := n.NetWarnPct
	if pct <= 0 || pct > 100 {
		pct = 80
	}
	switch {
	case used >= n.NetLimit:
		return 2
	case float64(used) >= float64(n.NetLimit)*float64(pct)/100:
		return 1
	}
	return 0
}

func netAlertText(n *store.Node, lvl int, now time.Time) string {
	used := netUsed(n)
	share := int(math.Round(100 * float64(used) / float64(n.NetLimit)))
	reset := ""
	if n.NetResetDay > 0 {
		reset = "，" + nextNetReset(n.NetResetDay, now).Format("1月2日") + "重置"
	}
	if lvl == 2 {
		return fmt.Sprintf("🚫 节点 %s 本周期流量已超出限额：%s / %s（%d%%）%s", n.Name, humanBytes(used), humanBytes(n.NetLimit), share, reset)
	}
	return fmt.Sprintf("⚠️ 节点 %s 本周期流量已用 %d%%：%s / %s%s", n.Name, share, humanBytes(used), humanBytes(n.NetLimit), reset)
}

func humanBytes(b int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v, i := float64(b), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}

// cycleStart is the start (local midnight) of the cycle containing now for a
// reset day; a day past the month's end means its last day.
func cycleStart(day int, now time.Time) time.Time {
	at := func(y int, m time.Month) time.Time {
		last := time.Date(y, m+1, 0, 0, 0, 0, 0, now.Location()).Day()
		return time.Date(y, m, min(day, last), 0, 0, 0, 0, now.Location())
	}
	t := at(now.Year(), now.Month())
	if t.After(now) {
		t = at(now.Year(), now.Month()-1)
	}
	return t
}

func nextNetReset(day int, now time.Time) time.Time {
	c := cycleStart(day, now)
	last := time.Date(c.Year(), c.Month()+2, 0, 0, 0, 0, 0, now.Location()).Day()
	return time.Date(c.Year(), c.Month()+1, min(day, last), 0, 0, 0, 0, now.Location())
}

// resetNodeCycles starts a new cycle on nodes whose reset day has come
// (hourly job).
func (s *Server) resetNodeCycles() {
	now := time.Now()
	var ns []store.Node
	s.st.DB.Where("net_reset_day > 0").Find(&ns)
	for _, n := range ns {
		start := cycleStart(n.NetResetDay, now)
		if n.NetCycleStart == nil {
			// Just configured: the current cycle runs from its start; keep what was counted.
			s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Update("net_cycle_start", start)
			continue
		}
		if !n.NetCycleStart.Before(start) {
			continue
		}
		s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{
			"net_rx": 0, "net_tx": 0, "net_offset": 0, "net_warned": 0, "net_cycle_start": start})
		s.log.Info("node traffic cycle reset", "node", n.Name)
	}
}

type netReq struct {
	LimitGB  *float64 `json:"limitGb"`
	Mode     *string  `json:"mode"`
	ResetDay *int     `json:"resetDay"`
	WarnPct  *int     `json:"warnPct"`
	UsedGB   *float64 `json:"usedGb"` // calibrate the cycle total to this value
}

const gib = 1 << 30

// putNodeNet updates a node's traffic allowance settings.
func (s *Server) putNodeNet(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req netReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	upd := map[string]any{}
	if req.LimitGB != nil {
		if *req.LimitGB < 0 {
			badRequest(c, errors.New("限额不能为负数"))
			return
		}
		n.NetLimit = int64(*req.LimitGB * gib)
		upd["net_limit"] = n.NetLimit
	}
	if req.Mode != nil {
		switch *req.Mode {
		case "", "sum", "up", "down", "max":
		default:
			badRequest(c, errors.New("计费方式应为 sum、up、down 或 max"))
			return
		}
		n.NetMode = *req.Mode
		upd["net_mode"] = n.NetMode
	}
	if req.ResetDay != nil {
		if *req.ResetDay < 0 || *req.ResetDay > 31 {
			badRequest(c, errors.New("重置日应在 0–31 之间"))
			return
		}
		if *req.ResetDay != n.NetResetDay {
			n.NetResetDay = *req.ResetDay
			upd["net_reset_day"] = n.NetResetDay
			if n.NetResetDay > 0 {
				start := cycleStart(n.NetResetDay, time.Now())
				upd["net_cycle_start"] = start
			}
		}
	}
	if req.WarnPct != nil {
		if *req.WarnPct < 0 || *req.WarnPct > 100 {
			badRequest(c, errors.New("提醒阈值应在 0–100 之间"))
			return
		}
		n.NetWarnPct = *req.WarnPct
		upd["net_warn_pct"] = n.NetWarnPct
	}
	if req.UsedGB != nil {
		if *req.UsedGB < 0 {
			badRequest(c, errors.New("已用流量不能为负数"))
			return
		}
		n.NetOffset = 0
		n.NetOffset = int64(*req.UsedGB*gib) - netUsed(&n)
		upd["net_offset"] = n.NetOffset
	}
	// Re-arm alerts for the new limit / threshold / total.
	n.NetWarned = netAlertLevel(&n)
	upd["net_warned"] = n.NetWarned
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(upd).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "node.traffic", n.Name, fmt.Sprintf("limit %s, mode %s, reset day %d", humanBytes(n.NetLimit), n.NetMode, n.NetResetDay))
	s.st.DB.First(&n, "id = ?", n.ID)
	c.JSON(http.StatusOK, n)
}

// resetNodeNet zeroes the node's cycle by hand.
func (s *Server) resetNodeNet(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	now := time.Now()
	s.st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{
		"net_rx": 0, "net_tx": 0, "net_offset": 0, "net_warned": 0, "net_cycle_start": now})
	s.auditCtx(c, "node.traffic.reset", n.Name, "")
	c.Status(http.StatusNoContent)
}

// nodeNetDaily returns the node's daily NIC traffic for the last N days.
func (s *Server) nodeNetDaily(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "31"))
	if days <= 0 || days > 366 {
		days = 31
	}
	from := time.Now().AddDate(0, 0, -days+1).Format("2006-01-02")
	rows := []store.NodeTrafficDaily{}
	s.st.DB.Where("node_id = ? AND day >= ?", c.Param("id"), from).Order("day").Find(&rows)
	c.JSON(http.StatusOK, rows)
}
