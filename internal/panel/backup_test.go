package panel

import (
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestBackupRestore(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	restarted := make(chan struct{}, 1)
	s.restartHook = func() { restarted <- struct{}{} }
	s.st.DB.Create(&store.User{ID: "u1", Username: "before", UUID: "11111111-1111-1111-1111-111111111111", Password: "p", Status: "active", SubToken: "t1"})

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
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, restoreDir)); !os.IsNotExist(err) {
		t.Fatal("staging dir left behind")
	}
	pre, _ := filepath.Glob(filepath.Join(s.cfg.DataDir, backupDir, "pre-restore-*", "p.db"))
	if len(pre) != 1 {
		t.Fatalf("replaced database not kept: %v", pre)
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
	foreign := filepath.Join(t.TempDir(), "f.tar.gz")
	if err := o2.writeBackup(foreign); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(foreign)
	defer f.Close()
	if err := s2.stageRestore(f); err == nil {
		t.Fatal("foreign-key backup accepted")
	}
}
