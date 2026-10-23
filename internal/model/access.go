package model

import (
	"fmt"
	"net/netip"
	"sort"
)

// NormalizeAccessRules merges paths sharing a port. An empty source set is a
// deliberate deny-all policy; loopback is handled by the enforcement layer.
func NormalizeAccessRules(rules []AccessRule) ([]AccessRule, error) {
	if len(rules) > 4096 {
		return nil, fmt.Errorf("too many access rules")
	}
	byKey := map[string]AccessRule{}
	for _, r := range rules {
		if (r.Protocol != "tcp" && r.Protocol != "udp") || r.Port < 1 || r.Port > 65535 || len(r.Sources) > 256 {
			return nil, fmt.Errorf("invalid access rule for %s/%d", r.Protocol, r.Port)
		}
		key := fmt.Sprintf("%s/%05d", r.Protocol, r.Port)
		v := byKey[key]
		v.Protocol, v.Port = r.Protocol, r.Port
		for _, source := range r.Sources {
			p, err := netip.ParsePrefix(source)
			if err != nil {
				return nil, fmt.Errorf("invalid source CIDR %q", source)
			}
			v.Sources = append(v.Sources, p.Masked().String())
		}
		byKey[key] = v
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]AccessRule, 0, len(keys))
	for _, k := range keys {
		r := byKey[k]
		sort.Strings(r.Sources)
		clean := make([]string, 0, len(r.Sources))
		for _, s := range r.Sources {
			if len(clean) == 0 || clean[len(clean)-1] != s {
				clean = append(clean, s)
			}
		}
		if len(clean) > 256 {
			return nil, fmt.Errorf("too many sources for %s/%d", r.Protocol, r.Port)
		}
		r.Sources = clean
		out = append(out, r)
	}
	return out, nil
}

// TransitionalAccess keeps every protected port covered while cores change.
// Shared ports use the intersection, never temporarily trusting extra sources.
func TransitionalAccess(previous, next []AccessRule) ([]AccessRule, error) {
	a, e := NormalizeAccessRules(previous)
	if e != nil {
		return nil, e
	}
	b, e := NormalizeAccessRules(next)
	if e != nil {
		return nil, e
	}
	byKey := map[string]AccessRule{}
	key := func(r AccessRule) string { return fmt.Sprintf("%s/%d", r.Protocol, r.Port) }
	for _, r := range a {
		byKey[key(r)] = r
	}
	for _, r := range b {
		if old, ok := byKey[key(r)]; ok {
			var sources []string
			for _, x := range old.Sources {
				p, _ := netip.ParsePrefix(x)
				for _, y := range r.Sources {
					q, _ := netip.ParsePrefix(y)
					if p.Addr().BitLen() != q.Addr().BitLen() {
						continue
					}
					if p.Contains(q.Addr()) && p.Bits() <= q.Bits() {
						sources = append(sources, q.String())
					} else if q.Contains(p.Addr()) && q.Bits() <= p.Bits() {
						sources = append(sources, p.String())
					}
				}
			}
			r.Sources = sources
		}
		byKey[key(r)] = r
	}
	var out []AccessRule
	for _, r := range byKey {
		out = append(out, r)
	}
	return NormalizeAccessRules(out)
}
