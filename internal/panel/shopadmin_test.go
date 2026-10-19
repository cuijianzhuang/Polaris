package panel

import (
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestRedeemCodeAdmin: the code list tells usable, used and expired codes
// apart, says who used a code and which order it made, and deletes codes
// by selection or by state; used codes go only when asked.
func TestRedeemCodeAdmin(t *testing.T) {
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(24*time.Hour)
	db.Create(&store.User{ID: "u1", Username: "alice", UUID: "u1", SubToken: "u1-token-xxxxxxxx"})
	db.Create(&store.RedeemCode{Code: "AAAA-1111", PlanID: "p", Batch: "b1"})
	db.Create(&store.RedeemCode{Code: "BBBB-2222", PlanID: "p", Batch: "b1", ExpiresAt: &future})
	db.Create(&store.RedeemCode{Code: "CCCC-3333", PlanID: "p", Batch: "b1", ExpiresAt: &past})
	db.Create(&store.RedeemCode{Code: "DDDD-4444", PlanID: "p", Batch: "b2", UsedBy: "u1", UsedAt: &past})
	db.Create(&store.Order{ID: "o-code", Method: payCode, Amount: "DDDD-4444", Status: "paid", UserID: "u1", CreatedAt: past})

	code, out := admin.do("GET", "/redeem-codes", nil)
	counts, _ := out["counts"].(map[string]any)
	if code != 200 || out["total"] != 4.0 || counts["unused"] != 2.0 || counts["used"] != 1.0 || counts["expired"] != 1.0 {
		t.Fatalf("list %d %v", code, out)
	}
	if out["usernames"].(map[string]any)["u1"] != "alice" || out["orders"].(map[string]any)["DDDD-4444"] != "o-code" {
		t.Fatalf("used code without its user and order: %v", out)
	}
	for state, want := range map[string]float64{"no": 2, "yes": 1, "expired": 1} {
		if _, out := admin.do("GET", "/redeem-codes?used="+state, nil); out["total"] != want {
			t.Fatalf("state %s: %v", state, out["total"])
		}
	}
	if _, out := admin.do("GET", "/redeem-codes?q=bbbb", nil); out["total"] != 1.0 {
		t.Fatalf("search: %v", out["total"])
	}

	// Selected: a used code stays unless asked for.
	if _, out := admin.do("DELETE", "/redeem-codes", map[string]any{"codes": []string{"AAAA-1111", "DDDD-4444"}}); out["deleted"] != 1.0 {
		t.Fatalf("delete selected: %v", out)
	}
	if _, out := admin.do("DELETE", "/redeem-codes", map[string]any{"codes": []string{"DDDD-4444"}, "used": true}); out["deleted"] != 1.0 {
		t.Fatalf("delete used: %v", out)
	}
	var o store.Order
	if db.First(&o, "id = ?", "o-code").Error != nil {
		t.Fatal("the code's order went with it")
	}
	if _, out := admin.do("DELETE", "/redeem-codes?state=expired", nil); out["deleted"] != 1.0 {
		t.Fatalf("clear expired: %v", out)
	}
	var left []string
	db.Model(&store.RedeemCode{}).Pluck("code", &left)
	if len(left) != 1 || left[0] != "BBBB-2222" {
		t.Fatalf("left %v", left)
	}
	if code, _ := admin.do("DELETE", "/redeem-codes", nil); code != 400 {
		t.Fatalf("delete everything: %d", code)
	}
}

// TestOrderDelete: only expired or cancelled orders without a payment,
// closed for a day, can be deleted; the list says which.
func TestOrderDelete(t *testing.T) {
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	now := time.Now()
	old, recent := now.Add(-48*time.Hour), now.Add(-time.Hour)
	ref := "epay:T1"
	for _, o := range []store.Order{
		{ID: "exp-old", Status: "expired", Method: payEPay, CreatedAt: old, ExpiresAt: old},
		{ID: "exp-new", Status: "expired", Method: payEPay, CreatedAt: recent, ExpiresAt: recent},
		{ID: "can-old", Status: "cancelled", Method: payStars, CreatedAt: old, ExpiresAt: old},
		{ID: "paid-old", Status: "paid", Method: payEPay, CreatedAt: old, ExpiresAt: old, PayRef: &ref},
		{ID: "pend", Status: "pending", Method: payEPay, CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
	} {
		db.Create(&o)
	}
	_, out := admin.do("GET", "/orders", nil)
	del := map[string]bool{}
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		del[m["id"].(string)] = m["deletable"] == true
	}
	if !del["exp-old"] || del["exp-new"] || !del["can-old"] || del["paid-old"] || del["pend"] {
		t.Fatalf("deletable %v", del)
	}
	if _, out := admin.do("DELETE", "/orders", map[string]any{"ids": []string{"exp-old", "exp-new", "paid-old", "pend"}}); out["deleted"] != 1.0 {
		t.Fatalf("delete selected: %v", out)
	}
	if code, _ := admin.do("DELETE", "/orders?status=paid", nil); code != 400 {
		t.Fatal("paid orders deletable in bulk")
	}
	if _, out := admin.do("DELETE", "/orders?status=cancelled", nil); out["deleted"] != 1.0 {
		t.Fatalf("clean cancelled: %v", out)
	}
	var ids []string
	db.Model(&store.Order{}).Order("id").Pluck("id", &ids)
	if len(ids) != 3 || ids[0] != "exp-new" || ids[1] != "paid-old" || ids[2] != "pend" {
		t.Fatalf("left %v", ids)
	}
}
