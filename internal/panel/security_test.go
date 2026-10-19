package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Regression tests for the pre-release security review.

type apiClient struct {
	t    *testing.T
	base string
	tok  string
}

func (a *apiClient) do(method, path string, body any) (int, map[string]any) {
	a.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	if a.tok != "" {
		req.Header.Set("Authorization", "Bearer "+a.tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func newAdminClient(t *testing.T) (*Server, *httptest.Server, *apiClient) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)
	h, _ := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	s.st.DB.Create(&store.Admin{ID: "a1", Username: "root", PasswordHash: string(h)})
	c := &apiClient{t: t, base: srv.URL + s.cfg.APIBase()}
	code, out := c.do("POST", "/auth/login", map[string]string{"username": "root", "password": "old-password"})
	if code != 200 {
		t.Fatalf("login %d %v", code, out)
	}
	c.tok = out["token"].(string)
	// Re-verified, as an admin is right after confirming the password: the
	// sensitive routes are open (TestSudo covers the gate itself).
	if code, out := c.do("POST", "/auth/sudo", map[string]string{"password": "old-password"}); code != 200 {
		t.Fatalf("sudo %d %v", code, out)
	}
	return s, srv, c
}

func TestTOTPRebindKeepsProtection(t *testing.T) {
	s, _, c := newAdminClient(t)
	code, out := c.do("POST", "/auth/totp/setup", nil)
	if code != 200 {
		t.Fatalf("setup %d", code)
	}
	first := out["secret"].(string)
	now, _ := totp.GenerateCode(first, time.Now())
	if code, out := c.do("POST", "/auth/totp/enable", map[string]any{"code": now, "enable": true}); code != 200 || len(out["recoveryCodes"].([]any)) != recoveryCount {
		t.Fatalf("enable %d %v", code, out)
	}
	// Starting a re-bind must not switch 2FA off.
	c.do("POST", "/auth/totp/setup", nil)
	var a store.Admin
	s.st.DB.First(&a, "id = ?", "a1")
	if !a.TOTPEnabled || a.TOTPPending == "" {
		t.Fatalf("re-bind disabled TOTP: %+v", a)
	}
	// Login still needs the old authenticator until the new one is confirmed.
	anon := &apiClient{t: t, base: c.base}
	if code, out := anon.do("POST", "/auth/login", map[string]string{"username": "root", "password": "old-password"}); code != 401 || out["totpRequired"] != true {
		t.Fatalf("login without code: %d %v", code, out)
	}
	if code, _ := anon.do("POST", "/auth/login", map[string]string{"username": "root", "password": "old-password", "totp": now}); code != 200 {
		t.Fatalf("old authenticator should still work: %d", code)
	}
}

func TestPasswordChangeRevokesSessions(t *testing.T) {
	_, srv, c := newAdminClient(t)
	other := &apiClient{t: t, base: c.base, tok: c.tok} // e.g. a stolen token
	code, out := c.do("POST", "/auth/password", map[string]string{"old": "old-password", "new": "new-password-123"})
	if code != 200 {
		t.Fatalf("change password %d %v", code, out)
	}
	c.tok = out["token"].(string)
	if code, _ := c.do("GET", "/auth/me", nil); code != 200 {
		t.Fatalf("current session should continue: %d", code)
	}
	if code, _ := other.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("old token still valid: %d", code)
	}
	// ?token= is only for the WebSocket endpoint.
	resp, _ := http.Get(c.base + "/nodes?token=" + c.tok)
	if resp.StatusCode != 401 {
		t.Fatalf("query token accepted on REST endpoint: %d", resp.StatusCode)
	}
	_ = srv
}

