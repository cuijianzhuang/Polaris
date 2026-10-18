package panel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// The fake service is used by real REST checks and the real scheduler tick.
func policyLicense(t *testing.T, s *Server, offline bool) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.licKeys = []ed25519.PublicKey{pub}
	text, err := license.Sign(&license.License{ID: "policy", Plan: "pro", Instance: s.instanceID(), Offline: offline, Features: license.AllFeatures}, priv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.installLicense(text); err != nil {
		t.Fatal(err)
	}
	return text
}
func expirePolicyGrace(t *testing.T, s *Server) {
	t.Helper()
	past := time.Now().Add(-license.Grace - time.Hour).Format(time.RFC3339)
	if err := s.st.SetSetting("license_check_at", past); err != nil {
		t.Fatal(err)
	}
	s.reloadLicense()
}
func assertPolicyDelay(t *testing.T, s *Server, minDelay, maxDelay time.Duration) {
	t.Helper()
	s.licenseMu.Lock()
	delay := time.Until(s.licenseSchedule.next)
	s.licenseMu.Unlock()
	if delay < minDelay-5*time.Second || delay > maxDelay+time.Second {
		t.Fatalf("delay %v, want %v..%v", delay, minDelay, maxDelay)
	}
}

func TestLicenseSchedulerStartupRetryAndRecovery(t *testing.T) {
	s, _, _ := newAdminClient(t)
	policyLicense(t, s, false)
	var calls atomic.Int32
	var fail atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer remote.Close()
	t.Setenv("POLARIS_LICENSE_SERVER", remote.URL)
	s.licenseCheckTick(context.Background()) // first eligible tick at startup
	if calls.Load() != 1 || settingTime(s, "license_check_at").IsZero() {
		t.Fatal("startup check missing")
	}
	assertPolicyDelay(t, s, licenseCheckInterval, licenseCheckInterval+licenseCheckJitter)
	s.licenseCheckTick(context.Background())
	if calls.Load() != 1 {
		t.Fatal("polled before due")
	}
	fail.Store(true)
	for _, delay := range []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 2 * time.Hour} {
		s.licenseMu.Lock()
		s.licenseSchedule.next = time.Now().Add(-time.Second)
		s.licenseMu.Unlock()
		s.licenseCheckTick(context.Background())
		assertPolicyDelay(t, s, delay, delay)
	}
	expirePolicyGrace(t, s)
	if !s.license().ManagementReadOnly {
		t.Fatal("outage must pause writes")
	}
	fail.Store(false)
	s.licenseMu.Lock()
	s.licenseSchedule.lastAttempt = time.Time{}
	s.licenseMu.Unlock()
	if err := s.performLicenseCheck(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if s.license().ManagementReadOnly || s.licenseSchedule.failures != 0 {
		t.Fatal("successful manual recovery did not cancel retry")
	}
	assertPolicyDelay(t, s, licenseCheckInterval, licenseCheckInterval+licenseCheckJitter)
}

func TestLicenseManualCooldownAndSingleFlight(t *testing.T) {
	s, _, c := newAdminClient(t)
	policyLicense(t, s, false)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var block atomic.Bool
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if block.Load() {
			entered <- struct{}{}
			<-release
		}
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer func() { unblock(); remote.Close() }()
	t.Setenv("POLARIS_LICENSE_SERVER", remote.URL)
	if code, out := c.do("POST", "/license/check", nil); code != 200 || out["checkAvailableAt"] == nil {
		t.Fatalf("first %d %v", code, out)
	}
	if code, out := c.do("POST", "/license/check", nil); code != 429 || out["retryAfter"].(float64) <= 0 {
		t.Fatalf("cooldown %d %v", code, out)
	}
	if calls.Load() != 1 {
		t.Fatal("cooldown reached server")
	}
	// A scheduled request and a click must never run overlapping checks.
	block.Store(true)
	done := make(chan error, 1)
	go func() { done <- s.performLicenseCheck(context.Background(), false) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("check did not start")
	}
	err := s.performLicenseCheck(context.Background(), true)
	var wait *licenseCheckWait
	if !errors.As(err, &wait) {
		t.Fatalf("overlap %v", err)
	}
	// Reading license status must stay responsive while the network is stalled.
	view := make(chan struct{})
	go func() { s.licenseView(); close(view) }()
	select {
	case <-view:
	case <-time.After(time.Second):
		t.Fatal("GET status blocked behind network")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("overlapping requests: %d", calls.Load())
	}
}

