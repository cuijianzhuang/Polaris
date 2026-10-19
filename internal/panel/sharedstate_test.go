package panel

import (
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// testRedis connects to POLARIS_TEST_REDIS_URL with a key prefix of its
// own, or skips the test.
func testRedis(t *testing.T) *redisState {
	t.Helper()
	url := os.Getenv("POLARIS_TEST_REDIS_URL")
	if url == "" {
		t.Skip("POLARIS_TEST_REDIS_URL not set")
	}
	r, err := newRedisState(url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r.prefix = "polaris-test:" + store.NewID() + ":"
	t.Cleanup(func() {
		ctx, cancel := r.ctx()
		defer cancel()
		if keys, _ := r.c.Keys(ctx, r.prefix+"*").Result(); len(keys) > 0 {
			r.c.Del(ctx, keys...)
		}
		r.c.Close()
	})
	return r
}

// useTestRedis switches a test server to Redis shared state.
func useTestRedis(t *testing.T, s *Server) {
	r := testRedis(t)
	s.shared = r
	s.subLimit.s, s.shopLimit.s = r, r
}

// memOnly puts a test server back on in-memory shared state.
func memOnly(s *Server) *memState {
	m := newMemState()
	s.shared = m
	s.subLimit.s, s.shopLimit.s = m, m
	return m
}

// twin returns a second instance on the same Redis keys.
func twin(t *testing.T, r *redisState) *redisState {
	b, err := newRedisState(os.Getenv("POLARIS_TEST_REDIS_URL"), r.log)
	if err != nil {
		t.Fatal(err)
	}
	b.prefix = r.prefix
	t.Cleanup(func() { b.c.Close() })
	return b
}

func sorted(xs []string) string {
	sort.Strings(xs)
	return strings.Join(xs, ",")
}

// Two panel instances see the same online IPs: node n1 reports to one,
// node n2 to the other.
func TestRedisOnlineIPsShared(t *testing.T) {
	a := testRedis(t)
	b := twin(t, a)
	now := time.Now()
	a.setNodeIPs("u", "n1", []string{"1.1.1.1", "2001:db8::1"}, now)
	if got := sorted(b.setNodeIPs("u", "n2", []string{"3.3.3.3", "1.1.1.1"}, now)); got != "1.1.1.1,2001:db8::1,3.3.3.3" {
		t.Fatalf("union across instances: %s", got)
	}
	// A node's new report replaces its old list.
	if got := sorted(a.setNodeIPs("u", "n1", []string{"4.4.4.4"}, now)); got != "1.1.1.1,3.3.3.3,4.4.4.4" {
		t.Fatalf("replace: %s", got)
	}
	snap := b.onlineIPs([]string{"u", "nobody"})
	if len(snap["u"]["n1"]) != 1 || len(snap["u"]["n2"]) != 2 || len(snap["nobody"]) != 0 {
		t.Fatalf("snapshot: %v", snap)
	}
	if at := snap["u"]["n1"]["4.4.4.4"]; now.Sub(at).Abs() > time.Millisecond {
		t.Fatalf("seen time %v vs %v", at, now)
	}
	// IPs a node stopped reporting expire after the window.
	later := now.Add(onlineIPWindow + time.Second)
	if got := sorted(a.setNodeIPs("u", "n1", []string{"5.5.5.5"}, later)); got != "5.5.5.5" {
		t.Fatalf("stale IPs kept: %s", got)
	}
}

// Rate limits and alert de-duplication count across instances.
func TestRedisLimitsShared(t *testing.T) {
	a := testRedis(t)
	b := twin(t, a)
	allowed := 0
	for i := 0; i < 30; i++ {
		r := a
		if i%2 == 1 {
			r = b
		}
		if r.allow("sub", 12, "9.9.9.9") {
			allowed++
		}
	}
	if allowed != 12 {
		t.Fatalf("allowed %d of 30 across instances, want 12", allowed)
	}
	if !a.allow("sub", 12, "8.8.8.8") || !a.allow("shop", 12, "9.9.9.9") {
		t.Fatal("limits leaked to another key or bucket")
	}
	if !a.once("ip:u", time.Minute) || b.once("ip:u", time.Minute) {
		t.Fatal("alert fired twice across instances")
	}
}

// When Redis goes away the panel keeps working on its own memory.
func TestRedisFallback(t *testing.T) {
	r := testRedis(t)
	r.c.Close()
	if !r.allow("sub", 10, "1.1.1.1") {
		t.Fatal("limit refused while Redis is down")
	}
	if got := r.setNodeIPs("u", "n1", []string{"1.1.1.1"}, time.Now()); len(got) != 1 {
		t.Fatalf("online IPs lost while Redis is down: %v", got)
	}
	if len(r.onlineIPs([]string{"u"})["u"]["n1"]) != 1 {
		t.Fatal("fallback snapshot")
	}
	if !r.once("k", time.Minute) || r.once("k", time.Minute) {
		t.Fatal("fallback once")
	}
}

// The panel's subscription limit is shared by instances on one Redis.
func TestRedisSubscriptionLimitAcrossPanels(t *testing.T) {
	r := testRedis(t)
	a, b := newTestServer(t), newTestServer(t)
	useTestRedis(t, a)
	a.shared, b.shared = r, twin(t, r)
	a.subLimit.s, b.subLimit.s = a.shared, b.shared
	a.subLimit.perMinute, b.subLimit.perMinute = 10, 10
	n := 0
	for i := 0; i < 20; i++ {
		s := a
		if i%2 == 1 {
			s = b
		}
		if s.subLimit.Allow("7.7.7.7") {
			n++
		}
	}
	if n != 10 {
		t.Fatalf("allowed %d, want 10", n)
	}
}
