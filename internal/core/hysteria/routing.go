package hysteria

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Hysteria2 routes with its own outbounds (direct, SOCKS5, HTTP) and an ACL:
// one line per rule, "outbound(address[, proto/port])", first match wins.

// hyOutbounds are the outbound types Hysteria can carry.
var hyOutbounds = []model.OutboundType{model.OutDirect, model.OutBlock, model.OutSOCKS, model.OutHTTP}

// renderRouting returns the outbounds and ACL lines for the inbound tag,
// or nils when the spec has nothing Hysteria needs to route.
func renderRouting(spec *model.NodeSpec, tag, directMode string) ([]any, []string) {
	direct := map[string]any{"name": "direct", "type": "direct"}
	if directMode != "" {
		direct["direct"] = map[string]any{"mode": directMode}
	}
	outs := []any{direct} // the first outbound is Hysteria's default
	have := map[string]string{"direct": "direct", "block": "reject"}
	for _, o := range spec.Outbounds {
		if ob := renderOutbound(o); ob != nil {
			outs = append(outs, ob)
			have[o.Tag] = o.Tag
		}
	}
	rs := append([]model.RouteRule(nil), spec.Routes...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Priority < rs[j].Priority })
	var acl []string
	for _, r := range rs {
		target, ok := have[r.Outbound]
		if !ok {
			continue // an egress Hysteria can't carry (VLESS, groups, chained…)
		}
		acl = append(acl, aclLines(r.Match, tag, target)...)
	}
	if len(acl) == 0 && len(outs) == 1 {
		if directMode != "" {
			return outs, nil
		}
		return nil, nil
	}
	return outs, acl
}

// renderOutbound maps a SOCKS / HTTP egress; anything else (or one chained
// through another outbound) has no Hysteria equivalent.
func renderOutbound(o model.Outbound) map[string]any {
	if o.Detour != "" || (o.Type != model.OutSOCKS && o.Type != model.OutHTTP) || o.Tag == "direct" || o.Tag == "reject" || o.Tag == "default" {
		return nil
	}
	s, err := o.ParseSettings()
	if err != nil || s.Server == "" || s.Port == 0 {
		return nil
	}
	addr := net.JoinHostPort(s.Server, strconv.Itoa(s.Port))
	if o.Type == model.OutSOCKS {
		c := map[string]any{"addr": addr}
		if s.Username != "" {
			c["username"], c["password"] = s.Username, s.Password
		}
		return map[string]any{"name": o.Tag, "type": "socks5", "socks5": c}
	}
	u := url.URL{Scheme: "http", Host: addr}
	c := map[string]any{}
	if s.Security != nil && s.Security.Type == model.SecTLS {
		u.Scheme = "https"
		if s.Security.Insecure {
			c["insecure"] = true
		}
	}
	if s.Username != "" {
		u.User = url.UserPassword(s.Username, s.Password)
	}
	c["url"] = u.String()
	return map[string]any{"name": o.Tag, "type": "http", "http": c}
}

// aclLines turns one rule into ACL lines for the inbound tag. Destinations
// (domains, suffixes, geosite, geoip, CIDRs) are alternatives, as are the
// protocol / port pairs; user conditions can't be expressed, so such rules
// are left out rather than applied to everyone.
func aclLines(m model.RouteMatch, tag, target string) []string {
	if len(m.Users) > 0 || len(m.UserGroups) > 0 {
		return nil
	}
	if len(m.Inbound) > 0 && !contains(m.Inbound, tag) {
		return nil
	}
	var addrs []string
	addrs = append(addrs, m.Domain...)
	for _, d := range m.DomainSuffix {
		addrs = append(addrs, "suffix:"+d)
	}
	for _, g := range m.Geosite {
		addrs = append(addrs, "geosite:"+g)
	}
	for _, g := range m.GeoIP {
		addrs = append(addrs, "geoip:"+g)
	}
	addrs = append(addrs, m.IPCIDR...)
	if len(addrs) == 0 {
		if len(m.DomainKeyword) > 0 {
			return nil // keyword-only: Hysteria has no keyword matcher
		}
		addrs = []string{"all"}
	}
	protos := []string{"*"}
	if m.Network != "" {
		protos = strings.Split(m.Network, ",")
	}
	var pp []string
	for _, p := range protos {
		p = strings.TrimSpace(p)
		if len(m.Port) == 0 {
			if p != "*" {
				pp = append(pp, p)
			}
			continue
		}
		for _, port := range m.Port {
			pp = append(pp, p+"/"+port)
		}
	}
	var out []string
	for _, a := range addrs {
		if len(pp) == 0 {
			out = append(out, fmt.Sprintf("%s(%s)", target, a))
			continue
		}
		for _, p := range pp {
			out = append(out, fmt.Sprintf("%s(%s, %s)", target, a, p))
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
