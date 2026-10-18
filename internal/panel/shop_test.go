package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func (f *fakeTG) callsOf(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.calls[method]...)
}

// TestShop buys plans in the bot four ways: Telegram Stars (invoice,
// pre-checkout, payment), EPay (signed callback), USDT (TronGrid transfer
// matched by amount) and a redeem code. A buyer without an account gets one;
// every payment counts once; a bound user renews.
func TestShop(t *testing.T) {
	fake := &fakeTG{}
	api := httptest.NewServer(fake)
	defer api.Close()
	var tmu sync.Mutex
	var transfers []map[string]any
	tron := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tmu.Lock()
		defer tmu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": transfers})
	}))
	defer tron.Close()

	s, srv, admin := newAdminClient(t)
	for k, v := range map[string]string{"tg_enabled": "true", "tg_api_base": api.URL, "tg_poll_timeout": "0",
		"shop_enabled": "true", "shop_stars": "true", "shop_epay_url": "https://pay.example.com", "shop_epay_pid": "1001",
		"shop_usdt_address": "TShopAddress", "shop_usdt_api": tron.URL} {
		s.st.SetSetting(k, v)
	}
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.SetSetting("shop_epay_key", s.st.Box.Seal("merchant-key"))
	s.st.DB.Create(&store.Node{ID: "n1", Name: "n1", Address: "203.0.113.1", Level: 1})
	s.st.DB.Create(&store.Inbound{ID: "in-n1", NodeID: "n1", Tag: "t1", Core: model.CoreXray, Port: 22000, Enabled: true})
	code, out := admin.do("POST", "/plans", map[string]any{"name": "专业版", "level": 1, "trafficLimit": 100 << 30, "days": 30,
		"sale": true, "priceStars": 100, "priceCny": 15, "priceUsdt": 2.5})
	if code != 200 {
		t.Fatalf("plan %d %v", code, out)
	}
	plan := out["id"].(string)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	waitUntil(t, "bot running", s.tg.isRunning)
	userOf := func(tg int64) *store.User {
		var u store.User
		if s.st.DB.First(&u, "telegram_id = ?", tg).Error != nil {
			return nil
		}
		return &u
	}

	// --- Telegram Stars ---
	fake.push(msg(500, "/buy"))
	waitUntil(t, "shop list", func() bool {
		return strings.Contains(fake.textsTo(500), "专业版") && strings.Contains(fake.textsTo(500), "100 ⭐")
	})
	fake.push(cb(500, "s:m:"+plan+":stars"))
	waitUntil(t, "invoice", func() bool { return len(fake.callsOf("sendInvoice")) == 1 })
	inv := fake.callsOf("sendInvoice")[0]
	orderID := inv["payload"].(string)
	if inv["currency"] != "XTR" {
		t.Fatalf("invoice %v", inv)
	}
	pre := func(id string, amount int, from int64) map[string]any {
		return map[string]any{"pre_checkout_query": map[string]any{"id": id, "from": map[string]any{"id": from}, "currency": "XTR",
			"total_amount": amount, "invoice_payload": orderID}}
	}
	fake.push(pre("q-bad", 1, 500))
	fake.push(pre("q-other", 100, 999))
	fake.push(pre("q-ok", 100, 500))
	waitUntil(t, "pre-checkout answers", func() bool { return len(fake.callsOf("answerPreCheckoutQuery")) == 3 })
	for _, a := range fake.callsOf("answerPreCheckoutQuery") {
		if (a["pre_checkout_query_id"] == "q-ok") != (a["ok"] == true) {
			t.Fatalf("pre-checkout %v", a)
		}
	}
	paid := func(charge string) map[string]any {
		m := msg(500, "")
		m["message"].(map[string]any)["successful_payment"] = map[string]any{"currency": "XTR", "total_amount": 100,
			"invoice_payload": orderID, "telegram_payment_charge_id": charge}
		return m
	}
	fake.push(paid("charge-1"))
	waitUntil(t, "account created", func() bool { u := userOf(500); return u != nil && u.PlanID == plan })
	u := userOf(500)
	if u.TrafficLimit != 100<<30 || u.ExpireAt == nil || len(s.nodesForUsers(u.ID)) != 1 {
		t.Fatalf("stars buyer: %+v", u)
	}
	waitUntil(t, "delivery message", func() bool { return strings.Contains(fake.textsTo(500), "支付成功") })
	firstExp := *u.ExpireAt
	fake.push(paid("charge-1")) // a duplicate update changes nothing
	time.Sleep(300 * time.Millisecond)
	if got := userOf(500); !got.ExpireAt.Equal(firstExp) {
		t.Fatal("a repeated payment applied twice")
	}

	// My orders lists it; the admin can refund the Stars.
	fake.push(cb(500, "v:orders"))
	waitUntil(t, "my orders", func() bool {
		return strings.Contains(fake.textsTo(500), orderID) && strings.Contains(fake.textsTo(500), "已支付")
	})
	if code, out := admin.do("POST", "/orders/"+orderID+"/refund", nil); code != 200 || out["status"] != "refunded" {
		t.Fatalf("refund %d %v", code, out)
	}
	if r := fake.callsOf("refundStarPayment"); len(r) != 1 || r[0]["telegram_payment_charge_id"] != "charge-1" || r[0]["user_id"].(float64) != 500 {
		t.Fatalf("refund call %v", r)
	}
	// An expired Stars order is refused before any money moves.
	old, _ := s.newOrder(500, "u", u.ID, plan, payStars)
	s.st.DB.Model(&store.Order{}).Where("id = ?", old.ID).Update("expires_at", time.Now().Add(-time.Minute))
	fake.push(map[string]any{"pre_checkout_query": map[string]any{"id": "q-late", "from": map[string]any{"id": 500}, "currency": "XTR",
		"total_amount": 100, "invoice_payload": old.ID}})
	waitUntil(t, "late pre-checkout", func() bool { return len(fake.callsOf("answerPreCheckoutQuery")) == 4 })
	if a := fake.callsOf("answerPreCheckoutQuery")[3]; a["ok"] != false {
		t.Fatalf("expired order approved: %v", a)
	}

	// A bound buyer renews: a second order extends the expiry by 30 days.
	o2, err := s.newOrder(500, "u", u.ID, plan, payStars)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, fresh, err := s.fulfil(o2.ID, "stars:charge-2", nil); err != nil || !fresh {
		t.Fatalf("renew: %v %v", fresh, err)
	}
	if got := userOf(500); got.ExpireAt.Sub(firstExp) < 30*24*time.Hour-time.Minute {
		t.Fatalf("renew moved expiry by %v", got.ExpireAt.Sub(firstExp))
	}

	// --- EPay ---
	o3, err := s.newOrder(600, "buyer6", "", plan, payEPay)
	if err != nil || o3.Amount != "15.00" {
		t.Fatalf("epay order %v %v", o3, err)
	}
	link, _ := url.Parse(s.epayLink(o3, "alipay"))
	if link.Query().Get("sign") != epaySign(link.Query(), "merchant-key") || link.Query().Get("money") != "15.00" {
		t.Fatalf("epay link %s", link)
	}
	notify := func(money, sign string) string {
		v := url.Values{"pid": {"1001"}, "trade_no": {"T123"}, "out_trade_no": {o3.ID}, "type": {"alipay"}, "name": {"x"},
			"money": {money}, "trade_status": {"TRADE_SUCCESS"}}
		if sign == "" {
			sign = epaySign(v, "merchant-key")
		}
		v.Set("sign", sign)
		v.Set("sign_type", "MD5")
		resp, err := http.Get(srv.URL + epayNotifyPath + "?" + v.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if r := notify("15.00", "forged"); r != "fail" || userOf(600) != nil {
		t.Fatalf("forged callback: %q", r)
	}
	if r := notify("1.00", ""); r != "fail" || userOf(600) != nil {
		t.Fatalf("wrong amount: %q", r)
	}
	if r := notify("15.00", ""); r != "success" || userOf(600) == nil || userOf(600).PlanID != plan {
		t.Fatalf("epay callback: %q", r)
	}
	if r := notify("15.00", ""); r != "success" {
		t.Fatalf("repeated callback: %q", r)
	}

	// --- USDT ---
	o4, err := s.newOrder(700, "buyer7", "", plan, payUSDT)
	if err != nil {
		t.Fatal(err)
	}
	o5, _ := s.newOrder(701, "buyer71", "", plan, payUSDT)
	if a, _ := usdtMicros(o4.Amount); o4.Amount == o5.Amount || a <= 2500000 || a > 2550000 {
		t.Fatalf("usdt amounts %s %s", o4.Amount, o5.Amount)
	}
	micros, _ := usdtMicros(o4.Amount)
	tmu.Lock()
	transfers = []map[string]any{
		{"transaction_id": "tx-wrong", "block_timestamp": time.Now().UnixMilli(), "to": "TShopAddress", "value": "2500000",
			"token_info": map[string]any{"address": usdtContract, "decimals": 6}},
		{"transaction_id": "tx-1", "block_timestamp": time.Now().UnixMilli(), "to": "TShopAddress", "value": fmt.Sprint(micros),
			"token_info": map[string]any{"address": usdtContract, "decimals": 6}},
	}
	tmu.Unlock()
	s.pollUSDT()
	s.pollUSDT()
	if userOf(700) == nil || userOf(701) != nil {
		t.Fatal("usdt transfer not matched to its order only")
	}
	var o store.Order
	s.st.DB.First(&o, "id = ?", o4.ID)
	if o.Status != "paid" || o.PayRef == nil || *o.PayRef != "usdt:tx-1" {
		t.Fatalf("usdt order %+v", o)
	}

	// --- redeem codes ---
	code, out = admin.do("POST", "/redeem-codes", map[string]any{"planId": plan, "count": 2, "batch": "淘宝"})
	if code != 200 {
		t.Fatalf("codes %d %v", code, out)
	}
	rc := out["codes"].([]any)[0].(string)
	fake.push(msg(800, "/redeem "+strings.ToLower(rc)))
	waitUntil(t, "redeemed", func() bool { u := userOf(800); return u != nil && u.PlanID == plan })
	fake.push(msg(801, "/redeem "+rc))
	waitUntil(t, "reuse refused", func() bool { return strings.Contains(fake.textsTo(801), "已被使用") })
	if userOf(801) != nil {
		t.Fatal("a used code made an account")
	}

	// Admin views: orders with revenue; closing the shop stops new orders.
	code, out = admin.do("GET", "/orders?status=paid", nil)
	if code != 200 || out["total"].(float64) != 4 {
		t.Fatalf("orders %d %v", code, out)
	}
	s.st.SetSetting("shop_enabled", "false")
	if _, err := s.newOrder(900, "x", "", plan, payStars); err == nil {
		t.Fatal("closed shop took an order")
	}
}

// TestOrdersEmpty: with nothing sold the lists are [] — a null revenue
// list once blanked the shop page.
func TestOrdersEmpty(t *testing.T) {
	_, _, admin := newAdminClient(t)
	code, out := admin.do("GET", "/orders", nil)
	if code != 200 {
		t.Fatalf("orders %d", code)
	}
	for _, k := range []string{"items", "revenue"} {
		if _, ok := out[k].([]any); !ok {
			t.Fatalf("%s is %#v, want []", k, out[k])
		}
	}
}
