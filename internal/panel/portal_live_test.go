package panel

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// Exercise the JavaScript delivered by /u against the real login and poll
// handlers. TestPortal separately covers approval via the fake Telegram API,
// session issuance, rejection, logout and isolation from the admin API.
func TestLivePortalTelegramBrowser(t *testing.T) {
	if os.Getenv("POLARIS_PORTAL_BROWSER_TEST") == "" {
		t.Skip("POLARIS_PORTAL_BROWSER_TEST required")
	}
	for _, mail := range []bool{false, true} {
		t.Run("mail="+strconv.FormatBool(mail), func(t *testing.T) {
			s := newTestServer(t)
			s.st.SetSetting("portal_enabled", "true")
			if mail {
				useMail(t, s)
			}
			// Only bot availability is stubbed. No real Telegram messages are sent.
			s.tg.mu.Lock()
			s.tg.me, s.tg.running = "polaris_test_bot", true
			s.tg.mu.Unlock()
			srv := httptest.NewServer(s.Router())
			defer srv.Close()
			cmd := exec.CommandContext(t.Context(), "python3", "../../web/scripts/portal-telegram-live.py")
			cmd.Env = append(os.Environ(), "POLARIS_PORTAL_TEST_URL="+srv.URL, "POLARIS_PORTAL_EXPECT_MAIL="+strconv.FormatBool(mail))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("browser: %v\n%s", err, out)
			}
			t.Log(string(out))
		})
	}
}
