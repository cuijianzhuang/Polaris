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
	"net/url"
	"sort"
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
	// OnSettingChange, when set, is called after SetSetting stores a value
	// (other panel instances drop their cached copy).
	OnSettingChange func(key string)
}

type cachedSetting struct {
	value string
	found bool
	at    time.Time
}

// settingTTL bounds how long a cached setting is trusted (a safety net for
// writes that bypass SetSetting, such as a restored database).
const settingTTL = time.Minute

// requiredSQLitePragmas are applied to every SQLite DSN, whether or not the
// caller already supplied query parameters. A caller-supplied pragma with the
// same name wins: it is kept and the default is not appended, so an operator
// who deliberately chose e.g. journal_mode(DELETE) is not silently
// overridden.
//
// synchronous and cache_size are deliberately not forced here: with WAL,
// SQLite's default synchronous=NORMAL is the documented safe setting for
// the journal (durable across process crashes; FULL only adds protection
// against OS/power failure at a write cost), and there is no evidence of
// cache pressure that would justify a non-default cache_size.
var requiredSQLitePragmas = []string{
	"busy_timeout(5000)",
	"journal_mode(WAL)",
	"foreign_keys(1)",
}

// withSQLitePragmas returns dsn with every required pragma present. The
// original DSN is otherwise left byte-for-byte intact.
func withSQLitePragmas(dsn string) string {
	_, query, found := strings.Cut(dsn, "?")
	if !found {
		return dsn + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	}
	present := map[string]bool{}
	for _, part := range strings.Split(query, "&") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || key != "_pragma" {
			continue
		}
		if v, err := url.QueryUnescape(value); err == nil {
			value = v
		}
		present[sqlitePragmaName(value)] = true
	}
	var missing []string
	for _, p := range requiredSQLitePragmas {
		if !present[sqlitePragmaName(p)] {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return dsn
	}
	sep := "&"
	if query == "" {
		sep = ""
	}
	var b strings.Builder
	b.WriteString(dsn)
	b.WriteString(sep)
	for i, p := range missing {
		if i > 0 {
			b.WriteString("&")
		}
		b.WriteString("_pragma=")
		b.WriteString(p)
	}
	return b.String()
}

// sqlitePragmaName returns the lower-cased pragma name, e.g. "journal_mode"
// for "journal_mode(WAL)" (also tolerates a leading space).
func sqlitePragmaName(pragma string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(pragma), "(")
	return strings.ToLower(strings.TrimSpace(name))
}

// Open connects and migrates. PostgreSQL DSNs are recognised by
// IsPostgresDSN; anything else is a SQLite file path.
func Open(dsn string, box *secret.Box, debug bool) (*Store, error) {
	var dial gorm.Dialector
	if IsPostgresDSN(dsn) {
		dial = postgres.Open(dsn)
	} else {
		dial = sqlite.Open(withSQLitePragmas(dsn))
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
	probes := probeMigrations(db, migrations)
	if err := db.AutoMigrate(AllModels()...); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := runMigrations(db, migrations, probes); err != nil {
		return nil, err
	}
	if err := SealLegacySubSources(db, box); err != nil {
		return nil, fmt.Errorf("encrypt subscription sources: %w", err)
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
	if err == nil && s.OnSettingChange != nil {
		s.OnSettingChange(key)
	}
	return err
}

// SetSettings commits related settings together. Readers that need a coherent
// group must fetch those rows in one query. Publish invalidations after commit.
func (s *Store) SetSettings(values map[string]string) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys) // keep row locking order consistent across writers
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		for _, key := range keys {
			if err := tx.Save(&Setting{Key: key, Value: values[key]}).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	s.setMu.Lock()
	for _, key := range keys {
		delete(s.settings, key)
	}
	s.setMu.Unlock()
	if s.OnSettingChange != nil {
		for _, key := range keys {
			s.OnSettingChange(key)
		}
	}
	return nil
}

// ForgetSetting drops a cached setting so the next read goes to the database.
func (s *Store) ForgetSetting(key string) {
	s.setMu.Lock()
	delete(s.settings, key)
	s.setMu.Unlock()
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
