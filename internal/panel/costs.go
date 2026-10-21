package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Server cost bookkeeping: what each node's server costs, in which currency
// and cycle, when it renews; totals in one base currency, cost per GB of
// server traffic, renewal reminders, and auto-renewing servers rolled
// forward on their renewal day.

var cycleMonths = map[string]float64{"month": 1, "quarter": 3, "halfyear": 6, "year": 12, "once": 0}

// defaultRates convert one unit of a currency into CNY.
var defaultRates = map[string]float64{"CNY": 1, "USD": 7.2, "EUR": 7.8, "GBP": 9.1, "HKD": 0.92, "JPY": 0.048, "SGD": 5.4, "TWD": 0.23, "KRW": 0.0053, "RUB": 0.08}

type costSettings struct {
	Base       string             `json:"base"`
	Rates      map[string]float64 `json:"rates"` // 1 unit of currency = rate × base
	RemindDays int                `json:"remindDays"`
}

func (s *Server) costSettings() (costSettings, error) {
	// One SELECT observes the same committed version of all three settings.
	var rows []store.Setting
	if err := s.st.DB.Where("key IN ?", []string{"cost_base", "cost_rates", "cost_remind_days"}).Find(&rows).Error; err != nil {
		return costSettings{}, err
	}
	values := map[string]string{}
	for _, row := range rows {
		values[row.Key] = row.Value
	}
	cs := costSettings{Base: orDefault(values["cost_base"], "CNY"), RemindDays: atoiDefault(values["cost_remind_days"], 7), Rates: map[string]float64{}}
	if raw := values["cost_rates"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &cs.Rates); err != nil {
			return cs, fmt.Errorf("invalid stored cost exchange rates: %w", err)
		}
	}
	if len(cs.Rates) == 0 {
		cs.Rates = map[string]float64{}
		// Unknown custom bases have no implicit conversion from CNY.
		if b := defaultRates[cs.Base]; b > 0 {
			for k, v := range defaultRates {
				cs.Rates[k] = v / b
			}
		}
	}
	cs.Rates[cs.Base] = 1
	return cs, nil
}

func validCurrency(code string) bool {
	if len(code) < 3 || len(code) > 4 {
		return false
	}
	for _, ch := range code {
		if ch < 'A' || ch > 'Z' {
			return false
		}
	}
	return true
}

// toBase converts an amount into the base currency (0 when the rate is unknown).
func (cs costSettings) toBase(amount float64, cur string) (float64, bool) {
	if cur == "" || cur == cs.Base {
		return amount, true
	}
	r, ok := cs.Rates[cur]
	return amount * r, ok && r > 0
}

type costItem struct {
	store.NodeCost
	Name              string  `json:"name"`
	Flag              string  `json:"flag"`
	Monthly           float64 `json:"monthly"` // in base currency
	Converted         bool    `json:"converted"`
	DaysLeft          *int    `json:"daysLeft"`
	Traffic30         int64   `json:"traffic30"` // server NIC bytes, last 30 days
	TrafficRx30       int64   `json:"trafficRx30"`
	TrafficTx30       int64   `json:"trafficTx30"`
	TrafficMode       string  `json:"trafficMode"`
	TrafficLegacyDays int     `json:"trafficLegacyDays"`
	TrafficDays       int     `json:"trafficDays"`
	PerGB             float64 `json:"perGb"` // monthly cost / 30-day traffic, base currency
}

