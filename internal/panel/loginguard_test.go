package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// from sends a request as if from ip (the test server sits on loopback, a
// trusted proxy, so X-Forwarded-For names the client).
func from(t *testing.T, c *apiClient, ip, method, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, c.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0) Chrome/130")
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestIPBan(t *testing.T) {
	s, _, admin := newAdminClient(t)
	ch := events(s)
	anon := &apiClient{t: t, base: admin.base}
	bad := map[string]string{"username": "nobody", "password": "x"}
	for i := 0; i < 10; i++ {
		from(t, anon, "198.51.100.7", "POST", "/auth/login", bad)
	}
	if code, out := from(t, anon, "198.51.100.7", "POST", "/auth/login", map[string]string{"username": "root", "password": "old-password"}); code != 429 || out["bannedUntil"] == nil {
		t.Fatalf("banned IP signed in: %d %v", code, out)
	}
	if txt := next(t, ch); !strings.Contains(txt, "198.51.100.7") {
		t.Fatalf("ban alert: %q", txt)
	}
	// Another address is unaffected; the ban grows the next time.
	if code, _ := from(t, anon, "198.51.100.8", "POST", "/auth/login", map[string]string{"username": "root", "password": "old-password"}); code != 200 {
		t.Fatalf("other IP: %d", code)
	}
	var b store.LoginBan
	s.st.DB.First(&b, "ip = ?", "198.51.100.7")
	first := b.Until.Sub(b.UpdatedAt)
	past := time.Now().Add(-time.Second)
	s.st.DB.Model(&b).Update("until", past)
	for i := 0; i < 10; i++ {
		from(t, anon, "198.51.100.7", "POST", "/auth/login", bad)
	}
	s.st.DB.First(&b, "ip = ?", "198.51.100.7")
	if b.Bans != 2 || b.Until.Sub(b.UpdatedAt) < 2*first-time.Second {
		t.Fatalf("second ban not longer: %+v (first %s)", b, first)
	}
	// Unban from the security page.
	if code, _ := admin.do("DELETE", "/security/bans/198.51.100.7", nil); code != 204 {
		t.Fatalf("unban %d", code)
	}
	if code, _ := from(t, anon, "198.51.100.7", "POST", "/auth/login", map[string]string{"username": "root", "password": "old-password"}); code != 200 {
		t.Fatalf("after unban: %d", code)
	}
	// Allowlisted addresses are never banned.
	s.st.SetSetting("login_allowlist", "203.0.113.0/24")
	for i := 0; i < 12; i++ {
		from(t, anon, "203.0.113.5", "POST", "/auth/login", bad)
	}
	if code, _ := from(t, anon, "203.0.113.5", "POST", "/auth/login", map[string]string{"username": "root", "password": "old-password"}); code != 200 {
		t.Fatalf("allowlisted IP banned: %d", code)
	}
}

