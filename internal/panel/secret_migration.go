// Secret-key rotation: the leader-only re-encryption migration.
// See docs/design/secret-key-rotation.md. The migration scans every column
// that can hold a sealed value (enumerated by tracing the Box.Seal call
// sites to the columns they write), re-seals each token with the current
// key, and records resumable progress in the settings table. Rows that
// cannot be opened are logged and skipped, never overwritten. Plaintext
// legacy rows are left alone.
package panel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Settings keys for the rotation state.
const (
	secretKeyVersionSetting   = "secret_key_version"
	secretKeyMigrationSetting = "secret_key_migration"
)

// validateKeyVersion refuses to start when the database names a key
// version this panel does not know: a half-finished rotation from a newer
// key would otherwise silently lose data. An absent setting means a
// pre-rotation database, which adopts this panel's current key.
func validateKeyVersion(st *store.Store, box *secret.BoxSet) error {
	v := st.GetSetting(secretKeyVersionSetting, "")
	if v == "" {
		return st.SetSetting(secretKeyVersionSetting, box.CurrentKid())
	}
	if v == box.CurrentKid() || v == box.PreviousKid() {
		return nil
	}
	return fmt.Errorf("secret_key_version %q is neither this panel's current key (%s) nor its previous key (%s): finish the rotation that wrote it, or restore the previous key",
		v, box.CurrentKid(), box.PreviousKid())
}

// secretMigrationTarget is one table whose text columns can hold sealed
// values. Cols are the column names; IDCol is the primary key used as
// the resume cursor (IDNumeric when the key is an integer).
type secretMigrationTarget struct {
	Table     string
	IDCol     string
	IDNumeric bool
	Cols      []string
}

// secretMigrationTargets enumerates every sealed column, traced from the
// Box.Seal call sites to the columns they write. JSON columns (inbounds
// protocol/transport/security/extra, plugins.settings) hold sealed tokens
// embedded in the JSON text; the same token replacement covers them.
// Tables with no sealed column are deliberately absent (users passwords
// are bcrypt, traffic_* holds no sealed values).
var secretMigrationTargets = []secretMigrationTarget{
	{"settings", "key", false, []string{"value"}},
	{"cluster_secrets", "name", false, []string{"value"}},
	{"certificates", "id", false, []string{"key"}},
	{"config_revisions", "id", true, []string{"spec"}},
	{"admins", "id", false, []string{"totp_secret", "totp_pending"}},
	{"passkeys", "id", false, []string{"data"}},
	{"plugins", "id", false, []string{"secret", "settings"}},
	{"sub_sources", "id", false, []string{"url", "content", "cache"}},
	{"sub_source_nodes", "id", false, []string{"payload"}},
	{"node_providers", "node_id", false, []string{"api_key"}},
	{"sso_providers", "id", false, []string{"client_secret"}},
	{"dns_providers", "id", false, []string{"token"}},
	{"cdn_profiles", "id", false, []string{"tunnel_credential", "api_token"}},
	{"chains", "id", false, []string{"tunnel_auth"}},
	{"webhooks", "id", false, []string{"secret"}},
	{"refund_jobs", "order_id", false, []string{"bot_token"}},
	{"ephemeral_kvs", "name", false, []string{"value"}},
	{"inbounds", "id", false, []string{"protocol", "transport", "security", "extra"}},
}

// secretMigrationBatch is the rows scanned per batch (and per progress save).
const secretMigrationBatch = 500

// secretMigrationProgress is the resumable state in the settings table.
// Kid binds the progress to one rotation: a new current key restarts it.
type secretMigrationProgress struct {
	Kid        string   `json:"kid"`
	Table      string   `json:"table"`
	LastID     string   `json:"last_id"`
	Updated    int64    `json:"updated"`
	Failed     int64    `json:"failed"`
	FailedRows []string `json:"failed_rows,omitempty"`
	Done       bool     `json:"done"`
}

// secretMigrationResult is what one run of the migration did.
type secretMigrationResult struct {
	Updated    int64
	Failed     int64
	FailedRows []string
	CAChanged  bool
	Done       bool
}

