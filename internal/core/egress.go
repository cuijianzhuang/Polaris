package core

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/model"
)

type EgressPlan struct {
	Spec            *model.NodeSpec
	OutboundOrigins map[string]string
}

// PlanEgress isolates each inbound's egress graph. It preserves route ordering
// and group/fallback/detour references rather than changing shared outbounds.
func PlanEgress(input *model.NodeSpec, typ model.CoreType, version string, wire *model.LimitWire) (*EgressPlan, error) {
	var policies []model.Inbound
	for _, in := range input.InboundsFor(typ) {
		if e := in.OutboundIPStrategy.Validate(); e != nil {
			return nil, e
		}
		if !in.OutboundIPStrategy.IsAuto() {
			policies = append(policies, in)
		}
	}
	if len(policies) == 0 {
		return &EgressPlan{Spec: input}, nil
	}
	for _, in := range policies {
		if len(in.Extra) > 0 {
			var extra map[string]any
			if e := json.Unmarshal(in.Extra, &extra); e != nil {
				return nil, e
			}
			for _, k := range []string{"tag", "detour"} {
				if _, ok := extra[k]; ok {
					return nil, fmt.Errorf("outbound IP strategy cannot override inbound %s", k)
				}
			}
		}
		if e := CheckOutboundIP(typ, version, in.OutboundIPStrategy); e != nil {
			return nil, e
		}
		if typ == model.CoreXray && len(input.LimitedUsers(in.Users)) > 0 && (wire == nil || wire.ReentryProtocol != "vless") {
			return nil, fmt.Errorf("Xray outbound IP strategy with user limits requires identity-preserving VLESS limiter re-entry")
		}
		if len(input.LimitedUsers(in.Users)) > 0 && (wire == nil || !wire.Contexts) {
			return nil, fmt.Errorf("outbound IP strategy on %s requires an Agent with scoped limiter support", in.Tag)
		}
	}
	if err := CheckOutboundIPDNS(typ, input.DNS); err != nil {
		return nil, err
	}

	spec := input.Clone()
	obs := map[string]model.Outbound{"direct": {Tag: "direct", Type: model.OutDirect}, "block": {Tag: "block", Type: model.OutBlock}}
	starts := map[rune]bool{}
	for _, o := range spec.Outbounds {
		obs[o.Tag] = o
		for _, r := range o.Tag {
			starts[r] = true
			break
		}
	}
	lead := '!'
	for starts[lead] {
		lead++
	}
	if e := model.ValidateOutboundGraph(spec.Outbounds); e != nil {
		return nil, e
	}
	routes := append([]model.RouteRule(nil), spec.Routes...)
	sort.SliceStable(routes, func(i, j int) bool { return routes[i].Priority < routes[j].Priority })
	// Native renderers treat empty-match rules as a final outbound, not an
	// early catch-all. Keep their last-final-wins behavior for each scope.
	final := "direct"
	var matched []model.RouteRule
	for _, r := range routes {
		b, _ := json.Marshal(r.Match)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if len(m) == 0 {
			final = r.Outbound
			continue
		}
		matched = append(matched, r)
	}
	origins := map[string]string{}
	var scoped []model.RouteRule
	for _, in := range policies {
		tags := map[string]string{}
		var clone func(string, bool) (string, error)
		clone = func(tag string, udp bool) (string, error) {
			key := fmt.Sprintf("%s/%t", tag, udp)
			if tag == "block" {
				return tag, nil
			}
			if v := tags[key]; v != "" {
				return v, nil
			}
			o, ok := obs[tag]
			if !ok {
				return "", fmt.Errorf("outbound %s not found", tag)
			}
			if o.Type == model.OutWireGuard {
				return "", fmt.Errorf("outbound IP strategy does not support WireGuard egress %s", tag)
			}
			sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s/%t", in.Tag, tag, udp)))
			next := fmt.Sprintf("%cpolaris-ip-%x", lead, sum[:12])
			tags[key] = next
			o.Tag = next
			o.DialUDP = udp
			s, e := o.ParseSettings()
			if e != nil {
				return "", e
			}
			if o.Detour != "" {
				o.Detour, e = clone(o.Detour, udp)
				if e != nil {
					return "", e
				}
			} else if !o.Type.IsGroup() && o.Type != model.OutBlock {
				o.DialIPStrategy = in.OutboundIPStrategy
				if in.OutboundIPStrategy.Strict() && s.Server != "" {
					if ip, e := netip.ParseAddr(s.Server); e == nil && ip.Unmap().Is6() != in.OutboundIPStrategy.IPv6() {
						tags[key] = "block"
						return "block", nil
					}
				}
			}
			for i, member := range s.Members {
				s.Members[i], e = clone(member, udp)
				if e != nil {
					return "", e
				}
			}
			if s.Fallback != "" {
				s.Fallback, e = clone(s.Fallback, udp)
				if e != nil {
					return "", e
				}
			}
			o.Settings, e = json.Marshal(s)
			if e != nil {
				return "", e
			}
			if len(spec.Outbounds) > 4096 {
				return "", fmt.Errorf("too many scoped outbounds")
			}
			origins[next] = tag
			spec.Outbounds = append(spec.Outbounds, o)
			return next, nil
		}
		own := func(r model.RouteRule, limited bool) (model.RouteRule, bool) {
			if len(r.Match.UserGroups) > 0 && len(r.Match.Users) == 0 {
				return r, false
			}
			if len(r.Match.Inbound) > 0 && !slices.Contains(r.Match.Inbound, in.Tag) {
				return r, false
			}
			r.Match.Inbound = []string{in.Tag}
			if limited {
				var users []string
				for _, u := range input.LimitedUsers(in.Users) {
					if len(r.Match.Users) == 0 || slices.Contains(r.Match.Users, u) {
						users = append(users, model.ScopedLimitUser(u, in.Tag)+model.LimitAlias)
					}
				}
				if len(users) == 0 {
					return r, false
				}
				r.Match.Inbound = []string{model.LimitInbound}
				r.Match.Users = users
			}
			return r, true
		}
		appendRule := func(r model.RouteRule, limited bool) error {
			r, ok := own(r, limited)
			if !ok {
				return nil
			}
			original := obs[r.Outbound]
			guardDirect := original.Type == model.OutDirect && original.Detour == ""
			if typ == model.CoreSingbox && in.OutboundIPStrategy.Strict() {
				direct, proxy, err := singStrictPath(obs, r.Outbound, map[string]bool{})
				if err != nil {
					return err
				}
				if direct && proxy {
					return fmt.Errorf("sing-box strict IP strategy cannot mix direct and proxy members in %s", r.Outbound)
				}
				guardDirect = direct
			}
			if typ == model.CoreXray && !in.OutboundIPStrategy.Strict() && (r.Match.Network == "" || r.Match.Network == "udp" || r.Match.Network == "tcp,udp") {
				u := r
				u.Match.Network = "udp"
				var e error
				u.Outbound, e = clone(r.Outbound, true)
				if e != nil {
					return e
				}
				scoped = append(scoped, u)
				if r.Match.Network == "udp" {
					return nil
				}
			}
			target, e := clone(r.Outbound, false)
			if e != nil {
				return e
			}
			r.Outbound = target
			// sing-box's ip_version is conjunctive with CIDR/geosite match fields.
			// Only direct sockets reject opposite-family final destinations; a
			// proxy can carry either family through an allowed next-hop socket.

			if typ == model.CoreSingbox && in.OutboundIPStrategy.Strict() && guardDirect {
				g := r
				g.Outbound = "block"
				g.Match.NativeIPVersion = 6
				if in.OutboundIPStrategy.IPv6() {
					g.Match.NativeIPVersion = 4
				}
				scoped = append(scoped, g)
			}
			scoped = append(scoped, r)
			return nil
		}
		for _, r := range matched {
			if e := appendRule(r, false); e != nil {
				return nil, e
			}
			if e := appendRule(r, true); e != nil {
				return nil, e
			}
		}
		def := model.RouteRule{Outbound: final}
		if e := appendRule(def, false); e != nil {
			return nil, e
		}
		if e := appendRule(def, true); e != nil {
			return nil, e
		}
	}
	// Keep the generated scoped rules first while preserving their stable
	// order and the original priorities among all unmodified rules.
	for i := range scoped {
		scoped[i].Priority = i
	}
	offset := len(scoped)
	for i := range routes {
		routes[i].Priority = offset + i
	}
	spec.Routes = append(scoped, routes...)
	return &EgressPlan{Spec: spec, OutboundOrigins: origins}, nil
}

