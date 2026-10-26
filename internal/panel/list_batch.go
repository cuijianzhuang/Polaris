package panel

import (
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Batch loaders for list pages: one query per relation instead of one per
// row, so a list of N groups or plans costs a fixed number of queries.

// groupMembers returns each group's inbound and node ids.
func groupMembers(db *gorm.DB, ids []string) (inbounds, nodes map[string][]string) {
	inbounds, nodes = map[string][]string{}, map[string][]string{}
	if len(ids) == 0 {
		return
	}
	var gi []store.GroupInbound
	db.Where("group_id IN ?", ids).Find(&gi)
	for _, r := range gi {
		inbounds[r.GroupID] = append(inbounds[r.GroupID], r.InboundID)
	}
	var gn []store.GroupNode
	db.Where("group_id IN ?", ids).Find(&gn)
	for _, r := range gn {
		nodes[r.GroupID] = append(nodes[r.GroupID], r.NodeID)
	}
	return
}

// countBy counts rows of model per value of col, for the given values.
func countBy(db *gorm.DB, model any, col string, vals []string) map[string]int64 {
	out := map[string]int64{}
	if len(vals) == 0 {
		return out
	}
	var rows []struct {
		K string
		N int64
	}
	db.Model(model).Select(col+" AS k, COUNT(*) AS n").Where(col+" IN ?", vals).Group(col).Scan(&rows)
	for _, r := range rows {
		out[r.K] = r.N
	}
	return out
}

// countDistinctBy counts distinct values of field per value of col.
func countDistinctBy(db *gorm.DB, model any, col, field string, vals []string) map[string]int64 {
	out := map[string]int64{}
	if len(vals) == 0 {
		return out
	}
	var rows []struct {
		K string
		N int64
	}
	db.Model(model).Select(col+" AS k, COUNT(DISTINCT "+field+") AS n").Where(col+" IN ?", vals).Group(col).Scan(&rows)
	for _, r := range rows {
		out[r.K] = r.N
	}
	return out
}

// plansNodeRates returns each plan's per-node traffic rates.
func plansNodeRates(db *gorm.DB, ids []string) map[string]map[string]float64 {
	out := map[string]map[string]float64{}
	if len(ids) == 0 {
		return out
	}
	var rs []store.PlanNodeRate
	db.Where("plan_id IN ?", ids).Find(&rs)
	for _, r := range rs {
		if out[r.PlanID] == nil {
			out[r.PlanID] = map[string]float64{}
		}
		out[r.PlanID][r.NodeID] = r.Rate
	}
	return out
}
