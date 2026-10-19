package panel

import (
	"context"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestOrderCancelResume: a buyer finds unpaid orders in the bot, pays one
// again (picking another EPay type, or the same native Stars checkout) and cancels
// another. A cancelled Stars invoice is refused at checkout; money that
// still arrives for a cancelled order is delivered.
func TestOrderCancelResume(t *testing.T) {
	fake := &fakeTG{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	api := srv.URL
	s, _, admin := newAdminClient(t)
	for k, v := range map[string]string{"tg_enabled": "true", "tg_api_base": api, "tg_poll_timeout": "0",
		"shop_enabled": "true", "shop_auto_create": "true", "shop_stars": "true", "shop_epay_url": "https://pay.example.com",
		"shop_epay_pid": "1001", "shop_epay_types": "alipay,wxpay"} {
		s.st.SetSetting(k, v)
	}
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.SetSetting("shop_epay_key", s.st.Box.Seal("merchant-key"))
	code, out := admin.do("POST", "/plans", map[string]any{"name": "基础版", "level": 1, "trafficLimit": 10 << 30, "days": 30,
		"sale": true, "priceStars": 50, "priceCny": 10})
	if code != 200 {
		t.Fatalf("plan %d %v", code, out)
	}
	plan := out["id"].(string)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	waitUntil(t, "bot running", s.tg.isRunning)
	status := func(id string) string {
		var o store.Order
		s.st.DB.First(&o, "id = ?", id)
		return o.Status
	}
	answered := func(want string) func() bool {
		return func() bool {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return slices.ContainsFunc(fake.answers, func(a string) bool { return strings.Contains(a, want) })
		}
	}

	// An EPay order: the payment message offers cancelling.
	fake.push(cb(700, "s:m:"+plan+":epay:alipay"))
	var epay store.Order
	waitUntil(t, "epay order row", func() bool { return s.st.DB.First(&epay, "telegram_id = ? AND method = ?", 700, payEPay).Error == nil })
	waitUntil(t, "epay cancel button", func() bool { return hasButton(fake, "s:c:"+epay.ID) })
	// A Stars order.
	fake.push(cb(700, "s:m:"+plan+":stars"))
	waitUntil(t, "invoice link", func() bool { return len(fake.callsOf("createInvoiceLink")) == 1 })
	stars := fake.callsOf("createInvoiceLink")[0]["payload"].(string)
	waitUntil(t, "Stars cancel button", func() bool { return hasButton(fake, "s:c:"+stars) })

	// The shop and the unpaid list show both.
	fake.push(msg(700, "/buy"))
	waitUntil(t, "unpaid button", func() bool { return hasButtonText(fake, "未付款订单（2）") })
	fake.push(cb(700, "v:unpaid"))
	waitUntil(t, "unpaid list", func() bool {
		t := fake.textsTo(700)
		return strings.Contains(t, epay.ID) && strings.Contains(t, stars) && hasButton(fake, "s:r:"+stars) && hasButton(fake, "s:c:"+stars)
	})

	// Resume the EPay order with WeChat Pay instead.
	fake.push(cb(700, "s:r:"+epay.ID))
	waitUntil(t, "pay type picker", func() bool { return hasButton(fake, "s:r:"+epay.ID+":wxpay") })
	fake.push(cb(700, "s:r:"+epay.ID+":wxpay"))
	waitUntil(t, "wxpay link", func() bool {
		for _, l := range buttonURLs(fake) {
			if u, err := url.Parse(l); err == nil && u.Query().Get("out_trade_no") == epay.ID && u.Query().Get("type") == "wxpay" {
				return true
			}
		}
		return false
	})
	// Resume Stars by editing the current card with its cached native link.
	edits := len(fake.callsOf("editMessageText"))
	fake.push(cb(700, "s:r:"+stars))
	waitUntil(t, "resumed card", func() bool { return len(fake.callsOf("editMessageText")) > edits })
	if len(fake.callsOf("createInvoiceLink")) != 1 || len(fake.callsOf("sendInvoice")) != 0 {
		t.Fatal("resuming Stars sent another invoice")
	}

	// Someone else cannot cancel it; the buyer can, after confirming.
	fake.push(cb(701, "s:x:"+stars))
	waitUntil(t, "foreign cancel refused", answered("订单不存在"))
	fake.push(cb(700, "s:c:"+stars))
	waitUntil(t, "confirm", func() bool { return hasButton(fake, "s:x:"+stars) })
	if status(stars) != "pending" {
		t.Fatal("cancelled before confirming")
	}
	fake.push(cb(700, "s:x:"+stars))
	waitUntil(t, "cancelled", func() bool { return status(stars) == "cancelled" })
	waitUntil(t, "cancelled receipt", func() bool { return strings.Contains(fake.textsTo(700), "<b>订单已取消</b>") })
	if n := s.unpaidCount(700); n != 1 {
		t.Fatalf("unpaid after cancel %d", n)
	}
	fake.push(cb(700, "s:r:"+stars))
	waitUntil(t, "resume refused", answered("已取消"))
	fake.push(map[string]any{"pre_checkout_query": map[string]any{"id": "q-c", "from": map[string]any{"id": 700}, "currency": "XTR",
		"total_amount": 50, "invoice_payload": stars}})
	waitUntil(t, "pre-checkout", func() bool { return len(fake.callsOf("answerPreCheckoutQuery")) == 1 })
	if a := fake.callsOf("answerPreCheckoutQuery")[0]; a["ok"] != false {
		t.Fatalf("cancelled order approved: %v", a)
	}

	// The admin cancels the EPay order; the payment still lands later.
	if code, out := admin.do("POST", "/orders/"+epay.ID+"/cancel", nil); code != 200 || out["status"] != "cancelled" {
		t.Fatalf("admin cancel %d %v", code, out)
	}
	if _, u, fresh, err := s.fulfil(epay.ID, "epay:late", nil); err != nil || !fresh || u.PlanID != plan {
		t.Fatalf("late payment %v %v %+v", fresh, err, u)
	}
	if code, _ := admin.do("POST", "/orders/"+epay.ID+"/cancel", nil); code != 400 {
		t.Fatalf("cancel a paid order: %d", code)
	}
	if code, _ := admin.do("POST", "/orders/nope/cancel", nil); code != 404 {
		t.Fatalf("cancel a missing order: %d", code)
	}
}

func (f *fakeTG) markups() [][][]button {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][][]button
	for _, m := range f.sent {
		var rows [][]button
		if rm, ok := m["reply_markup"].(map[string]any); ok {
			kb, _ := rm["inline_keyboard"].([]any)
			for _, r := range kb {
				var row []button
				for _, b := range r.([]any) {
					bm := b.(map[string]any)
					d, _ := bm["callback_data"].(string)
					u, _ := bm["url"].(string)
					row = append(row, button{Text: bm["text"].(string), Data: d, URL: u})
				}
				rows = append(rows, row)
			}
		}
		out = append(out, rows)
	}
	return out
}

func eachButton(f *fakeTG, fn func(button) bool) bool {
	for _, kb := range f.markups() {
		for _, r := range kb {
			if slices.ContainsFunc(r, fn) {
				return true
			}
		}
	}
	return false
}

func hasButton(f *fakeTG, data string) bool {
	return eachButton(f, func(b button) bool { return b.Data == data })
}

func hasButtonText(f *fakeTG, text string) bool {
	return eachButton(f, func(b button) bool { return strings.Contains(b.Text, text) })
}

func buttonURLs(f *fakeTG) []string {
	var out []string
	eachButton(f, func(b button) bool {
		if b.URL != "" {
			out = append(out, b.URL)
		}
		return false
	})
	return out
}
