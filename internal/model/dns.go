package model

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// DNSSpec is the resolver configuration of a node's cores. Servers are tried
// in order: one with Domains answers only those names (split DNS), the first
// one without Domains answers everything else.
type DNSSpec struct {
	Servers []DNSServer `json:"servers"`
	// Strategy: "" (core default), "prefer_ipv4", "prefer_ipv6", "ipv4_only", "ipv6_only".
	Strategy string `json:"strategy,omitempty"`
	// Hosts pins names to addresses.
	Hosts  map[string][]string `json:"hosts,omitempty"`
	FakeIP *FakeIPSpec         `json:"fakeip,omitempty"`
	// DomainStrategy decides whether routing resolves domains to match IP
	// rules: "AsIs" (default), "IPIfNonMatch" or "IPOnDemand".
	DomainStrategy string `json:"domainStrategy,omitempty"`
}

// DNSServer is one upstream. Address forms: "8.8.8.8" or "udp://8.8.8.8:53",
// "tcp://h", "tls://h" (DoT), "https://h/dns-query" (DoH), "quic://h" (DoQ)
// and "local" (the system resolver).
type DNSServer struct {
	Tag     string `json:"tag"`
	Address string `json:"address"`
	// Domains this server answers: "geosite:cn", "full:a.com", "domain:a.com"
	// (suffix), "keyword:a" or a bare name (suffix). The panel expands
	// "ruleset:<tag>" before the spec reaches the node.
	Domains []string `json:"domains,omitempty"`
	// Detour sends queries through an outbound tag (sing-box, mihomo, Xray).
	Detour string `json:"detour,omitempty"`
}

// FakeIPSpec hands out addresses from a reserved range instead of real ones;
// the core maps them back to the name when a connection arrives.
type FakeIPSpec struct {
	Inet4Range string `json:"inet4Range,omitempty"` // default 198.18.0.0/15
	Inet6Range string `json:"inet6Range,omitempty"` // default fc00::/18
	// Domains limits fake answers to these names (same forms as a server's
	// Domains); empty = every A/AAAA query.
	Domains []string `json:"domains,omitempty"`
}

// DNSAddr is a parsed server address.
type DNSAddr struct {
	Scheme string // udp, tcp, tls, https, quic, local
	Host   string
	Port   int // 0 = protocol default
	Path   string
}

// HostIsIP reports whether the server is addressed by IP (no bootstrap
// resolution needed).
func (a DNSAddr) HostIsIP() bool {
	_, err := netip.ParseAddr(a.Host)
	return err == nil
}

// DefaultPort is the protocol's standard port.
func (a DNSAddr) DefaultPort() int {
	switch a.Scheme {
	case "tls", "quic":
		return 853
	case "https":
		return 443
	}
	return 53
}

// ParseDNSAddr parses a DNSServer address.
func ParseDNSAddr(s string) (DNSAddr, error) {
	s = strings.TrimSpace(s)
	if s == "local" || s == "system" || s == "localhost" {
		return DNSAddr{Scheme: "local"}, nil
	}
	if !strings.Contains(s, "://") {
		s = "udp://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return DNSAddr{}, fmt.Errorf("DNS 服务器地址无效：%s", s)
	}
	a := DNSAddr{Scheme: strings.ToLower(u.Scheme), Host: u.Hostname()}
	switch a.Scheme {
	case "udp", "tcp", "tls", "quic":
		if u.Path != "" && u.Path != "/" {
			return DNSAddr{}, fmt.Errorf("DNS 服务器地址不应带路径：%s", s)
		}
	case "https":
		a.Path = u.Path
		if a.Path == "" || a.Path == "/" {
			a.Path = "/dns-query"
		}
	default:
		return DNSAddr{}, fmt.Errorf("不支持的 DNS 协议 %s（可用 udp、tcp、tls、https、quic、local）", u.Scheme)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return DNSAddr{}, fmt.Errorf("DNS 服务器端口无效：%s", s)
		}
		a.Port = n
	}
	return a, nil
}

