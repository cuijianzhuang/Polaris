package panel

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Maintenance commands for the server operator (polaris admin …, polaris
// backup). They open the database directly and are safe to run while the
// panel is up (SQLite WAL + busy timeout, or PostgreSQL).

func openStore(cfg *Config) (*store.Store, error) {
	box, err := secret.New(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	return store.Open(cfg.DB.DSN, box, false)
}

func findAdmin(st *store.Store, username string) (*store.Admin, error) {
	var a store.Admin
	q := st.DB.Order("created_at")
	if username = strings.TrimSpace(username); username != "" {
		q = q.Where("LOWER(username) = LOWER(?)", username)
	}
	if err := q.First(&a).Error; err != nil {
		if username == "" {
			return nil, errors.New("no admin account exists yet; start the panel once to create it")
		}
		return nil, fmt.Errorf("admin %q not found", username)
	}
	return &a, nil
}

// AdminResetPassword sets a new password (random when empty), clears the
// login lockout and revokes every session. It returns the password.
func AdminResetPassword(cfg *Config, username, password string) (string, string, error) {
	st, err := openStore(cfg)
	if err != nil {
		return "", "", err
	}
	a, err := findAdmin(st, username)
	if err != nil {
		return "", "", err
	}
	if password == "" {
		password = pki.Password(16)
	} else if strings.TrimSpace(password) != password {
		return "", "", errors.New("password must not start or end with whitespace")
	} else if len(password) < 8 {
		return "", "", errors.New("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", "", err
	}
	err = st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Updates(map[string]any{
		"password_hash": string(h), "failed_attempts": 0, "locked_until": nil, "token_version": a.TokenVersion + 1,
	}).Error
	if err != nil {
		return "", "", err
	}
	st.DB.Create(&store.AuditLog{Admin: "cli", Action: "admin.password_reset", Target: a.Username})
	return a.Username, password, nil
}

// AdminDisable2FA turns TOTP off for an admin who lost their authenticator.
func AdminDisable2FA(cfg *Config, username string) (string, error) {
	st, err := openStore(cfg)
	if err != nil {
		return "", err
	}
	a, err := findAdmin(st, username)
	if err != nil {
		return "", err
	}
	if err := st.DB.Model(&store.Admin{}).Where("id = ?", a.ID).Updates(map[string]any{
		"totp_enabled": false, "totp_pending": "", "token_version": a.TokenVersion + 1,
	}).Error; err != nil {
		return "", err
	}
	st.DB.Create(&store.AuditLog{Admin: "cli", Action: "admin.totp_reset", Target: a.Username})
	return a.Username, nil
}

// AdminList prints the admin accounts.
func AdminList(cfg *Config) ([]string, error) {
	st, err := openStore(cfg)
	if err != nil {
		return nil, err
	}
	var as []store.Admin
	st.DB.Order("created_at").Find(&as)
	var out []string
	for _, a := range as {
		line := a.Username
		if a.TOTPEnabled {
			line += "  [2FA]"
		}
		if a.LastLoginAt != nil {
			line += "  last login " + a.LastLoginAt.Local().Format("2006-01-02 15:04")
		}
		out = append(out, line)
	}
	return out, nil
}

// Backup writes a consistent copy of a SQLite database (VACUUM INTO) to path.
func Backup(cfg *Config, path, pass string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err == nil {
		err = writeArchive(f, st, cfg.DataDir, cfg.SecretKey, pass)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// Restore replaces the database and CA with a backup's (stop the panel
// first). A snapshot of the current data is written next to the backups.
func Restore(cfg *Config, path, pass string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := openStore(cfg)
	if err != nil {
		return "", err
	}
	defer st.Close()
	arc, err := readArchive(f, pass, st.Box)
	if err != nil {
		return "", err
	}
	if err := arc.validate(st.Box); err != nil {
		return "", err
	}
	snap := filepath.Join(cfg.DataDir, backupDir, backupName("prerestore", false))
	if err := os.MkdirAll(filepath.Dir(snap), 0o700); err != nil {
		return "", err
	}
	if out, err := os.OpenFile(snap, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
		err = writeArchive(out, st, cfg.DataDir, cfg.SecretKey, "")
		out.Close()
		if err != nil {
			return "", fmt.Errorf("snapshot before restore: %w", err)
		}
	}
	return snap, restoreArchive(st, cfg.DataDir, arc, st.Box)
}

// LocalNodeToken creates (or reuses) a node record for an agent on the panel
// server itself and returns a fresh one-time install token. The node's public
// address defaults to the panel's own host.
func LocalNodeToken(cfg *Config, name string) (string, bool, error) {
	st, err := openStore(cfg)
	if err != nil {
		return "", false, err
	}
	if name == "" {
		name = "本机"
	}
	var n store.Node
	created := false
	if err := st.DB.First(&n, "name = ?", name).Error; err != nil {
		addr := ""
		if len(cfg.AgentHosts) > 0 {
			addr = cfg.AgentHosts[0]
		} else if u, err := url.Parse(cfg.PublicURL); err == nil {
			addr = u.Hostname()
		}
		n = store.Node{ID: store.NewID(), Name: name, Address: addr, Labels: store.NewJSON([]string{"本机"}),
			DefaultCore: model.CoreXray, TrafficRate: 1, Status: "pending"}
		// Resolved outside the transaction: SQLite allows one connection.
		xray := (&Server{st: st}).defaultCoreVersion(context.Background(), model.CoreXray)
		err = st.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&n).Error; err != nil {
				return err
			}
			return tx.Create(&store.Core{ID: store.NewID(), NodeID: n.ID, Type: model.CoreXray, Version: xray,
				Status: "installing", IsDefault: true, InstalledVersions: store.NewJSON([]string{})}).Error
		})
		if err != nil {
			return "", false, err
		}
		created = true
	}
	tok := pki.Token(24)
	if err := st.DB.Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{
		"token_hash": hashToken(tok), "token_expires": time.Now().Add(time.Hour)}).Error; err != nil {
		return "", false, err
	}
	st.DB.Create(&store.AuditLog{Admin: "cli", Action: "node.install_token", Target: n.Name})
	return tok, created, nil
}
