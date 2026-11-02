package panel

import (
	"github.com/cuijianzhuang/polaris/internal/store"
	"os"
	"os/exec"
	"testing"
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
