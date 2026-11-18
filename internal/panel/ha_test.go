package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestHAJoinTokenFlow covers the whole join-token lifecycle: minting via the
// admin API, fetching the join config with the token alone (no admin login),
// registering a peer (which consumes the token), and minting a new token
// voiding unused ones.
func TestHAJoinTokenFlow(t *testing.T) {
	s, _, c := newAdminClient(t)
	anon := &apiClient{t: t, base: c.base}

	code, out := c.do("POST", "/ha/token", nil)
	if code != 200 || out["token"] == "" {
		t.Fatalf("issue token %d %v", code, out)
	}
	tok := out["token"].(string)
	if !strings.Contains(out["command"].(string), "ha-join "+tok) {
		t.Fatalf("join command does not carry the token: %v", out["command"])
	}

	// The join config needs the token only, no admin session.
	anon.tok = tok
	code, out = anon.do("GET", "/ha/join-config", nil)
	if code != 200 {
		t.Fatalf("join-config %d %v", code, out)
	}
	if out["secret_key"] != "x" || out["panel_version"] != Version {
		t.Fatalf("join-config bundle: %v", out)
	}
	if out["wg_ip"] != "10.8.0.2" || out["pg_version"] != "" {
		t.Fatalf("join-config defaults: wg_ip %v pg_version %v", out["wg_ip"], out["pg_version"])
	}
	if wg, ok := out["wg"].(map[string]any); !ok || wg["port"] != float64(51820) {
		t.Fatalf("join-config wg: %v", out["wg"])
	}

	// Registering the peer consumes the token in the same transaction.
	code, out = anon.do("POST", "/ha/peers", map[string]any{
		"wg_ip": "10.8.0.2", "pubkey": strings.Repeat("A", 44), "endpoint": "45.78.33.200",
	})
	if code != 200 {
		t.Fatalf("register peer %d %v", code, out)
	}
	code, out = anon.do("GET", "/ha/join-config", nil)
	if code != 401 {
		t.Fatalf("join-config after use: %d %v", code, out)
	}
	var row store.HAJoinToken
	if err := s.st.DB.First(&row, "token_hash = ?", hashToken(tok)).Error; err != nil {
		t.Fatal(err)
	}
	if row.UsedAt == nil || row.UsedByWGIP != "10.8.0.2" {
		t.Fatalf("token not marked used: %+v", row)
	}
	code, out = c.do("GET", "/ha/peers", nil)
	if code != 200 {
		t.Fatalf("list peers %d %v", code, out)
	}
	peers := out["peers"].([]any)
	if len(peers) != 1 || peers[0].(map[string]any)["wgIp"] != "10.8.0.2" {
		t.Fatalf("peers: %v", peers)
	}

	// Minting again voids the previous unused token.
	_, out = c.do("POST", "/ha/token", nil)
	first := out["token"].(string)
	_, out = c.do("POST", "/ha/token", nil)
	second := out["token"].(string)
	anon.tok = first
	if code, _ := anon.do("GET", "/ha/join-config", nil); code != 401 {
		t.Fatalf("voided token still worked: %d", code)
	}
	anon.tok = second
	if code, _ := anon.do("GET", "/ha/join-config", nil); code != 200 {
		t.Fatalf("fresh token rejected: %d", code)
	}
}

// TestHAJoinTokenRejected covers unknown, expired and missing tokens. The
// failure must not reveal which case it is, nor echo the token.
func TestHAJoinTokenRejected(t *testing.T) {
	s, _, c := newAdminClient(t)
	anon := &apiClient{t: t, base: c.base}
	now := time.Now().UTC()
	expired := "expired-token"
	s.st.DB.Create(&store.HAJoinToken{ID: store.NewID(), TokenHash: hashToken(expired),
		CreatedBy: "test", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)})

	for _, tok := range []string{"", "no-such-token", expired} {
		anon.tok = tok
		code, out := anon.do("GET", "/ha/join-config", nil)
		if code != 401 || out["error"] != "invalid or expired token" {
			t.Fatalf("token %q: %d %v", tok, code, out)
		}
	}
	if code, _ := anon.do("POST", "/ha/peers", map[string]any{"wg_ip": "10.8.0.3", "pubkey": "x"}); code != 401 {
		t.Fatalf("register without a valid token: %d", code)
	}
}