func loadMigrationProgress(st *store.Store, kid string) secretMigrationProgress {
	prog := secretMigrationProgress{Kid: kid}
	if raw := st.GetSetting(secretKeyMigrationSetting, ""); raw != "" {
		var saved secretMigrationProgress
		if err := json.Unmarshal([]byte(raw), &saved); err == nil && saved.Kid == kid {
			prog = saved
		}
	}
	return prog
}

func (s *Server) saveMigrationProgress(prog secretMigrationProgress) {
	raw, err := json.Marshal(prog)
	if err != nil {
		return
	}
	_ = s.st.SetSetting(secretKeyMigrationSetting, string(raw))
}

var secretMigrationMu sync.Mutex

// secretMigrationTick runs the migration when the leader should: at
// startup and every 15 minutes (startJobs). It is a no-op once the
// migration is done for the current key, and never runs twice at once.
func (s *Server) secretMigrationTick() {
	if !s.isLeader() {
		return
	}
	if !secretMigrationMu.TryLock() {
		return
	}
	defer secretMigrationMu.Unlock()
	res := s.runSecretMigration()
	if res.Done {
		s.log.Info("secret-key migration finished", "updated", res.Updated, "failed", res.Failed)
	} else if res.Failed > 0 || res.Updated > 0 {
		s.log.Warn("secret-key migration incomplete", "updated", res.Updated, "failed", res.Failed, "failed_rows", res.FailedRows)
	}
}

// runSecretMigration re-encrypts every sealed value with the current key.
// It is idempotent and resumable: a restart continues from the saved
// (table, row id) cursor instead of starting over. The result counts what
// this run did, not the cumulative totals in the progress setting.
func (s *Server) runSecretMigration() secretMigrationResult {
	// The migration writes the settings table directly, so the settings
	// cache must be dropped on every path out of here.
	defer s.st.InvalidateSettingsCache()
	box := s.st.Box
	prog := loadMigrationProgress(s.st, box.CurrentKid())
	if prog.Done {
		return secretMigrationResult{Done: true}
	}
	updatedBefore, failedBefore := prog.Updated, prog.Failed

	// The CA private-key file is re-encrypted first, while the old key is
	// still available; the old copy is kept until the migration succeeds.
	changed, err := s.reencryptCAKeyFile()
	if err != nil {
		s.log.Error("secret-key migration: CA key file", "err", err)
		return secretMigrationResult{Updated: prog.Updated - updatedBefore, Failed: prog.Failed - failedBefore, FailedRows: prog.FailedRows}
	}

	// Resume after the last saved position.
	start, lastID := 0, ""
	if prog.Table != "" {
		for i, t := range secretMigrationTargets {
			if t.Table == prog.Table {
				start, lastID = i, prog.LastID
				break
			}
		}
	}
	for i := start; i < len(secretMigrationTargets); i++ {
		t := secretMigrationTargets[i]
		cursor := ""
		if i == start {
			cursor = lastID
		}
		if err := s.migrateSecretTable(t, &cursor, &prog); err != nil {
			s.log.Error("secret-key migration: table failed", "table", t.Table, "err", err)
			s.saveMigrationProgress(prog)
			return secretMigrationResult{Updated: prog.Updated - updatedBefore, Failed: prog.Failed - failedBefore, FailedRows: prog.FailedRows}
		}
		prog.Table = t.Table
		prog.LastID = cursor
		s.saveMigrationProgress(prog)
	}
	prog.Table, prog.LastID = "", ""
	// A clean finish clears the failed-row list and records this key as
	// the version: the next rotation starts from a clean slate. With
	// failures the run stays not-done, so the next tick (or restart)
	// retries once the missing key is supplied; the "remove the previous
	// key" step is blocked while the failed count is > 0.
	if err := s.st.SetSetting(secretKeyVersionSetting, box.CurrentKid()); err != nil {
		s.log.Error("secret-key migration: record key version", "err", err)
	}
	if prog.Failed == 0 {
		prog.Done = true
		prog.FailedRows = nil
		s.saveMigrationProgress(prog)
		// The old CA key file copy is only needed while the migration
		// might still fail; a finished migration no longer needs it.
		_ = os.Remove(filepath.Join(s.cfg.DataDir, "ca.key.prev"))
		return secretMigrationResult{Updated: prog.Updated - updatedBefore, Failed: prog.Failed - failedBefore, FailedRows: prog.FailedRows, CAChanged: changed, Done: true}
	}
	s.saveMigrationProgress(prog)
	return secretMigrationResult{Updated: prog.Updated - updatedBefore, Failed: prog.Failed - failedBefore, FailedRows: prog.FailedRows, CAChanged: changed, Done: false}
}

