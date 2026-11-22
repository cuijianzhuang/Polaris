package store

import (
	"path/filepath"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/secret"
	"gorm.io/gorm"
)

func openStampTestStore(t *testing.T, path string) *Store {
	t.Helper()
	box, _ := secret.NewBoxSet("k", "")
	s, err := Open(path, box, false)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func phaseNames(s *Store) []string {
	names := []string{}
	for _, p := range s.OpenPhases() {
		names = append(names, p.Name)
	}
	return names
}

func hasPhase(s *Store, want string) bool {
	for _, n := range phaseNames(s) {
		if n == want {
			return true
		}
	}
	return false
}

// A first Open migrates and writes the stamp; a second Open on the same
// database sees the stamp match and skips the migration steps entirely.
func TestSchemaStampWrittenThenHit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	s := openStampTestStore(t, path)
	fp, err := schemaFingerprint(s.DB)
	if err != nil {
		t.Fatal(err)
	}
	if got := readSchemaStamp(s.DB); got != fp {
		t.Fatalf("stamp after first Open = %q, want fingerprint %q", got, fp)
	}
	if !hasPhase(s, "automigrate") || hasPhase(s, "skip_migrate") {
		t.Fatalf("first Open phases = %v, want a full migration run", phaseNames(s))
	}
	s.Close()

	s2 := openStampTestStore(t, path)
	defer s2.Close()
	if !hasPhase(s2, "skip_migrate") || hasPhase(s2, "automigrate") {
		t.Fatalf("second Open phases = %v, want skip_migrate", phaseNames(s2))
	}
}

// With a matching stamp, a column dropped behind the panel's back stays
// dropped: proof the AutoMigrate step really is skipped, not just fast.
func TestSchemaStampHitSkipsAutoMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	s := openStampTestStore(t, path)
	if err := s.DB.Exec("ALTER TABLE settings DROP COLUMN value").Error; err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := openStampTestStore(t, path)
	defer s2.Close()
	if !hasPhase(s2, "skip_migrate") {
		t.Fatalf("phases = %v, want skip_migrate", phaseNames(s2))
	}
	if s2.DB.Migrator().HasColumn(&Setting{}, "value") {
		t.Fatal("dropped column was restored despite a matching stamp")
	}
}

// A stamp that does not match the fingerprint (a different build wrote
// it, or it was damaged) falls back to the full migration run, which
// restores the schema and rewrites the stamp.
func TestSchemaStampMismatchMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	s := openStampTestStore(t, path)
	if err := s.DB.Exec("ALTER TABLE settings DROP COLUMN value").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Exec("UPDATE " + schemaStampTable + " SET fingerprint = 'bogus'").Error; err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := openStampTestStore(t, path)
	defer s2.Close()
	if hasPhase(s2, "skip_migrate") || !hasPhase(s2, "automigrate") {
		t.Fatalf("phases = %v, want a full migration run", phaseNames(s2))
	}
	if !s2.DB.Migrator().HasColumn(&Setting{}, "value") {
		t.Fatal("dropped column was not restored after a stamp mismatch")
	}
	fp, err := schemaFingerprint(s2.DB)
	if err != nil {
		t.Fatal(err)
	}
	if got := readSchemaStamp(s2.DB); got != fp {
		t.Fatalf("stamp after mismatch = %q, want fingerprint %q", got, fp)
	}
}

type fpModelV1 struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

func (fpModelV1) TableName() string { return "fp_models" }

type fpModelV2 struct {
	ID   uint `gorm:"primaryKey"`
	Name int
}

func (fpModelV2) TableName() string { return "fp_models" }

// The fingerprint is stable across computations and changes when a
// column type or the migration list changes.
func TestFingerprintStableAndSensitive(t *testing.T) {
	s := openStampTestStore(t, filepath.Join(t.TempDir(), "p.db"))
	defer s.Close()
	db := s.DB

	base, err := fingerprintOf(db, []any{&fpModelV1{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := fingerprintOf(db, []any{&fpModelV1{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base != again {
		t.Fatalf("fingerprint not stable: %q vs %q", base, again)
	}
	typed, err := fingerprintOf(db, []any{&fpModelV2{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if typed == base {
		t.Fatal("fingerprint did not change when a column type changed")
	}
	one := []Migration{{Version: 1, Name: "first", Up: func(*gorm.DB, any) error { return nil }}}
	withMig, err := fingerprintOf(db, []any{&fpModelV1{}}, one)
	if err != nil {
		t.Fatal(err)
	}
	if withMig == base {
		t.Fatal("fingerprint did not change when a migration was added")
	}
	renamed := []Migration{{Version: 1, Name: "renamed", Up: func(*gorm.DB, any) error { return nil }}}
	withRenamed, err := fingerprintOf(db, []any{&fpModelV1{}}, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if withRenamed == withMig {
		t.Fatal("fingerprint did not change when a migration was renamed")
	}
}