// TestHACFTokenSealed stores the Cloudflare token sealed and never echoes
// it back; the join config can still serve it to a valid token.
func TestHACFTokenSealed(t *testing.T) {
	s, _, c := newAdminClient(t)
	const cfToken = "cf-test-token-123"
	code, out := c.do("POST", "/ha/cf-token", map[string]any{"token": cfToken})
	if code != 200 || out["ok"] != true {
		t.Fatalf("save cf-token %d %v", code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), cfToken) {
		t.Fatalf("response echoes the token: %s", raw)
	}
	var row store.ClusterSecret
	if err := s.st.DB.First(&row, "name = ?", "cf_api_token").Error; err != nil {
		t.Fatal(err)
	}
	if row.Value == cfToken || row.Value == "" {
		t.Fatalf("cf token stored in plaintext or empty")
	}
	if got := s.st.Box.MustOpen(row.Value); got != cfToken {
		t.Fatalf("sealed cf token does not open: %q", got)
	}

	_, out = c.do("POST", "/ha/token", nil)
	anon := &apiClient{t: t, base: c.base, tok: out["token"].(string)}
	code, out = anon.do("GET", "/ha/join-config", nil)
	if code != 200 || out["cf_api_token"] != cfToken {
		t.Fatalf("join-config cf token: %d %v", code, out)
	}
}

// TestHAStatus returns the member list, the leader and null replication
// state on SQLite (replication queries only exist on PostgreSQL).
func TestHAStatus(t *testing.T) {
	s, _, c := newAdminClient(t)
	now := time.Now().UTC()
	s.st.DB.Create(&store.ClusterMember{InstanceID: "m1", MemberID: "mid1", Host: "h1",
		Version: "v0.1.260", Edition: "pro", StartedAt: now, SeenAt: now})
	code, out := c.do("GET", "/ha/status", nil)
	if code != 200 {
		t.Fatalf("status %d %v", code, out)
	}
	if out["role"] != "none" || out["redisConfigured"] != false {
		t.Fatalf("status: %v", out)
	}
	if out["leader"] != s.procID {
		t.Fatalf("leader on SQLite should be self: %v", out["leader"])
	}
	if out["replication"] != nil || out["inRecovery"] != nil || out["replayLagSeconds"] != nil {
		t.Fatalf("replication state must be null on SQLite: %v", out)
	}
	found := false
	for _, m := range out["members"].([]any) {
		if m.(map[string]any)["id"] == "m1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("members missing m1: %v", out["members"])
	}
	if err := s.st.SetSetting("ha.role", "primary"); err != nil {
		t.Fatal(err)
	}
	_, out = c.do("GET", "/ha/status", nil)
	if out["role"] != "primary" {
		t.Fatalf("role after setting: %v", out["role"])
	}
}

// TestHATokenRequiresSudo: an admin who has not re-verified (no sudo) gets
// 403 from the token endpoint.
func TestHATokenRequiresSudo(t *testing.T) {
	s, _, c := newAdminClient(t)
	h, _ := bcrypt.GenerateFromPassword([]byte("pw2"), bcrypt.MinCost)
	s.st.DB.Create(&store.Admin{ID: "a2", Username: "second", PasswordHash: string(h)})
	plain := &apiClient{t: t, base: c.base}
	code, out := plain.do("POST", "/auth/login", map[string]string{"username": "second", "password": "pw2"})
	if code != 200 {
		t.Fatalf("login %d %v", code, out)
	}
	plain.tok = out["token"].(string)
	code, out = plain.do("POST", "/ha/token", nil)
	if code != http.StatusForbidden || out["sudoRequired"] != true {
		t.Fatalf("non-sudo issue token: %d %v", code, out)
	}
}

// TestHAFailoverCheck is read-only: it reports local health, the database
// role and a static promote checklist, and performs no writes.
func TestHAFailoverCheck(t *testing.T) {
	_, _, c := newAdminClient(t)
	code, out := c.do("POST", "/ha/failover-check", nil)
	if code != 200 {
		t.Fatalf("failover-check %d %v", code, out)
	}
	if out["healthy"] != true || out["role"] != "none" {
		t.Fatalf("failover-check: %v", out)
	}
	if out["inRecovery"] != nil || out["replayLagSeconds"] != nil {
		t.Fatalf("replication state must be null on SQLite: %v", out)
	}
	if steps, ok := out["steps"].([]any); !ok || len(steps) == 0 {
		t.Fatalf("failover-check steps: %v", out["steps"])
	}
}

