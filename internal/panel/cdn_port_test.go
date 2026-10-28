package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Cloudflare reaches the origin on the port the client used, so a CDN node
// must use the inbound's own port when the CDN proxies it over HTTPS
// (clients always use TLS to the CDN, so its plain-HTTP ports do not count).
func TestCDNNodePort(t *testing.T) {
	p := &store.CDNProfile{Ports: store.NewJSON([]int{443})}
	for _, c := range []struct{ in, want int }{{2087, 2087}, {2083, 2083}, {443, 443}, {8080, 443}, {2052, 443}, {10086, 443}} {
		if got := cdnNodePort(p, c.in, false); got != c.want {
			t.Fatalf("inbound %d: node port %d, want %d", c.in, got, c.want)
		}
	}
	// Caddy fronting: everything goes through the profile's public port.
	if got := cdnNodePort(p, 2087, true); got != 443 {
		t.Fatalf("fronted: %d", got)
	}
	// No ports configured: 443 for ports the CDN cannot proxy.
	if got := cdnNodePort(&store.CDNProfile{}, 10086, false); got != 443 {
		t.Fatalf("default: %d", got)
	}
}
