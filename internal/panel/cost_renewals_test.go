package panel

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func costTestDate(t *testing.T, date string) time.Time {
	t.Helper()
	out, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCostRenewalCalendar(t *testing.T) {
	for _, tc := range []struct {
		cycle, from, first, second string
	}{
		{"month", "2026-01-31", "2026-02-28", "2026-03-31"},
		{"month", "2024-01-31", "2024-02-29", "2024-03-31"},
		{"month", "2026-01-30", "2026-02-28", "2026-03-30"},
		{"quarter", "2026-01-31", "2026-04-30", "2026-07-31"},
		{"halfyear", "2026-08-31", "2027-02-28", "2027-08-31"},
		{"year", "2024-02-29", "2025-02-28", "2026-02-28"},
	} {
		t.Run(tc.cycle+tc.from, func(t *testing.T) {
			from := costTestDate(t, tc.from)
			first, err := addCostCycles(from, tc.cycle, from.Day(), 1)
			if err != nil || first.Format("2006-01-02") != tc.first {
				t.Fatalf("first renewal: %s %v", first, err)
			}
			second, err := addCostCycles(first, tc.cycle, from.Day(), 1)
			if err != nil || second.Format("2006-01-02") != tc.second {
				t.Fatalf("second renewal: %s %v", second, err)
			}
		})
	}
	leap, err := addCostCycles(costTestDate(t, "2024-02-29"), "year", 29, 4)
	if err != nil || leap.Format("2006-01-02") != "2028-02-29" {
		t.Fatalf("leap day anchor lost: %v %v", leap, err)
	}
	for _, cycle := range []string{"once", "invalid"} {
		if _, err := addCostCycles(costTestDate(t, "2026-01-01"), cycle, 1, 1); err == nil {
			t.Fatal("invalid renewal accepted", cycle)
		}
	}
	if _, err := addCostCycles(costTestDate(t, "9999-12-31"), "month", 31, 1); err == nil {
		t.Fatal("out of range renewal accepted")
	}
}

func TestCostRenewalCatchUp(t *testing.T) {
	for _, tc := range []struct {
		from, now, want string
	}{
		{"2026-01-31", "2026-01-31", "2026-01-31"},
		{"2026-01-31", "2026-02-01", "2026-02-28"},
		{"2026-01-31", "2026-03-10", "2026-03-31"},
		{"2026-01-31", "2026-03-31", "2026-03-31"},
		{"2026-01-31", "2026-04-01", "2026-04-30"},
	} {
		exp := costTestDate(t, tc.from)
		nc := store.NodeCost{ExpireAt: &exp, Cycle: "month", AutoRenew: true}
		changed, err := catchUpCostExpiry(&nc, costTestDate(t, tc.now))
		if err != nil || nc.ExpireAt.Format("2006-01-02") != tc.want || changed != (tc.from != tc.want) {
			t.Fatalf("catch up from %s at %s: %v %v changed=%v", tc.from, tc.now, nc.ExpireAt, err, changed)
		}
	}
	// Expiry is valid through its whole calendar day, including DST changes.
	for _, date := range []string{"2026-03-08", "2026-11-01"} {
		exp := costTestDate(t, date)
		end := exp.AddDate(0, 0, 1)
		nc := store.NodeCost{ExpireAt: &exp, Cycle: "month", AutoRenew: true}
		if changed, err := catchUpCostExpiry(&nc, end.Add(-time.Nanosecond)); err != nil || changed {
			t.Fatal("auto-renewed before calendar expiry", date, err)
		}
		if changed, err := catchUpCostExpiry(&nc, end); err != nil || !changed {
			t.Fatal("did not renew after calendar expiry", date, err)
		}
		if got := costDaysLeft(exp, end.Add(time.Minute)); got != -1 {
			t.Fatal("DST changed overdue day count", date, got)
		}
	}
}

func TestCostConcurrentManualRenewals(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		exp := costTestDate(t, "2026-01-31")
		ss[0].st.DB.Create(&store.Node{ID: "n", Name: "n"})
		ss[0].st.DB.Create(&store.NodeCost{NodeID: "n", Amount: 60, Currency: "USD", Cycle: "month", ExpireAt: &exp})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, s := range ss {
			wg.Add(1)
			go func(s *Server) {
				defer wg.Done()
				_, err := s.renewCost("n")
				errs <- err
			}(s)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		var nc store.NodeCost
		ss[0].st.DB.First(&nc, "node_id = ?", "n")
		if nc.ExpireAt == nil || costDate(*nc.ExpireAt).Format("2006-01-02") != "2026-03-31" || nc.RenewalDay != 31 || nc.Revision != 2 || nc.Amount != 60 || nc.Currency != "USD" {
			t.Fatalf("concurrent renewal lost a period: %+v", nc)
		}
	})
}

