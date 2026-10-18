package hysteria

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
)

func auth(t *testing.T, a *Adapter, password, addr string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"addr": addr, "auth": password})
	r := httptest.NewRequest(http.MethodPost, "/auth?tag=hy", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("auth %d", w.Code)
	}
}

// OnlineIPs combines the addresses seen when clients authenticated with the
// per-user connection counts of the stats API.
func TestOnlineIPs(t *testing.T) {
	var mu sync.Mutex
	online := map[string]int{"u1": 2, "u2": 1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/online" || r.Header.Get("Authorization") != "secret" {
			http.Error(w, "no", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(online)
	}))
	defer srv.Close()
	a := New(core.Options{WorkDir: t.TempDir()})
	a.secret = "secret"
	a.users["hy"] = map[string]string{"pw1": "u1", "pw2": "u2", "pw3": "u3"}
	a.insts["hy"] = &instance{tag: "hy", api: strings.TrimPrefix(srv.URL, "http://")}

	auth(t, a, "pw1", "1.1.1.1:1000")
	auth(t, a, "pw1", "2.2.2.2:2000")
	auth(t, a, "pw1", "2.2.2.2:2001") // same IP, second connection
	auth(t, a, "pw2", "3.3.3.3:3000")
	auth(t, a, "pw3", "4.4.4.4:4000") // authenticated but not online (any more)
	auth(t, a, "wrong", "5.5.5.5:5000")

	got, err := a.OnlineIPs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// u1 has 2 connections: the newest two addresses are both 2.2.2.2.
	if ips := got["u1"]; len(ips) != 1 || ips[0] != "2.2.2.2" {
		t.Fatalf("u1: %v", ips)
	}
	if ips := got["u2"]; len(ips) != 1 || ips[0] != "3.3.3.3" {
		t.Fatalf("u2: %v", ips)
	}
	if _, ok := got["u3"]; ok {
		t.Fatalf("u3 is not online: %v", got["u3"])
	}
	if _, ok := a.clients["u3"]; ok {
		t.Fatal("what was remembered for an offline user should be dropped")
	}

	// u1 now has three connections from three IPs.
	mu.Lock()
	online["u1"] = 3
	mu.Unlock()
	auth(t, a, "pw1", "6.6.6.6:6000")
	got, _ = a.OnlineIPs(context.Background())
	ips := append([]string(nil), got["u1"]...)
	sort.Strings(ips)
	if len(ips) != 2 || ips[0] != "2.2.2.2" || ips[1] != "6.6.6.6" {
		t.Fatalf("u1 after another client: %v", ips)
	}

	// Stats API down: an error, not an empty answer.
	srv.Close()
	if _, err := a.OnlineIPs(context.Background()); err == nil {
		t.Fatal("expected an error when the stats API is unreachable")
	}
}
