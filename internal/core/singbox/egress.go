package singbox

import "github.com/cuijianzhuang/polaris/internal/model"

// Dialer resolvers follow the configured default DNS server. A node without
// explicit DNS gets its own local resolver; other inbounds remain unchanged.
func applyOutboundIPs(cfg map[string]any, spec *model.NodeSpec) {
	policies := map[string]model.OutboundIPStrategy{}
	for _, o := range spec.Outbounds {
		if !o.DialIPStrategy.IsAuto() {
			policies[o.Tag] = o.DialIPStrategy
		}
	}
	if len(policies) == 0 {
		return
	}
	dns, _ := cfg["dns"].(map[string]any)
	if dns == nil {
		dns = map[string]any{}
		cfg["dns"] = dns
	}
	resolver, _ := dns["final"].(string)
	if resolver == "" {
		used := map[string]bool{}
		servers, _ := dns["servers"].([]any)
		for _, v := range servers {
			if s, ok := v.(map[string]any); ok {
				tag, _ := s["tag"].(string)
				used[tag] = true
			}
		}
		resolver = "polaris-ip-resolver"
		for used[resolver] {
			resolver += "-"
		}
		dns["servers"] = append(servers, map[string]any{"type": "local", "tag": resolver})
		if _, ok := dns["final"]; !ok {
			dns["final"] = resolver
		}
	}
	if r, ok := cfg["route"].(map[string]any); ok {
		if _, ok := r["default_domain_resolver"]; !ok {
			r["default_domain_resolver"] = resolver
		}
	}
	for _, v := range cfg["outbounds"].([]any) {
		ob := v.(map[string]any)
		tag, _ := ob["tag"].(string)
		p := policies[tag]
		if p.IsAuto() {
			continue
		}
		ob["domain_resolver"] = map[string]any{"server": resolver, "strategy": p}
		if !p.Strict() {
			ob["fallback_delay"] = "250ms"
		}
	}
}
