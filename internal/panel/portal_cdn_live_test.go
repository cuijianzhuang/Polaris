package panel

import (
	"github.com/cuijianzhuang/polaris/internal/store"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestLivePortalCDNBrowser(t *testing.T) {
	if os.Getenv("POLARIS_PORTAL_CDN_BROWSER_TEST") == "" {
		t.Skip("POLARIS_PORTAL_CDN_BROWSER_TEST required")
	}
	s, base, _ := cdnRegionFixture(t)
	// The matrix makes many subscription fetches from one loopback address.
	s.subLimit = &sharedLimiter{s: s.shared, name: "portal-cdn-browser-sub", perMinute: 600000}
	var user store.User
	if err := s.st.DB.First(&user, "id = ?", "ra").Error; err != nil {
		t.Fatal(err)
	}
	token, err := s.issuePortalToken(&user)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "python3", "../../web/scripts/portal-cdn-live.py")
	cmd.Env = append(os.Environ(), "POLARIS_PORTAL_TEST_URL="+base, "POLARIS_PORTAL_TEST_COOKIE="+token)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, out)
	}
	t.Log(string(out))
}

func TestLivePersonalCDNPoolBrowser(t *testing.T) {
	if os.Getenv("POLARIS_PERSONAL_CDN_BROWSER_TEST") == "" {
		t.Skip("POLARIS_PERSONAL_CDN_BROWSER_TEST required")
	}
	s, base, _ := cdnRegionFixture(t)
	if err := s.st.DB.Create(&store.Endpoint{ID: "personal-ui-port", InboundID: "ri", Kind: "cdn", CDNProfileID: "regional", Port: 2053}).Error; err != nil {
		t.Fatal(err)
	}

	s.cfg.PublicURL = base
	s.subLimit = &sharedLimiter{s: s.shared, name: "personal-cdn-browser", perMinute: 600000}
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", "ra").Error; err != nil {
		t.Fatal(err)
	}
	token, err := s.issuePortalToken(&u)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "node", "../../web/scripts/portal-cdn-pool-live.mjs")
	cmd.Env = append(os.Environ(), "POLARIS_PORTAL_TEST_URL="+base, "POLARIS_PORTAL_TEST_COOKIE="+token)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, out)
	}
	t.Log(string(out))
}

func TestLivePersonalCDNReliabilityBrowser(t *testing.T) {
	if os.Getenv("POLARIS_PERSONAL_CDN_RELIABILITY_BROWSER_TEST") == "" {
		t.Skip("POLARIS_PERSONAL_CDN_RELIABILITY_BROWSER_TEST required")
	}
	s, base, _ := cdnRegionFixture(t)
	s.cfg.PublicURL = base
	s.subLimit = &sharedLimiter{s: s.shared, name: "personal-cdn-reliability-browser", perMinute: 600000}
	if err := s.st.DB.Create(&store.Endpoint{ID: "reliability-port", InboundID: "ri", Kind: "cdn", CDNProfileID: "regional", Port: 2053, Host: "host.example.com", SNI: "sni.example.com", Path: "/custom"}).Error; err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	pool := store.PersonalCDNPool{StalePolicy: "fallback", StaleHours: 24, Automatic: map[string][]string{"443": {"104.20.7.7"}, "2053": {"104.20.8.8"}}, PortReports: map[string]store.PersonalCDNReportMeta{"443": {At: old, Scheme: "https"}, "2053": {At: time.Now(), Scheme: "https"}}}
	if err := s.st.DB.Model(&store.User{}).Where("id = ?", "ra").Updates(map[string]any{"cdn_preferred": store.NewJSON(map[string][]string{"regional": {"manual.example.com"}}), "cdn_pool": store.NewJSON(map[string]store.PersonalCDNPool{"regional": pool})}).Error; err != nil {
		t.Fatal(err)
	}
	var user store.User
	s.st.DB.First(&user, "id = ?", "ra")
	token, err := s.issuePortalToken(&user)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "node", "../../web/scripts/portal-cdn-reliability-live.mjs")
	cmd.Env = append(os.Environ(), "POLARIS_PORTAL_TEST_URL="+base, "POLARIS_PORTAL_TEST_COOKIE="+token)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, out)
	}
	t.Log(string(out))
}
