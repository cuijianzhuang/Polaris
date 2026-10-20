package panel

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func recoveryOrder(t *testing.T, s *Server, id, user, method string) *store.Order {
	t.Helper()
	p := store.Group{ID: "recovery-plan", Name: "recovery-plan", Plan: true, Days: 30, TrafficLimit: 100 << 30}
	if e := s.st.DB.Where("id = ?", p.ID).FirstOrCreate(&p).Error; e != nil {
		t.Fatal(e)
	}
	if user != "" {
		exp := time.Now().Add(24 * time.Hour)
		u := store.User{ID: user, Username: user, UUID: user, SubToken: user, Status: "active", PlanID: p.ID, ExpireAt: &exp, TrafficLimit: 100 << 30, TelegramID: 700}
		if e := s.st.DB.Where("id = ?", user).FirstOrCreate(&u).Error; e != nil {
			t.Fatal(e)
		}
	}
	now := time.Now()
	o := &store.Order{ID: id, UserID: user, TelegramID: 700, PlanID: p.ID, PlanName: p.Name, Method: method, Amount: "15.00", Currency: "CNY", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Entitlements: freezePlan(&p)}
	if method == payStars {
		o.Amount, o.Currency, o.StarsBotID = "10", "XTR", 1
	}
	if e := s.st.DB.Create(o).Error; e != nil {
		t.Fatal(e)
	}
	return o
}

func TestPaymentReceiptSurvivesDeliveryFailure(t *testing.T) {
	s := newTestServer(t)
	o := recoveryOrder(t, s, "receipt", "u", payEPay)
	s.st.DB.Callback().Create().Before("gorm:create").Register("test:receipt-write", func(tx *gorm.DB) {
		if tx.Statement.Table == "payment_receipts" {
			tx.AddError(fmt.Errorf("storage unavailable"))
		}
	})
	if e := s.acceptPayment(o, "epay:receipt"); e == nil {
		t.Fatal("acknowledged an uncommitted receipt")
	}
	var current store.Order
	s.st.DB.First(&current, "id = ?", o.ID)
	if current.Status != "pending" {
		t.Fatal("failed receipt changed order", current.Status)
	}
	s.st.DB.Callback().Create().Remove("test:receipt-write")
	s.st.DB.Callback().Update().Before("gorm:update").Register("test:delivery-write", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(fmt.Errorf("temporary delivery failure"))
		}
	})
	if e := s.acceptPayment(o, "epay:receipt"); e != nil {
		t.Fatal(e)
	}
	var p store.PaymentReceipt
	s.st.DB.First(&p, "ref = ?", "epay:receipt")
	s.st.DB.First(&current, "id = ?", o.ID)
	if p.Status != "pending" || current.Status != "processing" {
		t.Fatalf("receipt/order: %+v %+v", p, current)
	}
	s.st.DB.Callback().Update().Remove("test:delivery-write")
	s.st.DB.Model(&p).Update("next_at", time.Now().Add(-time.Second))
	s.retryPayments()
	s.st.DB.First(&p, "ref = ?", p.Ref)
	s.st.DB.First(&current, "id = ?", o.ID)
	if p.Status != "done" || current.Status != "paid" {
		t.Fatalf("not recovered: %+v %+v", p, current)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "u")
	expiry := *u.ExpireAt
	if e := s.acceptPayment(o, p.Ref); e != nil {
		t.Fatal(e)
	}
	s.st.DB.First(&u, "id = ?", "u")
	if !u.ExpireAt.Equal(expiry) {
		t.Fatal("receipt replay renewed twice")
	}
	if e := s.acceptPayment(o, "epay:extra"); e != nil {
		t.Fatal(e)
	}
	var extra store.PaymentReceipt
	s.st.DB.First(&extra, "ref = ?", "epay:extra")
	if extra.Status != "review" {
		t.Fatal("extra charge was silently delivered", extra.Status)
	}
}

func TestPaymentReviewRetry(t *testing.T) {
	s := newTestServer(t)
	o := recoveryOrder(t, s, "review", "u", payEPay)
	s.st.DB.Where("id = ?", o.PlanID).Delete(&store.Group{})
	if e := s.acceptPayment(o, "epay:review"); e != nil {
		t.Fatal(e)
	}
	var p store.PaymentReceipt
	s.st.DB.First(&p, "ref = ?", "epay:review")
	if p.Status != "review" {
		t.Fatal("missing plan not held", p.Status)
	}
	recoveryOrder(t, s, "spare", "u", payEPay)
	if e := s.retryOrderPayment(o.ID); e != nil {
		t.Fatal(e)
	}
	s.processPaymentReceipt(p.Ref)
	s.st.DB.First(&p, "ref = ?", p.Ref)
	if p.Status != "done" {
		t.Fatal("review retry not delivered", p.Status)
	}
}

// Freeze the account row until both workers are waiting on it. Without a
// pre-read user lock they compute expiry from the same stale snapshot.
func TestShopConcurrentRenewalsPostgres(t *testing.T) {
	testConcurrentEntitlements(t, false)
}

