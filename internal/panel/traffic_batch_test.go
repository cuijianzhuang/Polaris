package panel

import (
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// countWrites counts INSERT / UPDATE / DELETE / raw statements while fn runs.
func countWrites(t *testing.T, db *gorm.DB, fn func()) int64 {
	t.Helper()
	var n atomic.Int64
	var on atomic.Bool
	hook := func(*gorm.DB) {
		if on.Load() {
			n.Add(1)
		}
	}
	name := fmt.Sprintf("test:writes:%p", &n)
	cb := db.Callback()
	for _, err := range []error{
		cb.Create().After("gorm:create").Register(name, hook),
		cb.Update().After("gorm:update").Register(name, hook),
		cb.Delete().After("gorm:delete").Register(name, hook),
		cb.Raw().After("gorm:raw").Register(name, hook),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cb.Create().Remove(name)
		cb.Update().Remove(name)
		cb.Delete().Remove(name)
		cb.Raw().Remove(name)
	})
	on.Store(true)
	fn()
	on.Store(false)
	return n.Load()
}

// Settlement writes in batches: the number of statements does not grow with
// the number of active users, and every user still gets exactly their bytes,
// across batch boundaries and over repeated rounds in the same hour.
func TestSettleTrafficBatches(t *testing.T) {
	s := newTestServer(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "n1", TrafficRate: 1})
	db.Create(&store.Node{ID: "n2", Name: "n2", TrafficRate: 2})
	const users = 1200 // more than two batches
	var rows []store.User
	for i := 0; i < users; i++ {
		rows = append(rows, store.User{ID: fmt.Sprintf("u%04d", i), Username: fmt.Sprintf("u%04d", i),
			SubToken: fmt.Sprintf("s%04d", i), UUID: fmt.Sprintf("uuid-%04d", i)})
	}
	if err := db.CreateInBatches(rows, 300).Error; err != nil {
		t.Fatal(err)
	}
	round := func(n int) int64 {
		var ds []model.TrafficDelta
		for i := 0; i < n; i++ {
			ds = append(ds, model.TrafficDelta{UserID: fmt.Sprintf("u%04d", i), Up: int64(i + 1), Down: 10})
		}
		s.traffic.Add("n1", ds)
		s.traffic.Add("n2", ds)
		return countWrites(t, db, func() {
			if err := s.settleTraffic(); err != nil {
				t.Fatal(err)
			}
		})
	}
	small := round(50)
	big := round(users)
	// 1200 users span three batches per table instead of one: allow that,
	// but nothing per user (that would be thousands of statements).
	t.Logf("writes: %d for 50 users, %d for %d users", small, big, users)
	if big > small+20 {
		t.Fatalf("writes grew with users: %d for 50, %d for %d", small, big, users)
	}
	var us []store.User
	db.Order("id").Find(&us)
	for i, u := range us {
		up := int64(i+1) * 3 // node rates 1 + 2
		down := int64(10 * 3)
		if i < 50 {
			up, down = up*2, down*2 // in both rounds
		}
		if u.UsedUp != up || u.UsedDown != down || u.OnlineAt == nil {
			t.Fatalf("%s used %d/%d online=%v, want %d/%d", u.ID, u.UsedUp, u.UsedDown, u.OnlineAt != nil, up, down)
		}
	}
	var h store.TrafficHourly
	db.First(&h, "user_id = ? AND node_id = ?", "u0007", "n2")
	if h.Up != 2*8*2 || h.RawUp != 8*2 || h.AccountUp != 2*8*2 || h.RateMin != 2 || h.RateMax != 2 {
		t.Fatalf("hourly u0007/n2 = %+v", h)
	}
	var d store.TrafficDaily
	db.First(&d, "user_id = ? AND node_id = ?", "u0999", "n1")
	if d.Up != 1000 || d.Down != 10 {
		t.Fatalf("daily u0999/n1 = %+v", d)
	}
}

// A batch where one row was already taken by another instance is refused as
// a whole, and the transaction leaves every row as it was.
func TestTakeInboxRefusesPartlyTakenBatch(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		s, _ := newTrafficServers(t, pgDSN)
		if err := s.traffic.Add("n1", []model.TrafficDelta{{UserID: "u1", Up: 10, Down: 1}, {UserID: "u2", Up: 20, Down: 2}, {UserID: "u3", Up: 30, Down: 3}}); err != nil {
			t.Fatal(err)
		}
		snap, err := s.traffic.Drain()
		if err != nil {
			t.Fatal(err)
		}
		s.st.DB.Model(&store.TrafficInbox{}).Where("user_id = ?", "u2").Update("up", 5) // settled elsewhere
		if err := s.st.DB.Transaction(func(tx *gorm.DB) error { return takeInbox(tx, snap) }); err != errInboxTaken {
			t.Fatalf("got %v, want errInboxTaken", err)
		}
		var rows []store.TrafficInbox
		s.st.DB.Order("user_id").Find(&rows)
		got := fmt.Sprint(len(rows))
		for _, r := range rows {
			got += fmt.Sprintf(" %s:%d/%d", r.UserID, r.Up, r.Down)
		}
		if got != "3 u1:10/1 u2:5/2 u3:30/3" {
			t.Fatalf("inbox after refused take = %s", got)
		}
	})
}