func TestLicenseGraceManagementAndRecovery(t *testing.T) {
	s, srv, c := newAdminClient(t)
	_, token := c.do("POST", "/tokens", map[string]any{"name": "policy-token", "scopes": []string{"users:write"}})
	tc := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: token["secret"].(string)}
	text := policyLicense(t, s, false)
	expirePolicyGrace(t, s)
	if code, out := tc.do("POST", "/users", map[string]string{"username": "token-denied"}); code != 402 || out["licenseReadOnly"] != true {
		t.Fatalf("API token bypass %d %v", code, out)
	}
	anonymous := &apiClient{t: t, base: srv.URL + s.cfg.APIBase()}
	if code, _ := anonymous.do("POST", "/license/check", nil); code != 401 {
		t.Fatalf("unauthenticated recovery %d", code)
	}
	// A local import must not count as a successful online check.
	before := s.st.GetSetting("license_check_at", "")
	if code, out := c.do("POST", "/license", map[string]string{"text": text}); code != 200 {
		t.Fatalf("import %d %v", code, out)
	}
	if s.st.GetSetting("license_check_at", "") != before || !s.license().ManagementReadOnly {
		t.Fatal("local re-import refreshed grace")
	}
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/users", map[string]string{"username": "denied"}},
		{"POST", "/nodes", map[string]string{"name": "denied"}},
		{"PUT", "/settings", map[string]string{"tg_enabled": "false"}},
		{"POST", "/users/u/renew", map[string]int{"days": 30}},
		{"POST", "/nodes/n/sync", nil},
		{"DELETE", "/users/u", nil},
	} {
		if code, out := c.do(req.method, req.path, req.body); code != 402 || out["licenseReadOnly"] != true {
			t.Fatalf("%s %s %d %v", req.method, req.path, code, out)
		}
	}
	for _, path := range []string{"/users", "/nodes", "/license", "/auth/me"} {
		if code, out := c.do("GET", path, nil); code != 200 {
			t.Fatalf("read %s %d %v", path, code, out)
		}
	}
	if code, out := c.do("POST", "/auth/sudo", map[string]string{"password": "old-password"}); code != 200 {
		t.Fatalf("recovery authentication %d %v", code, out)
	}
	// Existing traffic still settles; the guard does not touch Agent services.
	s.st.DB.Create(&store.User{ID: "u", Username: "existing", UUID: "u", SubToken: "u", Status: "active"})
	if err := s.traffic.Add("n", []model.TrafficDelta{{UserID: "u", Up: 10, Down: 20}}); err != nil {
		t.Fatal(err)
	}
	if err := s.settleTraffic(); err != nil {
		t.Fatal(err)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "u")
	if u.Used() != 30 {
		t.Fatalf("settlement stopped: %d", u.Used())
	}
	// An authenticated manual check is a recovery path, and success unlocks writes.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(map[string]bool{"ok": true}) }))
	defer remote.Close()
	t.Setenv("POLARIS_LICENSE_SERVER", remote.URL)
	if code, out := c.do("POST", "/license/check", nil); code != 200 || s.license().ManagementReadOnly {
		t.Fatalf("online recovery %d %v", code, out)
	}
	if code, out := c.do("POST", "/users", map[string]string{"username": "recovered"}); code != 201 {
		t.Fatalf("writes not restored %d %v", code, out)
	}
}

func TestLicenseOfflineAndCLIRefresh(t *testing.T) {
	s, _, c := newAdminClient(t)
	online := policyLicense(t, s, false)
	// Missing check history uses a persistent first-seen anchor, not each import.
	if !settingTime(s, "license_check_at").IsZero() {
		t.Fatal("import fabricated check")
	}
	past := time.Now().Add(-license.Grace - time.Hour).Format(time.RFC3339)
	s.st.SetSetting("license_online_since", past)
	s.reloadLicense()
	if _, err := s.installLicense(online); err != nil {
		t.Fatal(err)
	}
	if s.st.GetSetting("license_online_since", "") != past || !s.license().ManagementReadOnly {
		t.Fatal("import refreshed first-seen")
	}
	// Simulate the CLI writing a valid offline license while this panel runs.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s.licKeys = append(s.licKeys, pub)
	offline, _ := license.Sign(&license.License{ID: "offline", Plan: "pro", Offline: true, Instance: s.instanceID()}, priv)
	s.st.SetSetting("license_text", offline)
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer remote.Close()
	t.Setenv("POLARIS_LICENSE_SERVER", remote.URL)
	s.licenseCheckTick(context.Background())
	if !s.installedLicense().Offline || s.license().ManagementReadOnly || calls.Load() != 0 {
		t.Fatal("offline/CLI refresh must remain local")
	}
	if code, out := c.do("POST", "/license/check", nil); code != 400 {
		t.Fatalf("offline check %d %v", code, out)
	}
}

