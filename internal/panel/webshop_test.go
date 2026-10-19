package panel

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func newShopClient(t *testing.T, base string) *portalClient {
	jar, _ := cookiejar.New(nil)
	return &portalClient{t: t, base: base, hc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// TestWebShopCommunity: the user center's web shop lists plans on sale and,
// without online payments, sells them by redeem code. A buyer without an
// account gets one and its subscription link on the order page, which only
// this browser (or the saved ?k= link) can open.
func TestWebShopCommunity(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	licenseNone(s)
	buyer := newShopClient(t, srv.URL)
	if code, _ := buyer.req("GET", shopWebPath, nil, false); code != 404 {
		t.Fatalf("shop open while the user center is off: %d", code)
	}
	for k, v := range map[string]string{"portal_enabled": "true", "shop_enabled": "true", "shop_terms": "虚拟商品不退款", "shop_epay_url": "https://pay.example.com",
		"shop_epay_pid": "1", "shop_usdt_address": "TAddr"} {
		s.st.SetSetting(k, v)
	}
	s.st.SetSetting("shop_epay_key", s.st.Box.Seal("k"))
	code, out := admin.do("POST", "/plans", map[string]any{"name": "基础版", "level": 1, "trafficLimit": 50 << 30, "days": 30, "sale": true, "priceCny": 10})
	if code != 200 {
		t.Fatalf("plan %d %v", code, out)
	}
	plan := out["id"].(string)
	if _, body := buyer.req("GET", shopWebPath, nil, false); !strings.Contains(body, "暂未开放") {
		t.Fatal("web shop open before its switch")
	}
	s.st.SetSetting("shop_web_enabled", "true")
	_, body := buyer.req("GET", shopWebPath, nil, false)
	if !strings.Contains(body, "基础版") || !strings.Contains(body, "兑换码") || strings.Contains(body, "支付宝") || !strings.Contains(body, "虚拟商品不退款") {
		t.Fatalf("shop page: %s", body)
	}
	if code, _ := buyer.req("POST", shopWebPath+"/order", map[string]any{"kind": "plan", "id": plan, "method": payEPay, "type": "alipay"}, true); code != 400 {
		t.Fatalf("online payment without the license: %d", code)
	}
	code, out = admin.do("POST", "/redeem-codes", map[string]any{"planId": plan, "count": 1})
	if code != 200 {
		t.Fatalf("codes %d", code)
	}
	rc := out["codes"].([]any)[0].(string)
	if code, _ := buyer.req("POST", shopWebPath+"/redeem", map[string]any{"code": rc}, false); code != 403 {
		t.Fatalf("redeem without the CSRF header: %d", code)
	}
	code, body = buyer.req("POST", shopWebPath+"/redeem", map[string]any{"code": rc}, true)
	if code != 200 {
		t.Fatalf("redeem %d %s", code, body)
	}
	var res struct{ ID, URL string }
	json.Unmarshal([]byte(body), &res)
	var o store.Order
	s.st.DB.First(&o, "id = ?", res.ID)
	var u store.User
	if o.Status != "paid" || o.WebKey == "" || s.st.DB.First(&u, "id = ?", o.UserID).Error != nil || u.PlanID != plan || u.TelegramID != 0 {
		t.Fatalf("redeemed order %+v user %+v", o, u)
	}
	code, page := buyer.req("GET", res.URL, nil, false)
	if code != 200 || !strings.Contains(page, u.Username) || !strings.Contains(page, u.SubToken) {
		t.Fatalf("order page %d: %s", code, page)
	}
	stranger := newShopClient(t, srv.URL)
	if code, body := stranger.req("GET", res.URL, nil, false); code != 404 || strings.Contains(body, u.SubToken) {
		t.Fatal("another browser read the order")
	}
	link := regexp.MustCompile(`\?k=([0-9a-f]{32})`).FindStringSubmatch(page)
	if len(link) != 2 {
		t.Fatal("no saved order link")
	}
	if code, body := stranger.req("GET", res.URL+"?k="+link[1], nil, false); code != 200 || !strings.Contains(body, u.SubToken) {
		t.Fatal("saved link did not open the order")
	}
	if code, _ := stranger.req("GET", res.URL, nil, false); code != 200 {
		t.Fatal("saved link not remembered")
	}
	if _, body := buyer.req("GET", shopWebPath, nil, false); !strings.Contains(body, res.ID) {
		t.Fatal("my orders missing")
	}
	// Back from a payment gateway: web orders return to their page.
	if code, _ := buyer.req("GET", epayReturnPath+"?out_trade_no="+res.ID, nil, false); code != 302 {
		t.Fatalf("pay return %d", code)
	}
}
