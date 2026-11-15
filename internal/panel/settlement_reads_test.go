package panel

import (
	"errors"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestSettlementReadFailureRetainsBillingForRetry(t *testing.T) {
	for _, tc := range []struct {
		name, table, columns string
	}{
		{"node rates", "nodes", "id,traffic_rate"},
		{"node quota packs", "user_node_quota", ""},
		{"forward rules", "forward_rules", "id,user_id"},
		{"forward owner rates", "users", "id,fwd_rate"},
		{"first-use expiry", "users", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			db := s.st.DB
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(db.Create(&store.User{ID: "u", Username: "reader", UUID: "u", SubToken: "u", Status: "active", ExpireAfterDays: 7,
				ForwardEntitlement: store.ForwardEntitlement{FwdRate: 3}}).Error)
			must(db.Create(&store.Node{ID: "n", Name: "rate-two", TrafficRate: 2}).Error)
			must(db.Create(&store.UserNodeQuota{UserID: "u", NodeID: "n", Extra: 50}).Error)
			must(db.Create(&store.ForwardRule{ID: "f", NodeID: "n", UserID: "u"}).Error)
			must(s.traffic.Add("n", []model.TrafficDelta{
				{UserID: "u", Up: 10, Down: 20},
				{UserID: "inbound>>>fwd-f", Up: 5, Down: 7},
			}))

			// Resolve the quota table from the model: GORM's pluralisation of
			// "quota" is dialect/config dependent.
			table := tc.table
			if table == "user_node_quota" {
				stmt := &gorm.Statement{DB: db}
				must(stmt.Parse(&store.UserNodeQuota{}))
				table = stmt.Schema.Table
			}
			failure := errors.New("injected settlement read failure")
			const callback = "test:settlement-read-failure"
			injected := false
			must(db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table && strings.Join(tx.Statement.Selects, ",") == tc.columns {
					injected = true
					tx.AddError(failure)
				}
			}))
			t.Cleanup(func() { db.Callback().Query().Remove(callback) })
			enforced, err := s.settle()
			db.Callback().Query().Remove(callback)
			if !injected || !errors.Is(err, failure) || enforced {
				t.Fatalf("read failure ignored: injected=%v enforced=%v err=%v", injected, enforced, err)
			}
			pending, err := s.traffic.Drain()
			must(err)
			if pending["n"]["u"] != [2]int64{10, 20} || pending["n"]["inbound>>>fwd-f"] != [2]int64{5, 7} {
				t.Fatalf("pending traffic lost: %+v", pending)
			}
			var u store.User
			var quota store.UserNodeQuota
			must(db.First(&u, "id = ?", "u").Error)
			must(db.First(&quota, "user_id = ? AND node_id = ?", "u", "n").Error)
			if u.Used() != 0 || u.FwdUsed != 0 || u.ExpireAt != nil || quota.Used != 0 || quota.ExtraUsed != 0 {
				t.Fatalf("failed round committed billing: user=%+v quota=%+v", u, quota)
			}
			for _, row := range []any{&store.TrafficHourly{}, &store.TrafficDaily{}, &store.TagTraffic{}, &store.TagTrafficDaily{}} {
				var n int64
				must(db.Model(row).Count(&n).Error)
				if n != 0 {
					t.Fatalf("failed round committed %T rows: %d", row, n)
				}
			}

			// Retry, then run an empty round: neither charge nor provenance
			// may be lost or billed twice.
			must(s.settleTraffic())
			must(s.settleTraffic())
			must(db.First(&u, "id = ?", "u").Error)
			must(db.First(&quota, "user_id = ? AND node_id = ?", "u", "n").Error)
			if u.Used() != 10 || u.FwdUsed != 72 || u.ExpireAt == nil || u.ExpireAfterDays != 0 || quota.Used != 60 || quota.ExtraUsed != 50 {
				t.Fatalf("retry billed incorrectly: user=%+v quota=%+v", u, quota)
			}
			var hourly store.TrafficHourly
			must(db.First(&hourly, "user_id = ?", "u").Error)
			if hourly.RawUp != 10 || hourly.RawDown != 20 || hourly.Up != 20 || hourly.Down != 40 || hourly.AccountUp != 4 || hourly.AccountDown != 6 {
				t.Fatalf("retry duplicated provenance: %+v", hourly)
			}
			var tag store.TagTraffic
			var tagDay store.TagTrafficDaily
			var daily store.TrafficDaily
			must(db.First(&tag).Error)
			must(db.First(&tagDay).Error)
			must(db.First(&daily).Error)
			if tag.Up != 5 || tag.Down != 7 || tagDay.Up != 5 || tagDay.Down != 7 || daily.Up != 20 || daily.Down != 40 {
				t.Fatalf("retry duplicated daily/tag totals: tag=%+v tagDay=%+v daily=%+v", tag, tagDay, daily)
			}
			pending, err = s.traffic.Drain()
			must(err)
			if len(pending) != 0 {
				t.Fatalf("successful retry left pending traffic: %+v", pending)
			}
		})
	}
}
