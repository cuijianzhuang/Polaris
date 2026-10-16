package panel

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestAdminMaintenanceCommands(t *testing.T) {
	s := newTestServer(t)
	lock := time.Now().Add(time.Hour)
	s.st.DB.Create(&store.Admin{ID: "a1", Username: "root", PasswordHash: "x", TOTPEnabled: true, LockedUntil: &lock, FailedAttempts: 3})

	name, pw, err := AdminResetPassword(s.cfg, "root", "")
	if err != nil || name != "root" || len(pw) < 12 {
		t.Fatalf("reset: %q %q %v", name, pw, err)
	}
	var a store.Admin
	s.st.DB.First(&a, "id = ?", "a1")
	if bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(pw)) != nil || a.LockedUntil != nil || a.FailedAttempts != 0 || a.TokenVersion != 1 {
		t.Fatalf("after reset: %+v", a)
	}
	if _, _, err := AdminResetPassword(s.cfg, "root", "short"); err == nil {
		t.Fatal("short password accepted")
	}
	if _, _, err := AdminResetPassword(s.cfg, "nobody", ""); err == nil {
		t.Fatal("unknown admin accepted")
	}

	if _, err := AdminDisable2FA(s.cfg, "root"); err != nil {
		t.Fatal(err)
	}
	s.st.DB.First(&a, "id = ?", "a1")
	if a.TOTPEnabled || a.TokenVersion != 2 {
		t.Fatalf("2fa not reset: %+v", a)
	}

	out := filepath.Join(t.TempDir(), "backup.tar.gz.enc")
	if err := Backup(s.cfg, out, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := Backup(s.cfg, out, "pw"); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	s.st.DB.Model(&store.Admin{}).Where("username = ?", "root").Update("username", "renamed")
	if _, err := Restore(s.cfg, out, "wrong"); err == nil {
		t.Fatal("wrong passphrase restored")
	}
	if _, err := Restore(s.cfg, out, "pw"); err != nil {
		t.Fatal(err)
	}
	var b store.Admin
	if err := s.st.DB.First(&b, "username = ?", "root").Error; err != nil || b.TOTPEnabled {
		t.Fatalf("restored content: %+v %v", b, err)
	}
}