func TestCostConcurrentAutomaticRenewalAndReminder(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		exp, soon := costTestDate(t, "2026-01-31"), costTestDate(t, "2026-03-16")
		for _, id := range []string{"auto", "manual"} {
			ss[0].st.DB.Create(&store.Node{ID: id, Name: id})
		}
		ss[0].st.DB.Create(&store.NodeCost{NodeID: "auto", Cycle: "month", ExpireAt: &exp, AutoRenew: true})
		ss[0].st.DB.Create(&store.NodeCost{NodeID: "manual", Cycle: "month", ExpireAt: &soon})
		watch := []chan Event{events(ss[0]), events(ss[1])}
		var wg sync.WaitGroup
		for _, s := range ss {
			wg.Add(1)
			go func(s *Server) {
				defer wg.Done()
				s.costRenewalsAt(costTestDate(t, "2026-03-15"))
			}(s)
		}
		wg.Wait()
		var nc store.NodeCost
		ss[0].st.DB.First(&nc, "node_id = ?", "auto")
		if nc.ExpireAt == nil || costDate(*nc.ExpireAt).Format("2006-01-02") != "2026-03-31" || nc.RenewalDay != 31 || nc.Revision != 1 {
			t.Fatalf("auto renewal skipped or duplicated a period: %+v", nc)
		}
		count := 0
		for _, ch := range watch {
			for len(ch) > 0 {
				if ev := <-ch; ev.Kind == "server_renewal" && ev.NodeID == "manual" {
					count++
				}
			}
		}
		if count != 1 {
			t.Fatal("concurrent reminder count", count)
		}
	})
}

func TestCostStaleEditorCannotUndoRenewal(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n", Name: "n"})
	body := map[string]any{"amount": 60, "currency": "USD", "cycle": "month", "expireAt": "2026-01-31", "expectedRevision": 0}
	if status, out := c.do("PUT", "/nodes/n/cost", body); status != 200 || out["revision"] != float64(1) {
		t.Fatal(status, out)
	}
	body["expectedRevision"] = 1
	if status, out := c.do("POST", "/nodes/n/renew", nil); status != 200 || out["revision"] != float64(2) {
		t.Fatal(status, out)
	}
	body["note"] = "stale edit"
	if status, out := c.do("PUT", "/nodes/n/cost", body); status != 409 {
		t.Fatalf("stale form undid renewal: %d %v", status, out)
	}
	var nc store.NodeCost
	s.st.DB.First(&nc, "node_id = ?", "n")
	if nc.Revision != 2 || nc.Note != "" || costDate(*nc.ExpireAt).Format("2006-01-02") != "2026-02-28" {
		t.Fatal("conflict changed state", nc)
	}
	// A refreshed editor keeps the original 31st anchor when the date is unchanged.
	body["expectedRevision"], body["expireAt"] = 2, "2026-02-28"
	if status, out := c.do("PUT", "/nodes/n/cost", body); status != 200 {
		t.Fatal(status, out)
	}
	if _, err := s.renewCost("n"); err != nil {
		t.Fatal(err)
	}
	s.st.DB.First(&nc, "node_id = ?", "n")
	if costDate(*nc.ExpireAt).Format("2006-01-02") != "2026-03-31" {
		t.Fatal("editing note lost billing day", nc)
	}
	// Explicitly changing expiry establishes the new billing day.
	body["expectedRevision"], body["expireAt"] = 4, "2026-04-27"
	if status, out := c.do("PUT", "/nodes/n/cost", body); status != 200 {
		t.Fatal(status, out)
	}
	if _, err := s.renewCost("n"); err != nil {
		t.Fatal(err)
	}
	s.st.DB.First(&nc, "node_id = ?", "n")
	if nc.RenewalDay != 27 || costDate(*nc.ExpireAt).Format("2006-01-02") != "2026-05-27" {
		t.Fatal("explicit expiry did not change billing day", nc)
	}
}

func TestCostRenewalWriteFailureIsNotSuccess(t *testing.T) {
	s, _, c := newAdminClient(t)
	exp := costTestDate(t, "2026-01-31")
	for _, id := range []string{"auto", "manual"} {
		s.st.DB.Create(&store.Node{ID: id, Name: id})
		s.st.DB.Create(&store.NodeCost{NodeID: id, Cycle: "month", ExpireAt: &exp, AutoRenew: id == "auto"})
	}
	watch := events(s)
	s.st.DB.Callback().Update().Before("gorm:update").Register("test:renewal-fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "node_costs" {
			tx.AddError(errors.New("renewal write failed"))
		}
	})
	if status, out := c.do("POST", "/nodes/manual/renew", nil); status != 500 {
		t.Fatalf("failed renewal acknowledged: %d %v", status, out)
	}
	s.costRenewalsAt(costTestDate(t, "2026-02-01"))
	s.st.DB.Callback().Update().Remove("test:renewal-fail")
	var rows []store.NodeCost
	s.st.DB.Find(&rows)
	for _, nc := range rows {
		if nc.Revision != 0 || nc.RenewalDay != 0 || nc.WarnedFor != "" || !costDate(*nc.ExpireAt).Equal(exp) {
			t.Fatal("failed operation changed state", nc)
		}
	}
	select {
	case ev := <-watch:
		t.Fatal("failed reminder sent alert", ev)
	default:
	}
	if status, out := c.do("POST", "/nodes/manual/renew", nil); status != 200 {
		t.Fatalf("retry failed: %d %v", status, out)
	}
}
