package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func starsCheckoutFixture(t *testing.T) (*Server, *fakeTG, *store.Group) {
	t.Helper()
	s := newTestServer(t)
	licenseAll(s)
	s.st.SetSetting("shop_enabled", "true")
	s.st.SetSetting("shop_stars", "true")
	p := &store.Group{ID: "stars-plan", Name: "专业版", Plan: true, Sale: true, PriceStars: 10, Days: 30, TrafficLimit: 100 << 30}
	if err := s.st.DB.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	f := &fakeTG{}
	api := httptest.NewServer(f)
	t.Cleanup(api.Close)
	s.tg.token, s.tg.base, s.tg.me = "123:abc", api.URL, "polaris_bot"
	s.tg.selfID = 1
	return s, f, p
}

func TestPanelStarsRepeatedPurchaseAndCancellation(t *testing.T) {
	s, f, p := starsCheckoutFixture(t)
	b := s.tg
	ctx := context.Background()
	for range 3 {
		b.shopCallback(ctx, []string{"s", "m", p.ID, payStars}, 700, 9, tgUser{ID: 700}, tgIdentity{chat: 700}, func(string) {})
	}
	var orders []store.Order
	s.st.DB.Where("telegram_id = ?", 700).Find(&orders)
	if len(orders) != 1 || len(f.callsOf("createInvoiceLink")) != 1 || len(f.callsOf("sendInvoice")) != 0 || len(f.callsOf("sendMessage")) != 0 {
		t.Fatal("repeated purchase duplicated orders or invoice messages", len(orders), f.calls)
	}
	o := orders[0]
	if o.StarsURL == "" || !hasButton(f, "s:c:"+o.ID) || !strings.Contains(f.textsTo(700), "关闭付款窗口不会取消订单") {
		t.Fatal("missing native payment card or cancel instructions")
	}
	linkCall := f.callsOf("createInvoiceLink")[0]
	if linkCall["currency"] != "XTR" || linkCall["provider_token"] != "" || linkCall["payload"] != o.ID {
		t.Fatal("native Stars checkout changed payment identity", linkCall)
	}
	// Reconstruct the bot as after a restart: the original order and link
	// survive, and selecting continue updates the current message in place.
	restarted := newTGBot(s)
	restarted.token, restarted.base, restarted.me = b.token, b.base, b.me
	restarted.selfID = b.selfID
	restarted.resumeOrder(ctx, []string{"s", "r", o.ID}, 700, 11, func(string) {})
	if len(f.callsOf("createInvoiceLink")) != 1 || len(f.callsOf("sendInvoice")) != 0 {
		t.Fatal("restart/resume sent another invoice")
	}
	edits := f.callsOf("editMessageText")
	if edits[len(edits)-1]["message_id"] != float64(11) {
		t.Fatal("resume did not edit the selected card")
	}
	// Asking does not cancel. The buyer may return to the same checkout.
	b.shopCallback(ctx, []string{"s", "c", o.ID}, 700, 9, tgUser{ID: 700}, tgIdentity{chat: 700}, func(string) {})
	if !hasButton(f, "s:x:"+o.ID) || !hasButton(f, "s:r:"+o.ID) {
		t.Fatal("missing cancel confirmation or return to checkout")
	}
	var current store.Order
	s.st.DB.First(&current, "id = ?", o.ID)
	if current.Status != "pending" {
		t.Fatal("order cancelled before confirmation")
	}
	b.shopCallback(ctx, []string{"s", "x", o.ID}, 700, 9, tgUser{ID: 700}, tgIdentity{chat: 700}, func(string) {})
	edits = f.callsOf("editMessageText")
	last := edits[len(edits)-1]
	markup, _ := json.Marshal(last["reply_markup"])
	if !strings.Contains(last["text"].(string), "订单已取消") || strings.Contains(string(markup), "https://t.me/") {
		t.Fatal("cancellation kept a payment button or showed only an empty list", last)
	}
	if _, err := b.starsLink(ctx, 700, &store.Order{ID: o.ID}); err == nil {
		t.Fatal("cancelled order returned a payment link")
	}
	b.onPreCheckout(&tgPreCheckout{ID: "old-link", From: tgUser{ID: 700}, Currency: "XTR", TotalAmount: 10, Payload: o.ID})
	if f.callsOf("answerPreCheckoutQuery")[0]["ok"] != false {
		t.Fatal("cancelled link passed pre-checkout")
	}
	if _, u, fresh, err := s.fulfil(o.ID, "stars:in-flight", nil); err != nil || !fresh || u.PlanID != p.ID {
		t.Fatal("real in-flight payment was lost after cancellation", err)
	}
	if _, _, fresh, err := s.fulfil(o.ID, "stars:in-flight", nil); err != nil || fresh {
		t.Fatal("repeated payment applied twice", err)
	}
}

