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
}

// Open connects and migrates. DSNs starting with postgres:// or postgresql://
// use PostgreSQL; anything else is a SQLite file path.
func Open(dsn string, box *secret.Box, debug bool) (*Store, error) {
	var dial gorm.Dialector
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
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
	if sqlDB, err := db.DB(); err == nil && dial.Name() == "sqlite" {
		sqlDB.SetMaxOpenConns(1) // SQLite: serialise writers
	}
	if err := db.AutoMigrate(AllModels()...); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{DB: db, Box: box}, nil
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
	var st Setting
	if err := s.DB.First(&st, "key = ?", key).Error; err != nil {
		return def
	}
	return st.Value
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(key, value string) error {
	return s.DB.Save(&Setting{Key: key, Value: value}).Error
}
