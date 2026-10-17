package xray

import (
	"fmt"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// renderDNS returns Xray's "dns" and "fakedns" blocks and the routing rules
// that send a server's queries through its detour.
func renderDNS(d *model.DNSSpec, balancers map[string]map[string]any) (dns map[string]any, fake []any, rules []any, err error) {
	var servers []any
	if f := d.FakeIP; f != nil {
		v4, v6 := f.FakeIPRanges()
		fake = []any{map[string]any{"ipPool": v4, "poolSize": 65535}, map[string]any{"ipPool": v6, "poolSize": 65535}}
		srv := map[string]any{"address": "fakedns"}
		if ds := xrayDomains(f.Domains); len(ds) > 0 {
			srv["domains"] = ds
			srv["skipFallback"] = true
		}
		servers = append(servers, srv)
	}
	for _, s := range d.Servers {
		a, err := model.ParseDNSAddr(s.Address)
		if err != nil {
			return nil, nil, nil, err
		}
		srv := map[string]any{}
		switch a.Scheme {
		case "local":
			srv["address"] = "localhost"
		case "udp":
			srv["address"] = a.Host
			if a.Port != 0 {
				srv["port"] = a.Port
			}
		case "tcp":
			srv["address"] = "tcp://" + a.HostPort()
		case "https":
			srv["address"] = "https://" + a.HostPort() + a.Path
		case "quic":
			srv["address"] = "quic+local://" + a.HostPort()
		case "tls":
			return nil, nil, nil, fmt.Errorf("DNS 服务器 %s：Xray 不支持 DoT（tls://），请改用 https:// 或 tcp://", s.Tag)
		}
		if ds := xrayDomains(s.Domains); len(ds) > 0 {
			srv["domains"] = ds
			srv["skipFallback"] = true
		}
		if s.Detour != "" && a.Scheme != "local" {
			tag := "dns-" + s.Tag
			srv["tag"] = tag
			r := map[string]any{"type": "field", "inboundTag": []string{tag}}
			if _, ok := balancers[s.Detour]; ok {
				r["balancerTag"] = s.Detour
			} else {
				r["outboundTag"] = s.Detour
			}
			rules = append(rules, r)
		}
		// A server without domains is plain; keep the short form when possible.
		if len(srv) == 1 {
			servers = append(servers, srv["address"])
		} else {
			servers = append(servers, srv)
		}
	}
	dns = map[string]any{"servers": servers}
	switch d.Strategy {
	case "ipv4_only":
		dns["queryStrategy"] = "UseIPv4"
	case "ipv6_only":
		dns["queryStrategy"] = "UseIPv6"
	case "prefer_ipv4", "prefer_ipv6":
		dns["queryStrategy"] = "UseIP"
	}
	if len(d.Hosts) > 0 {
		h := map[string]any{}
		for name, ips := range d.Hosts {
			if len(ips) == 1 {
				h[name] = ips[0]
			} else {
				h[name] = ips
			}
		}
		dns["hosts"] = h
	}
	return dns, fake, rules, nil
}

// xrayDomains normalises selectors into Xray's domain matcher syntax.
func xrayDomains(sel []string) []string {
	d := model.ParseDomains(sel)
	var out []string
	for _, v := range d.Geosite {
		out = append(out, "geosite:"+v)
	}
	for _, v := range d.Full {
		out = append(out, "full:"+v)
	}
	for _, v := range d.Suffix {
		out = append(out, "domain:"+v)
	}
	for _, v := range d.Keyword {
		out = append(out, "keyword:"+v)
	}
	return out
}
