package panel

import (
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestPortalShopHistoryAndClosure(t *testing.T) {
	s, srv, _, _ := paymentSwitchFixture(t)
	u := newUser(&userReq{Username: "shop-alice"})
	u.Email = "shop-alice@example.com"
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	b := newShopClient(t, srv.URL)
	key := strings.Repeat("a", 32)
	b.setCookie(shopWebCookie, key)
	for _, o := range []store.Order{
		{ID: "ownorder1", UserID: u.ID, PlanName: "Own Telegram renewal", Method: payStars, Status: "paid", Amount: "10"},
		{ID: "guestorder1", UserID: "different-user", WebKey: webKeyHash(key), PlanName: "Private guest purchase", Status: "paid", Amount: "20"},
	} {
		if err := s.st.DB.Create(&o).Error; err != nil {
			t.Fatal(err)
		}
	}
	_, page := b.req("GET", shopWebPath, nil, false)
	if !strings.Contains(page, "Private guest purchase") || strings.Contains(page, "Own Telegram renewal") {
		t.Fatal("guest history does not follow receipt")
	}
	token, _ := s.issuePortalMailToken(&u)
	b.setCookie(portalCookie, token)
	_, page = b.req("GET", shopWebPath, nil, false)
	if !strings.Contains(page, "Own Telegram renewal") || strings.Contains(page, "Private guest purchase") {
		t.Fatal("signed-in history mixes other browser receipts")
	}
	s.st.SetSetting("shop_web_enabled", "false")
	code, page := b.req("GET", shopWebPath, nil, false)
	if code != 200 || !strings.Contains(page, `id="lookup"`) || !strings.Contains(page, "Own Telegram renewal") || !strings.Contains(page, "暂未开放") || strings.Contains(page, `data-checkout>`) {
		t.Fatal("closure hides lookup or keeps checkout available")
	}
	if code, _ := b.req("POST", shopWebPath+"/order", map[string]any{"kind": "plan", "id": "switch-plan", "method": payUSDT}, true); code != 503 {
		t.Fatal("closed shop accepted purchase", code)
	}
}

func TestPortalOrderPaymentStates(t *testing.T) {
	s, srv, _, _ := paymentSwitchFixture(t)
	b := newShopClient(t, srv.URL)
	key := strings.Repeat("b", 32)
	b.setCookie(shopWebCookie, key)
	for _, tc := range []struct {
		id, state, expect string
		expired           bool
	}{
		{"expiredorder1", "pending", "已过支付时限", true},
		{"processingorder1", "processing", "款项已收到，正在交付", false},
		{"refundedorder1", "refunded", "已退款", false},
	} {
		expires := time.Now().Add(time.Hour)
		if tc.expired {
			expires = time.Now().Add(-time.Hour)
		}
		o := store.Order{ID: tc.id, PlanName: "测试套餐", Status: tc.state, WebKey: webKeyHash(key), Method: payUSDT, Amount: "1", ExpiresAt: expires}
		if err := s.st.DB.Create(&o).Error; err != nil {
			t.Fatal(err)
		}
		code, page := b.req("GET", shopWebPath+"/order/"+o.ID, nil, false)
		if code != 200 || !strings.Contains(page, tc.expect) || !strings.Contains(page, `id="check-order"`) || strings.Contains(page, `id="cancel"`) {
			t.Fatal("incorrect terminal/processing state", tc.id, code)
		}
		if !strings.Contains(page, `aria-label="手机导航"`) {
			t.Fatal("order lost shared layout")
		}
	}
}
