package panel

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestCouponDiscount(t *testing.T) {
	pc := &store.Coupon{Percent: 20}
	if st, cny, usdt := discounted(pc, 99, 15, 2.5); st != 80 || cny != 12 || usdt != 2 {
		t.Fatalf("20%% off: %d %v %v", st, cny, usdt)
	}
	fixed := &store.Coupon{OffCNY: 5, OffStars: 200, OffUSDT: 0.3333}
	if st, cny, usdt := discounted(fixed, 100, 15, 2.5); st != 0 || cny != 10 || usdt != 2.1667 {
		t.Fatalf("fixed: %d %v %v", st, cny, usdt)
	}
	if couponLabel(pc) != "减 20%" || couponLabel(&store.Coupon{Percent: 100}) != "免费" || couponLabel(fixed) != "减 ¥5 / 0.3333 USDT / 200 ⭐" {
		t.Fatalf("labels %q %q", couponLabel(pc), couponLabel(fixed))
	}
}

// Coupon rules: enabled, time window, products, total and per-buyer limits
// (open orders count toward the total).
func TestCouponRules(t *testing.T) {
	s, _, admin := newAdminClient(t)
	p := store.Group{ID: "p1", Name: "月付", Plan: true, Sale: true, PriceCNY: 15}
	other := store.Group{ID: "p2", Name: "年付", Plan: true, Sale: true, PriceCNY: 150}
	s.st.DB.Create(&p)
	s.st.DB.Create(&other)

	if st, out := admin.do("POST", "/coupons", map[string]any{"code": "x", "percent": 10}); st != 400 {
		t.Fatalf("short code accepted: %d %v", st, out)
	}
	if st, out := admin.do("POST", "/coupons", map[string]any{"code": "BOTH", "percent": 10, "offCny": 1}); st != 400 {
		t.Fatalf("percent and amount accepted: %d %v", st, out)
	}
	if st, out := admin.do("POST", "/coupons", map[string]any{"code": "save20", "percent": 20, "planIds": []string{"p1"}, "maxUses": 2}); st != 200 {
		t.Fatalf("create %d %v", st, out)
	}
	if st, _ := admin.do("POST", "/coupons", map[string]any{"code": "SAVE20", "percent": 5}); st != 400 {
		t.Fatal("duplicate accepted")
	}
	st, out := admin.do("POST", "/coupons", map[string]any{"prefix": "vip", "count": 3, "offCny": 3, "packs": true, "perUser": 0})
	if st != 200 || len(out["codes"].([]any)) != 3 || !strings.HasPrefix(out["codes"].([]any)[0].(string), "VIP") {
		t.Fatalf("batch %d %v", st, out)
	}

	alice := shopBuyer{TGID: 1}
	if _, err := s.couponFor("save20", "plan", "p1", alice); err != nil {
		t.Fatalf("valid coupon: %v", err)
	}
	if _, err := s.couponFor("SAVE20", "plan", "p2", alice); err == nil || !strings.Contains(err.Error(), "该套餐") {
		t.Fatalf("other plan: %v", err)
	}
	if _, err := s.couponFor("SAVE20", kindPack, "k1", alice); err == nil {
		t.Fatal("plan coupon used for a pack")
	}
	if _, err := s.couponFor("NOPE", "plan", "p1", alice); err == nil {
		t.Fatal("unknown coupon")
	}

	// A paid order counts for the buyer and toward the total.
	now := time.Now()
	o := store.Order{ID: store.NewID(), TelegramID: 1, PlanID: "p1", PlanName: "月付", Method: payManual, Amount: "12.00", Currency: "CNY",
		Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour), CouponCode: "SAVE20", OrigAmount: "15.00"}
	s.st.DB.Create(&o)
	s.st.SetSetting("shop_auto_create", "true")
	if _, _, fresh, err := s.fulfil(o.ID, "manual:1", nil); err != nil || !fresh {
		t.Fatalf("fulfil: %v", err)
	}
	var cp store.Coupon
	s.st.DB.First(&cp, "code = ?", "SAVE20")
	var uses int64
	s.st.DB.Model(&store.CouponUse{}).Where("code = ? AND order_id = ?", "SAVE20", o.ID).Count(&uses)
	if cp.Used != 1 || uses != 1 {
		t.Fatalf("use not recorded: used %d rows %d", cp.Used, uses)
	}
	if _, err := s.couponFor("SAVE20", "plan", "p1", alice); err == nil || !strings.Contains(err.Error(), "用过") {
		t.Fatalf("second use by the same buyer: %v", err)
	}
	// Bob's open order takes the last use.
	s.st.DB.Create(&store.Order{ID: store.NewID(), TelegramID: 2, PlanID: "p1", Method: payEPay, Amount: "12.00", Status: "pending",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), CouponCode: "SAVE20"})
	if _, err := s.couponFor("SAVE20", "plan", "p1", shopBuyer{TGID: 3}); err == nil || !strings.Contains(err.Error(), "领完") {
		t.Fatalf("over the total: %v", err)
	}

	// Disabled, not started, expired.
	s.st.DB.Model(&store.Coupon{}).Where("code = ?", "SAVE20").Updates(map[string]any{"max_uses": 0, "per_user": 0})
	later := now.Add(time.Hour)
	s.st.DB.Model(&store.Coupon{}).Where("code = ?", "SAVE20").Update("starts_at", later)
	if _, err := s.couponFor("SAVE20", "plan", "p1", alice); err == nil {
		t.Fatal("used before its start")
	}
	earlier := now.Add(-time.Hour)
	s.st.DB.Model(&store.Coupon{}).Where("code = ?", "SAVE20").Updates(map[string]any{"starts_at": nil, "expires_at": earlier})
	if _, err := s.couponFor("SAVE20", "plan", "p1", alice); err == nil {
		t.Fatal("used after it expired")
	}
	s.st.DB.Model(&store.Coupon{}).Where("code = ?", "SAVE20").Updates(map[string]any{"expires_at": nil, "enabled": false})
	if _, err := s.couponFor("SAVE20", "plan", "p1", alice); err == nil {
		t.Fatal("disabled coupon used")
	}

	// Admin list, uses, update and delete.
	_, out = admin.do("GET", "/coupons", nil)
	if n := len(out["items"].([]any)); n != 4 {
		t.Fatalf("list %d", n)
	}
	_, out = admin.do("GET", "/coupons/SAVE20/uses", nil)
	if n := len(out["orders"].([]any)); n != 1 {
		t.Fatalf("uses %d", n)
	}
	if st, out := admin.do("PUT", "/coupons/SAVE20", map[string]any{"percent": 30, "enabled": true}); st != 200 || out["percent"].(float64) != 30 || out["used"].(float64) != 1 {
		t.Fatalf("update %d %v", st, out)
	}
	if st, _ := admin.do("DELETE", "/coupons/SAVE20", nil); st != 204 {
		t.Fatal("delete")
	}
	var o2 store.Order
	s.st.DB.First(&o2, "id = ?", o.ID)
	if o2.CouponCode != "SAVE20" || o2.OrigAmount != "15.00" {
		t.Fatalf("order lost its coupon: %+v", o2)
	}
}

