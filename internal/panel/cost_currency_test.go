package panel

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestCostCurrencySwitchKeepsNodePrices(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n", Name: "n"})
	if status, out := c.do("PUT", "/nodes/n/cost", map[string]any{"amount": 120, "currency": "USD", "cycle": "year"}); status != 200 {
		t.Fatalf("create cost: %d %v", status, out)
	}
	if status, out := c.do("PUT", "/costs/settings", map[string]any{"base": "USD", "rates": map[string]float64{"USD": 1, "CNY": 1 / 7.2, "EUR": 7.8 / 7.2}, "remindDays": 5}); status != 200 {
		t.Fatalf("switch base: %d %v", status, out)
	}
	status, out := c.do("GET", "/costs", nil)
	if status != 200 || out["total"].(map[string]any)["monthly"] != float64(10) {
		t.Fatalf("converted totals: %d %v", status, out)
	}
	item := out["items"].([]any)[0].(map[string]any)
	if item["currency"] != "USD" || item["amount"] != float64(120) || item["monthly"] != float64(10) {
		t.Fatal("base switch changed original node price", item)
	}
	bad := []map[string]any{
		{"base": "123", "rates": map[string]any{"USD": 1}, "remindDays": 5},
		{"base": "USD", "rates": map[string]any{"USD": 7.2}, "remindDays": 5},
		{"base": "USD", "rates": map[string]any{"CNY": -1}, "remindDays": 5},
		{"base": "USD", "rates": map[string]any{"CNY": 1, " cny ": 2}, "remindDays": 5},
		{"base": "USD", "rates": map[string]any{"C!Y": 1}, "remindDays": 5},
	}
	for _, req := range bad {
		if status, out := c.do("PUT", "/costs/settings", req); status != 400 {
			t.Fatalf("invalid settings accepted: %d %v", status, out)
		}
	}
	settings, err := s.costSettings()
	if err != nil || settings.Base != "USD" || settings.RemindDays != 5 || settings.Rates["CNY"] != 1/7.2 {
		t.Fatal("invalid request changed stored settings", settings, err)
	}
}

func TestCostSettingsWriteFailureRollsBack(t *testing.T) {
	s, _, c := newAdminClient(t)
	if err := s.st.SetSettings(map[string]string{"cost_base": "CNY", "cost_rates": `{"CNY":1,"USD":7.2}`, "cost_remind_days": "7"}); err != nil {
		t.Fatal(err)
	}
	// Saving cost_base succeeds first, then cost_rates fails.
	s.st.DB.Callback().Update().Before("gorm:update").Register("test:cost-setting-fail", func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*store.Setting); ok && row.Key == "cost_rates" {
			tx.AddError(errors.New("rates write failed"))
		}
	})
	status, out := c.do("PUT", "/costs/settings", map[string]any{"base": "USD", "rates": map[string]any{"CNY": 1 / 7.2}, "remindDays": 1})
	s.st.DB.Callback().Update().Remove("test:cost-setting-fail")
	if status != 500 {
		t.Fatalf("failed write acknowledged: %d %v", status, out)
	}
	cs, err := s.costSettings()
	if err != nil || cs.Base != "CNY" || cs.RemindDays != 7 || cs.Rates["USD"] != 7.2 {
		t.Fatal("partially saved cost settings", cs, err)
	}
}

func TestCostSettingsSnapshotAcrossInstances(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		configs := []map[string]string{
			{"cost_base": "CNY", "cost_rates": `{"CNY":1,"USD":7}`, "cost_remind_days": "7"},
			{"cost_base": "USD", "cost_rates": `{"USD":1,"CNY":0.125}`, "cost_remind_days": "1"},
		}
		if err := ss[0].st.SetSettings(configs[0]); err != nil {
			t.Fatal(err)
		}
		// A local cache of the previous currency must not split the snapshot.
		ss[1].st.GetSetting("cost_base", "")
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i, s := range ss {
			wg.Add(1)
			go func(s *Server, config map[string]string) {
				defer wg.Done()
				for j := 0; j < 15; j++ {
					if err := s.st.SetSettings(config); err != nil {
						errs <- err
						return
					}
				}
			}(s, configs[i])
		}
		for i := 0; i < 40; i++ {
			cs, err := ss[1].costSettings()
			if err != nil {
				t.Error(err)
				break
			}
			if !(cs.Base == "CNY" && cs.Rates["USD"] == 7 && cs.RemindDays == 7) && !(cs.Base == "USD" && cs.Rates["CNY"] == 0.125 && cs.RemindDays == 1) {
				t.Errorf("mixed cost configuration: %+v", cs)
				break
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})
}

func TestCostPrecisionAndCustomBase(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n", Name: "n"})
	s.st.DB.Create(&store.NodeCost{NodeID: "n", Amount: 1, Currency: "JPY", Cycle: "year"})
	// The monthly cost is 0.004 CNY; rounding it first used to erase it.
	cs, err := s.costSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.st.DB.Create(&store.NodeTrafficDaily{NodeID: "n", Day: time.Now().Format("2006-01-02"), Rx: 2 * gib})
	status, out := c.do("GET", "/costs", nil)
	if status != 200 {
		t.Fatal(status, out)
	}
	item := out["items"].([]any)[0].(map[string]any)
	monthly := cs.Rates["JPY"] / 12
	if math.Abs(item["monthly"].(float64)-monthly) > 1e-12 || math.Abs(item["perGb"].(float64)-monthly/2) > 1e-12 {
		t.Fatal("small recurring cost lost precision", item)
	}
	if err := s.st.SetSettings(map[string]string{"cost_base": "XYZ", "cost_rates": ""}); err != nil {
		t.Fatal(err)
	}
	cs, err = s.costSettings()
	if err != nil || len(cs.Rates) != 1 || cs.Rates["XYZ"] != 1 {
		t.Fatal("invented default exchange rates for custom base", cs, err)
	}
}

func TestCostReadFailuresAreNotZeroTotals(t *testing.T) {
	s, _, c := newAdminClient(t)
	for _, table := range []string{"settings", "nodes", "node_costs"} {
		s.st.DB.Callback().Query().Before("gorm:query").Register("test:cost-read-fail", func(tx *gorm.DB) {
			if tx.Statement.Table == table {
				tx.AddError(errors.New("cost query failed"))
			}
		})
		status, out := c.do("GET", "/costs", nil)
		s.st.DB.Callback().Query().Remove("test:cost-read-fail")
		if status != 500 {
			t.Fatalf("%s query failure looked like empty costs: %d %v", table, status, out)
		}
	}
	if err := s.st.SetSetting("cost_rates", "not JSON"); err != nil {
		t.Fatal(err)
	}
	if status, out := c.do("GET", "/costs", nil); status != 500 {
		t.Fatalf("malformed stored rates silently defaulted: %d %v", status, out)
	}
}