func CheckOutboundIP(typ model.CoreType, version string, strategy model.OutboundIPStrategy) error {
	if e := strategy.Validate(); e != nil {
		return e
	}
	if strategy.IsAuto() {
		return nil
	}
	switch typ {
	case model.CoreXray:
		// finalRules enforces the family after routing and DNS while keeping
		// the core's existing default final rules for other destinations.
		if !VersionAtLeast(version, "26.5.9") {
			return fmt.Errorf("outbound IP strategy requires Xray 26.5.9+")
		}
	case model.CoreSingbox:
		if !VersionAtLeast(version, "1.12.0") {
			return fmt.Errorf("outbound IP strategy requires sing-box 1.12+")
		}
	default:
		return fmt.Errorf("outbound IP strategy is not supported by %s", typ)
	}
	return nil
}

// CheckOutboundIPDNS rejects conflicting global resolver policies rather than
// silently weakening a strict per-inbound family restriction.
func CheckOutboundIPDNS(typ model.CoreType, dns *model.DNSSpec) error {
	if dns == nil {
		return nil
	}
	if typ == model.CoreSingbox {
		if len(dns.Hosts) > 0 || dns.FakeIP != nil {
			return fmt.Errorf("sing-box outbound IP strategy currently requires DNS without hosts or FakeIP")
		}
		for _, server := range dns.Servers {
			if len(server.Domains) > 0 {
				return fmt.Errorf("sing-box outbound IP strategy currently requires a default DNS resolver without split DNS")
			}
		}
	}
	if typ == model.CoreSingbox && dns.DomainStrategy != "" && dns.DomainStrategy != "AsIs" {
		return fmt.Errorf("sing-box outbound IP strategy cannot be combined with pre-routing IP resolution")
	}
	if dns.Strategy == "ipv4_only" || dns.Strategy == "ipv6_only" {
		return fmt.Errorf("outbound IP strategy requires dual-stack DNS; remove the global DNS family restriction")
	}
	return nil
}

