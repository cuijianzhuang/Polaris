package hysteria

import (
	"github.com/cuijianzhuang/polaris/internal/model"
)

// renderResolver maps the default DNS server onto Hysteria's single
// "resolver" (UDP, TCP, DoT or DoH). Hysteria has no split DNS, hosts or
// FakeIP, and no DoQ; with a system or unsupported default it keeps the
// system resolver (nil).
func renderResolver(d *model.DNSSpec) map[string]any {
	def := d.DefaultServer()
	if def == nil {
		return nil
	}
	a, err := model.ParseDNSAddr(def.Address)
	if err != nil {
		return nil
	}
	switch a.Scheme {
	case "udp", "tcp":
		return map[string]any{"type": a.Scheme, a.Scheme: map[string]any{"addr": a.HostPort(), "timeout": "4s"}}
	case "tls", "https":
		r := map[string]any{"addr": a.HostPort(), "timeout": "10s"}
		if !a.HostIsIP() {
			r["sni"] = a.Host
		}
		return map[string]any{"type": a.Scheme, a.Scheme: r}
	}
	return nil
}

// directMode maps the query strategy onto the direct outbound's IP mode.
func directMode(strategy string) string {
	switch strategy {
	case "prefer_ipv4":
		return "46"
	case "prefer_ipv6":
		return "64"
	case "ipv4_only":
		return "4"
	case "ipv6_only":
		return "6"
	}
	return ""
}
