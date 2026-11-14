package panel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Admin-configured outbound fetches go through the SSRF-guarded clients:
// rule lists follow allow_private_sources; webhooks, SSO, plugins and the
// external converter may reach the LAN but never link-local / metadata.
func TestOutboundClientsGuardPrivateTargets(t *testing.T) {
	s := newTestServer(t)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("example.com\n")) }))
	defer local.Close()

	if _, err := fetchRuleList(context.Background(), s.safeClient(5*time.Second), local.URL); !errors.Is(err, errPrivateTarget) {
		t.Fatalf("rule list on loopback without opt-in: %v", err)
	}
	s.st.SetSetting("allow_private_sources", "true")
	if _, err := fetchRuleList(context.Background(), s.safeClient(5*time.Second), local.URL); err != nil {
		t.Fatalf("rule list on loopback with opt-in: %v", err)
	}

	for name, c := range map[string]*http.Client{"webhook": webhookClient, "sso": ssoClient, "plugin": pluginClient} {
		resp, err := c.Get(local.URL)
		if err != nil {
			t.Fatalf("%s to loopback: %v", name, err)
		}
		resp.Body.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
		_, err = c.Do(req)
		cancel()
		if !errors.Is(err, errPrivateTarget) {
			t.Fatalf("%s reached the metadata address: %v", name, err)
		}
	}
}
