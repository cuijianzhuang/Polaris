package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// A new password bcrypt cannot hash (over 72 bytes) is refused; it used to
// store an empty hash and lock the admin out.
func TestChangePasswordTooLongKeepsOldPassword(t *testing.T) {
	s, srv, c := newAdminClient(t)
	long := strings.Repeat("密", 25) // 75 bytes
	if code, out := c.do("POST", "/auth/password", map[string]string{"old": "old-password", "new": long}); code != 400 {
		t.Fatalf("too-long password: %d %v", code, out)
	}
	var a store.Admin
	s.st.DB.First(&a, "id = ?", "a1")
	if a.PasswordHash == "" {
		t.Fatal("password hash was cleared")
	}
	anon := &apiClient{t: t, base: srv.URL + s.cfg.APIBase()}
	if code, out := anon.do("POST", "/auth/login", map[string]string{"username": "root", "password": "old-password"}); code != 200 {
		t.Fatalf("old password no longer works: %d %v", code, out)
	}
	// A normal change still works and signs the other sessions out.
	if code, out := c.do("POST", "/auth/password", map[string]string{"old": "old-password", "new": "new-password-1"}); code != 200 {
		t.Fatalf("change: %d %v", code, out)
	}
	var live int64
	s.st.DB.Model(&store.AdminSession{}).Where("admin_id = ? AND revoked = ?", "a1", false).Count(&live)
	if live != 1 {
		t.Fatalf("sessions left after a password change = %d, want only the new one", live)
	}
}
