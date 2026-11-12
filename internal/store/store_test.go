package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/secret"
)

func TestSQLitePragmasMergedIntoDSN(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"p.db", "p.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"},
		{"p.db?", "p.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"},
		{"p.db?cache=shared", "p.db?cache=shared&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"},
		// An explicit user value wins and is not duplicated.
		{"p.db?_pragma=journal_mode(DELETE)", "p.db?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"},
		{"p.db?_pragma=busy_timeout(1000)&_pragma=foreign_keys(0)", "p.db?_pragma=busy_timeout(1000)&_pragma=foreign_keys(0)&_pragma=journal_mode(WAL)"},
		{"p.db?_pragma=JOURNAL_MODE(WAL)", "p.db?_pragma=JOURNAL_MODE(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"},
		{"p.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", "p.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"},
	} {
		if got := withSQLitePragmas(tc.in); got != tc.want {
			t.Errorf("withSQLitePragmas(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOpenAppliesPragmasWithQueryDSN(t *testing.T) {
	box, _ := secret.NewBoxSet("k", "")
	// The old code only added pragmas when the DSN had no "?"; a DSN with
	// an unrelated parameter must still end up in WAL with FKs on.
	s, err := Open(filepath.Join(t.TempDir(), "p.db")+"?cache=shared", box, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode string
	if err := s.DB.Raw("PRAGMA journal_mode").Scan(&mode).Error; err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, err %v", mode, err)
	}
	var fk, timeout int
	if err := s.DB.Raw("PRAGMA foreign_keys").Scan(&fk).Error; err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, err %v", fk, err)
	}
	if err := s.DB.Raw("PRAGMA busy_timeout").Scan(&timeout).Error; err != nil || timeout != 5000 {
		t.Fatalf("busy_timeout = %d, err %v", timeout, err)
	}
	// An explicit journal_mode still wins.
	s2, err := Open(filepath.Join(t.TempDir(), "p2.db")+"?_pragma=journal_mode(DELETE)", box, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.DB.Raw("PRAGMA journal_mode").Scan(&mode).Error; err != nil || mode != "delete" {
		t.Fatalf("journal_mode override = %q, err %v", mode, err)
	}
	if err := s2.DB.Raw("PRAGMA busy_timeout").Scan(&timeout).Error; err != nil || timeout != 5000 {
		t.Fatalf("busy_timeout with override = %d, err %v", timeout, err)
	}
}

func TestOpenAndJSON(t *testing.T) {
	box, _ := secret.NewBoxSet("k", "")
	s, err := Open(filepath.Join(t.TempDir(), "p.db"), box, false)
	if err != nil {
		t.Fatal(err)
	}
	in := Inbound{ID: NewID(), NodeID: "n", Tag: "a", Port: 443,
		Protocol: NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"})}
	if err := s.DB.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	var got Inbound
	if err := s.DB.First(&got, "id = ?", in.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Protocol.V.Flow != "xtls-rprx-vision" {
		t.Fatalf("json column roundtrip failed: %+v", got.Protocol.V)
	}
	dup := Inbound{ID: NewID(), NodeID: "n", Tag: "a", Port: 444}
	if err := s.DB.Create(&dup).Error; !IsDuplicate(err) {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

// TestOpenStoresUTCTimestamps proves the NowFunc fix: GORM's automatic
// CreatedAt/UpdatedAt must be stored as UTC, even when the process runs in
// a non-UTC zone. Before the fix they were local-time strings, which SQLite
// compares lexicographically against UTC values and gets wrong by the UTC
// offset.
func TestOpenStoresUTCTimestamps(t *testing.T) {
	t.Setenv("TZ", "Asia/Shanghai")
	box, _ := secret.NewBoxSet("k", "")
	s, err := Open(filepath.Join(t.TempDir(), "tz.db"), box, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u := User{ID: NewID(), Username: "tzuser", UUID: NewID(), SubToken: NewID(), Status: "active"}
	if err := s.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.DB.Raw("SELECT created_at FROM users WHERE id = ?", u.ID).Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	// modernc sqlite stores time.Time as RFC3339, e.g.
	// "2026-11-11T17:19:59.729793698Z" for UTC or
	// "2026-11-12T01:19:49.632899293+08:00" for Shanghai time. Parse it and
	// require a zero UTC offset.
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("created_at %q does not parse as RFC3339: %v", raw, err)
	}
	if _, offset := parsed.Zone(); offset != 0 {
		t.Fatalf("created_at stored as %q, want a UTC timestamp", raw)
	}
}