// HostPort is host:port with the default port filled in.
func (a DNSAddr) HostPort() string {
	p := a.Port
	if p == 0 {
		p = a.DefaultPort()
	}
	return net.JoinHostPort(a.Host, strconv.Itoa(p))
}

// DomainSet is a list of domain selectors split by kind.
type DomainSet struct {
	Full, Suffix, Keyword, Geosite []string
}

// Empty reports whether nothing is selected.
func (d DomainSet) Empty() bool {
	return len(d.Full)+len(d.Suffix)+len(d.Keyword)+len(d.Geosite) == 0
}

// ParseDomains splits selectors ("geosite:cn", "full:a", "domain:a",
// "keyword:a", bare = suffix) by kind.
func ParseDomains(sel []string) DomainSet {
	var d DomainSet
	for _, s := range sel {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		k, v, ok := strings.Cut(s, ":")
		if !ok {
			d.Suffix = append(d.Suffix, strings.TrimPrefix(s, "."))
			continue
		}
		switch strings.ToLower(k) {
		case "geosite":
			d.Geosite = append(d.Geosite, v)
		case "full":
			d.Full = append(d.Full, v)
		case "domain":
			d.Suffix = append(d.Suffix, v)
		case "keyword":
			d.Keyword = append(d.Keyword, v)
		default:
			d.Suffix = append(d.Suffix, s)
		}
	}
	return d
}

var dnsStrategies = map[string]bool{"": true, "prefer_ipv4": true, "prefer_ipv6": true, "ipv4_only": true, "ipv6_only": true}

// Validate checks a DNSSpec; outbound tags are checked by the caller.
func (d *DNSSpec) Validate() error {
	if len(d.Servers) == 0 {
		return fmt.Errorf("至少需要一个 DNS 服务器")
	}
	if !dnsStrategies[d.Strategy] {
		return fmt.Errorf("未知的解析策略：%s", d.Strategy)
	}
	switch d.DomainStrategy {
	case "", "AsIs", "IPIfNonMatch", "IPOnDemand":
	default:
		return fmt.Errorf("未知的路由域名策略：%s", d.DomainStrategy)
	}
	tags := map[string]bool{}
	fallback := false
	for i, s := range d.Servers {
		if s.Tag == "" {
			return fmt.Errorf("第 %d 个 DNS 服务器缺少标签", i+1)
		}
		if tags[s.Tag] || s.Tag == "fakeip" || s.Tag == "hosts" || s.Tag == "dns-local" {
			return fmt.Errorf("DNS 服务器标签重复或为保留名：%s", s.Tag)
		}
		tags[s.Tag] = true
		if _, err := ParseDNSAddr(s.Address); err != nil {
			return err
		}
		if len(s.Domains) == 0 {
			fallback = true
		}
	}
	if !fallback {
		return fmt.Errorf("至少需要一个不限域名的 DNS 服务器作为默认")
	}
	for name, ips := range d.Hosts {
		if name == "" || len(ips) == 0 {
			return fmt.Errorf("hosts 条目不完整：%s", name)
		}
		for _, ip := range ips {
			if _, err := netip.ParseAddr(ip); err != nil {
				return fmt.Errorf("hosts %s 的地址无效：%s", name, ip)
			}
		}
	}
	if f := d.FakeIP; f != nil {
		for _, r := range []string{f.Inet4Range, f.Inet6Range} {
			if r == "" {
				continue
			}
			if _, err := netip.ParsePrefix(r); err != nil {
				return fmt.Errorf("FakeIP 地址段无效：%s", r)
			}
		}
	}
	return nil
}

// FakeIPRanges returns the configured ranges with defaults.
func (f *FakeIPSpec) FakeIPRanges() (string, string) {
	v4, v6 := f.Inet4Range, f.Inet6Range
	if v4 == "" {
		v4 = "198.18.0.0/15"
	}
	if v6 == "" {
		v6 = "fc00::/18"
	}
	return v4, v6
}

// DefaultServer is the first server without a domain list.
func (d *DNSSpec) DefaultServer() *DNSServer {
	for i := range d.Servers {
		if len(d.Servers[i].Domains) == 0 {
			return &d.Servers[i]
		}
	}
	return nil
}
