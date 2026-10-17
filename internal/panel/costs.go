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

func (s *Server) costSettings() costSettings {
	cs := costSettings{Base: s.st.GetSetting("cost_base", "CNY"), RemindDays: atoiDefault(s.st.GetSetting("cost_remind_days", "7"), 7)}
	cs.Rates = map[string]float64{}
	if raw := s.st.GetSetting("cost_rates", ""); raw != "" {
		_ = json.Unmarshal([]byte(raw), &cs.Rates)
	}
	if len(cs.Rates) == 0 {
		// Defaults are CNY-based; rebase them when another base is chosen.
		b := defaultRates[cs.Base]
		if b == 0 {
			b = 1
		}
		for k, v := range defaultRates {
			cs.Rates[k] = v / b
		}
	}
	cs.Rates[cs.Base] = 1
	return cs
}

// toBase converts an amount into the base currency (0 when the rate is unknown).
func (cs costSettings) toBase(amount float64, cur string) (float64, bool) {
	if cur == "" || cur == cs.Base {
		return amount, true
	}
	r, ok := cs.Rates[cur]
	return amount * r, ok && r > 0
}

// addCycle moves a renewal date forward by one billing cycle.
func addCycle(t time.Time, cycle string) time.Time {
	switch cycle {
	case "quarter":
		return t.AddDate(0, 3, 0)
	case "halfyear":
		return t.AddDate(0, 6, 0)
	case "year":
		return t.AddDate(1, 0, 0)
	case "once":
		return t
	}
	return t.AddDate(0, 1, 0)
}

type costItem struct {
	store.NodeCost
	Name      string  `json:"name"`
	Flag      string  `json:"flag"`
	Monthly   float64 `json:"monthly"` // in base currency
	Converted bool    `json:"converted"`
	DaysLeft  *int    `json:"daysLeft"`
	Traffic30 int64   `json:"traffic30"` // server NIC bytes, last 30 days
	PerGB     float64 `json:"perGb"`     // monthly cost / 30-day traffic, base currency
}

