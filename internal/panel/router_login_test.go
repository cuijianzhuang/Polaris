package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// serveIndex must inline the login flags so the login page can render the
// passkey / GitHub / Telegram / SSO buttons on first paint instead of
// waiting for a GET /info round trip after mount.
func TestServeIndexInlinesLoginInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newTestServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html><head></head><body></body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.webFS = http.Dir(dir)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	s.serveIndex(c, "/secret")

	if w.Code != http.StatusOK {
		t.Fatalf("serveIndex status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `window.__POLARIS_BASE__="/secret/"`) {
		t.Fatalf("base not inlined: %s", body)
	}
	idx := strings.Index(body, `window.__POLARIS_LOGIN__=`)
	if idx < 0 {
		t.Fatalf("login info not inlined: %s", body)
	}
	rest := body[idx:]
	for _, want := range []string{`"passkeyLogin":`, `"githubLogin":`, `"telegramLogin":`, `"sso":`} {
		if !strings.Contains(rest, want) {
			t.Errorf("inlined login info missing %s: %s", want, rest)
		}
	}
	// json.Marshal escapes < as \u003c, so the inlined JSON cannot close
	// its own <script> tag even if a provider name contained markup.
	if end := strings.Index(rest, `;</script>`); end < 0 || strings.Contains(rest[:end], "</script>") {
		t.Errorf("inlined login JSON breaks out of its script tag: %s", rest)
	}
}
