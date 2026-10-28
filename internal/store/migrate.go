package store

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Versioned data migrations.
//
// AutoMigrate creates tables and adds columns, but it never renames or drops
// a column, changes a type, or fixes data. Those steps go here: each runs once
// per database, in version order, inside its own transaction, and is
// recorded in schema_migrations. On PostgreSQL a transaction-scoped advisory
// lock serialises panel instances that start at the same time; SQLite has a
// single writer already.
//
// Adding a step: append to migrations with the next version number, never
// renumber or edit a step that shipped, and make Up safe on both SQLite and
// PostgreSQL. If the step depends on what the schema looked like before
// AutoMigrate ran (a column that did not exist yet), read it in Probe.
//
// schema_migrations is not part of backups: restoring a backup brings back
// data, not the record of which fixes ran, so a step must also be correct for
// data restored from a backup taken before it (or the restore path must
// apply it).

// SchemaMigration records a migration that ran.
type SchemaMigration struct {
	Version   int       `gorm:"primaryKey;autoIncrement:false" json:"version"`
	Name      string    `gorm:"size:100" json:"name"`
	AppliedAt time.Time `json:"appliedAt"`
}

// Migration is one numbered step.
type Migration struct {
	Version int
	Name    string
	// Probe, when set, runs before AutoMigrate; its result is passed to Up.
	Probe func(db *gorm.DB) any
	// Up runs inside a transaction.
	Up func(tx *gorm.DB, probe any) error
}

// migrations is the ordered list of every step. Never reorder or edit one
// that has shipped.
var migrations = []Migration{
	{
		Version: 1,
		Name:    "mark shop accounts as self-service",
		// Accounts the shop made before the self_service mark existed are
		// recognised by their remark. Only when the column is new: later the
		// mark belongs to the admin.
		Probe: func(db *gorm.DB) any {
			return db.Migrator().HasTable(&User{}) && !db.Migrator().HasColumn(&User{}, "self_service")
		},
		Up: func(tx *gorm.DB, probe any) error {
			if added, _ := probe.(bool); !added {
				return nil
			}
			return tx.Model(&User{}).Where("remark LIKE ? OR remark LIKE ?", "Telegram 购买 %", "网页购买 %").Update("self_service", true).Error
		},
	},
}

// migrationLock is the PostgreSQL advisory lock key ("polaris" in ASCII).
const migrationLock = 0x706f6c61726973

// probeMigrations runs every Probe before AutoMigrate.
func probeMigrations(db *gorm.DB, ms []Migration) map[int]any {
	out := map[int]any{}
	for _, m := range ms {
		if m.Probe != nil {
			out[m.Version] = m.Probe(db)
		}
	}
	return out
}

// runMigrations applies the steps that have not run yet.
func runMigrations(db *gorm.DB, ms []Migration, probes map[int]any) error {
	if err := checkMigrations(ms); err != nil {
		return err
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		return err
	}
	for _, m := range ms {
		err := db.Transaction(func(tx *gorm.DB) error {
			if tx.Dialector.Name() == "postgres" {
				if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", migrationLock).Error; err != nil {
					return err
				}
			}
			var n int64
			if err := tx.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&n).Error; err != nil {
				return err
			}
			if n > 0 {
				return nil // already applied, perhaps by another instance
			}
			if err := m.Up(tx, probes[m.Version]); err != nil {
				return err
			}
			return tx.Create(&SchemaMigration{Version: m.Version, Name: m.Name, AppliedAt: time.Now().UTC()}).Error
		})
		if err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

// checkMigrations rejects a list that is not strictly increasing from 1.
func checkMigrations(ms []Migration) error {
	for i, m := range ms {
		if m.Version != i+1 || m.Up == nil || m.Name == "" {
			return fmt.Errorf("migration list broken at position %d (version %d)", i, m.Version)
		}
	}
	return nil
}

// AppliedMigrations lists what ran, oldest first, for diagnostics.
func AppliedMigrations(db *gorm.DB) ([]SchemaMigration, error) {
	var out []SchemaMigration
	err := db.Order("version").Find(&out).Error
	return out, err
}

// LatestMigration is the newest version this build knows.
func LatestMigration() int { return len(migrations) }
