package panel

import (
	"encoding/gob"
	"errors"
	"io"
	"log/slog"
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

// Backups are format-2 archives (backupfmt.go): every table as plain rows,
// plus the internal CA, so a restore into SQLite or PostgreSQL keeps every
// enrolled agent working. With a passphrase set they are encrypted and carry
// the secret key, so they also restore on a new server. They are kept in
// <data>/backups (optionally on a schedule) and can be pushed to Telegram,
// WebDAV and S3. A restore is staged in <data>/restore-pending and applied
// by the next start (the panel restarts right after staging), after a
// snapshot of the current data that retention never deletes.

const (
	backupDir  = "backups"
	restoreDir = "restore-pending"
	maxUpload  = 512 << 20
)

var backupNameRe = regexp.MustCompile(`^polaris-\d{8}-\d{6}(-[a-z]+)?\.tar\.gz(\.enc)?$`)

// sqlitePath is the database file, or "" for PostgreSQL / URI DSNs.
func sqlitePath(cfg *Config) string {
	dsn := cfg.DB.DSN
	if strings.Contains(dsn, "://") || strings.HasPrefix(dsn, "file:") || strings.Contains(dsn, "?") {
		return ""
	}
	return dsn
}

// backupPassphrase is the configured backup passphrase ("" = unencrypted).
func (s *Server) backupPassphrase() string {
	return s.st.Box.MustOpen(s.st.GetSetting("backup_passphrase", ""))
}

// writeBackup creates a backup archive at path.
func (s *Server) writeBackup(path, pass string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = writeArchive(f, s.st, s.cfg.DataDir, s.cfg.SecretKey, pass)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

func backupName(kind string, encrypted bool) string {
	n := "polaris-" + time.Now().Format("20060102-150405")
	if kind != "" {
		n += "-" + kind
	}
	n += ".tar.gz"
	if encrypted {
		n += ".enc"
	}
	return n
}

// createBackup writes a new archive into <data>/backups and prunes old ones.
func (s *Server) createBackup(kind string) (string, error) {
	dir := filepath.Join(s.cfg.DataDir, backupDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	pass := s.backupPassphrase()
	name := backupName(kind, pass != "")
	if err := s.writeBackup(filepath.Join(dir, name), pass); err != nil {
		return "", err
	}
	s.pruneBackups()
	return name, nil
}

type backupFile struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Created   time.Time `json:"created"`
	Encrypted bool      `json:"encrypted"`
}

func (s *Server) listBackupFiles() []backupFile {
	ents, _ := os.ReadDir(filepath.Join(s.cfg.DataDir, backupDir))
	out := []backupFile{}
	for _, e := range ents {
		if !backupNameRe.MatchString(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, backupFile{Name: e.Name(), Size: fi.Size(), Created: fi.ModTime(), Encrypted: strings.HasSuffix(e.Name(), ".enc")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// pruneBackups keeps the newest backup_keep archives; snapshots taken
// before a restore are never deleted automatically.
func (s *Server) pruneBackups() {
	keep := max(atoiDefault(s.st.GetSetting("backup_keep", "7"), 7), 1)
	i := 0
	for _, f := range s.listBackupFiles() {
		if strings.Contains(f.Name, "-prerestore") {
			continue
		}
		if i >= keep {
			_ = os.Remove(filepath.Join(s.cfg.DataDir, backupDir, f.Name))
		}
		i++
	}
}

// scheduledBackup runs hourly; it backs up when the schedule is due and
// pushes the archive to the configured destinations.
func (s *Server) scheduledBackup() {
	sched := s.st.GetSetting("backup_schedule", "off")
	if sched != "daily" && sched != "weekly" {
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
	s.pushBackup(filepath.Join(s.cfg.DataDir, backupDir, name), true)
}

// --- restore ---

func stagedPath(dataDir string) string { return filepath.Join(dataDir, restoreDir, "staged.gob") }

// stageRestore reads and checks an archive, then stages it for the next start.
func (s *Server) stageRestore(r io.Reader, pass string) error {
	arc, err := readArchive(r, pass, s.st.Box)
	if err != nil {
		return err
	}
	if err := arc.validate(s.st.Box); err != nil {
		return err
	}
	dir := filepath.Join(s.cfg.DataDir, restoreDir)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(stagedPath(s.cfg.DataDir), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = gob.NewEncoder(f).Encode(arc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.RemoveAll(dir)
	}
	return err
}

// applyStagedRestore applies a staged restore once the database is open:
// first a snapshot of the current data (kept, never pruned), then the
// archive, in one transaction. On failure the current data stays.
func applyStagedRestore(st *store.Store, cfg *Config, box *secret.Box, log *slog.Logger) {
	path := stagedPath(cfg.DataDir)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	var arc backupArchive
	err = gob.NewDecoder(f).Decode(&arc)
	f.Close()
	defer os.RemoveAll(filepath.Dir(path))
	if err != nil {
		log.Error("staged restore unreadable", "err", err)
		return
	}
	snap := filepath.Join(cfg.DataDir, backupDir, backupName("prerestore", false))
	if err := os.MkdirAll(filepath.Dir(snap), 0o700); err == nil {
		if out, err := os.OpenFile(snap, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			if err := writeArchive(out, st, cfg.DataDir, cfg.SecretKey, ""); err != nil {
				log.Error("snapshot before restore", "err", err)
			}
			out.Close()
		}
	}
	if err := restoreArchive(st, cfg.DataDir, &arc, box); err != nil {
		log.Error("restore failed; current data kept", "err", err)
		return
	}
	log.Warn("restored backup", "from_format", arc.Manifest.Format, "created", arc.Manifest.Created, "snapshot", snap)
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
	c.JSON(http.StatusOK, gin.H{"supported": true, "db": s.st.DB.Dialector.Name(), "dbSize": size, "files": s.listBackupFiles(),
		"dir": filepath.Join(s.cfg.DataDir, backupDir), "encrypted": s.backupPassphrase() != "", "targets": s.pushTargets()})
}

// backupCreate: POST /backup[?push=1].
func (s *Server) backupCreate(c *gin.Context) {
	name, err := s.createBackup("")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.auditCtx(c, "backup.create", name, "")
	out := gin.H{"name": name}
	if c.Query("push") == "1" || c.Query("telegram") == "1" {
		out["push"] = s.pushBackup(filepath.Join(s.cfg.DataDir, backupDir, name), false)
	}
	c.JSON(http.StatusOK, out)
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

// backupRestore: POST /backup/restore (multipart "file", "passphrase") or
// /backup/files/:name/restore {passphrase}. The panel restarts to apply it.
func (s *Server) backupRestore(c *gin.Context) {
	var r io.Reader
	src := ""
	pass := c.PostForm("passphrase")
	if c.Param("name") != "" {
		p, ok := s.backupPath(c)
		if !ok {
			return
		}
		var body struct {
			Passphrase string `json:"passphrase"`
		}
		_ = c.ShouldBindJSON(&body)
		if body.Passphrase != "" {
			pass = body.Passphrase
		}
		if pass == "" && strings.HasSuffix(p, ".enc") {
			pass = s.backupPassphrase() // made here with the current passphrase
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
	if err := s.stageRestore(r, pass); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "needPassphrase": errors.Is(err, errNeedPassphrase) || errors.Is(err, errBadPassphrase)})
		return
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
