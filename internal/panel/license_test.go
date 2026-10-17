package panel

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func licenseKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestLicenseInstallAndGates(t *testing.T) {
	s, _, c := newAdminClient(t)
	pub, priv := licenseKeys(t)
	s.licKeys = []ed25519.PublicKey{pub}
	s.reloadLicense()

	// Community: changing a speed limit needs a license; other edits do not.
	code, g := c.do("POST", "/groups", map[string]any{"name": "plan", "speedDownMbps": 50})
	if code != http.StatusPaymentRequired || g["feature"] != license.FeatureSpeedLimit {
		t.Fatalf("speed plan without license: %d %v", code, g)
	}
	if code, _ := c.do("POST", "/groups", map[string]any{"name": "plain"}); code != 200 {
		t.Fatalf("plain group: %d", code)
	}

	// Install a license bound to this instance.
	text, _ := license.Sign(&license.License{ID: "lic_1", Plan: "pro", Limits: license.Limits{Nodes: 20, Users: 2000},
		Features: []string{license.FeatureSpeedLimit}, Instance: s.instanceID(), IssuedAt: "2026-01-01"}, priv)
	code, out := c.do("POST", "/license", map[string]any{"text": text})
	if code != 200 || out["state"].(map[string]any)["plan"] != "pro" {
		t.Fatalf("install: %d %v", code, out)
	}
	if code, g := c.do("POST", "/groups", map[string]any{"name": "plan", "speedDownMbps": 50}); code != 200 {
		t.Fatalf("speed plan with license: %d %v", code, g)
	}
	// A license for another instance is refused.
	other, _ := license.Sign(&license.License{ID: "lic_2", Plan: "pro", Instance: "inst_other", IssuedAt: "2026-01-01"}, priv)
	if code, _ := c.do("POST", "/license", map[string]any{"text": other}); code != http.StatusBadRequest {
		t.Fatalf("foreign instance: %d", code)
	}
	// Backup push and the Telegram menus are not in this license.
	if res := s.pushBackup("/nonexistent", true); res["license"] == "" && res["read"] == "" {
		t.Fatalf("push: %v", res)
	}
	if !tgAdvanced([]string{"v", "users", "0"}) || tgAdvanced([]string{"me", "sub"}) || tgAdvanced([]string{"v", "pro"}) {
		t.Fatal("tgAdvanced classification")
	}

	// User cap: a license limit applies to creation.
	text3, _ := license.Sign(&license.License{ID: "lic_3", Plan: "pro", Limits: license.Limits{Users: 1}, Instance: s.instanceID(), IssuedAt: "2026-01-01"}, priv)
	if code, _ := c.do("POST", "/license", map[string]any{"text": text3}); code != 200 {
		t.Fatal("install lic_3")
	}
	if code, _ := c.do("POST", "/users", map[string]any{"username": "u1"}); code != http.StatusCreated {
		t.Fatalf("first user: %d", code)
	}
	if code, out := c.do("POST", "/users", map[string]any{"username": "u2"}); code != http.StatusPaymentRequired || out["limit"] != "users" {
		t.Fatalf("over the cap: %d %v", code, out)
	}
}

func TestLicenseTrialKeepsWhatIsConfigured(t *testing.T) {
	s, _, c := newAdminClient(t)
	pub, _ := licenseKeys(t)
	s.licKeys = []ed25519.PublicKey{pub}
	// An install that already limits users gets the trial at (first) start.
	s.st.DB.Create(&store.User{ID: "u", Username: "alice", UUID: "u", Status: "active", SubToken: "u", SpeedDownMbps: 30})
	s.st.SetSetting("license_trial_checked", "")
	s.initLicense()
	if st := s.license(); st.Plan != "trial" || !st.Allow(license.FeatureSpeedLimit) {
		t.Fatalf("trial %+v", st)
	}
	// After the trial: the limit can no longer change, but it stays in effect.
	s.st.SetSetting("license_trial_until", time.Now().Add(-time.Hour).Format(time.RFC3339))
	s.reloadLicense()
	code, _ := c.do("PUT", "/users/u", map[string]any{"username": "alice", "speedDownMbps": 60})
	if code != http.StatusPaymentRequired {
		t.Fatalf("change after trial: %d", code)
	}
	if code, _ := c.do("PUT", "/users/u", map[string]any{"username": "alice2", "speedDownMbps": 30}); code != 200 {
		t.Fatalf("other edits after trial: %d", code)
	}
	var plan [2]int
	var u store.User
	s.st.DB.First(&u, "id = ?", "u")
	if up, down := effectiveSpeed(&u, plan); up != 0 || down != 30 {
		t.Fatalf("configured limit lost: %d %d", up, down)
	}
}

