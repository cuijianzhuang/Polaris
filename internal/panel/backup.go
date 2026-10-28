package panel

import (
	"context"
	"encoding/gob"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/backupfmt"
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
	maxUpload  = backupfmt.MaxArchiveBytes
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

// createBackup writes a new archive, moves it into the backup store and
// prunes old ones. local is a copy on this machine for pushing elsewhere;
// call done once finished with it.
func (s *Server) createBackup(kind string) (name, local string, done func(), err error) {
	defer func() { s.recordOperationalResult("backup", "备份保存失败，请检查存储配置", err) }()
	bs, err := backupStoreFor(s.st, s.cfg.DataDir)
	if err != nil {
		return "", "", nil, err
	}
	pass := s.backupPassphrase()
	name = backupName(kind, pass != "")
	tmp := filepath.Join(s.cfg.DataDir, ".tmp-"+name)
	if err := s.writeBackup(tmp, pass); err != nil {
		return "", "", nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := bs.put(ctx, name, tmp); err != nil {
		os.Remove(tmp)
		return "", "", nil, err
	}
	if l, ok := bs.(*localStore); ok {
		local, done = filepath.Join(l.dir, name), func() {}
	} else {
		local, done = tmp, func() { os.Remove(tmp) }
	}
	s.pruneBackups(ctx, bs)
	return name, local, done, nil
}

type backupFile struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Created   time.Time `json:"created"`
	Encrypted bool      `json:"encrypted"`
}

// pruneBackups keeps the newest backup_keep archives; snapshots taken
// before a restore are never deleted automatically.
func (s *Server) pruneBackups(ctx context.Context, bs backupStore) {
	keep := max(atoiDefault(s.st.GetSetting("backup_keep", "7"), 7), 1)
	files, err := bs.list(ctx)
	if err != nil {
		s.log.Warn("prune backups", "err", err)
		return
	}
	i := 0
	for _, f := range files {
		if strings.Contains(f.Name, "-prerestore") {
			continue
		}
		if i >= keep {
			if err := bs.remove(ctx, f.Name); err != nil && !errors.Is(err, errBackupNotFound) {
				s.log.Warn("prune backups", "name", f.Name, "err", err)
			}
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
	if bs, err := backupStoreFor(s.st, s.cfg.DataDir); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		fs, err := bs.list(ctx)
		cancel()
		// Another instance may have run it already (shared S3 store).
		if err == nil && len(fs) > 0 && now.Sub(fs[0].Created) < 20*time.Hour && strings.Contains(fs[0].Name, "-auto") {
			return
		}
	}
	name, local, done, err := s.createBackup("auto")
	if err != nil {
		s.log.Error("scheduled backup", "err", err)
		s.notify(Event{Kind: "backup_failed", Text: "⚠️ 自动备份失败：" + err.Error()})
		return
	}
	s.audit("system", "backup.create", name, "scheduled", "")
	s.pushBackup(local, true)
	done()
}

// --- restore ---

func stagedPath(dataDir string) string { return filepath.Join(dataDir, restoreDir, "staged.gob") }

// stageRestore reads and checks an archive, then stages it for the next start.
func (s *Server) stageRestore(r io.Reader, pass string) error {
	arc, err := backupfmt.ReadArchive(r, pass, s.st.Box)
	if err != nil {
		return err
	}
	if err := arc.Validate(s.st.Box); err != nil {
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
func applyStagedRestore(st *store.Store, cfg *Config, box *secret.Box, log *slog.Logger) bool {
	path := stagedPath(cfg.DataDir)
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	var arc backupfmt.Archive
	err = gob.NewDecoder(f).Decode(&arc)
	f.Close()
	defer os.RemoveAll(filepath.Dir(path))
	if err != nil {
		log.Error("staged restore unreadable", "err", err)
		return false
	}
	snap := snapshotBeforeRestore(st, cfg, log)
	if err := restoreArchive(st, cfg.DataDir, &arc, box); err != nil {
		log.Error("restore failed; current data kept", "err", err)
		return false
	}
	log.Warn("restored backup", "from_format", arc.Manifest.Format, "created", arc.Manifest.Created, "snapshot", snap)
	return true
}

// snapshotBeforeRestore saves the current data as a "-prerestore" archive in
// the backup store (the local directory when the store is unusable) and
// returns where it went.
func snapshotBeforeRestore(st *store.Store, cfg *Config, log *slog.Logger) string {
	name := backupName("prerestore", false)
	tmp := filepath.Join(cfg.DataDir, ".tmp-"+name)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		log.Error("snapshot before restore", "err", err)
		return ""
	}
	defer os.Remove(tmp) // gone already once moved into the local store
	err = writeArchive(out, st, cfg.DataDir, cfg.SecretKey, "")
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		log.Error("snapshot before restore", "err", err)
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	bs, err := backupStoreFor(st, cfg.DataDir)
	if err == nil {
		if err = bs.put(ctx, name, tmp); err == nil {
			return bs.where() + "/" + name
		}
	}
	log.Error("snapshot before restore: backup store failed, keeping it locally", "err", err)
	l := &localStore{dir: filepath.Join(cfg.DataDir, backupDir)}
	if err := l.put(ctx, name, tmp); err != nil {
		log.Error("snapshot before restore", "err", err)
		return ""
	}
	return filepath.Join(l.dir, name)
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
	files, where, storeErr := []backupFile{}, "", ""
	bs, err := backupStoreFor(s.st, s.cfg.DataDir)
	if err == nil {
		where = bs.where()
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		var fs []backupFile
		if fs, err = bs.list(ctx); err == nil {
			files = fs
		}
		cancel()
	}
	if err != nil {
		storeErr = err.Error()
	}
	c.JSON(http.StatusOK, gin.H{"supported": true, "db": s.st.DB.Dialector.Name(), "dbSize": size, "files": files,
		"dir": where, "store": s.st.GetSetting("backup_store", "local"), "storeError": storeErr,
		"encrypted": s.backupPassphrase() != "", "targets": s.pushTargets()})
}

// backupCreate: POST /backup[?push=1].
func (s *Server) backupCreate(c *gin.Context) {
	name, local, done, err := s.createBackup("")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer done()
	s.auditCtx(c, "backup.create", name, "")
	out := gin.H{"name": name}
	if c.Query("push") == "1" || c.Query("telegram") == "1" {
		out["push"] = s.pushBackup(local, false)
	}
	c.JSON(http.StatusOK, out)
}

// openBackup opens the archive named in the URL.
func (s *Server) openBackup(c *gin.Context) (io.ReadCloser, int64, bool) {
	bs, ok := s.backupNamed(c)
	if !ok {
		return nil, 0, false
	}
	rc, n, err := bs.open(c.Request.Context(), c.Param("name"))
	if errors.Is(err, errBackupNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return nil, 0, false
	}
	if err != nil {
		badRequest(c, err)
		return nil, 0, false
	}
	return rc, n, true
}

// backupNamed checks the name in the URL and returns the store.
func (s *Server) backupNamed(c *gin.Context) (backupStore, bool) {
	if !backupNameRe.MatchString(c.Param("name")) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return nil, false
	}
	bs, err := backupStoreFor(s.st, s.cfg.DataDir)
	if err != nil {
		badRequest(c, err)
		return nil, false
	}
	return bs, true
}

// backupDownload: GET /backup/files/:name.
func (s *Server) backupDownload(c *gin.Context) {
	rc, n, ok := s.openBackup(c)
	if !ok {
		return
	}
	defer rc.Close()
	s.auditCtx(c, "backup.download", c.Param("name"), "")
	c.DataFromReader(http.StatusOK, n, "application/octet-stream", rc,
		map[string]string{"Content-Disposition": `attachment; filename="` + c.Param("name") + `"`})
}

// backupDelete: DELETE /backup/files/:name.
func (s *Server) backupDelete(c *gin.Context) {
	bs, ok := s.backupNamed(c)
	if !ok {
		return
	}
	err := bs.remove(c.Request.Context(), c.Param("name"))
	if errors.Is(err, errBackupNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		badRequest(c, err)
		return
	}
	s.auditCtx(c, "backup.delete", c.Param("name"), "")
	c.Status(http.StatusNoContent)
}

// backupRestore: POST /backup/restore (multipart "file", "passphrase") or
// /backup/files/:name/restore {passphrase}. The panel restarts to apply it.
func (s *Server) backupRestore(c *gin.Context) {
	var r io.Reader
	src := ""
	pass := c.PostForm("passphrase")
	if c.Param("name") != "" {
		rc, _, ok := s.openBackup(c)
		if !ok {
			return
		}
		defer rc.Close()
		var body struct {
			Passphrase string `json:"passphrase"`
		}
		_ = c.ShouldBindJSON(&body)
		if body.Passphrase != "" {
			pass = body.Passphrase
		}
		if pass == "" && strings.HasSuffix(c.Param("name"), ".enc") {
			pass = s.backupPassphrase() // made here with the current passphrase
		}
		r, src = rc, c.Param("name")
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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "needPassphrase": errors.Is(err, backupfmt.ErrNeedPassphrase) || errors.Is(err, backupfmt.ErrBadPassphrase)})
		return
	}
	s.auditCtx(c, "backup.restore", src, "restarting")
	c.JSON(http.StatusOK, gin.H{"restarting": true})
	go func() {
		time.Sleep(time.Second)
		s.log.Warn("restarting to apply restored backup", "from", src)
		s.restartNow()
	}()
}