func TestSessionsAndSudo(t *testing.T) {
	s, _, admin := newAdminClient(t)
	ch := events(s)
	login := func(ip string) *apiClient {
		c := &apiClient{t: t, base: admin.base}
		code, out := from(t, c, ip, "POST", "/auth/login", map[string]string{"username": "root", "password": "old-password"})
		if code != 200 {
			t.Fatalf("login %d %v", code, out)
		}
		c.tok = out["token"].(string)
		return c
	}
	other := login("192.0.2.50") // a new address: alert
	if txt := next(t, ch); !strings.Contains(txt, "192.0.2.50") || !strings.Contains(txt, "新地址") {
		t.Fatalf("new-IP alert: %q", txt)
	}
	login("192.0.2.50") // known now: no alert
	select {
	case e := <-ch:
		t.Fatalf("second login from a known IP alerted: %q", e.Text)
	default:
	}

	// A fresh session needs re-verification for sensitive actions.
	if code, out := other.do("POST", "/system/access", map[string]string{"action": "set-port", "value": "8443"}); code != 403 || out["sudoRequired"] != true {
		t.Fatalf("sensitive action without sudo: %d %v", code, out)
	}
	if code, _ := other.do("PUT", "/settings", map[string]string{"login_allowlist": "0.0.0.0/0"}); code != 403 {
		t.Fatalf("allowlist change without sudo: %d", code)
	}
	if code, _ := other.do("PUT", "/settings", map[string]string{"site_title": "x"}); code != 204 && code != 200 {
		t.Fatalf("ordinary setting: %d", code)
	}
	if code, _ := other.do("POST", "/auth/sudo", map[string]string{"password": "wrong"}); code != 401 {
		t.Fatalf("sudo with a wrong password: %d", code)
	}
	if code, _ := other.do("POST", "/auth/sudo", map[string]string{"password": "old-password"}); code != 200 {
		t.Fatalf("sudo: %d", code)
	}
	if code, _ := other.do("PUT", "/settings", map[string]string{"login_allowlist": "192.0.2.0/24"}); code != 204 && code != 200 {
		t.Fatalf("allowlist after sudo: %d", code)
	}

	// Sessions: listed, the current one marked; revoking ends it at once.
	req, _ := http.NewRequest("GET", admin.base+"/auth/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+admin.tok)
	resp, _ := http.DefaultClient.Do(req)
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 3 {
		t.Fatalf("sessions: %v", list)
	}
	var otherID string
	for _, x := range list {
		if x["ip"] == "192.0.2.50" && otherID == "" {
			otherID = x["id"].(string)
		}
	}
	if code, _ := admin.do("DELETE", "/auth/sessions/"+otherID, nil); code != 204 {
		t.Fatalf("revoke %d", code)
	}
	if code, out := admin.do("POST", "/auth/sessions/revoke-others", nil); code != 200 || out["revoked"].(float64) != 1 {
		t.Fatalf("revoke others: %d %v", code, out)
	}
	if code, _ := other.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("revoked session still works: %d", code)
	}
	if code, _ := admin.do("GET", "/auth/me", nil); code != 200 {
		t.Fatalf("current session ended: %d", code)
	}
	if code, _ := admin.do("POST", "/auth/logout", nil); code != 204 {
		t.Fatal("logout")
	}
	if code, _ := admin.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("logged-out session still works: %d", code)
	}
}

func TestRecoveryCodes(t *testing.T) {
	s, _, c := newAdminClient(t)
	_, out := c.do("POST", "/auth/totp/setup", nil)
	secret := out["secret"].(string)
	code, _ := totp.GenerateCode(secret, time.Now())
	_, out = c.do("POST", "/auth/totp/enable", map[string]any{"code": code, "enable": true})
	codes := out["recoveryCodes"].([]any)
	rc := codes[3].(string)
	anon := &apiClient{t: t, base: c.base}
	if st, _ := anon.do("POST", "/auth/login", map[string]string{"username": "root", "password": "old-password", "totp": strings.ToUpper(rc)}); st != 200 {
		t.Fatalf("recovery code login: %d", st)
	}
	if st, _ := anon.do("POST", "/auth/login", map[string]string{"username": "root", "password": "old-password", "totp": rc}); st != 401 {
		t.Fatalf("recovery code reused: %d", st)
	}
	var a store.Admin
	s.st.DB.First(&a, "id = ?", "a1")
	if len(a.RecoveryCodes.V) != recoveryCount-1 {
		t.Fatalf("codes left %d", len(a.RecoveryCodes.V))
	}
	if _, me := c.do("GET", "/auth/me", nil); me["recoveryCodesLeft"].(float64) != recoveryCount-1 {
		t.Fatalf("me: %v", me)
	}
	// With TOTP on, sudo takes a code, not the password.
	fresh := &apiClient{t: t, base: c.base}
	now, _ := totp.GenerateCode(secret, time.Now())
	_, out = fresh.do("POST", "/auth/login", map[string]string{"username": "root", "password": "old-password", "totp": now})
	fresh.tok = out["token"].(string)
	if st, _ := fresh.do("POST", "/auth/sudo", map[string]string{"password": "old-password"}); st != 401 {
		t.Fatalf("password sudo with TOTP on: %d", st)
	}
	if st, _ := fresh.do("POST", "/auth/sudo", map[string]string{"code": codes[5].(string)}); st != 200 {
		t.Fatalf("recovery code sudo: %d", st)
	}
	if st, out := fresh.do("POST", "/auth/totp/recovery-codes", nil); st != 200 || len(out["recoveryCodes"].([]any)) != recoveryCount {
		t.Fatalf("regenerate %d %v", st, out)
	}
}