func TestTelegramIgnoresGroupChats(t *testing.T) {
	fake := &fakeTG{}
	api := httptest.NewServer(fake)
	defer api.Close()
	s := newTestServer(t)
	s.st.SetSetting("tg_enabled", "true")
	s.st.SetSetting("tg_token", s.st.Box.Seal("123:abc"))
	s.st.SetSetting("tg_api_base", api.URL)
	s.st.SetSetting("tg_poll_timeout", "0")
	s.st.DB.Create(&store.User{ID: "u1", Username: "alice", UUID: "x", Status: "active", SubToken: "alice-subscription-token"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.tg.run(ctx)
	waitUntil(t, "bot running", s.tg.isRunning)

	group := func(chat, from int64, text string) map[string]any {
		return map[string]any{"message": map[string]any{"message_id": 1, "from": map[string]any{"id": from},
			"chat": map[string]any{"id": chat, "type": "supergroup"}, "text": text}}
	}
	s.tg.codes["OWNERCODE1"] = bindCode{Role: "owner", Expires: time.Now().Add(time.Minute)}
	fake.push(group(-1001, 100, "/bind OWNERCODE1"))
	fake.push(group(-1001, 555, "/users alice"))
	waitUntil(t, "refusal", func() bool { return strings.Count(fake.textsTo(-1001), "请私聊") == 2 })
	var n int64
	s.st.DB.Model(&store.TGAdmin{}).Count(&n)
	if n != 0 || strings.Contains(fake.textsTo(-1001), "alice") {
		t.Fatalf("group chat was served: admins=%d\n%s", n, fake.textsTo(-1001))
	}
	// A callback pressed by someone other than the private chat owner is ignored.
	s.st.DB.Create(&store.TGAdmin{ID: "t1", ChatID: 100, Role: "owner"})
	fake.push(map[string]any{"callback_query": map[string]any{"id": "cb", "from": map[string]any{"id": 999}, "data": "u:t:u1",
		"message": map[string]any{"message_id": 9, "chat": map[string]any{"id": 100, "type": "private"}}}})
	time.Sleep(300 * time.Millisecond)
	if len(fake.answers) != 0 {
		t.Fatalf("foreign callback handled: %v", fake.answers)
	}
}

func TestSecurityHeadersAndHost(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/sub/nonexistent-token-123456")
	for k, v := range map[string]string{"Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff"} {
		if resp.Header.Get(k) != v {
			t.Fatalf("%s = %q", k, resp.Header.Get(k))
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+"/install.sh", nil)
	req.Host = "evil.com$(id)"
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Fatalf("malicious Host accepted: %d", resp.StatusCode)
	}
}

func TestClientIPUsesProxyAppendedEntry(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.9") // client forged 1.1.1.1
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("clientIP = %s", got)
	}
	r.RemoteAddr = "198.51.100.1:5555" // public peer: headers ignored
	if got := clientIP(r); got != "198.51.100.1" {
		t.Fatalf("clientIP = %s", got)
	}
}

func TestNodeCannotReportForeignUsers(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "n1", DesiredRev: 1, AppliedRev: 1, TrafficRate: 1})
	spec, _ := json.Marshal(model.NodeSpec{Inbounds: []model.Inbound{{Tag: "a", Users: []model.User{{ID: "mine"}}}}})
	s.st.DB.Create(&store.ConfigRevision{NodeID: "n1", Rev: 1, Spec: s.st.Box.Seal(string(spec)), Status: "applied"})
	s.st.DB.Create(&store.User{ID: "mine", Username: "mine", UUID: "1", SubToken: "t1-000000000000000", Status: "active"})
	s.st.DB.Create(&store.User{ID: "victim", Username: "victim", UUID: "2", SubToken: "t2-000000000000000", Status: "active", IPLimit: 1})
	if set := s.nodeUsers("n1"); !set["mine"] || set["victim"] {
		t.Fatalf("node users: %v", set)
	}
	s.handleOnlineIPs("n1", &polarisv1.OnlineIPs{Users: []*polarisv1.UserIPs{{UserId: "victim", Ips: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}}}})
	_, tracked := s.shared.onlineIPs([]string{"victim"})["victim"]
	if tracked {
		t.Fatal("online IPs accepted for a user the node does not serve")
	}
}

func TestWeakSecretRejected(t *testing.T) {
	for _, key := range []string{"short", "change-me-to-a-long-random-string"} {
		os.Setenv("POLARIS_SECRET_KEY", key)
		if _, err := LoadConfig(""); err == nil {
			t.Fatalf("secret %q accepted", key)
		}
	}
	os.Setenv("POLARIS_SECRET_KEY", "0123456789abcdef0123")
	if _, err := LoadConfig(""); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("POLARIS_SECRET_KEY")
}
