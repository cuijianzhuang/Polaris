package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

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
