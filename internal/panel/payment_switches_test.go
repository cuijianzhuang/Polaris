package panel

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func paymentSwitchFixture(t *testing.T) (*Server, *httptest.Server, *apiClient, *store.Group) {
	t.Helper()
	s, srv, admin := newAdminClient(t)
	licenseAll(s)
	for k, v := range map[string]string{"shop_enabled": "true", "portal_enabled": "true", "shop_web_enabled": "true",
		"shop_stars": "true", "shop_epay_url": "https://pay.example.com", "shop_epay_pid": "1001", "shop_epay_types": "alipay,wxpay,qqpay,custom", "shop_usdt_address": "TTestAddress"} {
		s.st.SetSetting(k, v)
	}
	s.st.SetSetting("shop_epay_key", s.st.Box.Seal("switch-test-key"))
	p := &store.Group{ID: "switch-plan", Name: "测试套餐", Plan: true, Sale: true, PriceStars: 10, PriceCNY: 1, PriceUSDT: 1, Days: 30, TrafficLimit: 10 << 30}
	if err := s.st.DB.Create(p).Error; err != nil {
		t.Fatal(err)
	}
	return s, srv, admin, p
}

func switchButton(rows [][]button, data string) bool {
	for _, row := range rows {
		if slices.ContainsFunc(row, func(b button) bool { return b.Data == data }) {
			return true
		}
	}
	return false
}

func TestPaymentSwitchSettingsPersistWithoutClearingCredentials(t *testing.T) {
	s, _, admin, _ := paymentSwitchFixture(t)
	key := s.st.GetSetting("shop_epay_key", "")
	_, old := admin.do("GET", "/settings", nil)
	for _, k := range []string{"shop_epay_enabled", "shop_usdt_enabled", "shop_code_enabled"} {
		if old[k] != "true" {
			t.Fatal("legacy default changed", k, old[k])
		}
	}
	values := map[string]string{"shop_epay_enabled": "false", "shop_usdt_enabled": "false", "shop_code_enabled": "false", "shop_epay_disabled_types": `["alipay","custom"]`, "shop_epay_key": ""}
	if code, out := admin.do("PUT", "/settings", values); code != 200 || out["shop_epay_enabled"] != "false" {
		t.Fatal("switch settings were not saved", code, out)
	}
	if s.epayOn() || s.usdtOn() || s.codesOn() || !slices.Equal(s.epayTypes(), []string{"wxpay", "qqpay"}) {
		t.Fatal("saved switches did not take effect")
	}
	if s.st.GetSetting("shop_epay_key", "") != key || s.st.GetSetting("shop_usdt_address", "") != "TTestAddress" || s.st.GetSetting("shop_epay_types", "") != "alipay,wxpay,qqpay,custom" {
		t.Fatal("turning off payment erased configuration")
	}
	for _, invalid := range []map[string]string{{"shop_epay_enabled": "1"}, {"shop_usdt_enabled": "yes"}, {"shop_code_enabled": ""}, {"shop_epay_disabled_types": "null"}, {"shop_epay_disabled_types": `[42]`}, {"shop_epay_disabled_types": `[" alipay"]`}} {
		if _, exists := invalid["shop_code_enabled"]; !exists {
			invalid["shop_code_enabled"] = "true"
		}
		if code, _ := admin.do("PUT", "/settings", invalid); code != 400 || s.codesOn() {
			t.Fatal("invalid settings partially changed payment configuration", invalid, code)
		}
	}
}