func TestPanelStarsCheckoutReuseScopeAndConcurrency(t *testing.T) {
	s, _, p := starsCheckoutFixture(t)
	first, err := s.newOrder(700, "buyer", "", p.ID, payStars)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := s.newOrder(700, "buyer", "", p.ID, payStars)
			if err != nil || o.ID != first.ID || !o.ExpiresAt.Equal(first.ExpiresAt) {
				t.Error("duplicate tap created an order or extended expiry", o, err)
			}
		}()
	}
	wg.Wait()
	for _, who := range []struct {
		chat int64
		user string
	}{{701, ""}, {700, "bound-account"}} {
		o, err := s.newOrder(who.chat, "buyer", who.user, p.ID, payStars)
		if err != nil || o.ID == first.ID {
			t.Fatal("checkout reused across buyer/account", err)
		}
	}
	s.st.DB.Model(p).Update("price_stars", 11)
	changed, err := s.newOrder(700, "buyer", "", p.ID, payStars)
	if err != nil || changed.ID == first.ID || changed.Amount != "11" {
		t.Fatal("new price reused old order", changed, err)
	}
	s.st.DB.Model(&store.Order{}).Where("id = ?", changed.ID).Update("expires_at", time.Now().Add(-time.Minute))
	fresh, err := s.newOrder(700, "buyer", "", p.ID, payStars)
	if err != nil || fresh.ID == changed.ID {
		t.Fatal("expired order was revived", err)
	}
	if _, err = s.cancelOrder(700, fresh.ID); err != nil {
		t.Fatal(err)
	}
	fresh2, err := s.newOrder(700, "buyer", "", p.ID, payStars)
	if err != nil || fresh2.ID == fresh.ID {
		t.Fatal("cancelled order was revived", err)
	}
}

func TestPanelStarsLinkOwnershipAndBotChange(t *testing.T) {
	s, f, p := starsCheckoutFixture(t)
	o, err := s.newOrder(700, "buyer", "", p.ID, payStars)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = s.tg.starsLink(ctx, 701, &store.Order{ID: o.ID}); err == nil || len(f.callsOf("createInvoiceLink")) != 0 {
		t.Fatal("foreign buyer could open the payment")
	}
	if _, err = s.tg.starsLink(ctx, 700, o); err != nil {
		t.Fatal(err)
	}
	s.tg.selfID = 2
	if _, err = s.tg.starsLink(ctx, 700, o); err != nil || len(f.callsOf("createInvoiceLink")) != 2 {
		t.Fatal("new bot reused another bot's invoice link", err)
	}
	raw, _ := json.Marshal(o)
	if strings.Contains(string(raw), "https://t.me/") || strings.Contains(string(raw), "StarsBotID") {
		t.Fatal("private checkout metadata leaked into API responses")
	}
	s.st.DB.Model(o).Update("expires_at", time.Now().Add(-time.Minute))
	if _, err = s.tg.starsLink(ctx, 700, o); err == nil {
		t.Fatal("expired order returned a cached payment link")
	}
}

func TestPanelStarsLinkFailureRetriesOriginalOrder(t *testing.T) {
	s, f, p := starsCheckoutFixture(t)
	var fail atomic.Bool
	fail.Store(true)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/createInvoiceLink") && fail.Load() {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "private-api-response"})
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	s.tg.base = api.URL
	ctx := context.Background()
	purchase := func() {
		s.tg.shopCallback(ctx, []string{"s", "m", p.ID, payStars}, 700, 9, tgUser{ID: 700}, tgIdentity{chat: 700}, func(string) {})
	}
	purchase()
	if strings.Contains(f.textsTo(700), "private-api-response") || !strings.Contains(f.textsTo(700), "重试") {
		t.Fatal("failed invoice leaked upstream details or gave no retry path")
	}
	var before store.Order
	if err := s.st.DB.First(&before, "telegram_id = ?", 700).Error; err != nil {
		t.Fatal(err)
	}
	fail.Store(false)
	purchase()
	var os []store.Order
	s.st.DB.Where("telegram_id = ?", 700).Find(&os)
	if len(os) != 1 || os[0].ID != before.ID || os[0].StarsURL == "" || len(f.callsOf("sendInvoice")) != 0 {
		t.Fatal("failed checkout retry created a new order or invoice", os)
	}
}

func TestPanelStarsTrafficPackReusesCheckout(t *testing.T) {
	if !proBuild {
		t.Skip("traffic pack purchases require the professional build")
	}
	s, _, _ := starsCheckoutFixture(t)
	u := &store.User{ID: "pack-buyer", Username: "pack-buyer", TelegramID: 700, TrafficLimit: 100 << 30}
	if err := s.st.DB.Create(u).Error; err != nil {
		t.Fatal(err)
	}
	p := &store.TrafficPack{ID: "stars-pack", Name: "50 GB", Traffic: 50 << 30, Sale: true, PriceStars: 10}
	if err := s.st.DB.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	first, err := s.newPackOrder(700, "buyer", u, p.ID, payStars)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.newPackOrder(700, "buyer", u, p.ID, payStars)
	if err != nil || first.ID != second.ID || !first.ExpiresAt.Equal(second.ExpiresAt) {
		t.Fatal("repeated pack purchase duplicated checkout", second, err)
	}
	if _, _, fresh, err := s.fulfil(first.ID, "stars:pack-charge", nil); err != nil || !fresh {
		t.Fatal("pack payment failed", err)
	}
	if _, _, fresh, err := s.fulfil(first.ID, "stars:pack-charge", nil); err != nil || fresh {
		t.Fatal("pack was applied twice", err)
	}
	var got store.User
	s.st.DB.First(&got, "id = ?", u.ID)
	if got.ExtraTraffic != p.Traffic {
		t.Fatal("pack payment did not add traffic exactly once", got.ExtraTraffic)
	}
}