// restartNow restarts the panel (tests replace it with restartHook).
func (s *Server) restartNow() {
	if s.restartHook != nil {
		s.restartHook()
		return
	}
	s.restart()
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

// restoreArchive replaces the database (and CA files in dataDir) with the
// archive's, all or nothing for the database.
func restoreArchive(st *store.Store, dataDir string, arc *backupfmt.Archive, box *secret.Box) error {
	src, err := arc.SourceBox(box)
	if err != nil {
		return err
	}
	reseal := backupfmt.Resealer(src, box)
	memberID, err := loadMemberID(dataDir)
	if err != nil {
		return err
	}
	if err := st.DB.Transaction(func(tx *gorm.DB) error {
		if e := backupfmt.ImportAll(tx, arc.Tables, reseal); e != nil {
			return e
		}
		// Authentication challenges must never be revived from a backup.
		if e := tx.Where("name LIKE ?", "passkey:%").Delete(&store.EphemeralKV{}).Error; e != nil {
			return e
		}
		// A database rollback must not revive spent account recovery secrets or
		// sessions established with a passkey. Administrators regenerate codes after restore.
		if e := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&store.AccountRecoveryCode{}).Error; e != nil {
			return e
		}
		if e := tx.Model(&store.AdminSession{}).Where("via = ?", "passkey").Update("revoked", true).Error; e != nil {
			return e
		}
		if e := store.SealLegacySubSources(tx, box); e != nil {
			return e
		}
		if tx.Dialector.Name() == "postgres" {
			now := time.Now()
			_, e := saveClusterOperation(tx, store.ClusterOperation{ID: store.NewID(), Kind: "restore", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}, memberID, "", false)
			return e
		}
		return nil
	}); err != nil {
		return err
	}
	if arc.CACert != nil {
		if err := os.WriteFile(filepath.Join(dataDir, "ca.crt"), arc.CACert, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dataDir, "ca.key"), []byte(reseal(string(arc.CAKey))), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// writeArchive is backupfmt.WriteArchive stamped with this panel's version.
func writeArchive(w io.Writer, st *store.Store, dataDir, secretKey, pass string) error {
	return backupfmt.WriteArchive(w, st, dataDir, secretKey, pass, Version)
}
