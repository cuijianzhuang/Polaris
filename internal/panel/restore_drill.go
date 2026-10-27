package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// DrillRestore works even when the production database is unavailable. It only
// reads the archive and configuration key; stdout can be saved as a JSON report.
// No Server, workers, listeners, payment clients or notification sinks are built.
func DrillRestore(ctx context.Context, cfg *Config, path, pass string) (*store.RestoreDrill, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	box, err := secret.New(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	return runRestoreDrill(ctx, box, f, pass, filepath.Base(path)), nil
}

func runRestoreDrill(ctx context.Context, sourceBox *secret.Box, r io.Reader, pass, source string) (result *store.RestoreDrill) {
	result = &store.RestoreDrill{ID: store.NewID(), Source: source, Status: "failed", StartedAt: time.Now(),
		Checks: store.NewJSON([]store.RestoreDrillCheck{}), Counts: store.NewJSON(map[string]int64{})}
	defer func() {
		result.FinishedAt = time.Now()
		result.DurationMS = time.Since(result.StartedAt).Milliseconds()
	}()
	check := func(name string, fn func() error) bool {
		err := ctx.Err()
		if err == nil {
			err = fn()
		}
		c := store.RestoreDrillCheck{Name: name, State: "passed"}
		if err != nil {
			c.State = "failed"
			c.Detail = err.Error()
			result.Error = err.Error()
		}
		result.Checks.V = append(result.Checks.V, c)
		return err == nil
	}
	var arc *backupArchive
	h := sha256.New()
	limited := &io.LimitedReader{R: r, N: maxUpload + 1}
	if !check("archive", func() (err error) {
		arc, err = readArchive(io.TeeReader(limited, h), pass, sourceBox)
		if err != nil {
			result.NeedPassphrase = errors.Is(err, errNeedPassphrase) || errors.Is(err, errBadPassphrase)
			return err
		}
		if _, err = io.Copy(h, limited); err != nil {
			return err
		}
		if limited.N == 0 {
			return errors.New("backup exceeds restore drill size limit")
		}
		result.SHA256 = hex.EncodeToString(h.Sum(nil))
		if !arc.Manifest.Created.IsZero() {
			t := arc.Manifest.Created
			result.BackupAt = &t
			age := max(int64(0), int64(time.Since(t).Seconds()))
			result.RecoveryPointAgeSeconds = &age
		}
		return arc.validate(sourceBox)
	}) {
		return
	}
	if !check("source_secrets", func() error {
		src, err := arc.sourceBox(sourceBox)
		if err != nil {
			return err
		}
		for table, data := range arc.Tables {
			dec := gob.NewDecoder(bytes.NewReader(data))
			count := 0
			for {
				var rows []map[string]any
				if err := dec.Decode(&rows); err == io.EOF {
					break
				} else if err != nil {
					return fmt.Errorf("cannot decode table %s", table)
				}
				count += len(rows)
				for _, row := range rows {
					for col, v := range row {
						text := ""
						switch x := v.(type) {
						case string:
							text = x
						case []byte:
							text = string(x)
						}
						if err := verifyDrillSecrets(src, text); err != nil {
							return fmt.Errorf("cannot decrypt %s.%s with backup key", table, col)
						}
					}
				}
			}
			if expected, ok := arc.Manifest.Tables[table]; ok && count != expected {
				return fmt.Errorf("backup count mismatch: %s", table)
			}
		}
		return nil
	}) {
		return
	}
	dir, err := os.MkdirTemp("", "polaris-restore-drill-*")
	if err != nil {
		result.Error = "cannot create isolated restore directory"
		return
	}
	defer os.RemoveAll(dir)
	box := sourceBox // plain archives require the original installation key
	if arc.Key != "" {
		box, _ = secret.New(pki.Token(32))
	} // encrypted archives prove re-sealing for a new host
	var isolated *store.Store
	defer func() {
		if isolated != nil {
			_ = isolated.Close()
		}
	}()
	path := filepath.Join(dir, "polaris.db")
	if !check("restore", func() error {
		var err error
		isolated, err = store.Open(path, box, false)
		if err != nil {
			return errors.New("cannot open isolated database")
		}
		isolated.DB = isolated.DB.WithContext(ctx)
		if err = restoreArchive(isolated, dir, arc, box); err != nil {
			return fmt.Errorf("isolated restore failed: %w", err)
		}
		return nil
	}) {
		return
	}
	if !check("data_and_secrets", func() error {
		for _, m := range store.AllModels() {
			if runtimeControlModel(m) {
				continue
			}
			sch, err := tableSchema(isolated.DB, m)
			if err != nil {
				return err
			}
			count, err := exportTable(isolated.DB, m, func(rows []map[string]any) error {
				for _, row := range rows {
					for _, v := range row {
						text := ""
						switch x := v.(type) {
						case string:
							text = x
						case []byte:
							text = string(x)
						}
						if err := verifyDrillSecrets(box, text); err != nil {
							return fmt.Errorf("restored secrets unavailable: %s", sch.Table)
						}
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			result.Counts.V[sch.Table] = int64(count)
			// Restore intentionally invalidates recovery codes and passkey challenges.
			if sch.Table == "account_recovery_codes" || sch.Table == "ephemeral_kvs" {
				continue
			}
			if expected, ok := arc.Manifest.Tables[sch.Table]; ok && count != expected {
				return fmt.Errorf("restored count mismatch: %s", sch.Table)
			}
		}
		var admins []store.Admin
		if err := isolated.DB.Find(&admins).Error; err != nil {
			return err
		}
		if len(admins) == 0 {
			return errors.New("no administrator restored")
		}
		usable := false
		for _, a := range admins {
			if _, err := bcrypt.Cost([]byte(a.PasswordHash)); err == nil {
				usable = true
			}
		}
		if !usable {
			return errors.New("no usable administrator password hash restored")
		}
		return nil
	}) {
		return
	}
	if arc.CACert == nil {
		result.Checks.V = append(result.Checks.V, store.RestoreDrillCheck{Name: "ca_identity", State: "skipped", Detail: "archive has no CA; existing Agent trust was not verified"})
	} else if !check("ca_identity", func() error {
		cert, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
		if err != nil {
			return err
		}
		sealed, err := os.ReadFile(filepath.Join(dir, "ca.key"))
		if err != nil {
			return err
		}
		key, err := box.Open(strings.TrimSpace(string(sealed)))
		if err != nil {
			return errors.New("restored CA key cannot be decrypted")
		}
		ca, err := pki.LoadCA(cert, []byte(key))
		if err != nil {
			return errors.New("restored CA certificate/key mismatch")
		}
		if time.Now().Before(ca.Cert.NotBefore) || time.Now().After(ca.Cert.NotAfter) {
			return errors.New("restored CA is not currently valid")
		}
		if !ca.Cert.IsCA {
			return errors.New("restored certificate is not a CA")
		}
		_, err = ca.IssueServer([]string{"localhost"}, time.Hour)
		return err
	}) {
		return
	}
	if !check("database_restart", func() error {
		if err := isolated.Close(); err != nil {
			return err
		}
		isolated = nil
		var err error
		isolated, err = store.Open(path, box, false)
		if err != nil {
			return errors.New("restored database cannot reopen")
		}
		db, err := isolated.DB.DB()
		if err != nil {
			return err
		}
		if err := db.PingContext(ctx); err != nil {
			return err
		}
		var integrity string
		if err := isolated.DB.Raw("PRAGMA integrity_check").Scan(&integrity).Error; err != nil {
			return err
		}
		if integrity != "ok" {
			return errors.New("restored database integrity failed")
		}
		return nil
	}) {
		return
	}
	result.Checks.V = append(result.Checks.V, store.RestoreDrillCheck{Name: "live_acceptance", State: "skipped", Detail: "no interactive login, external license check, payments, notifications or Agent commands were run"})
	result.Status = "passed"
	return
}

func verifyDrillSecrets(box *secret.Box, text string) error {
	if !strings.Contains(text, "enc:v1:") {
		return nil
	}
	values := sealedRe.FindAllString(text, -1)
	if len(values) == 0 {
		return errors.New("invalid encrypted value")
	}
	for _, v := range values {
		if _, err := box.Open(v); err != nil {
			return err
		}
	}
	return nil
}

var restoreDrillSlot = make(chan struct{}, 1)

func (s *Server) backupDrill(c *gin.Context) {
	select {
	case restoreDrillSlot <- struct{}{}:
		defer func() { <-restoreDrillSlot }()
	default:
		c.JSON(http.StatusConflict, gin.H{"error": "已有恢复演练正在运行"})
		return
	}
	var body struct {
		Passphrase string `json:"passphrase"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequest(c, err)
		return
	}
	r, _, ok := s.openBackup(c)
	if !ok {
		return
	}
	defer r.Close()
	if body.Passphrase == "" && strings.HasSuffix(c.Param("name"), ".enc") {
		body.Passphrase = s.backupPassphrase()
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()
	result := runRestoreDrill(ctx, s.st.Box, r, body.Passphrase, c.Param("name"))
	if err := s.st.DB.Create(result).Error; err != nil {
		serverError(c, errors.New("无法保存恢复演练结果"))
		return
	}
	s.auditCtx(c, "backup.drill", result.Source, result.Status)
	c.JSON(http.StatusOK, result)
}

func (s *Server) backupDrills(c *gin.Context) {
	var rows []store.RestoreDrill
	if err := s.st.DB.Order("started_at DESC").Limit(20).Find(&rows).Error; err != nil {
		serverError(c, err)
		return
	}
	if rows == nil {
		rows = []store.RestoreDrill{}
	}
	c.JSON(http.StatusOK, rows)
}
