package panel

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{DataDir: dir, SecretKey: "x", AdminPath: "/"}
	cfg.DB.DSN = filepath.Join(dir, "p.db")
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDDNSFollowsAgentIP(t *testing.T) {
	var mu sync.Mutex
	var calls []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		calls = append(calls, m)
		mu.Unlock()
	}))
	defer hook.Close()
	s := newTestServer(t)
	n := store.Node{ID: "n1", Name: "jp", Address: "198.51.100.1", PublicIPv4: "198.51.100.1", DDNS: true}
	s.st.DB.Create(&n)
	s.st.DB.Create(&store.DNSProvider{ID: "p1", Name: "hook", Kind: "webhook", Endpoint: hook.URL})
	b := store.DomainBinding{ID: "b1", NodeID: "n1", ProviderID: "p1", Domain: "jp.example.com", Family: "ipv4"}
	s.st.DB.Create(&b)

	s.observePublicIP(&n, "203.0.113.7", "")
	var got store.Node
	s.st.DB.First(&got, "id = ?", "n1")
	if got.Address != "203.0.113.7" || got.PublicIPv4 != "203.0.113.7" {
		t.Fatalf("address not updated: %+v", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var bb store.DomainBinding
		s.st.DB.First(&bb, "id = ?", "b1")
		if bb.Status == "synced" {
			if bb.Current != "203.0.113.7" {
				t.Fatalf("current %q", bb.Current)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) == 0 || calls[0]["type"] != "A" || calls[0]["content"] != "203.0.113.7" {
				t.Fatalf("webhook calls %+v", calls)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("binding never synced")
}
