package panel

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/license"
)

// The license server can be set on the license page; the environment
// variable wins over it, and "check now" re-fetches the license.
func TestLicenseServerSettingAndCheck(t *testing.T) {
	s, _, c := newAdminClient(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s.licKeys = []ed25519.PublicKey{pub}
	features := []string{license.FeatureSpeedLimit}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		inst := req["instance"]
		text, _ := license.Sign(&license.License{ID: "lic_srv", Plan: "pro", Features: features, Instance: inst, IssuedAt: "2026-01-01"}, priv)
		switch r.URL.Path {
		case "/v1/activate":
			json.NewEncoder(w).Encode(map[string]string{"license": text})
		case "/v1/check":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "revoked": false, "license": text})
		}
	}))
	defer srv.Close()

	if _, out := c.do("GET", "/license", nil); out["server"] != false || out["serverSource"] != "" {
		t.Fatalf("no server yet: %v", out)
	}
	for _, bad := range []string{"ftp://x", "license.example.com", "https://u:p@x.com", "https://x.com/?a=1"} {
		if code, _ := c.do("POST", "/license/server", map[string]any{"url": bad}); code != http.StatusBadRequest {
			t.Fatalf("accepted %q: %d", bad, code)
		}
	}
	code, out := c.do("POST", "/license/server", map[string]any{"url": srv.URL + "/"})
	if code != 200 || out["serverURL"] != srv.URL || out["serverSource"] != "setting" {
		t.Fatalf("set server: %d %v", code, out)
	}
	if code, out := c.do("POST", "/license/activate", map[string]any{"key": "K"}); code != 200 {
		t.Fatalf("activate: %d %v", code, out)
	}
	if l := s.installedLicense(); l == nil || l.AllGranted() || len(l.Granted()) != 1 {
		t.Fatalf("installed %+v", l)
	}

	// The backend switches the license to every feature; "check now" picks it up.
	features = append(append([]string{}, license.AllFeatures...), license.EveryFeature)
	code, out = c.do("POST", "/license/check", nil)
	lic, _ := out["license"].(map[string]any)
	if code != 200 || out["warning"] != nil || lic["allGranted"] != true || len(lic["features"].([]any)) != len(license.AllFeatures) {
		t.Fatalf("check: %d %v", code, out)
	}

	t.Setenv("POLARIS_LICENSE_SERVER", "https://env.example.com")
	if _, out := c.do("GET", "/license", nil); out["serverSource"] != "env" {
		t.Fatalf("env: %v", out)
	}
	// Unreachable server: the check reports it.
	t.Setenv("POLARIS_LICENSE_SERVER", "http://127.0.0.1:1")
	if code, out := c.do("POST", "/license/check", nil); code != 200 || out["warning"] == nil {
		t.Fatalf("unreachable: %d %v", code, out)
	}
	// Clearing the setting falls back to the build default (none here).
	t.Setenv("POLARIS_LICENSE_SERVER", "")
	if _, out := c.do("POST", "/license/server", map[string]any{"url": ""}); out["server"] != false {
		t.Fatalf("cleared: %v", out)
	}
}
