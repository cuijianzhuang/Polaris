package xray

import "github.com/cuijianzhuang/polaris/internal/model"

func applyOutboundIP(ob map[string]any, o model.Outbound) {
	p := o.DialIPStrategy
	if p.IsAuto() {
		return
	}
	st, _ := ob["streamSettings"].(map[string]any)
	if st == nil {
		st = map[string]any{}
		ob["streamSettings"] = st
	}
	sock, _ := st["sockopt"].(map[string]any)
	if sock == nil {
		sock = map[string]any{}
		st["sockopt"] = sock
	}
	strategy := "ForceIPv4"
	blocked := "::/0"
	if p.IPv6() {
		strategy = "ForceIPv6"
		blocked = "0.0.0.0/0"
	}
	if p.Strict() {
		sock["domainStrategy"] = strategy
		if o.Type == model.OutDirect {
			settings, _ := ob["settings"].(map[string]any)
			if settings == nil {
				settings = map[string]any{}
				ob["settings"] = settings
			}
			settings["domainStrategy"] = strategy
			settings["finalRules"] = []any{map[string]any{"action": "block", "ip": []string{blocked}}}
		}
	} else if o.DialUDP && (o.Type == model.OutDirect || o.Type == model.OutShadowsocks) {
		strategy = "ForceIPv4v6"
		if p.IPv6() {
			strategy = "ForceIPv6v4"
		}
		sock["domainStrategy"] = strategy
		if o.Type == model.OutDirect {
			settings, _ := ob["settings"].(map[string]any)
			if settings == nil {
				settings = map[string]any{}
				ob["settings"] = settings
			}
			settings["domainStrategy"] = strategy
		}
	} else {
		sock["domainStrategy"] = "ForceIP"
		sock["happyEyeballs"] = map[string]any{"prioritizeIPv6": p.IPv6(), "tryDelayMs": 250, "interleave": 1, "maxConcurrentTry": 4}
	}
}