func TestPaymentCodeSwitchHidesAndBlocksRedemption(t *testing.T) {
	s, srv, _, p := paymentSwitchFixture(t)
	code := "SWITCH-TEST-CODE"
	s.st.DB.Create(&store.RedeemCode{Code: code, PlanID: p.ID})
	s.st.SetSetting("shop_code_enabled", "false")
	buyer := newShopClient(t, srv.URL)
	if _, body := buyer.req("GET", shopWebPath, nil, false); strings.Contains(body, `id="redeem"`) {
		t.Fatal("disabled redemption form still visible")
	}
	if status, _ := buyer.req("POST", shopWebPath+"/redeem", map[string]any{"code": code}, true); status != 400 {
		t.Fatal("direct web request bypassed redemption switch", status)
	}
	if _, _, err := s.redeem(700, "buyer", "", code); err == nil {
		t.Fatal("direct redemption bypassed switch")
	}
	_, rows := s.tg.shopView(tgIdentity{chat: 700})
	if switchButton(rows, "v:redeem") || switchButton(s.tg.homeButtons(tgIdentity{chat: 700}), "v:redeem") {
		t.Fatal("bot still offers disabled redemption")
	}
	if text, _ := s.tg.view("redeem", "", tgIdentity{chat: 700}); !strings.Contains(text, "停用") {
		t.Fatal("old redemption button still works")
	}
	var rc store.RedeemCode
	s.st.DB.First(&rc, "code = ?", code)
	if rc.UsedAt != nil {
		t.Fatal("blocked redemption consumed the code")
	}
	s.st.SetSetting("shop_code_enabled", "true")
	if status, _ := buyer.req("POST", shopWebPath+"/redeem", map[string]any{"code": code}, true); status != 200 {
		t.Fatal("re-enabling did not restore unused codes", status)
	}
}

func TestPaymentSwitchesBlockNewAndResumedPurchases(t *testing.T) {
	if !proBuild {
		t.Skip("online payments require the professional build")
	}
	for _, method := range []string{payStars, payEPay, payUSDT} {
		t.Run(method, func(t *testing.T) {
			s, srv, _, p := paymentSwitchFixture(t)
			o, err := s.newOrder(700, "buyer", "", p.ID, method)
			if err != nil {
				t.Fatal(err)
			}
			flag := map[string]string{payStars: "shop_stars", payEPay: "shop_epay_enabled", payUSDT: "shop_usdt_enabled"}[method]
			s.st.SetSetting(flag, "false")
			if _, err = s.newOrder(700, "buyer", "", p.ID, method); err == nil {
				t.Fatal("disabled method created a new order")
			}
			_, rows := s.tg.planCard(tgIdentity{chat: 700}, p.ID)
			for _, row := range rows {
				for _, b := range row {
					if strings.HasPrefix(b.Data, "s:m:"+p.ID+":"+method) {
						t.Fatal("disabled payment still offered by bot", b.Data)
					}
				}
			}
			_, rows = s.tg.unpaidView(700)
			if switchButton(rows, "s:r:"+o.ID) || !switchButton(rows, "s:c:"+o.ID) {
				t.Fatal("disabled method still resumable or cannot be cancelled")
			}
			answer := ""
			s.tg.resumeOrder(context.Background(), []string{"s", "r", o.ID}, 700, 9, func(v string) { answer = v })
			if !strings.Contains(answer, "停用") {
				t.Fatal("stale resume button bypassed switch", answer)
			}
			if method != payStars {
				buyer := newShopClient(t, srv.URL)
				if _, body := buyer.req("GET", shopWebPath, nil, false); strings.Contains(body, `data-method="`+method+`"`) {
					t.Fatal("disabled method still offered by web shop")
				}
				if status, _ := buyer.req("POST", shopWebPath+"/order", map[string]any{"id": p.ID, "kind": "plan", "method": method, "type": "alipay"}, true); status != 400 {
					t.Fatal("direct purchase bypassed switch", status)
				}
			}
			s.st.SetSetting(flag, "true")
			if !s.methodOn(method) {
				t.Fatal("method could not be re-enabled")
			}
		})
	}
}

