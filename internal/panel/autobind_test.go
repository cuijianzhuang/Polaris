package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

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
	if n.Domain != b.Domain || n.Address != "198.51.100.1" || !n.DDNS || nodeHost(&n) != b.Domain {
		t.Fatalf("node should get the domain (preferred by subscriptions) and follow its IP: %+v", n)
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

func TestSyncAllBindings(t *testing.T) {
	var mu sync.Mutex
	n := 0
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); n++; mu.Unlock() }))
	defer hook.Close()
	s := newTestServer(t)
	s.st.DB.Create(&store.DNSProvider{ID: "p1", Name: "hook", Kind: "webhook", Endpoint: hook.URL})
	s.st.DB.Create(&store.Node{ID: "n1", Name: "a", PublicIPv4: "198.51.100.1"})
	s.st.DB.Create(&store.Node{ID: "n2", Name: "b"})
	s.st.DB.Create(&store.DomainBinding{ID: "b1", NodeID: "n1", ProviderID: "p1", Domain: "a.example.com", Family: "ipv4", Current: "198.51.100.1", Status: "synced"})
	s.st.DB.Create(&store.DomainBinding{ID: "b2", NodeID: "n2", ProviderID: "p1", Domain: "b.example.com", Family: "ipv4"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/domains/sync-all", nil)
	s.syncAllBindingsAPI(c)
	var out []struct{ Domain, Status, Error string }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if out[0].Domain != "a.example.com" || out[0].Status != "synced" || out[1].Status != "pending" || out[1].Error == "" {
		t.Fatalf("results: %+v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if n == 0 {
		t.Fatal("an already synced record must still be rewritten")
	}
}

func TestNodeHostPrefersDomain(t *testing.T) {
	n := store.Node{Domain: "hk.example.com", Address: "198.51.100.1", PublicIPv4: "198.51.100.2"}
	if nodeHost(&n) != "hk.example.com" || nodeOrigin(&n) != "198.51.100.1" {
		t.Fatalf("with domain: host %q origin %q", nodeHost(&n), nodeOrigin(&n))
	}
	n.Domain = ""
	if nodeHost(&n) != "198.51.100.1" {
		t.Fatalf("without domain: %q", nodeHost(&n))
	}
	n = store.Node{Domain: "hk.example.com", PublicIPv4: "198.51.100.2"}
	if nodeOrigin(&n) != "198.51.100.2" {
		t.Fatalf("origin falls back to the detected IP: %q", nodeOrigin(&n))
	}
	if nodeHost(&store.Node{PublicIPv6: "2001:db8::1"}) != "2001:db8::1" {
		t.Fatal("detected IPv6 as last resort")
	}
}
