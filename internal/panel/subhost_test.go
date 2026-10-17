package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestSubHostOnlyServesSubscriptions(t *testing.T) {
	s := newTestServer(t)
	s.cfg.PublicURL = "https://panel.example.com"
	s.cfg.AdminPath = "/admin"
	s.st.SetSetting("sub_base_url", "https://sub.example.com")
	resetSubHostCache()
	h := s.Router()
	get := func(host, path string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	decoyCode, decoyBody := get("panel.example.com", "/no-such-page")
	api := s.cfg.APIBase() + "/auth/me"

	// The panel host is unchanged.
	if code, body := get("panel.example.com", api); code == decoyCode && body == decoyBody {
		t.Errorf("panel host API answered with the decoy")
	}
	// The subscription host hides the admin UI and API...
	for _, p := range []string{api, "/admin", "/admin/", "/install.sh"} {
		if code, body := get("sub.example.com", p); code != decoyCode || body != decoyBody {
			t.Errorf("sub host %s: %d %q, want the decoy", p, code, body[:min(len(body), 80)])
		}
	}
	// ...but serves subscriptions.
	s.st.DB.Create(&store.User{ID: "u1", Username: "alice", UUID: "11111111-1111-1111-1111-111111111111", Password: "p", Status: "active", SubToken: "tok1-0123456789abcdef"})
	if code, _ := get("sub.example.com:443", "/sub/tok1-0123456789abcdef"); code != http.StatusOK {
		t.Errorf("sub host subscription: %d", code)
	}
	// Strict mode off: no separation.
	s.st.SetSetting("sub_host_strict", "0")
	resetSubHostCache()
	if code, body := get("sub.example.com", api); code == decoyCode && body == decoyBody {
		t.Errorf("non-strict sub host still hides the API")
	}
}

func TestSubHostGuardsAdminLockout(t *testing.T) {
	s := newTestServer(t)
	s.cfg.PublicURL = ""
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(`{"sub_base_url":"https://panel.example.com"}`))
	c.Request.Host = "panel.example.com"
	c.Request.Header.Set("Content-Type", "application/json")
	s.putSettings(c)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "后台会打不开") {
		t.Fatalf("setting the admin's own host as sub domain: %d %s", w.Code, w.Body.String())
	}
	if got := s.st.GetSetting("sub_base_url", ""); got != "" {
		t.Fatalf("saved anyway: %q", got)
	}
}