func TestPaymentEPaySubchannelSwitchRejectsStaleButtons(t *testing.T) {
	if !proBuild {
		t.Skip("EPay requires the professional build")
	}
	s, srv, _, p := paymentSwitchFixture(t)
	s.st.SetSetting("shop_epay_disabled_types", `["alipay","custom"]`)
	if !s.epayOn() || !s.starsOn() || !s.usdtOn() {
		t.Fatal("subchannel switch disabled unrelated payment methods")
	}
	_, rows := s.tg.planCard(tgIdentity{chat: 700}, p.ID)
	if switchButton(rows, "s:m:"+p.ID+":epay:alipay") || !switchButton(rows, "s:m:"+p.ID+":epay:wxpay") {
		t.Fatal("bot did not filter individual EPay channels")
	}
	answer := ""
	s.tg.shopCallback(context.Background(), []string{"s", "m", p.ID, payEPay, "alipay"}, 700, 9, tgUser{ID: 700}, tgIdentity{chat: 700}, func(v string) { answer = v })
	var n int64
	s.st.DB.Model(&store.Order{}).Count(&n)
	if !strings.Contains(answer, "停用") || n != 0 {
		t.Fatal("stale Alipay button created an order", answer, n)
	}
	buyer := newShopClient(t, srv.URL)
	if status, _ := buyer.req("POST", shopWebPath+"/order", map[string]any{"id": p.ID, "method": payEPay, "type": "alipay"}, true); status != 400 {
		t.Fatal("direct Alipay request bypassed subchannel switch", status)
	}
	o, err := s.newOrder(700, "buyer", "", p.ID, payEPay)
	if err != nil || s.payLink(o, "alipay") != "" || s.payLink(o, "wxpay") == "" {
		t.Fatal("payment URL generation ignored subchannel state", err)
	}
	s.st.SetSetting("shop_epay_disabled_types", `["alipay","wxpay","qqpay","custom"]`)
	if s.epayOn() || !s.starsOn() || !s.usdtOn() {
		t.Fatal("all subchannels disabled did not disable EPay alone")
	}
}

func TestPaymentSwitchesDoNotDiscardVerifiedReceipts(t *testing.T) {
	if !proBuild {
		t.Skip("online payment receipt processing requires the professional build")
	}
	s, srv, _, p := paymentSwitchFixture(t)
	orders := map[string]*store.Order{}
	for i, method := range []string{payStars, payEPay, payUSDT} {
		o, err := s.newOrder(int64(700+i), "buyer", "", p.ID, method)
		if err != nil {
			t.Fatal(err)
		}
		orders[method] = o
	}
	for _, flag := range []string{"shop_stars", "shop_epay_enabled", "shop_usdt_enabled"} {
		s.st.SetSetting(flag, "false")
	}
	s.st.SetSetting("shop_epay_disabled_types", `["alipay"]`)
	f := &fakeTG{}
	api := httptest.NewServer(f)
	defer api.Close()
	s.tg.token, s.tg.base = "123:abc", api.URL
	stars := orders[payStars]
	s.tg.onPreCheckout(&tgPreCheckout{ID: "off", From: tgUser{ID: stars.TelegramID}, Payload: stars.ID, Currency: "XTR", TotalAmount: 10})
	if f.callsOf("answerPreCheckoutQuery")[0]["ok"] != false {
		t.Fatal("disabled Stars method accepted another checkout")
	}
	s.tg.onStarsPaid(stars.TelegramID, &tgPayment{Payload: stars.ID, Currency: "XTR", TotalAmount: 10, ChargeID: "switch-paid"})
	// A genuine EPay callback is accepted even after its channel is closed.
	ep := orders[payEPay]
	v := url.Values{"pid": {"1001"}, "trade_status": {"TRADE_SUCCESS"}, "out_trade_no": {ep.ID}, "trade_no": {"switch-paid"}, "money": {ep.Amount}, "type": {"alipay"}}
	var keys []string
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var fields []string
	for _, k := range keys {
		fields = append(fields, k+"="+v.Get(k))
	}
	digest := md5.Sum([]byte(strings.Join(fields, "&") + "switch-test-key"))
	v.Set("sign", hex.EncodeToString(digest[:]))
	resp, err := http.Get(srv.URL + epayNotifyPath + "?" + v.Encode())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "success" {
		t.Fatal("payment switch rejected verified EPay receipt", resp.StatusCode)
	}
	usdt := orders[payUSDT]
	a, _ := strconv.ParseFloat(usdt.Amount, 64)
	tron := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []any{map[string]any{
			"transaction_id": "switch-paid", "to": "TTestAddress", "value": fmt.Sprint(int64(math.Round(a * 1e6))), "block_timestamp": time.Now().UnixMilli(),
			"token_info": map[string]any{"address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "decimals": 6},
		}}})
	}))
	defer tron.Close()
	s.st.SetSetting("shop_usdt_api", tron.URL)
	s.pollUSDT()
	for method, o := range orders {
		var got store.Order
		s.st.DB.First(&got, "id = ?", o.ID)
		if got.Status != "paid" || got.UserID == "" {
			t.Fatal("disabled method lost its real receipt", method, got.Status)
		}
	}
}
