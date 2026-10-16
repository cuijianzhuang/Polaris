package panel

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Backups are .tar.gz archives of a consistent database copy (VACUUM INTO)
// plus the internal CA (ca.crt, sealed ca.key), so a restore on a new server
// keeps every enrolled agent working. They are kept in <data>/backups,
// optionally on a schedule and sent to Telegram owners. A restore is staged
// in <data>/restore-pending and applied by the next start (the panel exits
// right after staging; systemd / Docker bring it back).

const (
	backupDir  = "backups"
	restoreDir = "restore-pending"
	maxUpload  = 512 << 20
)

var backupNameRe = regexp.MustCompile(`^polaris-\d{8}-\d{6}(-[a-z]+)?\.tar\.gz$`)

// sqlitePath is the database file, or "" for PostgreSQL / URI DSNs.
func sqlitePath(cfg *Config) string {
	dsn := cfg.DB.DSN
	if strings.Contains(dsn, "://") || strings.HasPrefix(dsn, "file:") || strings.Contains(dsn, "?") {
		return ""
	}
	return dsn
}

// writeBackup creates a backup archive at path.
func (s *Server) writeBackup(path string) error {
	db := sqlitePath(s.cfg)
	if db == "" {
		return errors.New("仅 SQLite 支持面板备份；PostgreSQL 请用 pg_dump")
	}
	tmp := path + ".db.tmp"
	_ = os.Remove(tmp)
	abs, err := filepath.Abs(tmp)
	if err != nil {
		return err
	}
	if err := s.st.DB.Exec("VACUUM INTO ?", abs).Error; err != nil {
		return fmt.Errorf("数据库快照失败: %w", err)
	}
	defer os.Remove(tmp)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	add := func(name, src string) error {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	}
	err = add("polaris.db", tmp)
	for _, n := range []string{"ca.crt", "ca.key"} {
		if err == nil {
			err = add(n, filepath.Join(s.cfg.DataDir, n))
		}
	}
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

func backupName(kind string) string {
	n := "polaris-" + time.Now().Format("20060102-150405")
	if kind != "" {
		n += "-" + kind
	}
	return n + ".tar.gz"
}

// createBackup writes a new archive into <data>/backups and prunes old ones.
func (s *Server) createBackup(kind string) (string, error) {
	dir := filepath.Join(s.cfg.DataDir, backupDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := backupName(kind)
	if err := s.writeBackup(filepath.Join(dir, name)); err != nil {
		return "", err
	}
	s.pruneBackups()
	return name, nil
}

type backupFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

func (s *Server) listBackupFiles() []backupFile {
	ents, _ := os.ReadDir(filepath.Join(s.cfg.DataDir, backupDir))
	out := []backupFile{}
	for _, e := range ents {
		if !backupNameRe.MatchString(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, backupFile{Name: e.Name(), Size: fi.Size(), Created: fi.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (s *Server) pruneBackups() {
	keep := max(atoiDefault(s.st.GetSetting("backup_keep", "7"), 7), 1)
	for i, f := range s.listBackupFiles() {
		if i >= keep {
			_ = os.Remove(filepath.Join(s.cfg.DataDir, backupDir, f.Name))
		}
	}
}

// scheduledBackup runs hourly; it backs up when the schedule is due.
func (s *Server) scheduledBackup() {
	sched := s.st.GetSetting("backup_schedule", "off")
	if sched != "daily" && sched != "weekly" || sqlitePath(s.cfg) == "" {
		return
	}
	now := time.Now()
	if now.Hour() != max(min(atoiDefault(s.st.GetSetting("backup_hour", "4"), 4), 23), 0) {
		return
	}
	if sched == "weekly" && now.Weekday() != time.Monday {
		return
	}
	if fs := s.listBackupFiles(); len(fs) > 0 && now.Sub(fs[0].Created) < 20*time.Hour && strings.Contains(fs[0].Name, "-auto") {
		return
	}
	name, err := s.createBackup("auto")
	if err != nil {
		s.log.Error("scheduled backup", "err", err)
		s.notify(Event{Kind: "backup_failed", Text: "⚠️ 自动备份失败：" + err.Error()})
		return
	}
	s.audit("system", "backup.create", name, "scheduled", "")
	if s.st.GetSetting("backup_tg", "false") == "true" {
		s.tg.sendBackup(filepath.Join(s.cfg.DataDir, backupDir, name))
	}
}

// --- restore ---

// stageRestore validates an uploaded archive (or bare .db) and stages it.
func (s *Server) stageRestore(r io.Reader) error {
	if sqlitePath(s.cfg) == "" {
		return errors.New("仅 SQLite 支持面板恢复")
	}
	dir := filepath.Join(s.cfg.DataDir, restoreDir)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()
	head := make([]byte, 16)
	n, _ := io.ReadFull(r, head)
	r = io.MultiReader(strings.NewReader(string(head[:n])), io.LimitReader(r, maxUpload))
	switch {
	case n >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		if err := extractBackup(r, dir); err != nil {
			return err
		}
	case n == 16 && string(head) == "SQLite format 3\x00":
		f, err := os.OpenFile(filepath.Join(dir, "polaris.db"), os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		f.Close()
		if err != nil {
			return err
		}
	default:
		return errors.New("不是 Polaris 备份文件（需要 .tar.gz 备份包或 SQLite 数据库）")
	}
	if err := validateRestore(dir, s.st.Box); err != nil {
		return err
	}
	ok = true
	return nil
}

func extractBackup(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}
	tr := tar.NewReader(gz)
	want := map[string]bool{"polaris.db": true, "ca.crt": true, "ca.key": true}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("解压失败: %w", err)
		}
		if !want[h.Name] || h.Typeflag != tar.TypeReg {
			continue
		}
		f, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, io.LimitReader(tr, maxUpload))
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// validateRestore checks the staged files open with this panel's secret key.
func validateRestore(dir string, box *secret.Box) error {
	if _, err := os.Stat(filepath.Join(dir, "polaris.db")); err != nil {
		return errors.New("备份中没有数据库文件")
	}
	_, e1 := os.Stat(filepath.Join(dir, "ca.crt"))
	_, e2 := os.Stat(filepath.Join(dir, "ca.key"))
	if (e1 == nil) != (e2 == nil) {
		return errors.New("备份中的 CA 证书不完整")
	}
	if e1 == nil {
		if _, err := loadOrCreateCA(dir, box); err != nil {
			return errors.New("无法解密备份中的 CA 私钥：备份来自使用不同 secret_key 的面板")
		}
	}
	st, err := store.Open(filepath.Join(dir, "polaris.db"), box, false)
	if err != nil {
		return fmt.Errorf("数据库无法打开: %w", err)
	}
	defer st.Close()
	var check string
	if st.DB.Raw("PRAGMA integrity_check").Scan(&check); check != "ok" {
		return fmt.Errorf("数据库已损坏: %s", check)
	}
	var admins int64
	if st.DB.Model(&store.Admin{}).Count(&admins); admins == 0 {
		return errors.New("备份中没有管理员账号")
	}
	var rev store.ConfigRevision
	if st.DB.Select("spec").First(&rev).Error == nil {
		if _, err := box.Open(rev.Spec); err != nil {
			return errors.New("无法解密备份中的数据：备份来自使用不同 secret_key 的面板")
		}
	}
	return nil
}

// applyPendingRestore swaps a staged restore in before the database opens;
// the replaced files move to <data>/backups/pre-restore-<time>/.
func applyPendingRestore(cfg *Config) (bool, error) {
	db := sqlitePath(cfg)
	dir := filepath.Join(cfg.DataDir, restoreDir)
	if db == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(dir, "polaris.db")); err != nil {
		return false, nil
	}
	old := filepath.Join(cfg.DataDir, backupDir, "pre-restore-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(old, 0o700); err != nil {
		return false, err
	}
	move := func(src, dst string) error {
		if _, err := os.Stat(src); err != nil {
			return nil
		}
		return os.Rename(src, dst)
	}
	for _, sfx := range []string{"", "-wal", "-shm"} {
		if err := move(db+sfx, filepath.Join(old, filepath.Base(db)+sfx)); err != nil {
			return false, err
		}
	}
	if err := move(filepath.Join(dir, "polaris.db"), db); err != nil {
		return false, err
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err == nil {
		for _, n := range []string{"ca.crt", "ca.key"} {
			if err := move(filepath.Join(cfg.DataDir, n), filepath.Join(old, n)); err != nil {
				return false, err
			}
			if err := move(filepath.Join(dir, n), filepath.Join(cfg.DataDir, n)); err != nil {
				return false, err
			}
		}
	}
	return true, os.RemoveAll(dir)
}

// --- API ---

// backupInfo: GET /backup.
func (s *Server) backupInfo(c *gin.Context) {
	var size int64
	if db := sqlitePath(s.cfg); db != "" {
		if fi, err := os.Stat(db); err == nil {
			size = fi.Size()
		}
	}
	c.JSON(http.StatusOK, gin.H{"supported": sqlitePath(s.cfg) != "", "dbSize": size, "files": s.listBackupFiles(),
		"dir": filepath.Join(s.cfg.DataDir, backupDir)})
}

// backupCreate: POST /backup.
func (s *Server) backupCreate(c *gin.Context) {
	name, err := s.createBackup("")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.auditCtx(c, "backup.create", name, "")
	if c.Query("telegram") == "1" {
		go s.tg.sendBackup(filepath.Join(s.cfg.DataDir, backupDir, name))
	}
	c.JSON(http.StatusOK, gin.H{"name": name})
}

func (s *Server) backupPath(c *gin.Context) (string, bool) {
	name := c.Param("name")
	if !backupNameRe.MatchString(name) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return "", false
	}
	p := filepath.Join(s.cfg.DataDir, backupDir, name)
	if _, err := os.Stat(p); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return "", false
	}
	return p, true
}

// backupDownload: GET /backup/files/:name.
func (s *Server) backupDownload(c *gin.Context) {
	if p, ok := s.backupPath(c); ok {
		s.auditCtx(c, "backup.download", c.Param("name"), "")
		c.FileAttachment(p, c.Param("name"))
	}
}

// backupDelete: DELETE /backup/files/:name.
func (s *Server) backupDelete(c *gin.Context) {
	if p, ok := s.backupPath(c); ok {
		_ = os.Remove(p)
		s.auditCtx(c, "backup.delete", c.Param("name"), "")
		c.Status(http.StatusNoContent)
	}
}

// backupRestore: POST /backup/restore (multipart "file") or
// /backup/files/:name/restore. The panel restarts to apply it.
func (s *Server) backupRestore(c *gin.Context) {
	var r io.Reader
	src := ""
	if c.Param("name") != "" {
		p, ok := s.backupPath(c)
		if !ok {
			return
		}
		f, err := os.Open(p)
		if err != nil {
			serverError(c, err)
			return
		}
		defer f.Close()
		r, src = f, c.Param("name")
	} else {
		fh, err := c.FormFile("file")
		if err != nil {
			badRequest(c, errors.New("请选择备份文件"))
			return
		}
		f, err := fh.Open()
		if err != nil {
			serverError(c, err)
			return
		}
		defer f.Close()
		r, src = f, "upload: "+fh.Filename
	}
	if err := s.stageRestore(r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Keep a copy of the current state, then restart into the restore.
	if name, err := s.createBackup("prerestore"); err == nil {
		s.log.Info("backup before restore", "file", name)
	}
	s.auditCtx(c, "backup.restore", src, "restarting")
	c.JSON(http.StatusOK, gin.H{"restarting": true})
	go func() {
		time.Sleep(time.Second)
		s.log.Warn("restarting to apply restored backup", "from", src)
		if s.restartHook != nil {
			s.restartHook()
			return
		}
		s.restart()
	}()
}

// restart re-executes the panel binary so a staged restore is applied; when
// exec is unavailable it exits and leaves the restart to systemd / Docker.
func (s *Server) restart() {
	_ = s.st.Close()
	if exe, err := os.Executable(); err == nil {
		err = syscall.Exec(exe, os.Args, os.Environ())
		s.log.Error("re-exec failed, exiting for the supervisor to restart", "err", err)
	}
	os.Exit(0)
}
