package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func (f *fakeTG) callsOf(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.calls[method]...)
}

// TestShopCommunity: without online payments the bot shop still lists the
// plans on sale and sells them by redeem code; configured Stars / EPay /
// USDT settings take no orders and the EPay callback is not served.
func TestShopCommunity(t *testing.T) {
	fake := &fakeTG{}
	api := httptest.NewServer(fake)
	defer api.Close()
	s, srv, admin := newAdminClient(t)
	licenseNone(s)
	for k, v := range map[string]string{"tg_enabled": "true", "tg_api_base": api.URL, "tg_poll_timeout": "0",
		"shop_enabled": "true", "shop_stars": "true", "shop_epay_url": "https://pay.example.com", "shop_epay_pid": "1001",
		"shop_usdt_address": "TShopAddress"} {
		s.st.SetSetting(k, v)
	}
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.SetSetting("shop_epay_key", s.st.Box.Seal("merchant-key"))
	code, out := admin.do("POST", "/plans", map[string]any{"name": "专业版", "level": 1, "trafficLimit": 100 << 30, "days": 30,
		"sale": true, "priceStars": 100, "priceCny": 15, "priceUsdt": 2.5})
	if code != 200 {
		t.Fatalf("plan %d %v", code, out)
	}
	plan := out["id"].(string)
	for _, m := range []string{payStars, payEPay, payUSDT} {
		if _, err := s.newOrder(500, "u", "", plan, m); err == nil {
			t.Fatalf("%s order taken without online payments", m)
		}
	}
	if code, out := admin.do("GET", "/orders", nil); code != 200 || out["status"].(map[string]any)["epay"] != false {
		t.Fatalf("shop status %d %v", code, out["status"])
	}
	resp, err := http.Get(srv.URL + epayNotifyPath + "?out_trade_no=x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !proBuild && resp.StatusCode != http.StatusNotFound { // pro builds still take callbacks for orders already paid
		t.Fatalf("epay callback served: %d", resp.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	waitUntil(t, "bot running", s.tg.isRunning)
	fake.push(msg(500, "/buy"))
	waitUntil(t, "shop list", func() bool {
		return strings.Contains(fake.textsTo(500), "专业版") && strings.Contains(fake.textsTo(500), "兑换码购买")
	})
	fake.push(cb(500, "s:p:"+plan))
	waitUntil(t, "plan card", func() bool { return strings.Contains(fake.textsTo(500), "/redeem 兑换码") })
	if hasButton(fake, "s:m:"+plan+":"+payStars) {
		t.Fatal("a payment button in the community shop")
	}
	// A Stars checkout is refused rather than left hanging.
	fake.push(map[string]any{"pre_checkout_query": map[string]any{"id": "q", "from": map[string]any{"id": 500}, "currency": "XTR",
		"total_amount": 100, "invoice_payload": "x"}})
	waitUntil(t, "pre-checkout", func() bool { return len(fake.callsOf("answerPreCheckoutQuery")) == 1 })
	if a := fake.callsOf("answerPreCheckoutQuery")[0]; a["ok"] != false {
		t.Fatalf("pre-checkout %v", a)
	}

	// Redeem codes sell the plan.
	code, out = admin.do("POST", "/redeem-codes", map[string]any{"planId": plan, "count": 1})
	if code != 200 {
		t.Fatalf("codes %d %v", code, out)
	}
	fake.push(msg(800, "/redeem "+out["codes"].([]any)[0].(string)))
	waitUntil(t, "redeemed", func() bool {
		var u store.User
		return s.st.DB.First(&u, "telegram_id = ?", 800).Error == nil && u.PlanID == plan
	})

	// An order left from a licensed period can be cancelled but not paid.
	now := time.Now()
	s.st.DB.Create(&store.Order{ID: "oldorder", TelegramID: 500, PlanID: plan, PlanName: "专业版", Method: payEPay, Amount: "15.00",
		Currency: "CNY", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	fake.push(cb(500, "s:r:oldorder"))
	waitUntil(t, "resume refused", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return slices.Contains(fake.answers, "这个付款方式已停用，请重新下单")
	})
	fake.push(cb(500, "s:x:oldorder"))
	waitUntil(t, "cancelled", func() bool {
		var o store.Order
		s.st.DB.First(&o, "id = ?", "oldorder")
		return o.Status == "cancelled"
	})
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

// TestPayReturn: payment gateways send the buyer back to the panel's own
// domain (the one authorized with them), which forwards to the bot.
func TestPayReturn(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(srv.URL + epayReturnPath + "?out_trade_no=x&trade_status=TRADE_SUCCESS")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("without a bot: %d", resp.StatusCode)
	}
	s.tg.mu.Lock()
	s.tg.me = "polaris_bot"
	s.tg.mu.Unlock()
	resp, err = noRedirect.Get(srv.URL + epayReturnPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://t.me/polaris_bot" {
		t.Fatalf("with a bot: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !strings.HasSuffix(s.epayReturnURL(), epayReturnPath) {
		t.Fatal(s.epayReturnURL())
	}
}
