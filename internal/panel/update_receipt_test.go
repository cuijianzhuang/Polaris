package panel

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// writeUpdateResult drops a fake updater result into s's data dir.
func writeUpdateResult(t *testing.T, s *Server, body string) {
	t.Helper()
	if err := os.WriteFile(s.updateResultPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func deliveryOf(t *testing.T, s *Server, opID, memberID string) store.ClusterDelivery {
	t.Helper()
	var d store.ClusterDelivery
	if err := s.st.DB.First(&d, "operation_id = ? AND member_id = ?", opID, memberID).Error; err != nil {
		t.Fatal(err)
	}
	return d
}

// acceptedUpdate runs the two-server cluster flow up to b having accepted
// an update delivery for target.
func acceptedUpdate(t *testing.T, target string) (a, b *Server, opID string) {
	t.Helper()
	ss := clusterServers(t, "", 2)
	a, b = ss[0], ss[1]
	withVersion(t, "v0.1.15")
	withUpdater(t, true)
	id, err := a.queueClusterOperation("update", target, true)
	if err != nil {
		t.Fatal(err)
	}
	b.processClusterOperations()
	if d := deliveryOf(t, b, id, b.memberID); d.Status != "accepted" {
		t.Fatalf("delivery not accepted: %+v", d)
	}
	return a, b, id
}

func TestUpdateReceiptDone(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		a, b := ss[0], ss[1]
		withVersion(t, "v0.1.15")
		withUpdater(t, true)
		id, err := a.queueClusterOperation("update", "v0.1.20", true)
		if err != nil {
			t.Fatal(err)
		}
		b.processClusterOperations()
		writeUpdateResult(t, b, "ok=1\naction=update\narg=\nfrom=v0.1.15\nto=v0.1.20\nlog:\nall good\n")
		b.checkUpdateResult()
		d := deliveryOf(t, b, id, b.memberID)
		if d.Status != "done" || d.AfterVersion != "v0.1.20" || d.FinishedAt == nil {
			t.Fatalf("receipt not written: %+v", d)
		}
		// The other instance's delivery is untouched.
		if d := deliveryOf(t, a, id, a.memberID); d.Status != "pending" {
			t.Fatalf("peer delivery changed: %+v", d)
		}
	})
}

func TestUpdateReceiptFailed(t *testing.T) {
	_, b, id := acceptedUpdate(t, "v0.1.20")
	writeUpdateResult(t, b, "ok=0\naction=update\narg=\nfrom=v0.1.15\nto=v0.1.15\nlog:\n✗ 下载失败\n")
	b.checkUpdateResult()
	d := deliveryOf(t, b, id, b.memberID)
	if d.Status != "failed" || d.FinishedAt == nil || !strings.Contains(d.LastError, "下载失败") {
		t.Fatalf("failure receipt not written: %+v", d)
	}
	if d.AfterVersion != "" {
		t.Fatalf("failed receipt must not claim a version: %+v", d)
	}
}

func TestUpdateReceiptLatestTarget(t *testing.T) {
	_, b, id := acceptedUpdate(t, "latest")
	writeUpdateResult(t, b, "ok=1\naction=update\narg=\nfrom=v0.1.15\nto=v0.1.20\nlog:\nok\n")
	b.checkUpdateResult()
	if d := deliveryOf(t, b, id, b.memberID); d.Status != "done" || d.AfterVersion != "v0.1.20" {
		t.Fatalf("latest receipt not written: %+v", d)
	}
}

func TestUpdateReceiptMismatchIgnored(t *testing.T) {
	_, b, id := acceptedUpdate(t, "v0.1.20")
	// A result for a different version (e.g. a manual update that finished
	// while the cluster delivery was outstanding) must not be attributed.
	writeUpdateResult(t, b, "ok=1\naction=update\narg=\nfrom=v0.1.15\nto=v0.1.99\nlog:\nok\n")
	b.checkUpdateResult()
	if d := deliveryOf(t, b, id, b.memberID); d.Status != "accepted" || d.FinishedAt != nil {
		t.Fatalf("mismatched result attributed: %+v", d)
	}
}

func TestUpdateReceiptStandalone(t *testing.T) {
	s := newTestServer(t)
	// No cluster delivery exists: the receipt write-back is a no-op and
	// the normal reporting path still runs.
	writeUpdateResult(t, s, "ok=1\naction=update\narg=\nfrom=v0.1.15\nto=v0.1.20\nlog:\nok\n")
	s.checkUpdateResult()
	var n int64
	s.st.DB.Model(&store.ClusterDelivery{}).Count(&n)
	if n != 0 {
		t.Fatal("standalone update created deliveries")
	}
}

func TestInferUpdateOutcome(t *testing.T) {
	s := newTestServer(t)
	since := time.Now().UTC().Add(-time.Minute)
	mk := func(memberID, version string, startedAt, seenAt time.Time) {
		if err := s.st.DB.Create(&store.ClusterMember{
			MemberID: memberID, InstanceID: "inst-" + memberID, Host: "h",
			Version: version, StartedAt: startedAt, SeenAt: seenAt,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	mk("at-target", "v0.1.20", now.Add(-time.Hour), now)
	mk("rolled-back", "v0.1.15", now.Add(-30*time.Second), now)
	mk("not-restarted", "v0.1.15", now.Add(-time.Hour), now)
	mk("offline-old", "v0.1.15", now.Add(-30*time.Second), now.Add(-time.Hour))

	cases := []struct {
		name   string
		d      store.ClusterDelivery
		status string
		ok     bool
	}{
		{"pending never inferred", store.ClusterDelivery{MemberID: "at-target", Status: "pending"}, "", false},
		{"already done left alone", store.ClusterDelivery{MemberID: "at-target", Status: "done"}, "", false},
		{"at target is done", store.ClusterDelivery{MemberID: "at-target", Status: "accepted"}, "done", true},
		{"restarted on old version is failed", store.ClusterDelivery{MemberID: "rolled-back", Status: "accepted"}, "failed", true},
		{"old version not restarted yet", store.ClusterDelivery{MemberID: "not-restarted", Status: "accepted"}, "", false},
		{"offline on old version stays open", store.ClusterDelivery{MemberID: "offline-old", Status: "accepted"}, "", false},
		{"unknown member stays open", store.ClusterDelivery{MemberID: "ghost", Status: "accepted"}, "", false},
	}
	for _, c := range cases {
		status, ok := inferUpdateOutcome(s.st.DB, c.d, "v0.1.20", since)
		if status != c.status || ok != c.ok {
			t.Fatalf("%s: got (%q, %v), want (%q, %v)", c.name, status, ok, c.status, c.ok)
		}
	}
}