func TestLicenseOnline(t *testing.T) {
	s, _, c := newAdminClient(t)
	pub, priv := licenseKeys(t)
	s.licKeys = []ed25519.PublicKey{pub}
	revoked := false
	var gotInstance string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		switch r.URL.Path {
		case "/v1/activate":
			if req["key"] != "KEY-123" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "激活码无效"})
				return
			}
			gotInstance = req["instance"]
			text, _ := license.Sign(&license.License{ID: "lic_on", Plan: "pro", Features: license.AllFeatures, Instance: req["instance"], IssuedAt: "2026-01-01"}, priv)
			json.NewEncoder(w).Encode(map[string]string{"license": text})
		case "/v1/check":
			json.NewEncoder(w).Encode(map[string]any{"ok": !revoked, "revoked": revoked})
		case "/v1/deactivate":
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer srv.Close()
	t.Setenv("POLARIS_LICENSE_SERVER", srv.URL)

	if code, out := c.do("POST", "/license/activate", map[string]any{"key": "WRONG"}); code != http.StatusBadRequest || out["error"] != "激活码无效" {
		t.Fatalf("bad key: %d %v", code, out)
	}
	code, out := c.do("POST", "/license/activate", map[string]any{"key": "KEY-123"})
	if code != 200 || out["state"].(map[string]any)["plan"] != "pro" || gotInstance != s.instanceID() {
		t.Fatalf("activate: %d %v", code, out)
	}
	s.checkLicense()
	if !s.license().Allow(license.FeatureTGMenu) {
		t.Fatal("still licensed after a good check")
	}
	// The server revokes: the panel falls back.
	revoked = true
	s.checkLicense()
	if st := s.license(); st.Plan != "community" {
		t.Fatalf("after revocation %+v", st)
	}
	// Server unreachable: no downgrade by itself.
	revoked = false
	s.checkLicense()
	srv.Close()
	s.st.SetSetting("license_check_at", time.Now().Add(-20*24*time.Hour).Format(time.RFC3339))
	s.checkLicense()
	if st := s.license(); st.Plan != "pro" || len(st.Warnings) == 0 {
		t.Fatalf("server gone for 20 days %+v", st)
	}
	// Deactivate removes the license locally even with the server down.
	if code, out := c.do("POST", "/license/deactivate", nil); code != 200 || out["warning"] == nil {
		t.Fatalf("deactivate: %d %v", code, out)
	}
	if s.license().Plan != "community" {
		t.Fatal("license still in effect after deactivation")
	}
}

// licenseAll unlocks every paid feature for tests of those features.
func licenseAll(s *Server) {
	s.st.SetSetting("license_trial_until", time.Now().Add(time.Hour).Format(time.RFC3339))
	s.reloadLicense()
}

func TestLicenseNotEnforcedWithoutKeys(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.licKeys = nil // a build without license keys: licensing not set up yet
	s.reloadLicense()
	if st := s.license(); st.Plan != "open" || !st.Allow(license.FeatureSpeedLimit) || st.Limit(license.LimitUsers) != 0 {
		t.Fatalf("unenforced %+v", st)
	}
	if code, _ := c.do("POST", "/groups", map[string]any{"name": "plan", "speedDownMbps": 50}); code != 200 {
		t.Fatalf("speed plan in an unlicensed build: %d", code)
	}
}
