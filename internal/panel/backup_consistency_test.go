package panel

import (
	"bytes"
	"encoding/gob"
	"io"
	"sync"
	"testing"

	"gorm.io/gorm"
)

// A writer settles after users have been read but before inboxes are read.
// Both tables must represent the same instant, also across pages/tables on PG.
func TestBackupConsistentTrafficSnapshot(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		a, b := newTrafficServers(t, dsn)
		ingest(t, a, batch("b1", "e1", 1, 100))
		var once sync.Once
		var settleErr error
		if e := a.st.DB.Callback().Query().After("gorm:query").Register("test:settle-during-backup", func(tx *gorm.DB) {
			if tx.Statement.Table == "users" {
				once.Do(func() { settleErr = b.settleTraffic() })
			}
		}); e != nil {
			t.Fatal(e)
		}
		var archive bytes.Buffer
		if e := writeArchive(&archive, a.st, a.cfg.DataDir, a.cfg.SecretKey, ""); e != nil {
			t.Fatal(e)
		}
		if settleErr != nil {
			t.Fatal(settleErr)
		}
		arc, e := readArchive(bytes.NewReader(archive.Bytes()), "", a.st.Box)
		if e != nil {
			t.Fatal(e)
		}
		sum := func(table, col string) int64 {
			d := gob.NewDecoder(bytes.NewReader(arc.Tables[table]))
			var total int64
			for {
				var rows []map[string]any
				e := d.Decode(&rows)
				if e == io.EOF {
					break
				}
				if e != nil {
					t.Fatal(e)
				}
				for _, r := range rows {
					if n, ok := r[col].(int64); ok {
						total += n
					}
				}
			}
			return total
		}
		if got := sum("users", "used_up") + sum("traffic_inboxes", "up"); got != 100 {
			t.Fatalf("backup contains %d of 100 bytes", got)
		}
		restored := newTestServer(t)
		if e := restoreArchive(restored.st, restored.cfg.DataDir, arc, restored.st.Box); e != nil {
			t.Fatal(e)
		}
		if got := usedUp(t, restored); got != 100 {
			t.Fatalf("restored usage %d", got)
		}
	})
}
