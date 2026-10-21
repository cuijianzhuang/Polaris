package panel

import (
	"errors"
	"math"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

var errCostChanged = errors.New("服务器成本已更新，请关闭编辑窗口并刷新后重试")

// Server costs use calendar dates in the panel's local timezone.
func costDate(t time.Time) time.Time {
	t = t.In(time.Local)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

func costDaysLeft(exp, now time.Time) int {
	exp, now = costDate(exp), costDate(now)
	utcDay := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	return int((utcDay(exp).Unix() - utcDay(now).Unix()) / (24 * 60 * 60))
}

func costRenewalDay(nc *store.NodeCost) int {
	if nc.RenewalDay >= 1 && nc.RenewalDay <= 31 {
		return nc.RenewalDay
	}
	return costDate(*nc.ExpireAt).Day()
}

// Keep the original billing day when a shorter month clamps the expiry.
func addCostCycles(exp time.Time, cycle string, day, periods int) (time.Time, error) {
	months := int(cycleMonths[cycle])
	if cycle == "once" {
		return time.Time{}, commandInputError{"一次性付费不需要续费"}
	}
	if months <= 0 || periods < 1 || periods > 120000 || day < 1 || day > 31 {
		return time.Time{}, commandInputError{"付费周期或续费日期无效"}
	}
	first := time.Date(exp.Year(), exp.Month()+time.Month(months*periods), 1, 0, 0, 0, 0, exp.Location())
	if first.Year() < 1 || first.Year() > 9999 {
		return time.Time{}, commandInputError{"续费日期超出支持范围"}
	}
	last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, exp.Location()).Day()
	return time.Date(first.Year(), first.Month(), min(day, last), 0, 0, 0, 0, exp.Location()), nil
}

// The parent node lock also serializes first-time cost creation and deletion.
func (s *Server) withNodeCost(id string, fn func(*gorm.DB, *store.Node, *store.NodeCost) error) error {
	return s.withNodeNet(id, func(db *gorm.DB, n *store.Node) error {
		nc := store.NodeCost{NodeID: id}
		if err := db.First(&nc, "node_id = ?", id).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return fn(db, n, &nc)
	})
}

func saveNodeCost(db *gorm.DB, nc *store.NodeCost) error {
	if nc.Revision >= math.MaxInt64 {
		return errors.New("server cost revision exceeds storage range")
	}
	nc.Revision++
	return db.Save(nc).Error
}

func (s *Server) renewCost(id string) (store.NodeCost, error) {
	var out store.NodeCost
	err := s.withNodeCost(id, func(db *gorm.DB, _ *store.Node, nc *store.NodeCost) error {
		if nc.ExpireAt == nil {
			return commandInputError{"先设置到期日"}
		}
		day := costRenewalDay(nc)
		next, err := addCostCycles(costDate(*nc.ExpireAt), nc.Cycle, day, 1)
		if err != nil {
			return err
		}
		nc.ExpireAt, nc.RenewalDay, nc.WarnedFor = &next, day, ""
		if err := saveNodeCost(db, nc); err != nil {
			return err
		}
		out = *nc
		return nil
	})
	return out, err
}

// Catch up directly from the original anchor, rather than drifting through
// each missed month. The expiry date is valid until the following midnight.
func catchUpCostExpiry(nc *store.NodeCost, now time.Time) (bool, error) {
	exp := costDate(*nc.ExpireAt)
	if now.Before(exp.AddDate(0, 0, 1)) {
		return false, nil
	}
	months := int(cycleMonths[nc.Cycle])
	if months <= 0 {
		return false, commandInputError{"付费周期或续费日期无效"}
	}
	localNow := now.In(time.Local)
	diff := (localNow.Year()-exp.Year())*12 + int(localNow.Month()-exp.Month())
	periods := max(1, diff/months)
	day := costRenewalDay(nc)
	next, err := addCostCycles(exp, nc.Cycle, day, periods)
	if err != nil {
		return false, err
	}
	if !now.Before(next.AddDate(0, 0, 1)) {
		next, err = addCostCycles(exp, nc.Cycle, day, periods+1)
		if err != nil {
			return false, err
		}
	}
	nc.ExpireAt, nc.RenewalDay, nc.WarnedFor = &next, day, ""
	return true, nil
}
