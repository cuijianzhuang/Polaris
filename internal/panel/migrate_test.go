package panel

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// A panel's SQLite data migrates to another database (PostgreSQL when
// POLARIS_TEST_POSTGRES_DSN is set) and the panel runs on the copy.
func TestMigrate(t *testing.T) {
	onEngines(t, func(t *testing.T, pgDSN string) {
		dir := t.TempDir()
		cfg := &Config{DataDir: dir, SecretKey: "migrate-secret", AdminPath: "/"}
		cfg.DB.DSN = filepath.Join(dir, "src.db")
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		src, err := New(cfg, log, nil)
		if err != nil {
			t.Fatal(err)
		}
		db := src.st.DB
		long := strings.Repeat("备注", 200) // SQLite ignores size:255
		db.Create(&store.User{ID: "u1", Username: "Alice", UUID: "u1", SubToken: "t1", Status: "active", Remark: long, Password: src.st.Box.Seal("pw")})
		db.Create(&store.Node{ID: "n1", Name: "HK"})
		exp := costTestDate(t, "2026-02-28")
		if err := db.Create(&store.NodeCost{NodeID: "n1", Cycle: "month", ExpireAt: &exp, RenewalDay: 31, Revision: 2}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&[]store.NodeNetBoot{{NodeID: "n1", BootID: "retired-boot", LastUptime: 900, Retired: true}, {NodeID: "n1", BootID: "current-boot", LastUptime: 100}}).Error; err != nil {
			t.Fatal(err)
		}
		db.Create(&store.TagTraffic{NodeID: "n1", Kind: "inbound", Tag: "a", Up: 1})
		db.Create(&store.TagTraffic{NodeID: "n1", Kind: "inbound", Tag: "b", Up: 2})
		db.Create(&store.TrafficInbox{NodeID: "n1", UserID: "u1", Up: 5})
		for i := 0; i < 3; i++ {
			src.audit("root", "test", "x", "y", "")
		}
		// More rows than one export page (1000) in tables with serial ids
		// and unique indexes: the copy once failed with duplicate keys here.
		for n := 0; n < 3; n++ {
			for r := 1; r <= 400; r++ {
				db.Create(&store.ConfigRevision{NodeID: fmt.Sprint("node", n), Rev: int64(r), Spec: "x"})
			}
		}
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}

		target := filepath.Join(dir, "dst.db")
		if pgDSN != "" {
			target = pgTestSchema(t, pgDSN)
		}
		res, err := Migrate(cfg, target, false)
		if err != nil {
			t.Fatal(err)
		}
		if pgDSN != "" && (len(res.Truncated) != 1 || res.Truncated[0] != "users.remark") {
			t.Fatalf("truncated: %v", res.Truncated)
		}
		if _, err := Migrate(cfg, target, false); err == nil || !strings.Contains(err.Error(), "-replace") {
			t.Fatalf("a non-empty target must be refused: %v", err)
		}
		if _, err := Migrate(cfg, target, true); err != nil {
			t.Fatal("replace:", err)
		}

		cfg2 := *cfg
		cfg2.DB.DSN = target
		dst, err := New(&cfg2, log, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if sqlDB, err := dst.st.DB.DB(); err == nil {
				sqlDB.Close()
			}
		})
		var u store.User
		if err := dst.st.DB.Where(dst.st.Like("username LIKE ?"), "ali%").First(&u).Error; err != nil {
			t.Fatal("case-insensitive search on the copy:", err)
		}
		if pw, err := dst.st.Box.Open(u.Password); err != nil || pw != "pw" {
			t.Fatalf("sealed value: %q %v", pw, err)
		}
		var tags, inbox int64
		renewed, err := dst.renewCost("n1")
		if err != nil || renewed.RenewalDay != 31 || renewed.Revision != 3 || renewed.ExpireAt.Format("2006-01-02") != "2026-03-31" {
			t.Fatalf("migration lost billing-day anchor: %+v %v", renewed, err)
		}
		var boots []store.NodeNetBoot
		if err := dst.st.DB.Where("node_id = ?", "n1").Order("boot_id").Find(&boots).Error; err != nil {
			t.Fatal(err)
		}
		if len(boots) != 2 || boots[0].BootID != "current-boot" || boots[0].LastUptime != 100 || boots[0].Retired || boots[1].BootID != "retired-boot" || boots[1].LastUptime != 900 || !boots[1].Retired {
			t.Fatalf("network boot checkpoints not migrated: %+v", boots)
		}
		dst.st.DB.Model(&store.TagTraffic{}).Count(&tags)
		dst.st.DB.Model(&store.TrafficInbox{}).Count(&inbox)
		var revs int64
		dst.st.DB.Model(&store.ConfigRevision{}).Count(&revs)
		if revs != 1200 {
			t.Fatalf("config revisions %d", revs)
		}
		if tags != 2 || inbox != 1 {
			t.Fatalf("tags %d inbox %d", tags, inbox)
		}
		// Sequences continue after the copied ids.
		dst.audit("root", "after", "x", "y", "")
		var last store.AuditLog
		dst.st.DB.Order("id desc").First(&last)
		if last.Action != "after" || last.ID < 4 {
			t.Fatalf("new audit row: %+v", last)
		}
	})
}

func TestMigrateRejectsSameDatabase(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir(), SecretKey: "x"}
	cfg.DB.DSN = filepath.Join(cfg.DataDir, "a.db")
	if _, err := Migrate(cfg, cfg.DB.DSN, false); err == nil {
		t.Fatal("same database accepted")
	}
	_ = os.Remove(cfg.DB.DSN)
}
