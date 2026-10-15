package panel

import (
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestComputeStatus(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	cases := []struct {
		u    store.User
		want string
	}{
		{store.User{Status: "active"}, "active"},
		{store.User{Status: "disabled", ExpireAt: &past}, "disabled"},
		{store.User{Status: "active", ExpireAt: &past}, "expired"},
		{store.User{Status: "active", ExpireAt: &future, TrafficLimit: 10, UsedUp: 6, UsedDown: 4}, "limited"},
		{store.User{Status: "limited", TrafficLimit: 10, UsedUp: 1}, "active"},
	}
	for i, c := range cases {
		if got := computeStatus(&c.u, now); got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func TestParseIPList(t *testing.T) {
	in := "# comment\n104.16.1.1\n104.16.1.1\n172.67.2.2:443\n[2606:4700::1]:2053\nip,latency\n198.41.0.3, 120ms\ngarbage\n"
	got := parseIPList(strings.NewReader(in))
	want := []string{"104.16.1.1", "172.67.2.2", "2606:4700::1", "198.41.0.3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v", got)
	}
}

func TestExpandVars(t *testing.T) {
	n := &store.Node{Name: "jp1", Domain: "jp1.example.com", Region: "Tokyo"}
	got := expandVars("{node.name}-{protocol}@{node.domain}/{rand.path}", n, "vless", map[string]string{"path": "abc"})
	if got != "jp1-vless@jp1.example.com/abc" {
		t.Fatal(got)
	}
}