// sing-box has no direct-outbound equivalent of Xray's ipsBlocked. A strict
// literal guard is safe for a group of direct/block leaves, but would wrongly
// reject a proxy's remotely dialed destinations in a mixed group.
func singStrictPath(obs map[string]model.Outbound, tag string, seen map[string]bool) (direct, proxy bool, err error) {
	if seen[tag] {
		return false, false, nil
	}
	seen[tag] = true
	o, ok := obs[tag]
	if !ok {
		return false, false, fmt.Errorf("outbound %s not found", tag)
	}
	if o.Detour != "" {
		return false, false, fmt.Errorf("sing-box strict IP strategy does not support detour %s", tag)
	}
	if o.Type == model.OutBlock {
		return false, false, nil
	}
	if !o.Type.IsGroup() {
		return o.Type == model.OutDirect, o.Type != model.OutDirect, nil
	}
	settings, err := o.ParseSettings()
	if err != nil {
		return false, false, err
	}
	members := append([]string(nil), settings.Members...)
	if settings.Fallback != "" {
		members = append(members, settings.Fallback)
	}
	for _, member := range members {
		d, p, e := singStrictPath(obs, member, seen)
		if e != nil {
			return false, false, e
		}
		direct = direct || d
		proxy = proxy || p
	}
	return
}

// EgressTraffic folds private socket counters into the original outbound.
// User and inbound counters keep their identities and are never combined.
func EgressTraffic(traffic map[string][2]int64, origins map[string]string) map[string][2]int64 {
	out := make(map[string][2]int64, len(traffic))
	for id, counts := range traffic {
		if tag, ok := strings.CutPrefix(id, TagPrefixOutbound); ok && origins[tag] != "" {
			id = TagPrefixOutbound + origins[tag]
		}
		v := out[id]
		v[0] += counts[0]
		v[1] += counts[1]
		out[id] = v
	}
	return out
}
