package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run after building web/dist. Chromium's virtual authenticator exercises real
// signatures against the actual HTTP handlers and the shipped React UI.
func TestLivePasskeyBrowser(t *testing.T) {
	if os.Getenv("POLARIS_PASSKEY_BROWSER_TEST") == "" {
		t.Skip("POLARIS_PASSKEY_BROWSER_TEST required")
	}
	s, _, _ := newAdminClient(t)
	dir, e := filepath.Abs("../../web/dist")
	if e != nil {
		t.Fatal(e)
	}
	s.webFS = http.Dir(dir)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()
	s.cfg.PublicURL = strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	cmd := exec.CommandContext(t.Context(), "python3", "../../web/scripts/passkeys-live.py")
	cmd.Env = append(os.Environ(), "POLARIS_PASSKEY_TEST_URL="+s.cfg.PublicURL)
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("browser: %v\n%s", e, out)
	}
	t.Log(string(out))
}
