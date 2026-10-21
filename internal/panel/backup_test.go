package panel

import (
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestBackupRestore(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	restarted := make(chan struct{}, 1)
	s.restartHook = func() { restarted <- struct{}{} }
	s.st.DB.Create(&store.User{ID: "u1", Username: "before", UUID: "11111111-1111-1111-1111-111111111111", Password: "p", Status: "active", SubToken: "t1"})
	s.st.DB.Create(&store.Node{ID: "net", Name: "net"})
	for _, sample := range []*polarisv1.SystemStats{
		bootSample("boot-1", "nic-a", 100, 100, 200),
		bootSample("boot-1", "nic-a", 200, 110, 220),
		bootSample("boot-2", "nic-b", 10, 1, 2),
	} {
		if err := s.accountNodeNet("net", sample, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	st, out := admin.do("POST", "/backup", nil)
	if st != 200 {
		t.Fatalf("create %d %v", st, out)
	}
	name := out["name"].(string)
	_, info := admin.do("GET", "/backup", nil)
	if files := info["files"].([]any); len(files) != 1 || files[0].(map[string]any)["name"] != name || info["supported"] != true {
		t.Fatalf("info %v", info)
	}
	req, _ := http.NewRequest("GET", admin.base+"/backup/files/"+name, nil)
	req.Header.Set("Authorization", "Bearer "+admin.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(archive) < 100 || archive[0] != 0x1f {
		t.Fatalf("download %d %d bytes", resp.StatusCode, len(archive))
	}
	if st, _ := admin.do("GET", "/backup/files/..%2Fp.db", nil); st != 404 {
		t.Fatalf("path traversal: %d", st)
	}

	// Change state after the backup, then restore the archive by upload.
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Update("username", "after")
	s.st.DB.Where("node_id = ?", "net").Delete(&store.NodeNetBoot{})
	upload := func(c *apiClient, data []byte) (int, string) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fw, _ := w.CreateFormFile("file", "b.tar.gz")
		fw.Write(data)
		w.Close()
		req, _ := http.NewRequest("POST", c.base+"/backup/restore", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+c.tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if st, body := upload(admin, []byte("not a backup")); st != 400 {
		t.Fatalf("garbage accepted: %d %s", st, body)
	}
	if st, body := upload(admin, archive); st != 200 {
		t.Fatalf("restore %d %s", st, body)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("no restart after restore")
	}
	srv.Close()
	s.st.Close()

	// Next start applies it: the user is back to its backed-up name.
	s2, err := New(s.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var u store.User
	s2.st.DB.First(&u, "id = ?", "u1")
	if u.Username != "before" {
		t.Fatalf("restored username %q", u.Username)
	}
	// A real restart after restore must keep the old boot retired and resume
	// the active boot's baseline without charging its lifetime bytes again.
	for _, sample := range []*polarisv1.SystemStats{
		bootSample("boot-1", "nic-a", 300, 120, 240),
		bootSample("boot-2", "nic-b", 20, 6, 12),
	} {
		if err := s2.accountNodeNet("net", sample, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var node store.Node
	if err := s2.st.DB.First(&node, "id = ?", "net").Error; err != nil {
		t.Fatal(err)
	}
	if node.NetRx != 15 || node.NetTx != 30 || node.NetBootID != "boot-2" {
		t.Fatalf("restore lost network replay protection: %+v", node)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, restoreDir)); !os.IsNotExist(err) {
		t.Fatal("staging dir left behind")
	}
	pre, _ := filepath.Glob(filepath.Join(s.cfg.DataDir, backupDir, "polaris-*-prerestore.tar.gz"))
	if len(pre) != 1 {
		t.Fatalf("snapshot before restore not kept: %v", pre)
	}
	// Retention never deletes the snapshot.
	s2.st.SetSetting("backup_keep", "1")
	s2.createBackup("")
	s2.createBackup("x")
	if pre2, _ := filepath.Glob(filepath.Join(s.cfg.DataDir, backupDir, "polaris-*-prerestore.tar.gz")); len(pre2) != 1 {
		t.Fatal("retention deleted the pre-restore snapshot")
	}

	// A backup from a panel with another secret key is refused.
	dir := t.TempDir()
	cfg := &Config{DataDir: dir, SecretKey: "different", AdminPath: "/"}
	cfg.DB.DSN = filepath.Join(dir, "polaris.db")
	o2, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	o2.st.DB.Create(&store.Admin{ID: "a", Username: "x"})
	o2.st.SetSetting("tg_token", o2.st.Box.Seal("123:secret-bot-token"))
	foreign := filepath.Join(t.TempDir(), "f.tar.gz")
	if err := o2.writeBackup(foreign, ""); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(foreign)
	defer f.Close()
	if err := s2.stageRestore(f, ""); err == nil || !strings.Contains(err.Error(), "secret_key") {
		t.Fatalf("foreign-key backup accepted: %v", err)
	}

	// Encrypted, it carries the key: it restores here, re-sealed with ours.
	enc := filepath.Join(t.TempDir(), "f.tar.gz.enc")
	if err := o2.writeBackup(enc, "correct horse"); err != nil {
		t.Fatal(err)
	}
	for _, pass := range []string{"", "wrong"} {
		f, _ := os.Open(enc)
		err := s2.stageRestore(f, pass)
		f.Close()
		if err == nil {
			t.Fatalf("passphrase %q accepted", pass)
		}
	}
	f2, _ := os.Open(enc)
	if err := s2.stageRestore(f2, "correct horse"); err != nil {
		t.Fatal(err)
	}
	f2.Close()
	s2.st.Close()
	s3, err := New(s.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var x store.Admin
	if s3.st.DB.First(&x, "username = ?", "x").Error != nil {
		t.Fatal("restored data missing")
	}
	var before int64
	if s3.st.DB.Model(&store.User{}).Where("username = ?", "before").Count(&before); before != 0 {
		t.Fatal("old data left after restore")
	}
	if got := s3.st.Box.MustOpen(s3.st.GetSetting("tg_token", "")); got != "123:secret-bot-token" {
		t.Fatalf("secret not re-sealed for this panel: %q", got)
	}
	if s3.ca == nil || string(s3.ca.CertPEM) != string(o2.ca.CertPEM) {
		t.Fatal("CA not restored")
	}
	s3.st.Close()
}

// TestLegacyBackupRestore: a format-1 backup (a bare SQLite file) still restores.
func TestLegacyBackupRestore(t *testing.T) {
	s, _, _ := newAdminClient(t)
	if s.st.DB.Dialector.Name() != "sqlite" {
		t.Skip("format-1 backups are SQLite database files")
	}
	s.st.DB.Create(&store.User{ID: "u1", Username: "legacy", UUID: "22222222-2222-2222-2222-222222222222", Password: "p", Status: "active", SubToken: "t2"})
	db := filepath.Join(t.TempDir(), "old.db")
	if err := s.st.DB.Exec("VACUUM INTO ?", db).Error; err != nil {
		t.Fatal(err)
	}
	s.st.DB.Where("id = ?", "u1").Delete(&store.User{})
	f, _ := os.Open(db)
	defer f.Close()
	arc, err := readArchive(f, "", s.st.Box)
	if err != nil || arc.Manifest.Format != 1 {
		t.Fatalf("legacy archive: %v %+v", err, arc)
	}
	if err := restoreArchive(s.st, s.cfg.DataDir, arc, s.st.Box); err != nil {
		t.Fatal(err)
	}
	var u store.User
	if s.st.DB.First(&u, "id = ?", "u1").Error != nil || u.Username != "legacy" {
		t.Fatal("legacy restore lost the user")
	}
}

func TestBackupEncryption(t *testing.T) {
	plain := bytes.Repeat([]byte("polaris backup "), 20000) // several chunks
	var buf bytes.Buffer
	w, err := encryptWriter(&buf, "pw")
	if err != nil {
		t.Fatal(err)
	}
	w.Write(plain)
	w.Close()
	if bytes.Contains(buf.Bytes(), []byte("polaris backup")) {
		t.Fatal("plaintext visible")
	}
	r, err := decryptReader(bytes.NewReader(buf.Bytes()), "pw")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip: %v (%d bytes)", err, len(got))
	}
	r, _ = decryptReader(bytes.NewReader(buf.Bytes()), "nope")
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("wrong passphrase decrypted")
	}
	r, _ = decryptReader(bytes.NewReader(buf.Bytes()[:buf.Len()-100]), "pw") // truncated
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("truncated archive accepted")
	}
}
