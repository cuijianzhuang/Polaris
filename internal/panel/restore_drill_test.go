package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/backupfmt"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestRestoreDrillIsolatedEncryptedAndRecorded(t *testing.T) {
	s, _, admin := newAdminClient(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if err := s.st.DB.Create(&store.User{ID: "drill-user", Username: "before", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	s.st.SetSetting("tg_token", s.st.Box.Seal("synthetic-bot-secret"))
	s.st.SetSetting("backup_passphrase", s.st.Box.Seal("synthetic backup passphrase"))
	if code, out := admin.do("POST", "/backup", nil); code != 200 {
		t.Fatal(code, out)
	}
	_, info := admin.do("GET", "/backup", nil)
	name := info["files"].([]any)[0].(map[string]any)["name"].(string)
	if err := s.st.DB.Model(&store.User{}).Where("id = ?", "drill-user").Update("username", "after").Error; err != nil {
		t.Fatal(err)
	}
	ca, _ := os.ReadFile(filepath.Join(s.cfg.DataDir, "ca.crt"))
	restarted := false
	s.restartHook = func() { restarted = true }
	code, out := admin.do("POST", "/backup/files/"+name+"/drill", map[string]any{})
	if code != 200 || out["status"] != "passed" {
		t.Fatalf("drill: %d %v", code, out)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "drill-user")
	if u.Username != "after" || restarted {
		t.Fatal("drill modified current service")
	}
	if got, _ := os.ReadFile(filepath.Join(s.cfg.DataDir, "ca.crt")); !bytes.Equal(got, ca) {
		t.Fatal("production CA changed")
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "polaris-restore-drill-*")); len(left) != 0 {
		t.Fatal("temporary restore data retained", left)
	}
	var row store.RestoreDrill
	if err := s.st.DB.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Counts.V["users"] != 1 || row.BackupAt == nil || row.SHA256 == "" || row.RecoveryPointAgeSeconds == nil {
		t.Fatal("incomplete drill report")
	}
	if code, _ := admin.do("GET", "/backup/drills", nil); code != 200 {
		t.Fatal(code)
	}
	// Drill history must neither travel inside a business backup nor disappear
	// when a production restore imports older business tables.
	files, _, err := backupfmt.ExportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["restore_drills"]; ok {
		t.Fatal("drill history exported")
	}
	f, err := os.Open(filepath.Join(s.cfg.DataDir, backupDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	arc, err := backupfmt.ReadArchive(f, "synthetic backup passphrase", s.st.Box)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreArchive(s.st, s.cfg.DataDir, arc, s.st.Box); err != nil {
		t.Fatal(err)
	}
	var n int64
	s.st.DB.Model(&store.RestoreDrill{}).Count(&n)
	if n != 1 {
		t.Fatal("business restore overwrote operational drill history")
	}
}

func TestRestoreDrillRejectsDamagedSecretsAndDoesNotNeedLiveDB(t *testing.T) {
	s, _, _ := newAdminClient(t)
	var archive bytes.Buffer
	if err := writeArchive(&archive, s.st, s.cfg.DataDir, s.cfg.SecretKey, ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "copy.tar.gz")
	if err := os.WriteFile(path, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := *s.cfg
	cfg.DB.DSN = "postgres://unreachable.invalid/must-not-connect"
	result, err := DrillRestore(context.Background(), &cfg, path, "")
	if err != nil || result.Status != "passed" {
		t.Fatal("drill required live database", err, result.Error)
	}
	s.st.SetSetting("tg_token", "enc:v1:broken")
	archive.Reset()
	if err := writeArchive(&archive, s.st, s.cfg.DataDir, s.cfg.SecretKey, ""); err != nil {
		t.Fatal(err)
	}
	result = runRestoreDrill(context.Background(), s.st.Box, bytes.NewReader(archive.Bytes()), "", "damaged")
	if result.Status != "failed" || result.Checks.V[len(result.Checks.V)-1].Name != "source_secrets" {
		t.Fatal("unreadable sealed value accepted")
	}
	b, _ := json.Marshal(result)
	if bytes.Contains(b, []byte("enc:v1:broken")) {
		t.Fatal("ciphertext leaked to report")
	}
	result = runRestoreDrill(context.Background(), s.st.Box, bytes.NewReader([]byte("not an archive")), "", "bad")
	if result.Status != "failed" {
		t.Fatal("damaged archive accepted")
	}
}