func (s *Server) listCosts(c *gin.Context) {
	cs := s.costSettings()
	var nodes []store.Node
	s.st.DB.Select("id", "name", "flag", "sort_order").Order("sort_order, name").Find(&nodes)
	var costs []store.NodeCost
	s.st.DB.Find(&costs)
	byNode := map[string]store.NodeCost{}
	for _, nc := range costs {
		byNode[nc.NodeID] = nc
	}
	type sum struct {
		NodeID string
		Total  int64
	}
	var sums []sum
	s.st.DB.Model(&store.NodeTrafficDaily{}).Select("node_id, SUM(rx + tx) AS total").
		Where("day >= ?", time.Now().AddDate(0, 0, -29).Format("2006-01-02")).Group("node_id").Scan(&sums)
	traffic := map[string]int64{}
	for _, x := range sums {
		traffic[x.NodeID] = x.Total
	}
	now := time.Now()
	items := []costItem{}
	var monthly float64
	due := 0
	unconverted := []string{}
	for _, n := range nodes {
		nc, ok := byNode[n.ID]
		if !ok {
			nc = store.NodeCost{NodeID: n.ID, Currency: cs.Base, Cycle: "month"}
		}
		it := costItem{NodeCost: nc, Name: n.Name, Flag: n.Flag, Traffic30: traffic[n.ID]}
		if m := cycleMonths[nc.Cycle]; m > 0 && nc.Amount > 0 {
			v, conv := cs.toBase(nc.Amount/m, nc.Currency)
			it.Monthly, it.Converted = round2(v), conv
			if conv {
				monthly += v
			} else if !contains(unconverted, nc.Currency) {
				unconverted = append(unconverted, nc.Currency)
			}
		}
		if nc.ExpireAt != nil {
			d := int(math.Ceil(nc.ExpireAt.Sub(now).Hours() / 24))
			it.DaysLeft = &d
			if d <= cs.RemindDays && !nc.AutoRenew {
				due++
			}
		}
		if it.Monthly > 0 && it.Traffic30 > 0 {
			it.PerGB = round2(it.Monthly / (float64(it.Traffic30) / (1 << 30)))
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
	Provider  string  `json:"provider"`
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	Cycle     string  `json:"cycle"`
	ExpireAt  string  `json:"expireAt"` // YYYY-MM-DD, "" = none
	AutoRenew bool    `json:"autoRenew"`
	Note      string  `json:"note"`
}

func (s *Server) putNodeCost(c *gin.Context) {
	var n store.Node
	if err := s.st.DB.Select("id", "name").First(&n, "id = ?", c.Param("id")).Error; err != nil {
		dbError(c, err)
		return
	}
	var req costReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if req.Amount < 0 {
		badRequest(c, errors.New("价格不能为负数"))
		return
	}
	if _, ok := cycleMonths[req.Cycle]; !ok {
		badRequest(c, errors.New("付费周期应为 month、quarter、halfyear、year 或 once"))
		return
	}
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	if req.Currency == "" {
		req.Currency = s.costSettings().Base
	}
	nc := store.NodeCost{NodeID: n.ID, Provider: strings.TrimSpace(req.Provider), Amount: req.Amount, Currency: req.Currency,
		Cycle: req.Cycle, AutoRenew: req.AutoRenew, Note: req.Note}
	if req.ExpireAt != "" {
		t, err := time.ParseInLocation("2006-01-02", req.ExpireAt, time.Local)
		if err != nil {
			badRequest(c, errors.New("到期日格式应为 YYYY-MM-DD"))
			return
		}
		nc.ExpireAt = &t
	}
	var old store.NodeCost
	if s.st.DB.First(&old, "node_id = ?", n.ID).Error == nil && old.ExpireAt != nil && nc.ExpireAt != nil && old.ExpireAt.Equal(*nc.ExpireAt) {
		nc.WarnedFor = old.WarnedFor
	}
	if err := s.st.DB.Save(&nc).Error; err != nil {
		dbError(c, err)
		return
	}
	s.auditCtx(c, "node.cost", n.Name, fmt.Sprintf("%.2f %s / %s", nc.Amount, nc.Currency, nc.Cycle))
	c.JSON(http.StatusOK, nc)
}

// renewNodeCost records a renewal: the expiry moves forward one cycle.
func (s *Server) renewNodeCost(c *gin.Context) {
	var nc store.NodeCost
	if err := s.st.DB.First(&nc, "node_id = ?", c.Param("id")).Error; err != nil || nc.ExpireAt == nil {
		badRequest(c, errors.New("先设置到期日"))
		return
	}
	if nc.Cycle == "once" {
		badRequest(c, errors.New("一次性付费不需要续费"))
		return
	}
	t := addCycle(*nc.ExpireAt, nc.Cycle)
	s.st.DB.Model(&store.NodeCost{}).Where("node_id = ?", nc.NodeID).Updates(map[string]any{"expire_at": t, "warned_for": ""})
	s.auditCtx(c, "node.renew", nc.NodeID, t.Format("2006-01-02"))
	c.JSON(http.StatusOK, gin.H{"expireAt": t})
}

func (s *Server) putCostSettings(c *gin.Context) {
	var req costSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	req.Base = strings.ToUpper(strings.TrimSpace(req.Base))
	if len(req.Base) < 3 || len(req.Base) > 4 {
		badRequest(c, errors.New("本币代码应为 3 位，如 CNY、USD"))
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
		if v <= 0 {
			badRequest(c, fmt.Errorf("%s 的汇率应大于 0", k))
			return
		}
		rates[k] = v
	}
	rates[req.Base] = 1
	b, _ := json.Marshal(rates)
	s.st.SetSetting("cost_base", req.Base)
	s.st.SetSetting("cost_rates", string(b))
	s.st.SetSetting("cost_remind_days", fmt.Sprint(req.RemindDays))
	s.auditCtx(c, "cost.settings", req.Base, "")
	c.JSON(http.StatusOK, s.costSettings())
}

// costRenewals is the daily job: remind before manual renewals and roll
// auto-renewing servers forward once their date has passed.
func (s *Server) costRenewals() {
	cs := s.costSettings()
	now := time.Now()
	var costs []store.NodeCost
	s.st.DB.Where("expire_at IS NOT NULL AND cycle <> ?", "once").Find(&costs)
	for _, nc := range costs {
		var n store.Node
		if s.st.DB.Select("id", "name").First(&n, "id = ?", nc.NodeID).Error != nil {
			continue
		}
		exp := *nc.ExpireAt
		if nc.AutoRenew {
			if now.After(exp.Add(24 * time.Hour)) {
				for now.After(exp.Add(24 * time.Hour)) {
					exp = addCycle(exp, nc.Cycle)
				}
				s.st.DB.Model(&store.NodeCost{}).Where("node_id = ?", nc.NodeID).Updates(map[string]any{"expire_at": exp, "warned_for": ""})
			}
			continue
		}
		days := int(math.Ceil(exp.Sub(now).Hours() / 24))
		key := exp.Format("2006-01-02")
		if cs.RemindDays > 0 && days <= cs.RemindDays && nc.WarnedFor != key {
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
			s.notify(Event{Kind: "server_renewal", NodeID: n.ID, Text: text})
			s.st.DB.Model(&store.NodeCost{}).Where("node_id = ?", nc.NodeID).Update("warned_for", key)
		}
	}
}
