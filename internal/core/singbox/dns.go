package singbox

import (
	"sort"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

const geositeSRS = "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/"

// renderDNS returns sing-box's "dns" block, the remote rule sets its rules
// use and the tag outbounds should resolve their own server names with.
// sing-box 1.12 replaced the address-string servers with typed ones; older
// versions get the legacy form.
func renderDNS(d *model.DNSSpec, version string) (dns map[string]any, ruleSets []any, resolver string, err error) {
	typed := core.VersionAtLeast(version, "1.12.0")
	var servers, rules []any
	needLocal := false
	for _, s := range d.Servers {
		a, err := model.ParseDNSAddr(s.Address)
		if err != nil {
			return nil, nil, "", err
		}
		srv := map[string]any{"tag": s.Tag}
		if typed {
			srv["type"] = a.Scheme
			if a.Scheme != "local" {
				srv["server"] = a.Host
				if a.Port != 0 {
					srv["server_port"] = a.Port
				}
				if a.Scheme == "https" && a.Path != "/dns-query" {
					srv["path"] = a.Path
				}
				if !a.HostIsIP() {
					srv["domain_resolver"] = "dns-local"
					needLocal = true
				}
			}
		} else {
			switch a.Scheme {
			case "local":
				srv["address"] = "local"
			case "https":
				srv["address"] = "https://" + a.HostPort() + a.Path
			default:
				srv["address"] = a.Scheme + "://" + a.HostPort()
			}
			if a.Scheme != "local" && !a.HostIsIP() {
				srv["address_resolver"] = "dns-local"
				needLocal = true
			}
		}
		// sing-box refuses a detour to a plain direct outbound.
		if s.Detour != "" && s.Detour != "direct" && a.Scheme != "local" {
			srv["detour"] = s.Detour
		}
		servers = append(servers, srv)
		if len(s.Domains) > 0 {
			r, sets := domainRule(s.Domains)
			r["server"] = s.Tag
			rules = append(rules, r)
			ruleSets = append(ruleSets, sets...)
		}
	}
	if needLocal {
		if typed {
			servers = append(servers, map[string]any{"type": "local", "tag": "dns-local"})
		} else {
			servers = append(servers, map[string]any{"tag": "dns-local", "address": "local"})
		}
	}
	var head []any
	if len(d.Hosts) > 0 && typed {
		names := make([]string, 0, len(d.Hosts))
		for n := range d.Hosts {
			names = append(names, n)
		}
		sort.Strings(names)
		servers = append(servers, map[string]any{"type": "hosts", "tag": "hosts", "predefined": d.Hosts})
		head = append(head, map[string]any{"domain": names, "server": "hosts"})
	}
	dns = map[string]any{}
	if f := d.FakeIP; f != nil {
		v4, v6 := f.FakeIPRanges()
		r := map[string]any{}
		if len(f.Domains) > 0 {
			var sets []any
			r, sets = domainRule(f.Domains)
			ruleSets = append(ruleSets, sets...)
		}
		r["query_type"] = []string{"A", "AAAA"}
		r["server"] = "fakeip"
		if typed {
			servers = append(servers, map[string]any{"type": "fakeip", "tag": "fakeip", "inet4_range": v4, "inet6_range": v6})
		} else {
			servers = append(servers, map[string]any{"tag": "fakeip", "address": "fakeip"})
			dns["fakeip"] = map[string]any{"enabled": true, "inet4_range": v4, "inet6_range": v6}
		}
		head = append(head, r)
	}
	dns["servers"] = servers
	if rs := append(head, rules...); len(rs) > 0 {
		dns["rules"] = rs
	}
	if def := d.DefaultServer(); def != nil {
		dns["final"] = def.Tag
		resolver = def.Tag
	}
	if d.Strategy != "" {
		dns["strategy"] = d.Strategy
	}
	return dns, ruleSets, resolver, nil
}

// domainRule turns selectors into a sing-box rule body plus the remote rule
// sets its geosite entries need.
func domainRule(sel []string) (map[string]any, []any) {
	ds := model.ParseDomains(sel)
	r := map[string]any{}
	if len(ds.Full) > 0 {
		r["domain"] = ds.Full
	}
	if len(ds.Suffix) > 0 {
		r["domain_suffix"] = ds.Suffix
	}
	if len(ds.Keyword) > 0 {
		r["domain_keyword"] = ds.Keyword
	}
	var sets []any
	var tags []string
	for _, g := range ds.Geosite {
		tag := "geosite-" + g
		tags = append(tags, tag)
		sets = append(sets, map[string]any{"type": "remote", "tag": tag, "format": "binary", "url": geositeSRS + tag + ".srs"})
	}
	if len(tags) > 0 {
		r["rule_set"] = tags
	}
	return r, sets
}