func (s *Server) listCosts(c *gin.Context) {
	cs, err := s.costSettings()
	if err != nil {
		serverError(c, err)
		return
	}
	var nodes []store.Node
	if err := s.st.DB.Select("id", "name", "flag", "sort_order", "net_mode").Order("sort_order, name").Find(&nodes).Error; err != nil {
		serverError(c, err)
		return
	}
	var costs []store.NodeCost
	if err := s.st.DB.Find(&costs).Error; err != nil {
		serverError(c, err)
		return
	}
	byNode := map[string]store.NodeCost{}
	for _, nc := range costs {
		byNode[nc.NodeID] = nc
	}
	type sum struct {
		NodeID     string
		Rx         int64
		Tx         int64
		Days       int
		LegacyDays int
	}
	var sums []sum
	now := time.Now()
	if e := s.st.DB.Model(&store.NodeTrafficDaily{}).Select("node_id, SUM(rx) AS rx, SUM(tx) AS tx, COUNT(*) AS days, SUM(CASE WHEN accounting_version = 1 THEN 0 ELSE 1 END) AS legacy_days").
		Where("day >= ? AND day <= ?", now.AddDate(0, 0, -29).Format("2006-01-02"), now.Format("2006-01-02")).Group("node_id").Scan(&sums).Error; e != nil {
		serverError(c, e)
		return
	}
	traffic := map[string]sum{}
	for _, x := range sums {
		traffic[x.NodeID] = x
	}
	items := []costItem{}
	var monthly float64
	due := 0
	unconverted := []string{}
	for _, n := range nodes {
		nc, ok := byNode[n.ID]
		if !ok {
			nc = store.NodeCost{NodeID: n.ID, Currency: cs.Base, Cycle: "month"}
		}
		if nc.ExpireAt != nil {
			exp := costDate(*nc.ExpireAt)
			nc.ExpireAt = &exp
		}
		tr := traffic[n.ID]
		mode := orDefault(n.NetMode, "sum")
		it := costItem{NodeCost: nc, Name: n.Name, Flag: n.Flag, Traffic30: nodeNetBytes(mode, tr.Rx, tr.Tx), TrafficRx30: tr.Rx, TrafficTx30: tr.Tx, TrafficMode: mode, TrafficDays: tr.Days, TrafficLegacyDays: tr.LegacyDays}
		if m := cycleMonths[nc.Cycle]; m > 0 && nc.Amount > 0 {
			v, conv := cs.toBase(nc.Amount/m, nc.Currency)
			it.Monthly, it.Converted = v, conv
			if conv {
				monthly += v
			} else if !contains(unconverted, nc.Currency) {
				unconverted = append(unconverted, nc.Currency)
			}
		}
		if nc.ExpireAt != nil {
			d := costDaysLeft(*nc.ExpireAt, now)
			it.DaysLeft = &d
			if d <= cs.RemindDays && !nc.AutoRenew {
				due++
			}
		}
		if it.Monthly > 0 && it.Traffic30 > 0 {
			it.PerGB = it.Monthly / (float64(it.Traffic30) / (1 << 30))
		}
		items = append(items, it)
	}
	sort.Strings(unconverted)
	c.JSON(http.StatusOK, gin.H{
		"settings": cs, "items": items,
		"total": gin.H{"monthly": round2(monthly), "yearly": round2(monthly * 12), "dueSoon": due, "unconverted": unconverted},
	})
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

type costReq struct {
	Provider         string  `json:"provider"`
	Amount           float64 `json:"amount"`
	Currency         string  `json:"currency"`
	Cycle            string  `json:"cycle"`
	ExpireAt         string  `json:"expireAt"` // YYYY-MM-DD, "" = none
	AutoRenew        bool    `json:"autoRenew"`
	Note             string  `json:"note"`
	ExpectedRevision *uint64 `json:"expectedRevision"`
}

func (s *Server) putNodeCost(c *gin.Context) {
	var req costReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Amount < 0 || math.IsNaN(req.Amount) || math.IsInf(req.Amount, 0) {
		badRequest(c, errors.New("价格不能为负数"))
		return
	}
	if _, ok := cycleMonths[req.Cycle]; !ok {
		badRequest(c, errors.New("付费周期应为 month、quarter、halfyear、year 或 once"))
		return
	}
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	if req.Currency == "" {
		cs, err := s.costSettings()
		if err != nil {
			serverError(c, err)
			return
		}
		req.Currency = cs.Base
	}
	if !validCurrency(req.Currency) {
		badRequest(c, errors.New("币种代码应为 3–4 位英文字母，如 CNY、USD、USDT"))
		return
	}
	nc := store.NodeCost{NodeID: c.Param("id"), Provider: strings.TrimSpace(req.Provider), Amount: req.Amount, Currency: req.Currency,
		Cycle: req.Cycle, AutoRenew: req.AutoRenew, Note: req.Note}
	if req.ExpireAt != "" {
		t, err := time.ParseInLocation("2006-01-02", req.ExpireAt, time.Local)
		if err != nil || t.Year() < 1 {
			badRequest(c, errors.New("到期日格式应为 YYYY-MM-DD"))
			return
		}
		nc.ExpireAt = &t
	}
	var name string
	err := s.withNodeCost(nc.NodeID, func(db *gorm.DB, n *store.Node, old *store.NodeCost) error {
		if req.ExpectedRevision != nil && *req.ExpectedRevision != old.Revision {
			return errCostChanged
		}
		name, nc.Revision = n.Name, old.Revision
		if nc.ExpireAt != nil {
			nc.RenewalDay = nc.ExpireAt.Day()
			if old.ExpireAt != nil && costDate(*old.ExpireAt).Equal(*nc.ExpireAt) {
				nc.WarnedFor, nc.RenewalDay = old.WarnedFor, costRenewalDay(old)
			}
		}
		return saveNodeCost(db, &nc)
	})
	if errors.Is(err, errCostChanged) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "node.cost", name, fmt.Sprintf("%.2f %s / %s", nc.Amount, nc.Currency, nc.Cycle))
	c.JSON(http.StatusOK, nc)
}