// migrateSecretTable scans one table in batches from cursor (updated to
// the last processed id), re-sealing each token with the current key.
// Only columns whose text actually changed are updated, and only when
// the row still holds the value that was read (a concurrent write wins).
func (s *Server) migrateSecretTable(t secretMigrationTarget, cursor *string, prog *secretMigrationProgress) error {
	box := s.st.Box
	cols := append([]string{t.IDCol}, t.Cols...)
	for {
		var rows []map[string]any
		q := s.st.DB.Table(t.Table).Select(cols)
		if t.IDNumeric {
			last := int64(0)
			if *cursor != "" {
				if n, err := strconv.ParseInt(*cursor, 10, 64); err == nil {
					last = n
				}
			}
			q = q.Where(t.IDCol+" > ?", last)
		} else {
			q = q.Where(t.IDCol+" > ?", *cursor)
		}
		if err := q.Order(t.IDCol).Limit(secretMigrationBatch).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			id := rowID(row[t.IDCol])
			rowFailed := false
			for _, col := range t.Cols {
				v, ok := stringCol(row[col])
				if !ok || v == "" || !secret.ContainsSealed(v) {
					continue
				}
				next, changed, failed := box.ResealText(v)
				if failed > 0 {
					rowFailed = true
					s.log.Warn("secret-key migration: row cannot be opened, skipped", "table", t.Table, "id", id, "column", col)
					continue
				}
				if changed == 0 {
					continue
				}
				res := s.st.DB.Table(t.Table).Where(t.IDCol+" = ? AND "+col+" = ?", row[t.IDCol], v).Update(col, next)
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected == 0 {
					continue // the row changed concurrently; its writer used the current key
				}
				prog.Updated++
			}
			if rowFailed {
				prog.Failed++
				if len(prog.FailedRows) < 50 {
					ref := t.Table + ":" + id
					seen := false
					for _, f := range prog.FailedRows {
						if f == ref {
							seen = true
							break
						}
					}
					if !seen {
						prog.FailedRows = append(prog.FailedRows, ref)
					}
				}
			}
			*cursor = id
		}
		// Save the resume point after every batch: a crash restarts from
		// here, and already-updated rows in the next batch are no-ops.
		prog.Table = t.Table
		prog.LastID = *cursor
		s.saveMigrationProgress(*prog)
		if len(rows) < secretMigrationBatch {
			return nil
		}
	}
}

func rowID(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case uint64:
		return strconv.FormatUint(x, 10)
	default:
		return fmt.Sprint(v)
	}
}

func stringCol(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	default:
		return "", false
	}
}

// reencryptCAKeyFile re-encrypts DataDir/ca.key with the current key.
// It writes to a temp file and renames over the original, keeping the old
// copy as ca.key.prev until the migration reports success. The database
// ca.key row is migrated by the table scan; the two agree at the end.
// A missing or plaintext file is left alone.
func (s *Server) reencryptCAKeyFile() (bool, error) {
	keyPath := filepath.Join(s.cfg.DataDir, "ca.key")
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return false, nil
	}
	sealed := strings.TrimSpace(string(raw))
	if sealed == "" || !secret.IsSealed(sealed) {
		return false, nil
	}
	next, err := s.st.Box.Reseal(sealed)
	if err != nil {
		return false, fmt.Errorf("CA key file cannot be opened with the configured keys: %w", err)
	}
	if next == sealed {
		return false, nil
	}
	bakPath := filepath.Join(s.cfg.DataDir, "ca.key.prev")
	if err := os.WriteFile(bakPath, raw, 0o600); err != nil {
		return false, err
	}
	tmpPath := keyPath + ".rotating"
	if err := os.WriteFile(tmpPath, []byte(next), 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmpPath, keyPath); err != nil {
		return false, err
	}
	return true, nil
}
