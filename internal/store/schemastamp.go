package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Schema stamp.
//
// Open used to run AutoMigrate plus the versioned migrations on every
// start. That is hundreds of sequential information_schema round trips,
// which is nothing against a local database but minutes for an instance
// reaching PostgreSQL through a tunnel (the HK standby measured 4m48s of
// AutoMigrate inside a 5m12s start). The stamp lets Open skip all of it
// when nothing the migration steps depend on has changed: a fingerprint
// of the model schemas plus the migration list is stored in a one-row
// table, and a match means the schema is already what this build would
// produce and every migration is already recorded.
//
// The stamp table is deliberately not part of AllModels: backups neither
// carry it nor restore over it, matching schema_migrations. Restores are
// unaffected — Restore opens (and thereby migrates) the destination
// before importing rows into it, exactly as before.
//
// A downgraded binary predates the stamp and simply migrates on every
// start as it always did; a changed binary computes a different
// fingerprint and migrates (then rewrites the stamp).

// schemaStampTable holds the single stamp row (id = 1).
const schemaStampTable = "schema_stamp"

// schemaFingerprint hashes the schema this build would migrate to: for
// every model in AllModels order, the table name, each column's DB name
// and effective type (data type, size, precision/scale, and the flags
// AutoMigrate acts on), and the index definitions (sorted by name); then
// the versioned migration list in order. Pure local computation — the
// schemas come from GORM's parse cache, no database round trips — and
// deterministic across processes.
func schemaFingerprint(db *gorm.DB) (string, error) {
	return fingerprintOf(db, AllModels(), migrations)
}

func fingerprintOf(db *gorm.DB, models []any, ms []Migration) (string, error) {
	var b strings.Builder
	for _, m := range models {
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(m); err != nil {
			return "", fmt.Errorf("schema fingerprint: parse %T: %w", m, err)
		}
		sch := stmt.Schema
		b.WriteString("table:")
		b.WriteString(sch.Table)
		b.WriteByte('\n')
		for _, f := range sch.Fields {
			if f.DBName == "" {
				continue // not a column
			}
			fmt.Fprintf(&b, "col:%s:%s:%d:%d:%d", f.DBName, f.DataType, f.Size, f.Precision, f.Scale)
			if f.PrimaryKey {
				b.WriteString(":pk")
			}
			if f.AutoIncrement {
				b.WriteString(":ai")
			}
			if f.NotNull {
				b.WriteString(":nn")
			}
			if f.Unique {
				b.WriteString(":uq")
			}
			if f.HasDefaultValue {
				b.WriteString(":def=")
				b.WriteString(f.DefaultValue)
			}
			b.WriteByte('\n')
		}
		indexes := sch.ParseIndexes()
		names := make([]string, 0, len(indexes))
		for name := range indexes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			ix := indexes[name]
			b.WriteString("idx:")
			b.WriteString(ix.Name)
			b.WriteByte(':')
			b.WriteString(ix.Class)
			b.WriteByte(':')
			for i, fo := range ix.Fields {
				if i > 0 {
					b.WriteByte(',')
				}
				if fo.Field != nil {
					b.WriteString(fo.DBName)
				} else {
					b.WriteString(fo.Expression)
				}
			}
			if ix.Where != "" {
				b.WriteString(" where ")
				b.WriteString(ix.Where)
			}
			b.WriteByte('\n')
		}
	}
	for _, m := range ms {
		fmt.Fprintf(&b, "mig:%d:%s\n", m.Version, m.Name)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

// readSchemaStamp returns the stored fingerprint, or "" when there is
// none (fresh database, or a database last opened by a build without
// stamps). Any read error also yields "": the caller falls back to a
// full migration run.
func readSchemaStamp(db *gorm.DB) string {
	var fp string
	row := db.Raw("SELECT fingerprint FROM " + schemaStampTable + " WHERE id = 1").Row()
	if err := row.Scan(&fp); err != nil {
		return ""
	}
	return fp
}

// writeSchemaStamp records fp as the schema's fingerprint. Best effort:
// if it fails, the next start simply migrates again, which is always
// safe. Delete-then-insert keeps the write identical on SQLite and
// PostgreSQL and idempotent when two instances race a cold start.
func writeSchemaStamp(db *gorm.DB, fp string) {
	_ = db.Exec("CREATE TABLE IF NOT EXISTS " + schemaStampTable +
		" (id INTEGER PRIMARY KEY, fingerprint TEXT NOT NULL, updated_at TIMESTAMP NOT NULL)").Error
	_ = db.Exec("DELETE FROM " + schemaStampTable + " WHERE id = 1").Error
	_ = db.Exec("INSERT INTO "+schemaStampTable+" (id, fingerprint, updated_at) VALUES (1, ?, ?)",
		fp, time.Now().UTC()).Error
}
