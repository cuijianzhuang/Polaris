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

// All writers of cycle totals and offsets must read under this same lock.
// In particular, a calibration must not use a snapshot from before a heartbeat.
func (s *Server) withNodeNet(id string, fn func(*gorm.DB, *store.Node) error) error {
	return s.st.DB.Transaction(func(db *gorm.DB) error {
		r := db.Exec("UPDATE nodes SET net_last_rx=net_last_rx WHERE id = ?", id)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		var n store.Node
		if err := db.First(&n, "id = ?", id).Error; err != nil {
			return err
		}
		return fn(db, &n)
	})
}

// Roll over before adding the first sample in a new cycle. Preserve the NIC
// baseline and daily history. The heartbeat interval belongs to its receipt day;
// there is no trustworthy way to split unobserved bytes at midnight.
func advanceNodeNetCycle(db *gorm.DB, n *store.Node, now time.Time) error {
	if n.NetResetDay <= 0 {
		return nil
	}
	start := cycleStart(n.NetResetDay, now)
	if n.NetCycleStart != nil && !n.NetCycleStart.Before(start) {
		return nil
	}
	updates := map[string]any{"net_cycle_start": start}
	if n.NetCycleStart != nil {
		n.NetRx, n.NetTx, n.NetOffset, n.NetWarned = 0, 0, 0, 0
		updates["net_rx"], updates["net_tx"], updates["net_offset"], updates["net_warned"] = 0, 0, 0, 0
	}
	n.NetCycleStart = &start
	return db.Model(&store.Node{}).Where("id = ?", n.ID).Updates(updates).Error
}

// Called under the node lock, in the same transaction as the NIC baseline.
// Uptime orders all interface epochs in one boot. Keep retired boots so a
// delayed pre-reboot message cannot switch the active source back.
func acceptNodeNetBoot(db *gorm.DB, id string, sample *polarisv1.SystemStats) (bool, error) {
	if sample.NetBootId == "" { // older agents have only an interface epoch
		return true, nil
	}
	var boot store.NodeNetBoot
	err := db.First(&boot, "node_id = ? AND boot_id = ?", id, sample.NetBootId).Error
	if err == nil {
		if boot.Retired || sample.NetSampleUptimeMillis <= boot.LastUptime {
			return false, nil
		}
		return true, db.Model(&boot).Update("last_uptime", sample.NetSampleUptimeMillis).Error
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	if err := db.Model(&store.NodeNetBoot{}).Where("node_id = ? AND retired = ?", id, false).Update("retired", true).Error; err != nil {
		return false, err
	}
	return true, db.Create(&store.NodeNetBoot{NodeID: id, BootID: sample.NetBootId, LastUptime: sample.NetSampleUptimeMillis}).Error
}

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
	if sample.NetBootId != "" && (!modern || len(sample.NetBootId) > 64) {
		return errors.New("invalid network boot identity")
	}
	if modern && sample.NetSampleUptimeMillis > math.MaxInt64 {
		return errors.New("network sample uptime exceeds storage range")
	}
	if rx > math.MaxInt64 || tx > math.MaxInt64 {
		return errors.New("network counters exceed storage range")
	}
	epoch := ""
	if modern {
		epoch = sample.NetEpoch
	}
	var alert *Event
	err := s.withNodeNet(id, func(db *gorm.DB, n *store.Node) error {
		accepted, err := acceptNodeNetBoot(db, id, sample)
		if err != nil || !accepted {
			return err
		}
		if e := advanceNodeNetCycle(db, n, now); e != nil {
			return e
		}
		if modern && n.NetBaseline && n.NetEpoch == epoch && sample.NetSampleUptimeMillis <= n.NetSampleUptime {
			return nil
		}
		baseline := n.NetBaseline || n.NetLastRx > 0 || n.NetLastTx > 0
		rebase := !baseline || n.NetEpoch != epoch || n.NetBootID != sample.NetBootId || rx < n.NetLastRx || tx < n.NetLastTx
		var drx, dtx int64
		if !rebase {
			drx, dtx = netDelta(n.NetLastRx, rx), netDelta(n.NetLastTx, tx)
		}
		if n.NetRx > math.MaxInt64-drx || n.NetTx > math.MaxInt64-dtx {
			return errors.New("network totals overflow")
		}
		updates := map[string]any{"net_last_rx": rx, "net_last_tx": tx, "net_baseline": true, "net_epoch": epoch, "net_boot_id": sample.NetBootId, "net_sample_uptime": sample.NetSampleUptimeMillis, "net_observed_at": now, "net_rx": gorm.Expr("net_rx + ?", drx), "net_tx": gorm.Expr("net_tx + ?", dtx)}
		row := store.NodeTrafficDaily{NodeID: id, Day: now.Format("2006-01-02"), Rx: drx, Tx: dtx}
		if modern {
			row.AccountingVersion = 1
		}
		if e := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "node_id"}, {Name: "day"}}, DoUpdates: clause.Assignments(map[string]any{"rx": gorm.Expr("node_traffic_dailies.rx + ?", drx), "tx": gorm.Expr("node_traffic_dailies.tx + ?", dtx), "accounting_version": gorm.Expr("CASE WHEN node_traffic_dailies.accounting_version = 1 AND excluded.accounting_version = 1 THEN 1 ELSE 0 END")})}).Create(&row).Error; e != nil {
			return e
		}
		n.NetRx += drx
		n.NetTx += dtx
		if lvl := netAlertLevel(n); lvl > n.NetWarned {
			updates["net_warned"] = lvl
			n.NetWarned = lvl
			alert = &Event{Kind: "node_traffic", NodeID: id, Text: netAlertText(n, lvl, now)}
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
	s.resetNodeCyclesAt(time.Now())
}

