package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Default echo services; each returns the caller's address as plain text.
var (
	defaultIPv4Echo = []string{"https://api4.ipify.org", "https://ipv4.icanhazip.com", "https://4.ipw.cn"}
	defaultIPv6Echo = []string{"https://api6.ipify.org", "https://ipv6.icanhazip.com", "https://6.ipw.cn"}
)

// ipDetector periodically discovers the node's public IPv4/IPv6 for DDNS.
type ipDetector struct {
	v4, v6 []string
	every  time.Duration

	mu       sync.Mutex
	ip4, ip6 string
	at       time.Time
}

func newIPDetector(v4, v6 []string, every time.Duration) *ipDetector {
	if len(v4) == 0 {
		v4 = defaultIPv4Echo
	}
	if len(v6) == 0 {
		v6 = defaultIPv6Echo
	}
	if every == 0 {
		every = 5 * time.Minute
	}
	return &ipDetector{v4: v4, v6: v6, every: every}
}

// Get returns cached addresses, refreshing them when stale.
func (d *ipDetector) Get(ctx context.Context) (string, string) {
	d.mu.Lock()
	if time.Since(d.at) < d.every {
		ip4, ip6 := d.ip4, d.ip6
		d.mu.Unlock()
		return ip4, ip6
	}
	d.at = time.Now()
	d.mu.Unlock()
	ip4 := probeIP(ctx, "tcp4", d.v4)
	ip6 := probeIP(ctx, "tcp6", d.v6)
	d.mu.Lock()
	defer d.mu.Unlock()
	if ip4 != "" || ip6 != "" {
		d.ip4, d.ip6 = ip4, ip6
	}
	return d.ip4, d.ip6
}

func probeIP(ctx context.Context, network string, urls []string) string {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return dialer.DialContext(ctx, network, addr) },
	}}
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		ip := net.ParseIP(strings.TrimSpace(string(b)))
		if ip == nil {
			continue
		}
		if (network == "tcp4") == (ip.To4() != nil) && ip.IsGlobalUnicast() && !ip.IsPrivate() {
			return ip.String()
		}
	}
	return ""
}
