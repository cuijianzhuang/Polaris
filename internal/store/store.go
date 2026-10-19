// Package store is the GORM repository (SQLite by default, PostgreSQL optional).
// Protocol parameters live in JSON columns so the schema never changes when
// protocols are added.
package store

import (
	"crypto/rand"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/cuijianzhuang/polaris/internal/secret"
)

// Store wraps the database and the column-encryption box.
type Store struct {
	DB  *gorm.DB
	Box *secret.Box

	// Settings are read on hot paths (every subscription request, request
	// middleware); SetSetting is the only writer, so reads come from memory.
	setMu    sync.Mutex
	settings map[string]cachedSetting
}

type cachedSetting struct {
	value string
	found bool
	at    time.Time
}

// settingTTL bounds how long a cached setting is trusted (a safety net for
// writes that bypass SetSetting, such as a restored database).
const settingTTL = time.Minute

// Open connects and migrates. PostgreSQL DSNs are recognised by
// IsPostgresDSN; anything else is a SQLite file path.
func Open(dsn string, box *secret.Box, debug bool) (*Store, error) {
	var dial gorm.Dialector
	if IsPostgresDSN(dsn) {
		dial = postgres.Open(dsn)
	} else {
		if !strings.Contains(dsn, "?") {
			dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
		}
		dial = sqlite.Open(dsn)
	}
	lvl := logger.Silent
	if debug {
		lvl = logger.Info
	}
	db, err := gorm.Open(dial, &gorm.Config{Logger: logger.Default.LogMode(lvl), TranslateError: true})
	if err != nil {
		return nil, err
	}
	if sqlDB, err := db.DB(); err == nil {
		if dial.Name() == "sqlite" {
			sqlDB.SetMaxOpenConns(1) // SQLite: serialise writers
		} else {
			// PostgreSQL: bounded so several panel instances fit within the
			// server's max_connections (100 by default).
			sqlDB.SetMaxOpenConns(20)
			sqlDB.SetMaxIdleConns(5)
			sqlDB.SetConnMaxLifetime(30 * time.Minute)
			sqlDB.SetConnMaxIdleTime(5 * time.Minute)
		}
	}
	hadSelfService := db.Migrator().HasColumn(&User{}, "self_service")
	if err := db.AutoMigrate(AllModels()...); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if !hadSelfService && db.Migrator().HasTable(&User{}) {
		// Accounts the shop made before the mark existed, by their remark.
		db.Model(&User{}).Where("remark LIKE ? OR remark LIKE ?", "Telegram 购买 %", "网页购买 %").Update("self_service", true)
	}
	return &Store{DB: db, Box: box}, nil
}

// IsPostgresDSN reports whether dsn names a PostgreSQL database: a
// postgres:// or postgresql:// URL, or a "host=... dbname=..." keyword
// string. Anything else is a SQLite file path.
func IsPostgresDSN(dsn string) bool {
	dsn = strings.TrimSpace(dsn)
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return true
	}
	for _, k := range []string{"host=", "dbname=", "user="} {
		if strings.HasPrefix(dsn, k) {
			return true
		}
	}
	return false
}

// NewID returns a short random id (16 lowercase base32 chars).
func NewID() string {
	const alphabet = "abcdefghijkmnopqrstuvwxyz23456789"
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// IsNotFound reports gorm.ErrRecordNotFound.
func IsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

// IsDuplicate reports a unique-constraint violation.
func IsDuplicate(err error) bool { return errors.Is(err, gorm.ErrDuplicatedKey) }

// JSON stores any value as a JSON text column.
type JSON[T any] struct{ V T }

// NewJSON wraps a value.
func NewJSON[T any](v T) JSON[T] { return JSON[T]{V: v} }

func (j JSON[T]) Value() (driver.Value, error) {
	b, err := json.Marshal(j.V)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (j *JSON[T]) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case nil:
		var zero T
		j.V = zero
		return nil
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("JSON: unsupported source %T", src)
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, &j.V)
}

// GormDataType makes the column TEXT on every dialect.
func (JSON[T]) GormDataType() string { return "text" }

func (j JSON[T]) MarshalJSON() ([]byte, error) { return json.Marshal(j.V) }
func (j *JSON[T]) UnmarshalJSON(b []byte) error {
	return json.Unmarshal(b, &j.V)
}

// GetSetting returns a setting or the default.
func (s *Store) GetSetting(key, def string) string {
	s.setMu.Lock()
	c, ok := s.settings[key]
	s.setMu.Unlock()
	if !ok || time.Since(c.at) > settingTTL {
		var st Setting
		err := s.DB.First(&st, "key = ?", key).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return def // database trouble: do not cache the miss
		}
		c = cachedSetting{value: st.Value, found: err == nil, at: time.Now()}
		s.setMu.Lock()
		if s.settings == nil {
			s.settings = map[string]cachedSetting{}
		}
		s.settings[key] = c
		s.setMu.Unlock()
	}
	if !c.found {
		return def
	}
	return c.value
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(key, value string) error {
	err := s.DB.Save(&Setting{Key: key, Value: value}).Error
	s.setMu.Lock()
	if s.settings == nil {
		s.settings = map[string]cachedSetting{}
	}
	if err == nil {
		s.settings[key] = cachedSetting{value: value, found: true, at: time.Now()}
	} else {
		delete(s.settings, key)
	}
	s.setMu.Unlock()
	return err
}

// Close closes the underlying database connections.
func (s *Store) Close() error {
	db, err := s.DB.DB()
	if err != nil {
		return err
	}
	return db.Close()
}

// Like rewrites "LIKE" in a condition to the database's case-insensitive
// form: SQLite's LIKE already ignores ASCII case, PostgreSQL needs ILIKE.
func (s *Store) Like(cond string) string {
	if s.DB.Dialector.Name() == "postgres" {
		return strings.ReplaceAll(cond, " LIKE ", " ILIKE ")
	}
	return cond
}