func TestTelegramLicenseReadOnlyActions(t *testing.T) {
	for _, data := range []string{"n:s:n", "n:b:n", "n:r:n:xray", "u:d:u", "u:e:u", "u:t:u", "u:x:u", "u:k:u", "up:panel", "up:mode"} {
		if !tgLicenseManagementWrite(strings.Split(data, ":")) {
			t.Fatalf("write allowed %s", data)
		}
	}
	for _, data := range []string{"n:l", "n:v:n", "u:v:u", "v:users:0", "v:status", "me:sub", "wl:id:1", "ul:id:1", "m:login"} {
		if tgLicenseManagementWrite(strings.Split(data, ":")) {
			t.Fatalf("view denied %s", data)
		}
	}
}

func TestLicenseUnconfirmedResponseDoesNotExtendGrace(t *testing.T) {
	for _, body := range []string{`{}`, `{"ok":false}`, `{"ok":true,"license":"invalid-signature"}`} {
		t.Run(body, func(t *testing.T) {
			s, _, _ := newAdminClient(t)
			text := policyLicense(t, s, false)
			expirePolicyGrace(t, s)
			last := s.st.GetSetting("license_check_at", "")
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer remote.Close()
			t.Setenv("POLARIS_LICENSE_SERVER", remote.URL)
			if err := s.performLicenseCheck(context.Background(), false); err == nil {
				t.Fatal("unconfirmed response accepted")
			}
			if s.st.GetSetting("license_check_at", "") != last || s.st.GetSetting("license_text", "") != text || !s.license().ManagementReadOnly {
				t.Fatal("unconfirmed response changed licensed state")
			}
			assertPolicyDelay(t, s, 5*time.Minute, 5*time.Minute)
		})
	}
}

func TestLicenseOfflineRecoveryAndRevocation(t *testing.T) {
	s, _, c := newAdminClient(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s.licKeys = []ed25519.PublicKey{pub}
	makeText := func(offline bool) string {
		t.Helper()
		text, err := license.Sign(&license.License{ID: "policy", Plan: "pro", Offline: offline, Instance: s.instanceID()}, priv)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	online := makeText(false)
	if _, err := s.installLicense(online); err != nil {
		t.Fatal(err)
	}
	expirePolicyGrace(t, s)
	if code, out := c.do("POST", "/license", map[string]string{"text": makeText(true)}); code != 200 || s.license().ManagementReadOnly {
		t.Fatalf("offline recovery %d %v", code, out)
	}
	// Re-importing a signed online document must not erase a server revocation.
	s.st.SetSetting("license_revoked", "1")
	if _, err := s.installLicense(online); err != nil {
		t.Fatal(err)
	}
	if s.license().Source == "license" {
		t.Fatal("online import erased revocation")
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]bool{"ok": true, "revoked": true})
	}))
	defer remote.Close()
	t.Setenv("POLARIS_LICENSE_SERVER", remote.URL)
	if err := s.performLicenseCheck(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if s.license().Source == "license" {
		t.Fatal("revocation ignored")
	}
	if err := s.deactivateLicense(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.installLicense(online); err != nil {
		t.Fatal(err)
	}
	if s.license().Source == "license" {
		t.Fatal("deactivate and import erased revocation")
	}
}

func TestLicenseCheckCancellationAndNoLicense(t *testing.T) {
	s := newTestServer(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() { close(release); remote.Close() }()
	t.Setenv("POLARIS_LICENSE_SERVER", remote.URL)
	s.licenseCheckTick(context.Background())
	select {
	case <-entered:
		t.Fatal("no license caused network request")
	default:
	}
	policyLicense(t, s, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.runLicenseChecks(ctx); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("startup check did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel in-flight check")
	}
}