// Free (100% off) orders skip payment, so their limits must hold under
// concurrent claims that all passed the up-front check.
func TestFreeCouponConcurrent(t *testing.T) {
	s, _, _ := newAdminClient(t)
	p := store.Group{ID: "pf", Name: "免费月", Plan: true, Sale: true, PriceCNY: 15}
	s.st.DB.Create(&p)
	s.st.SetSetting("shop_auto_create", "true")
	s.st.DB.Create(&store.Coupon{Code: "FREE2", Percent: 100, Plans: true, MaxUses: 2, PerUser: 1, Enabled: true, CreatedAt: time.Now()})
	s.st.DB.Create(&store.Coupon{Code: "FREE1PP", Percent: 100, Plans: true, PerUser: 1, Enabled: true, CreatedAt: time.Now()})
	claim := func(code string, tg int64) error {
		now := time.Now()
		o := &store.Order{ID: store.NewID(), TelegramID: tg, PlanID: p.ID, PlanName: p.Name, Currency: "CNY", Status: "pending",
			CreatedAt: now, ExpiresAt: now.Add(time.Hour), CouponCode: code, OrigAmount: "15.00", Entitlements: freezePlan(&p)}
		_, err := s.openCouponOrder(o, 0, true)
		return err
	}
	run := func(code string, tg func(i int) int64) int {
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok := 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if claim(code, tg(i)) == nil {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return ok
	}
	if n := run("FREE2", func(i int) int64 { return int64(100 + i) }); n != 2 {
		t.Fatalf("max 2 uses, %d free orders went through", n)
	}
	if n := run("FREE1PP", func(int) int64 { return 7 }); n != 1 {
		t.Fatalf("one per buyer, %d free orders went through", n)
	}
	var paid int64
	s.st.DB.Model(&store.Order{}).Where("status = ?", "paid").Count(&paid)
	if paid != 3 {
		t.Fatalf("paid orders %d", paid)
	}
}
