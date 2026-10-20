package panel

import (
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestClusterOperationWithoutNotification(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		a, b := ss[0], ss[1]
		withVersion(t, "v0.1.15")
		withUpdater(t, false)
		// No bus listener: notification loss cannot remove the database request.
		id, e := a.queueClusterOperation("update", "v0.1.20", true)
		if e != nil {
			t.Fatal(e)
		}
		b.processClusterOperations()
		var d store.ClusterDelivery
		b.st.DB.First(&d, "operation_id = ? AND member_id = ?", id, b.memberID)
		if d.Status != "pending" || d.LastError == "" {
			t.Fatal("missing updater not retried", d)
		}
		withUpdater(t, true)
		b.st.DB.Model(&d).Update("next_at", time.Now().Add(-time.Second))
		// The request reaches disk but its acknowledgement fails once.
		b.st.DB.Callback().Update().Before("gorm:update").Register("test:ack", func(tx *gorm.DB) {
			if tx.Statement.Table == "cluster_deliveries" {
				tx.AddError(fmt.Errorf("ack unavailable"))
			}
		})
		b.processClusterOperations()
		raw, e := os.ReadFile(b.updateRequestPath())
		if e != nil {
			t.Fatal(e)
		}
		b.st.DB.Callback().Update().Remove("test:ack")
		other, e := New(b.cfg, b.log, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer other.st.Close()
		if other.memberID != b.memberID || other.procID == b.procID {
			t.Fatal("installation identity not stable across restart")
		}
		other.processClusterOperations()
		other.st.DB.First(&d, "operation_id = ? AND member_id = ?", id, other.memberID)
		after, _ := os.ReadFile(other.updateRequestPath())
		if d.Status != "accepted" || !bytes.Equal(raw, after) {
			t.Fatal("durable request not acknowledged", d)
		}
	})
}

func TestBackupDoesNotReplayRuntimeOperations(t *testing.T) {
	s := newTestServer(t)
	id, e := s.queueClusterOperation("update", "v0.1.20", true)
	if e != nil {
		t.Fatal(e)
	}
	var raw bytes.Buffer
	if e = writeArchive(&raw, s.st, s.cfg.DataDir, s.cfg.SecretKey, ""); e != nil {
		t.Fatal(e)
	}
	arc, e := readArchive(bytes.NewReader(raw.Bytes()), "", s.st.Box)
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"cluster_operations", "cluster_deliveries", "cluster_members", "operational_statuses"} {
		if _, ok := arc.Tables[name]; ok {
			t.Fatal("runtime control in archive", name)
		}
	}
	if e = restoreArchive(s.st, s.cfg.DataDir, arc, s.st.Box); e != nil {
		t.Fatal(e)
	}
	var n int64
	s.st.DB.Model(&store.ClusterOperation{}).Where("id = ?", id).Count(&n)
	if n != 1 {
		t.Fatal("restore erased live operations")
	}
}

func TestRestoreCommitsClusterOperationAtomically(t *testing.T) {
	ss := clusterServers(t, pgOnly(t), 2)
	a, b := ss[0], ss[1]
	var raw bytes.Buffer
	if e := writeArchive(&raw, a.st, a.cfg.DataDir, a.cfg.SecretKey, ""); e != nil {
		t.Fatal(e)
	}
	arc, e := readArchive(bytes.NewReader(raw.Bytes()), "", a.st.Box)
	if e != nil {
		t.Fatal(e)
	}
	a.st.DB.Create(&store.User{ID: "after-backup", Username: "after-backup", UUID: "after-backup", SubToken: "after-backup"})
	a.st.DB.Callback().Create().Before("gorm:create").Register("test:restore-op", func(tx *gorm.DB) {
		if tx.Statement.Table == "cluster_operations" {
			tx.AddError(fmt.Errorf("operation storage unavailable"))
		}
	})
	if e = restoreArchive(a.st, a.cfg.DataDir, arc, a.st.Box); e == nil {
		t.Fatal("restored without durable peer restart")
	}
	var n int64
	a.st.DB.Model(&store.User{}).Where("id = ?", "after-backup").Count(&n)
	if n != 1 {
		t.Fatal("failed restore changed business data")
	}
	a.st.DB.Callback().Create().Remove("test:restore-op")
	if e = restoreArchive(a.st, a.cfg.DataDir, arc, a.st.Box); e != nil {
		t.Fatal(e)
	}
	var op store.ClusterOperation
	a.st.DB.Where("kind = ?", "restore").First(&op)
	a.st.DB.Model(&store.ClusterDelivery{}).Where("operation_id = ? AND member_id = ?", op.ID, b.memberID).Count(&n)
	if n != 1 {
		t.Fatal("successful restore has no peer request")
	}
	a.st.DB.Model(&store.User{}).Where("id = ?", "after-backup").Count(&n)
	if n != 0 {
		t.Fatal("business data not restored")
	}
}
