package panel

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"

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
	if username != "" {
		q = q.Where("username = ?", username)
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
func Backup(cfg *Config, path string) error {
	dsn := cfg.DB.DSN
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return errors.New("PostgreSQL: use pg_dump to back up the database")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	return st.DB.Exec("VACUUM INTO ?", abs).Error
}