func testConcurrentEntitlements(t *testing.T, pack bool) {
	ss := clusterServers(t, pgOnly(t), 2)
	a := ss[0]
	o1 := recoveryOrder(t, a, "r1", "u", payEPay)
	o2 := recoveryOrder(t, a, "r2", "u", payEPay)
	if e := a.st.DB.Model(o2).Updates(map[string]any{"telegram_id": 0, "web_key": "web-user"}).Error; e != nil {
		t.Fatal(e)
	}
	if pack {
		for _, s := range ss {
			licenseAll(s)
		}
		pk := store.TrafficPack{ID: "recovery-pack", Name: "pack", Traffic: 10 << 30}
		if e := a.st.DB.Create(&pk).Error; e != nil {
			t.Fatal(e)
		}
		for _, o := range []*store.Order{o1, o2} {
			if e := a.st.DB.Model(o).Updates(map[string]any{"kind": kindPack, "pack_id": pk.ID, "entitlements": freezePack(&pk)}).Error; e != nil {
				t.Fatal(e)
			}
		}
	}
	var original store.User
	a.st.DB.First(&original, "id = ?", "u")
	gate := a.st.DB.Begin()
	if gate.Error != nil {
		t.Fatal(gate.Error)
	}
	defer gate.Rollback()
	if e := gate.Exec("UPDATE users SET updated_at=updated_at WHERE id=?", "u").Error; e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, s := range ss {
		wg.Add(1)
		go func(i int, s *Server) {
			defer wg.Done()
			id := o1.ID
			if i == 1 {
				id = o2.ID
			}
			_, _, _, e := s.fulfil(id, fmt.Sprint("epay:renew", i), nil)
			errs <- e
		}(i, s)
	}
	ready := false
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		var n int64
		a.st.DB.Raw("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query ILIKE '%UPDATE%users%'").Scan(&n)
		if n >= 2 {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e := gate.Commit().Error; e != nil {
		t.Fatal(e)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if !ready {
		t.Fatal("workers did not overlap on the user row")
	}
	var got store.User
	a.st.DB.First(&got, "id = ?", "u")
	if pack {
		if got.TrafficLimit != original.TrafficLimit+(20<<30) || got.ExtraTraffic != 20<<30 {
			t.Fatalf("concurrent packs lost quota: %+v", got)
		}
		return
	}
	if !got.ExpireAt.Equal(original.ExpireAt.AddDate(0, 0, 60)) {
		t.Fatalf("two paid renewals: %v -> %v", original.ExpireAt, got.ExpireAt)
	}
}

func TestShopConcurrentFirstAccount(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		o1 := recoveryOrder(t, ss[0], "first1", "", payEPay)
		o2 := recoveryOrder(t, ss[0], "first2", "", payEPay)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i, s := range ss {
			wg.Add(1)
			go func(i int, s *Server) {
				defer wg.Done()
				id := o1.ID
				if i == 1 {
					id = o2.ID
				}
				_, _, _, e := s.fulfil(id, fmt.Sprint("epay:first", i), nil)
				errs <- e
			}(i, s)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
		var us []store.User
		ss[0].st.DB.Where("telegram_id = ?", 700).Find(&us)
		if len(us) != 1 || us[0].ExpireAt.Sub(o1.CreatedAt) < 59*24*time.Hour {
			t.Fatalf("first account duplicated/lost renewal: %+v", us)
		}
	})
}

func TestPaymentPendingExtraReceiptRequiresReview(t *testing.T) {
	s := newTestServer(t)
	o := recoveryOrder(t, s, "concurrent-receipts", "u", payEPay)
	// Both callbacks can persist before either worker has delivered the order.
	now := time.Now()
	for _, ref := range []string{"epay:first", "epay:second"} {
		if e := s.st.DB.Create(&store.PaymentReceipt{Ref: ref, OrderID: o.ID, Method: payEPay, Amount: o.Amount, Currency: o.Currency, Status: "pending", NextAt: now, CreatedAt: now}).Error; e != nil {
			t.Fatal(e)
		}
	}
	s.processPaymentReceipt("epay:first")
	s.processPaymentReceipt("epay:second")
	var p store.PaymentReceipt
	s.st.DB.First(&p, "ref = ?", "epay:second")
	if p.Status != "review" {
		t.Fatal("second payment silently acknowledged as delivered", p.Status)
	}
}

func TestPaymentReceiptPreventsOrderCleanup(t *testing.T) {
	s, _, admin := newAdminClient(t)
	old := time.Now().Add(-48 * time.Hour)
	o := store.Order{ID: "receipt-protected", Method: payEPay, Status: "expired", ExpiresAt: old, CreatedAt: old}
	if e := s.st.DB.Create(&o).Error; e != nil {
		t.Fatal(e)
	}
	if e := s.st.DB.Create(&store.PaymentReceipt{Ref: "epay:protected", OrderID: o.ID, Status: "review", CreatedAt: old}).Error; e != nil {
		t.Fatal(e)
	}
	if code, out := admin.do("DELETE", "/orders?status=expired", nil); code != 200 || out["deleted"] != float64(0) {
		t.Fatal("removed paid evidence", code, out)
	}
	if code, out := admin.do("GET", "/orders", nil); code != 200 {
		t.Fatal(code)
	} else {
		items := out["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["deletable"] != false {
			t.Fatal("paid receipt appears deletable", out)
		}
	}
}
