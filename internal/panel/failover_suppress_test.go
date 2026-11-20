package panel

import (
	"os"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestFailoverSuppressDeliveries(t *testing.T) {
	ss := clusterServers(t, "", 2)
	a, b := ss[0], ss[1]

	id, err := a.queueClusterOperation("suppress", "", false)
	if err != nil {
		t.Fatal(err)
	}
	b.processClusterOperations()
	var d store.ClusterDelivery
	if err := b.st.DB.First(&d, "operation_id = ? AND member_id = ?", id, b.memberID).Error; err != nil {
		t.Fatal(err)
	}
	if d.Status != "accepted" {
		t.Fatalf("suppress delivery not accepted: %+v", d)
	}
	raw, err := os.ReadFile(b.failoverSuppressPath())
	if err != nil {
		t.Fatal(err)
	}
	until, op, ok := parseFailoverSuppress(string(raw))
	if !ok || op != id {
		t.Fatalf("suppress file: %q", raw)
	}
	if left := time.Until(until); left < 29*time.Minute || left > 30*time.Minute {
		t.Fatalf("suppress window = %v, want just under 30m", left)
	}
	// The queueing instance got no delivery and wrote no file.
	if _, err := os.Stat(a.failoverSuppressPath()); !os.IsNotExist(err) {
		t.Fatal("suppress file written on the wrong instance")
	}

	// A retried write for the same op must not extend the window.
	if err := b.writeFailoverSuppress(id); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(b.failoverSuppressPath())
	if string(raw2) != string(raw) {
		t.Fatalf("suppress window extended on retry:\n%q\n%q", raw, raw2)
	}

	id2, err := a.queueClusterOperation("unsuppress", "", false)
	if err != nil {
		t.Fatal(err)
	}
	b.processClusterOperations()
	// Fresh destination: reusing d would add its primary key to the query.
	var d2 store.ClusterDelivery
	if err := b.st.DB.First(&d2, "operation_id = ? AND member_id = ?", id2, b.memberID).Error; err != nil {
		t.Fatal(err)
	}
	if d2.Status != "accepted" {
		t.Fatalf("unsuppress delivery not accepted: %+v", d2)
	}
	if _, err := os.Stat(b.failoverSuppressPath()); !os.IsNotExist(err) {
		t.Fatal("suppress file not removed")
	}
	// Clearing again (or with no file at all) is fine.
	if err := b.clearFailoverSuppress(); err != nil {
		t.Fatal(err)
	}
}

func TestParseFailoverSuppress(t *testing.T) {
	until, op, ok := parseFailoverSuppress("until=1893456000\nop=abc123\n")
	if !ok || op != "abc123" || until.Unix() != 1893456000 {
		t.Fatalf("round trip: %v %q %v", until, op, ok)
	}
	for _, bad := range []string{"", "op=abc123\n", "until=soon\nop=x\n", "until=\n"} {
		if _, _, ok := parseFailoverSuppress(bad); ok {
			t.Fatalf("accepted garbage: %q", bad)
		}
	}
}
