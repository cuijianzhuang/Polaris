package mihomo

import (
	"github.com/cuijianzhuang/polaris/internal/model"
)

// renderDNS returns mihomo's "dns" block and top-level "hosts".
func renderDNS(d *model.DNSSpec) (map[string]any, map[string]any, error) {
	dns := map[string]any{"enable": true, "ipv6": d.Strategy != "ipv4_only"}
	var defaults, bootstrap []string
	policy := map[string][]string{}
	var policyKeys []string
	for _, s := range d.Servers {
		a, err := model.ParseDNSAddr(s.Address)
		if err != nil {
			return nil, nil, err
		}
		var addr string
		switch a.Scheme {
		case "local":
			addr = "system"
		case "udp":
			addr = a.HostPort()
			if a.HostIsIP() {
				bootstrap = append(bootstrap, addr)
			}
		case "https":
			addr = "https://" + a.HostPort() + a.Path
		default:
			addr = a.Scheme + "://" + a.HostPort()
		}
		if s.Detour != "" && a.Scheme != "local" {
			addr += "#" + mapTag(s.Detour)
		}
		if len(s.Domains) == 0 {
			defaults = append(defaults, addr)
			continue
		}
		for _, k := range policyMatchers(s.Domains) {
			if _, ok := policy[k]; !ok {
				policyKeys = append(policyKeys, k)
			}
			policy[k] = append(policy[k], addr)
		}
	}
	dns["nameserver"] = defaults
	if len(policyKeys) > 0 {
		p := map[string]any{}
		for _, k := range policyKeys {
			p[k] = policy[k]
		}
		dns["nameserver-policy"] = p
	}
	if len(bootstrap) > 0 {
		dns["default-nameserver"] = bootstrap // resolves DoH / DoT host names
	}
	if f := d.FakeIP; f != nil {
		v4, v6 := f.FakeIPRanges()
		dns["enhanced-mode"] = "fake-ip"
		dns["fake-ip-range"] = v4
		dns["fake-ip-range6"] = v6
		if len(f.Domains) > 0 {
			dns["fake-ip-filter-mode"] = "whitelist"
			dns["fake-ip-filter"] = policyMatchers(f.Domains)
		}
	} else {
		dns["enhanced-mode"] = "redir-host"
	}
	var hosts map[string]any
	if len(d.Hosts) > 0 {
		hosts = map[string]any{}
		for name, ips := range d.Hosts {
			if len(ips) == 1 {
				hosts[name] = ips[0]
			} else {
				hosts[name] = ips
			}
		}
		dns["use-hosts"] = true
	}
	return dns, hosts, nil
}

// policyMatchers converts selectors into mihomo domain matchers. mihomo has
// no keyword matcher here, so keywords are dropped.
func policyMatchers(sel []string) []string {
	ds := model.ParseDomains(sel)
	var out []string
	for _, g := range ds.Geosite {
		out = append(out, "geosite:"+g)
	}
	out = append(out, ds.Full...)
	for _, s := range ds.Suffix {
		out = append(out, "+."+s)
	}
	return out
}