// TestHADeletePeer removes a peer record.
func TestHADeletePeer(t *testing.T) {
	s, _, c := newAdminClient(t)
	now := time.Now().UTC()
	s.st.DB.Create(&store.HAPeer{WGIP: "10.8.0.2", Pubkey: strings.Repeat("B", 44),
		Endpoint: "45.78.33.200", Role: "secondary", CreatedAt: now})
	code, _ := c.do("DELETE", "/ha/peers/10.8.0.2", nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete peer: %d", code)
	}
	var n int64
	s.st.DB.Model(&store.HAPeer{}).Count(&n)
	if n != 0 {
		t.Fatalf("peer not deleted: %d", n)
	}
}

// TestHATokenCLI mints through the operator CLI: the token is stored hashed
// only, and minting again voids the unused one.
func TestHATokenCLI(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DataDir: dir, SecretKey: "x"}
	cfg.DB.DSN = filepath.Join(dir, "cli.db")
	first, err := HAToken(cfg, "cli")
	if err != nil || first == "" {
		t.Fatalf("ha-token: %v", err)
	}
	second, err := HAToken(cfg, "cli")
	if err != nil || second == "" || second == first {
		t.Fatalf("ha-token again: %v %q", err, second)
	}
	box, err := secret.NewBoxSet("x", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DB.DSN, box, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int64
	st.DB.Model(&store.HAJoinToken{}).Where("used_at IS NULL").Count(&n)
	if n != 1 {
		t.Fatalf("unused tokens: %d", n)
	}
	var row store.HAJoinToken
	if err := st.DB.First(&row, "used_at IS NULL").Error; err != nil {
		t.Fatal(err)
	}
	if row.TokenHash != hashToken(second) || row.TokenHash == second {
		t.Fatalf("stored token is not the hash of the minted one")
	}
	if time.Until(row.ExpiresAt) > 10*time.Minute || time.Until(row.ExpiresAt) < 9*time.Minute {
		t.Fatalf("expiry not ~10m: %v", row.ExpiresAt)
	}
}

// TestHAJoinRateLimited hammers the token endpoints: the first 10 attempts
// in a minute pass, the 11th is rejected with 429.
func TestHAJoinRateLimited(t *testing.T) {
	_, _, c := newAdminClient(t)
	anon := &apiClient{t: t, base: c.base}
	_, out := c.do("POST", "/ha/token", nil)
	anon.tok = out["token"].(string)
	for i := 0; i < 10; i++ {
		if code, _ := anon.do("GET", "/ha/join-config", nil); code != 200 {
			t.Fatalf("attempt %d: %d", i+1, code)
		}
	}
	if code, _ := anon.do("GET", "/ha/join-config", nil); code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt: %d, want 429", code)
	}
}

// TestHAFailoverCheckPrimaryProbe points PublicURL at a local server and at
// a dead port: the probe reports reachable/unreachable without failing the
// check.
func TestHAFailoverCheckPrimaryProbe(t *testing.T) {
	s, _, c := newAdminClient(t)
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/info" {
			t.Errorf("probe hit unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"test"}`))
	}))
	t.Cleanup(probe.Close)
	s.cfg.PublicURL = probe.URL
	if code, out := c.do("POST", "/ha/failover-check", nil); code != 200 {
		t.Fatalf("failover-check %d %v", code, out)
	} else if out["primaryReachable"] != true || out["primaryStatus"] != float64(200) {
		t.Fatalf("reachable probe: %v", out)
	}
	s.cfg.PublicURL = "http://127.0.0.1:1"
	if code, out := c.do("POST", "/ha/failover-check", nil); code != 200 {
		t.Fatalf("failover-check %d %v", code, out)
	} else if out["primaryReachable"] != false || out["primaryStatus"] != nil {
		t.Fatalf("unreachable probe: %v", out)
	}
}
