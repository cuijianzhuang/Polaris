package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestMigrationListIsSound(t *testing.T) {
	if err := checkMigrations(migrations); err != nil {
		t.Fatal(err)
	}
	if LatestMigration() != len(migrations) {
		t.Fatal("latest")
	}
	if checkMigrations([]Migration{{Version: 2, Name: "x", Up: func(*gorm.DB, any) error { return nil }}}) == nil {
		t.Fatal("a gap was accepted")
	}
}

// Steps run once, in order, recorded; a failing step rolls back and stops
// the ones after it; the next start resumes from there.
func TestRunMigrations(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.db")
	st, err := Open(p, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	db := st.DB
	applied, _ := AppliedMigrations(db)
	if len(applied) != len(migrations) {
		t.Fatalf("fresh database applied %d of %d", len(applied), len(migrations))
	}
	var order []int
	fail := true
	steps := append(append([]Migration{}, migrations...),
		Migration{Version: len(migrations) + 1, Name: "add a setting", Up: func(tx *gorm.DB, _ any) error {
			order = append(order, 1)
			return tx.Create(&Setting{Key: "m1", Value: "x"}).Error
		}},
		Migration{Version: len(migrations) + 2, Name: "flaky", Up: func(tx *gorm.DB, _ any) error {
			order = append(order, 2)
			if err := tx.Create(&Setting{Key: "m2", Value: "x"}).Error; err != nil {
				return err
			}
			if fail {
				return errors.New("boom")
			}
			return nil
		}},
		Migration{Version: len(migrations) + 3, Name: "after", Up: func(tx *gorm.DB, _ any) error {
			order = append(order, 3)
			return nil
		}},
	)
	err = runMigrations(db, steps, nil)
	if err == nil || !strings.Contains(err.Error(), "flaky") {
		t.Fatalf("failure not reported: %v", err)
	}
	var n int64
	db.Model(&Setting{}).Where("key = ?", "m2").Count(&n)
	if n != 0 {
		t.Fatal("failed step was not rolled back")
	}
	fail = false
	if err := runMigrations(db, steps, nil); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(db, steps, nil); err != nil {
		t.Fatal(err)
	}
	if got := order; len(got) != 4 || got[0] != 1 || got[1] != 2 || got[2] != 2 || got[3] != 3 {
		t.Fatalf("run order %v (step 1 once, the flaky one retried, step 3 once)", got)
	}
	applied, _ = AppliedMigrations(db)
	if len(applied) != len(steps) || applied[len(applied)-1].Name != "after" {
		t.Fatalf("recorded %+v", applied)
	}
}

// Several panels starting together apply each step once.
func TestRunMigrationsConcurrently(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.db")
	st, err := Open(p, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	runs := 0
	steps := append(append([]Migration{}, migrations...), Migration{Version: len(migrations) + 1, Name: "count", Up: func(tx *gorm.DB, _ any) error {
		mu.Lock()
		runs++
		mu.Unlock()
		return nil
	}})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runMigrations(st.DB, steps, nil); err != nil && !IsDuplicate(err) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if runs != 1 {
		t.Fatalf("step ran %d times", runs)
	}
}

// On PostgreSQL two panels starting at once each run a step at most once
// between them; the advisory lock makes the second wait and then skip.
func TestRunMigrationsPostgres(t *testing.T) {
	dsn := os.Getenv("POLARIS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POLARIS_TEST_POSTGRES_DSN not set")
	}
	schema := "mig_" + strings.ToLower(NewID())
	admin, err := Open(dsn, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.DB.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.DB.Exec("DROP SCHEMA " + schema + " CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	scoped := dsn + sep + "search_path=" + schema
	var mu sync.Mutex
	runs := 0
	steps := append(append([]Migration{}, migrations...), Migration{Version: len(migrations) + 1, Name: "slow", Up: func(tx *gorm.DB, _ any) error {
		mu.Lock()
		runs++
		mu.Unlock()
		time.Sleep(200 * time.Millisecond) // hold the lock while the other instance arrives
		return nil
	}})
	var stores [2]*Store
	for i := range stores {
		if stores[i], err = Open(scoped, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, st := range stores {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			if err := runMigrations(st.DB, steps, nil); err != nil {
				t.Error(err)
			}
		}(st)
	}
	wg.Wait()
	if runs != 1 {
		t.Fatalf("step ran %d times", runs)
	}
	applied, _ := AppliedMigrations(stores[0].DB)
	if len(applied) != len(steps) {
		t.Fatalf("recorded %d of %d", len(applied), len(steps))
	}
}
