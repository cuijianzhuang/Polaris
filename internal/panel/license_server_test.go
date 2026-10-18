package panel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"

	"github.com/cuijianzhuang/polaris/internal/license"
)

// Deployment configuration stays private and immutable through the panel.
// Communication status is observed, not inferred from configuration.
func TestLicenseServerStatusAndCheck(t *testing.T) {
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

	if _, out := c.do("GET", "/license", nil); out["server"] != false || out["serverStatus"] != "unconfigured" {
		t.Fatalf("no server yet: %v", out)
	}
	for _, value := range []string{srv.URL, "", "https://replacement.example.com"} {
		if code, _ := c.do("POST", "/license/server", map[string]any{"url": value}); code != http.StatusForbidden {
			t.Fatalf("server endpoint accepted a write: %d", code)
		}
		if code, _ := c.do("PUT", "/settings", map[string]any{"license_server": value}); code != http.StatusForbidden {
			t.Fatalf("settings endpoint accepted a write: %d", code)
		}
	}
	if s.st.GetSetting("license_server", "") != "" {
		t.Fatal("denied request changed server")
	}
	// Existing installations retain their deployment configuration internally.
	s.st.SetSetting("license_server", srv.URL+"/")
	s.st.SetSetting("license_check_at", time.Now().Format(time.RFC3339))
	code, out := c.do("GET", "/license", nil)
	if code != 200 || out["server"] != true || out["serverStatus"] != "unchecked" || out["serverCheckedAt"] != nil {
		t.Fatalf("configuration is not a successful check: %d %v", code, out)
	}
	assertLicenseServerHidden(t, out, srv.URL)
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

	if out["serverStatus"] != "available" || out["serverCheckedAt"] == nil {
		t.Fatalf("successful request: %v", out)
	}
	assertLicenseServerHidden(t, out, srv.URL)
	if _, settings := c.do("GET", "/settings", nil); settings["license_server"] != nil || settings["license_server_health"] != nil {
		t.Fatal("internal server settings exposed")
	}
	s.st.DB.Create(&store.AuditLog{Action: "license.server", Target: srv.URL, Detail: "old configuration: " + srv.URL})
	_, audits := c.do("GET", "/audit?action=license.server", nil)
	assertLicenseServerHidden(t, audits, srv.URL)

	t.Setenv("POLARIS_LICENSE_SERVER", "https://env.example.com")
	if s.licenseServer() != "https://env.example.com" {
		t.Fatal("environment must still win")
	}
	if _, out := c.do("GET", "/license", nil); out["serverStatus"] != "unchecked" {
		t.Fatalf("new deployment target reused old status: %v", out)
	}
	// Neither a transport failure nor the API warning may disclose its URL.
	t.Setenv("POLARIS_LICENSE_SERVER", "http://127.0.0.1:1")
	code, out = c.do("POST", "/license/check", nil)
	if code != 200 || out["warning"] == nil || out["serverStatus"] != "unavailable" {
		t.Fatalf("unreachable: %d %v", code, out)
	}
	assertLicenseServerHidden(t, out, "127.0.0.1", "http://")
	if code, out := c.do("POST", "/license/deactivate", nil); code != 200 || out["warning"] == nil {
		t.Fatalf("failed deactivation: %d %v", code, out)
	} else {
		assertLicenseServerHidden(t, out, "127.0.0.1", "http://")
	}
	t.Setenv("POLARIS_LICENSE_SERVER", "")
	if s.licenseServer() != srv.URL {
		t.Fatal("legacy deployment configuration lost")
	}
}

func assertLicenseServerHidden(t *testing.T, out any, secrets ...string) {
	t.Helper()
	data, _ := json.Marshal(out)
	for _, secret := range append(secrets, "serverURL", "serverSource", "serverHash") {
		if strings.Contains(string(data), secret) {
			t.Fatalf("license server details disclosed: %s", secret)
		}
	}
}

func TestLicenseServerResponsePrivacy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		body   string
		status string
		want   string
	}{
		{"server-error", 503, `{"error":"upstream https://hidden.example.com failed"}`, "unavailable", "HTTP 503"},
		{"unknown-client-error", 400, `{"error":"https://hidden.example.com rejected request"}`, "available", "HTTP 400"},
		{"known-client-error", 404, `{"error":"激活码不存在，请检查是否输入正确"}`, "available", "激活码不存在"},
		{"expired", 403, `{"error":"该许可证已于 2026-01-01 到期，请续费后再激活"}`, "available", "2026-01-01"},
		{"invalid-expiry", 403, `{"error":"该许可证已于 https://hidden.example.com 到期，请续费后再激活"}`, "available", "HTTP 403"},
		{"malformed", 200, `https://hidden.example.com`, "unavailable", "响应无效"},
		{"oversized", 200, strings.Repeat("x", (64<<10)+1), "unavailable", "响应无效"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, c := newAdminClient(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code); w.Write([]byte(tc.body)) }))
			defer srv.Close()
			t.Setenv("POLARIS_LICENSE_SERVER", srv.URL)
			err := s.licenseCall(context.Background(), "/v1/activate", map[string]string{"key": "invalid"}, &map[string]any{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected response: %v", err)
			}
			assertLicenseServerHidden(t, err.Error(), srv.URL, "hidden.example.com")
			_, out := c.do("GET", "/license", nil)
			if out["serverStatus"] != tc.status {
				t.Fatalf("status: %v", out)
			}
			assertLicenseServerHidden(t, out, srv.URL, "hidden.example.com")
		})
	}
}

func TestLicenseServerBuildAndInvalidConfigurationStayPrivate(t *testing.T) {
	t.Setenv("POLARIS_LICENSE_SERVER", "")
	previous := DefaultLicenseServer
	DefaultLicenseServer = "https://build-private.example.test"
	t.Cleanup(func() { DefaultLicenseServer = previous })
	s, _, c := newAdminClient(t)
	_, out := c.do("GET", "/license", nil)
	if out["serverStatus"] != "unchecked" || out["server"] != true {
		t.Fatalf("build default: %v", out)
	}
	assertLicenseServerHidden(t, out, "build-private.example.test")
	t.Setenv("POLARIS_LICENSE_SERVER", "https://invalid-private.example.test/\x7f")
	err := s.licenseCall(context.Background(), "/v1/activate", nil, &map[string]any{})
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	assertLicenseServerHidden(t, err.Error(), "invalid-private.example.test")
}
