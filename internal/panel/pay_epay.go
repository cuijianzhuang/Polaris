package panel

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// EPay (易支付) is the payment-aggregator protocol many Chinese gateways
// speak: the buyer is sent to {api}/submit.php with signed parameters, and
// the gateway calls notify_url back with the result, signed the same way:
// md5 of the non-empty parameters (sign and sign_type left out) sorted by
// name as k=v&k=v, followed by the merchant key.

const epayNotifyPath = "/pay/epay/notify"

func epaySign(params url.Values, key string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k != "sign" && k != "sign_type" && params.Get(k) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k + "=" + params.Get(k))
	}
	sum := md5.Sum([]byte(b.String() + key))
	return hex.EncodeToString(sum[:])
}

func (s *Server) epayNotifyURL() string { return s.panelBase() + epayNotifyPath }

// epayLink is where the buyer pays order o with payment type typ (alipay,
// wxpay, …).
func (s *Server) epayLink(o *store.Order, typ string) string {
	api := strings.TrimRight(strings.TrimSpace(s.st.GetSetting("shop_epay_url", "")), "/")
	api = strings.TrimSuffix(api, "/submit.php")
	ret := s.panelBase()
	if bot := s.tg.username(); bot != "" {
		ret = "https://t.me/" + bot
	}
	v := url.Values{
		"pid": {s.st.GetSetting("shop_epay_pid", "")}, "type": {typ}, "out_trade_no": {o.ID},
		"notify_url": {s.epayNotifyURL()}, "return_url": {ret},
		"name": {s.st.GetSetting("site_title", "Polaris") + " " + o.PlanName}, "money": {o.Amount},
	}
	v.Set("sign", epaySign(v, s.st.Box.MustOpen(s.st.GetSetting("shop_epay_key", ""))))
	v.Set("sign_type", "MD5")
	return api + "/submit.php?" + v.Encode()
}

// epayNotify is the gateway's callback (GET or POST). It answers "success"
// once the payment is recorded so the gateway stops retrying.
func (s *Server) epayNotify(c *gin.Context) {
	_ = c.Request.ParseForm()
	v := c.Request.Form // query and form body
	key := s.st.Box.MustOpen(s.st.GetSetting("shop_epay_key", ""))
	if key == "" || v.Get("pid") != s.st.GetSetting("shop_epay_pid", "") ||
		subtle.ConstantTimeCompare([]byte(strings.ToLower(v.Get("sign"))), []byte(epaySign(v, key))) != 1 {
		c.String(http.StatusBadRequest, "fail")
		return
	}
	if v.Get("trade_status") != "TRADE_SUCCESS" {
		c.String(http.StatusOK, "success") // nothing to do for other states
		return
	}
	var o store.Order
	if s.st.DB.First(&o, "id = ? AND method = ?", v.Get("out_trade_no"), payEPay).Error != nil {
		c.String(http.StatusBadRequest, "fail")
		return
	}
	paid, err1 := strconv.ParseFloat(v.Get("money"), 64)
	want, err2 := strconv.ParseFloat(o.Amount, 64)
	if err1 != nil || err2 != nil || math.Abs(paid-want) > 0.001 {
		s.log.Warn("epay amount mismatch", "order", o.ID, "paid", v.Get("money"), "want", o.Amount)
		c.String(http.StatusBadRequest, "fail")
		return
	}
	ref := "epay:" + orDefault(v.Get("trade_no"), o.ID)
	if _, _, _, err := s.fulfil(o.ID, ref, nil); err != nil {
		s.log.Warn("epay fulfil", "order", o.ID, "err", err)
		// The payment is on record (failed orders keep it); stop the retries.
	}
	c.String(http.StatusOK, "success")
}
