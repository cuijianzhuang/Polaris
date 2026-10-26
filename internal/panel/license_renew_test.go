package panel

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/license"
)

// The check stores where to renew; the admin is told once per tier before
// the license expires.
func TestLicenseRenewLinksAndExpiryNotices(t *testing.T) {
	s, _, c := newAdminClient(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s.licKeys = []ed25519.PublicKey{pub}
	events := make(chan Event, 20)
	s.sinks = append(s.sinks, events)
	expires := time.Now().UTC().AddDate(0, 0, 5).Format("2006-01-02")
	renew := map[string]any{"renew_url": "https://lic.example.com/shop", "renew_bot": "https://t.me/polaris_shop_bot"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		text, _ := license.Sign(&license.License{ID: "lic_r", Plan: "pro", Instance: req["instance"], IssuedAt: "2026-01-01", ExpiresAt: expires}, priv)
		out := map[string]any{"ok": true, "revoked": false, "license": text}
		for k, v := range renew {
			out[k] = v
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	s.st.SetSetting("license_server", srv.URL)
	if code, out := c.do("POST", "/license/activate", map[string]any{"key": "K"}); code != 200 {
		t.Fatalf("activate %d %v", code, out)
	}
	s.checkLicense()
	_, out := c.do("GET", "/license", nil)
	if r, _ := out["renew"].(map[string]any); r["url"] != "https://lic.example.com/shop" || r["bot"] != "https://t.me/polaris_shop_bot" {
		t.Fatalf("renew links %v", out["renew"])
	}

	// Links that are not HTTPS (or not t.me for the bot) are dropped.
	renew = map[string]any{"renew_url": "javascript:alert(1)", "renew_bot": "https://evil.example.com/x"}
	s.checkLicense()
	if _, out = c.do("GET", "/license", nil); out["renew"] != nil {
		t.Fatalf("unsafe links kept: %v", out["renew"])
	}
	renew = map[string]any{"renew_bot": "https://t.me/polaris_shop_bot"}
	s.checkLicense()

	got := func() []string {
		var out []string
		for {
			select {
			case e := <-events:
				if e.Kind == "license" {
					out = append(out, e.Text)
				}
			default:
				return out
			}
		}
	}
	got()
	// 5 days left: the 7-day notice, once.
	s.remindLicenseExpiry()
	s.remindLicenseExpiry()
	if n := got(); len(n) != 1 || !strings.Contains(n[0], "还有 5 天") || !strings.Contains(n[0], "https://t.me/polaris_shop_bot") {
		t.Fatalf("7-day notice %v", n)
	}
	// The license is renewed: a new term, and nothing until it is near again.
	expires = time.Now().UTC().AddDate(0, 2, 0).Format("2006-01-02")
	s.checkLicense()
	s.remindLicenseExpiry()
	if n := got(); len(n) != 0 {
		t.Fatalf("notice for a renewed license %v", n)
	}
	// Tomorrow, then expired: one notice each.
	expires = time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	s.checkLicense()
	s.remindLicenseExpiry()
	s.remindLicenseExpiry()
	if n := got(); len(n) != 1 || !strings.Contains(n[0], "还有 1 天") {
		t.Fatalf("1-day notice %v", n)
	}
	expires = time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	s.checkLicense()
	s.remindLicenseExpiry()
	s.remindLicenseExpiry()
	if n := got(); len(n) != 1 || !strings.Contains(n[0], "已于") {
		t.Fatalf("expired notice %v", n)
	}
}