// renewNodeCost records a renewal: the expiry moves forward one cycle.
func (s *Server) renewNodeCost(c *gin.Context) {
	nc, err := s.renewCost(c.Param("id"))
	if err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "node.renew", nc.NodeID, nc.ExpireAt.Format("2006-01-02"))
	c.JSON(http.StatusOK, gin.H{"expireAt": nc.ExpireAt, "revision": nc.Revision})
}

func (s *Server) putCostSettings(c *gin.Context) {
	var req costSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	req.Base = strings.ToUpper(strings.TrimSpace(req.Base))
	if !validCurrency(req.Base) {
		badRequest(c, errors.New("本币代码应为 3–4 位英文字母，如 CNY、USD、USDT"))
		return
	}
	if req.RemindDays < 0 || req.RemindDays > 90 {
		badRequest(c, errors.New("提前提醒天数应在 0–90 之间"))
		return
	}
	rates := map[string]float64{}
	for k, v := range req.Rates {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if !validCurrency(k) || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			badRequest(c, fmt.Errorf("%s 的币种或汇率无效", k))
			return
		}
		if _, exists := rates[k]; exists {
			badRequest(c, fmt.Errorf("%s 的汇率重复", k))
			return
		}
		if k == req.Base && v != 1 {
			badRequest(c, errors.New("本币自身的汇率必须为 1，请先换算其他汇率"))
			return
		}
		rates[k] = v
	}
	rates[req.Base] = 1
	b, err := json.Marshal(rates)
	if err != nil {
		badRequest(c, err)
		return
	}
	if err := s.st.SetSettings(map[string]string{"cost_base": req.Base, "cost_rates": string(b), "cost_remind_days": fmt.Sprint(req.RemindDays)}); err != nil {
		serverError(c, err)
		return
	}
	s.auditCtx(c, "cost.settings", req.Base, "")
	req.Rates = rates
	c.JSON(http.StatusOK, req)
}

// costRenewals is the daily job: remind before manual renewals and roll
// auto-renewing servers forward once their date has passed.
func (s *Server) costRenewals() {
	s.costRenewalsAt(time.Now())
}

func (s *Server) costRenewalsAt(now time.Time) {
	cs, err := s.costSettings()
	if err != nil {
		s.log.Error("read server cost settings", "err", err)
		return
	}
	var costs []store.NodeCost
	if err := s.st.DB.Select("node_id").Where("expire_at IS NOT NULL AND cycle <> ?", "once").Find(&costs).Error; err != nil {
		s.log.Error("list server renewals", "err", err)
		return
	}
	for _, candidate := range costs {
		var alert *Event
		err := s.withNodeCost(candidate.NodeID, func(db *gorm.DB, n *store.Node, nc *store.NodeCost) error {
			// Settings and expiry may have changed since the candidate scan.
			if nc.ExpireAt == nil || nc.Cycle == "once" {
				return nil
			}
			if nc.AutoRenew {
				changed, err := catchUpCostExpiry(nc, now)
				if err != nil || !changed {
					return err
				}
				return saveNodeCost(db, nc)
			}
			exp := costDate(*nc.ExpireAt)
			days := costDaysLeft(exp, now)
			key := exp.Format("2006-01-02")
			if cs.RemindDays <= 0 || days > cs.RemindDays || nc.WarnedFor == key {
				return nil
			}
			price := ""
			if nc.Amount > 0 {
				price = fmt.Sprintf("，续费 %.2f %s", nc.Amount, nc.Currency)
			}
			who := ""
			if nc.Provider != "" {
				who = "（" + nc.Provider + "）"
			}
			var text string
			if days < 0 {
				text = fmt.Sprintf("⌛ 节点 %s%s 的服务器已于 %s 到期%s", n.Name, who, key, price)
			} else {
				text = fmt.Sprintf("💳 节点 %s%s 的服务器将于 %s 到期（还剩 %d 天）%s", n.Name, who, key, days, price)
			}
			if err := db.Model(nc).Update("warned_for", key).Error; err != nil {
				return err
			}
			alert = &Event{Kind: "server_renewal", NodeID: n.ID, Text: text}
			return nil
		})
		if err != nil {
			s.log.Error("process server renewal", "node", candidate.NodeID, "err", err)
			continue
		}
		if alert != nil {
			s.notify(*alert)
		}
	}
}