func (s *Server) resetNodeCyclesAt(now time.Time) {
	var ns []store.Node
	if err := s.st.DB.Select("id").Where("net_reset_day > 0").Find(&ns).Error; err != nil {
		s.log.Error("list node traffic cycles", "err", err)
		return
	}
	for _, n := range ns {
		if err := s.withNodeNet(n.ID, func(db *gorm.DB, n *store.Node) error {
			return advanceNodeNetCycle(db, n, now)
		}); err != nil {
			s.log.Error("reset node traffic cycle", "node", n.ID, "err", err)
		}
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
	var req netReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	n, err := s.updateNodeNet(c.Param("id"), req, time.Now())
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "node.traffic", n.Name, fmt.Sprintf("limit %s, mode %s, reset day %d", humanBytes(n.NetLimit), n.NetMode, n.NetResetDay))
	c.JSON(http.StatusOK, n)
}

func (s *Server) updateNodeNet(id string, req netReq, now time.Time) (store.Node, error) {
	var out store.Node
	validGB := func(v float64) bool {
		return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v*gib < float64(math.MaxInt64)
	}
	if req.LimitGB != nil && !validGB(*req.LimitGB) {
		return out, commandInputError{"限额应为有效的非负数，且不能超出存储范围"}
	}
	if req.UsedGB != nil && !validGB(*req.UsedGB) {
		return out, commandInputError{"已用流量应为有效的非负数，且不能超出存储范围"}
	}
	if req.Mode != nil {
		switch *req.Mode {
		case "", "sum", "up", "down", "max":
		default:
			return out, commandInputError{"计费方式应为 sum、up、down 或 max"}
		}
	}
	if req.ResetDay != nil && (*req.ResetDay < 0 || *req.ResetDay > 31) {
		return out, commandInputError{"重置日应在 0–31 之间"}
	}
	if req.WarnPct != nil && (*req.WarnPct < 0 || *req.WarnPct > 100) {
		return out, commandInputError{"提醒阈值应在 0–100 之间"}
	}
	err := s.withNodeNet(id, func(db *gorm.DB, n *store.Node) error {
		if err := advanceNodeNetCycle(db, n, now); err != nil {
			return err
		}
		upd := map[string]any{}
		if req.LimitGB != nil {
			n.NetLimit = int64(*req.LimitGB * gib)
			upd["net_limit"] = n.NetLimit
		}
		if req.Mode != nil {
			n.NetMode = *req.Mode
			upd["net_mode"] = n.NetMode
		}
		if req.ResetDay != nil && *req.ResetDay != n.NetResetDay {
			n.NetResetDay = *req.ResetDay
			upd["net_reset_day"] = n.NetResetDay
			if n.NetResetDay > 0 {
				start := cycleStart(n.NetResetDay, now)
				n.NetCycleStart = &start
				upd["net_cycle_start"] = start
			}
		}
		if req.WarnPct != nil {
			n.NetWarnPct = *req.WarnPct
			upd["net_warn_pct"] = n.NetWarnPct
		}
		if req.UsedGB != nil {
			n.NetOffset = int64(*req.UsedGB*gib) - nodeNetBytes(n.NetMode, n.NetRx, n.NetTx)
			upd["net_offset"] = n.NetOffset
		}
		n.NetWarned = netAlertLevel(n)
		upd["net_warned"] = n.NetWarned
		if err := db.Model(&store.Node{}).Where("id = ?", id).Updates(upd).Error; err != nil {
			return err
		}
		out = *n
		return nil
	})
	return out, err
}

// resetNodeNet zeroes the node's cycle by hand.
func (s *Server) resetNodeNet(c *gin.Context) {
	var name string
	err := s.withNodeNet(c.Param("id"), func(db *gorm.DB, n *store.Node) error {
		name = n.Name
		return db.Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{
			"net_rx": 0, "net_tx": 0, "net_offset": 0, "net_warned": 0, "net_cycle_start": time.Now()}).Error
	})
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "node.traffic.reset", name, "")
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
