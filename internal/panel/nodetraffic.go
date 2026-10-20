package panel

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Server traffic: agents report their NICs' cumulative byte counters with
// every heartbeat; the panel adds the deltas to the node's billing cycle and
// a per-day table, warns once at the threshold and once over the limit, and
// starts a new cycle on the reset day.

// A counter decrease is not sufficient evidence of a host reboot: interfaces
// can be replaced or removed. The next sample establishes a new baseline.
func netDelta(last, cur uint64) int64 {
	if cur < last || cur-last > math.MaxInt64 {
		return 0
	}
	return int64(cur - last)
}

func nodeNetBytes(mode string, rx, tx int64) int64 {
	switch mode {
	case "up":
		return tx
	case "down":
		return rx
	case "max":
		return max(rx, tx)
	default:
		return rx + tx
	}
}
func netUsed(n *store.Node) int64 { return nodeNetBytes(n.NetMode, n.NetRx, n.NetTx) + n.NetOffset }

// The baseline, billing-cycle counters and daily ledger commit together. Lock
// before reading so duplicate heartbeats on different instances count once.
func (s *Server) accountNodeNet(id string, sample *polarisv1.SystemStats, now time.Time) error {
	if sample == nil {
		return nil
	}
	rx, tx := sample.RxBytes, sample.TxBytes
	modern := sample.NetCountersValid
	if !modern && rx == 0 && tx == 0 {
		return nil
	}
	if modern && (sample.NetEpoch == "" || len(sample.NetEpoch) > 64 || sample.NetSampleUptimeMillis == 0) {
		return errors.New("invalid network sample identity")
	}
	if rx > math.MaxInt64 || tx > math.MaxInt64 {
		return errors.New("network counters exceed storage range")
	}
	epoch := ""
	if modern {
		epoch = sample.NetEpoch
	}
	var alert *Event
	err := s.st.DB.Transaction(func(db *gorm.DB) error {
		r := db.Exec("UPDATE nodes SET net_last_rx=net_last_rx WHERE id = ?", id)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		var n store.Node
		if e := db.First(&n, "id = ?", id).Error; e != nil {
			return e
		}
		if modern && n.NetBaseline && n.NetEpoch == epoch && sample.NetSampleUptimeMillis <= n.NetSampleUptime {
			return nil
		}
		baseline := n.NetBaseline || n.NetLastRx > 0 || n.NetLastTx > 0
		rebase := !baseline || n.NetEpoch != epoch || rx < n.NetLastRx || tx < n.NetLastTx
		var drx, dtx int64
		if !rebase {
			drx, dtx = netDelta(n.NetLastRx, rx), netDelta(n.NetLastTx, tx)
		}
		if n.NetRx > math.MaxInt64-drx || n.NetTx > math.MaxInt64-dtx {
			return errors.New("network totals overflow")
		}
		updates := map[string]any{"net_last_rx": rx, "net_last_tx": tx, "net_baseline": true, "net_epoch": epoch, "net_sample_uptime": sample.NetSampleUptimeMillis, "net_observed_at": now, "net_rx": gorm.Expr("net_rx + ?", drx), "net_tx": gorm.Expr("net_tx + ?", dtx)}
		row := store.NodeTrafficDaily{NodeID: id, Day: now.Format("2006-01-02"), Rx: drx, Tx: dtx}
		if modern {
			row.AccountingVersion = 1
		}
		if e := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "node_id"}, {Name: "day"}}, DoUpdates: clause.Assignments(map[string]any{"rx": gorm.Expr("node_traffic_dailies.rx + ?", drx), "tx": gorm.Expr("node_traffic_dailies.tx + ?", dtx), "accounting_version": gorm.Expr("CASE WHEN node_traffic_dailies.accounting_version = 1 AND excluded.accounting_version = 1 THEN 1 ELSE 0 END")})}).Create(&row).Error; e != nil {
			return e
		}
		n.NetRx += drx
		n.NetTx += dtx
		if lvl := netAlertLevel(&n); lvl > n.NetWarned {
			updates["net_warned"] = lvl
			n.NetWarned = lvl
			alert = &Event{Kind: "node_traffic", NodeID: id, Text: netAlertText(&n, lvl, now)}
		}
		return db.Model(&store.Node{}).Where("id = ?", id).Updates(updates).Error
	})
	if err == nil && alert != nil {
		s.notify(*alert)
	}
	return err
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
