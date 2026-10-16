package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestDomainLabel(t *testing.T) {
	for in, want := range map[string]string{"BWG-HK": "bwg-hk", "US DMIT 01": "us-dmit-01", "东京 01": "01", "香港": "", "--A__b--": "a-b"} {
		if got := domainLabel(in); got != want {
			t.Errorf("domainLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAutoBindNode(t *testing.T) {
	var mu sync.Mutex
	var writes []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		writes = append(writes, m)
		mu.Unlock()
	}))
	defer hook.Close()
	s := newTestServer(t)
	s.st.DB.Create(&store.DNSProvider{ID: "p1", Name: "hook", Kind: "webhook", Endpoint: hook.URL})
	s.st.DB.Create(&store.Node{ID: "n1abcdefgh", Name: "BWG HK", Address: "198.51.100.1", PublicIPv4: "198.51.100.1", DDNS: true})
	s.st.DB.Create(&store.Node{ID: "n2abcdefgh", Name: "bwg-hk", PublicIPv4: "198.51.100.2"})
	s.st.DB.Create(&store.Node{ID: "n3abcdefgh", Name: "香港"})
	ctx := context.Background()

	if b, err := s.autoBindNode(ctx, "n1abcdefgh"); b != nil || err != nil {
		t.Fatalf("bound while switched off: %+v %v", b, err)
	}
	s.st.SetSetting("ddns_auto_provider", "p1")
	s.st.SetSetting("ddns_auto_zone", "Node.Example.com.")

	b, err := s.autoBindNode(ctx, "n1abcdefgh")
	if err != nil || b == nil || b.Domain != "bwg-hk.node.example.com" || b.Status != "synced" {
		t.Fatalf("first node: %+v %v", b, err)
	}
	var n store.Node
	s.st.DB.First(&n, "id = ?", "n1abcdefgh")
	if n.Domain != b.Domain || n.Address != b.Domain || n.DDNS {
		t.Fatalf("node should use the domain as its address: %+v", n)
	}
	mu.Lock()
	if len(writes) == 0 || writes[len(writes)-1]["content"] != "198.51.100.1" {
		t.Fatalf("DNS not written with the public IP: %v", writes)
	}
	mu.Unlock()

	if b, _ := s.autoBindNode(ctx, "n1abcdefgh"); b != nil {
		t.Fatal("a node with a binding must not get a second one")
	}
	if b, _ := s.autoBindNode(ctx, "n2abcdefgh"); b == nil || b.Domain != "bwg-hk-2.node.example.com" {
		t.Fatalf("name clash: %+v", b)
	}
	b, _ = s.autoBindNode(ctx, "n3abcdefgh")
	if b == nil || b.Domain != "node-n3abcd.node.example.com" || b.Status != "pending" {
		t.Fatalf("no ASCII name / no IP yet: %+v", b)
	}
}
