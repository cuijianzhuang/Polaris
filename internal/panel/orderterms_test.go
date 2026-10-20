package panel

import (
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestOrderEntitlementsFrozen(t *testing.T) {
	s := newTestServer(t)
	p := store.Group{ID: "p", Name: "monthly", Plan: true, Sale: true, Days: 30, TrafficLimit: 100 << 30, DeviceLimit: 3, IPLimit: 2, ResetPolicy: "monthly", ResetDay: 10}
	u := store.User{ID: "u", Username: "alice", UUID: "u", SubToken: "u", Status: "active"}
	for _, v := range []any{&p, &u} {
		if e := s.st.DB.Create(v).Error; e != nil {
			t.Fatal(e)
		}
	}
	now := time.Now()
	o := &store.Order{ID: "o", UserID: u.ID, PlanID: p.ID, PlanName: p.Name, Method: payEPay, Amount: "15.00", Currency: "CNY", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Entitlements: freezePlan(&p)}
	if _, e := s.openShopOrder(o, 0); e != nil {
		t.Fatal(e)
	}
	if e := s.st.DB.Model(&p).Updates(map[string]any{"days": 1, "traffic_limit": int64(1 << 30), "device_limit": 1, "ip_limit": 1, "reset_day": 1}).Error; e != nil {
		t.Fatal(e)
	}
	_, got, fresh, e := s.fulfil(o.ID, "payment", nil)
	if e != nil || !fresh {
		t.Fatalf("fulfil: %v %v", fresh, e)
	}
	if got.TrafficLimit != 100<<30 || got.ExpireAt.Sub(now) < 29*24*time.Hour || got.DeviceLimit != 3 || got.IPLimit != 2 || got.ResetDay != 10 {
		t.Fatalf("changed entitlements: %+v", got)
	}
	if _, _, fresh, e = s.fulfil(o.ID, "payment", nil); e != nil || fresh {
		t.Fatalf("repeat: %v %v", fresh, e)
	}
	// Legacy orders still use their current plan; never invent old limits.
	o.ID, o.Entitlements, o.PayRef, o.Status = "legacy", "", nil, "pending"
	if e = s.st.DB.Create(o).Error; e != nil {
		t.Fatal(e)
	}
	_, got, _, e = s.fulfil(o.ID, "legacy-payment", nil)
	if e != nil || got.TrafficLimit != 1<<30 {
		t.Fatalf("legacy: %+v %v", got, e)
	}
}

func TestOrderTermsRejectCorruptSnapshot(t *testing.T) {
	for _, raw := range []string{`{}`, `{"version":2}`, `{"version":1,"pack":{"id":"other","traffic":100}}`} {
		var p store.TrafficPack
		if e := orderPack(nil, &store.Order{PackID: "expected", Entitlements: raw}, &p); e == nil {
			t.Fatal("invalid snapshot accepted", raw)
		}
	}
}
