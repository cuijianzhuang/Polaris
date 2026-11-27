package store

import (
	"path/filepath"
	"testing"
)

// Migration 3 marks the forwarding terms a plan already applied as
// plan-managed; terms without a plan keep an unknown source, and a
// source that is already set is never rewritten.
func TestMigrationBackfillsFwdSource(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB
	mk := func(id, planID, source string, max int) {
		t.Helper()
		u := User{ID: id, Username: id, UUID: "uuid-" + id, SubToken: "token-" + id, PlanID: planID, FwdSource: source}
		u.FwdMax = max
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk("planuser", "p1", "", 2)
	mk("noplan", "", "", 2)
	mk("noterms", "p1", "", 0)
	mk("granted", "p1", "grant", 2)
	// Pretend migration 3 has not run yet, then run the list again.
	if err := db.Where("version = ?", 3).Delete(&SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(db, migrations, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"planuser": "plan", "noplan": "", "noterms": "", "granted": "grant"}
	for id, source := range want {
		var u User
		if err := db.First(&u, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		if u.FwdSource != source {
			t.Fatalf("%s: source %q, want %q", id, u.FwdSource, source)
		}
	}
}
