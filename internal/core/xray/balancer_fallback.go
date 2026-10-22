package xray

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
)

type fallbackBinding struct {
	Port int
	UUID string
}
type fallbackPlan struct {
	Outbounds []model.Outbound
	Bridges   []map[string]any
	Reentries []any
	Rules     []any
}

// fallbackTag addresses a handler, not a balancer. Re-enter through a private
// authenticated VLESS listener, then route its tag to the next balancer. Native
// loopback retains the user's email and counts the same payload at every hop;
// the internal VLESS user has no email, so only the original user is charged.
func (a *Adapter) balancerFallbacks(spec *model.NodeSpec) (*fallbackPlan, error) {
	groups := map[string]bool{}
	used := map[string]bool{"api": true, "direct": true, "block": true}
	ports := map[int]bool{}
	first := map[rune]bool{}
	hasFallback := false
	for _, in := range spec.Inbounds {
		used[in.Tag] = true
		ports[in.Port] = true
	}
	_, apiPort, _ := net.SplitHostPort(a.opt.APIListen)
	api, _ := strconv.Atoi(apiPort)
	ports[api] = true
	if a.opt.Limit != nil {
		for _, addr := range []string{a.opt.Limit.Limiter, a.opt.Limit.Reentry} {
			_, p, _ := net.SplitHostPort(addr)
			n, _ := strconv.Atoi(p)
			ports[n] = true
		}
	}
	for _, o := range spec.Outbounds {
		used[o.Tag] = true
		for _, r := range o.Tag {
			first[r] = true
			break
		}
		groups[o.Tag] = o.Type.IsGroup()
		s, err := o.ParseSettings()
		if err != nil {
			return nil, err
		}
		hasFallback = hasFallback || s.Fallback != ""
	}
	if hasFallback {
		if err := model.ValidateOutboundGraph(spec.Outbounds); err != nil {
			return nil, err
		}
	}
	// Selectors are prefixes; internal handlers must not match any user prefix.
	lead := '!'
	for first[lead] {
		lead++
	}
	plan := &fallbackPlan{Outbounds: append([]model.Outbound(nil), spec.Outbounds...)}
	for i, o := range plan.Outbounds {
		s, _ := o.ParseSettings()
		if !o.Type.IsGroup() || !groups[s.Fallback] {
			continue
		}
		sum := sha256.Sum256([]byte(o.Tag))
		base := fmt.Sprintf("%cpolaris-fallback-%x", lead, sum[:8])
		tag := base
		for n := 1; used[tag]; n++ {
			tag = fmt.Sprintf("%s-%d", base, n)
		}
		used[tag] = true
		binding, err := a.fallbackBinding(tag, ports)
		if err != nil {
			return nil, err
		}
		ports[binding.Port] = true
		plan.Bridges = append(plan.Bridges, map[string]any{"tag": tag, "protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{"address": "127.0.0.1", "port": binding.Port, "users": []any{map[string]any{"id": binding.UUID, "encryption": "none"}}}}}})
		plan.Reentries = append(plan.Reentries, map[string]any{"tag": tag, "listen": "127.0.0.1", "port": binding.Port, "protocol": "vless", "settings": map[string]any{"decryption": "none", "clients": []any{map[string]any{"id": binding.UUID}}}})
		plan.Rules = append(plan.Rules, map[string]any{"type": "field", "inboundTag": []string{tag}, "balancerTag": s.Fallback})
		s.Fallback = tag
		plan.Outbounds[i].Settings, _ = json.Marshal(s)
	}
	return plan, nil
}

// Reuse bindings across renders and agent restarts. Native config is already
// private and contains the UUIDs; no extra secret/config file is necessary.
func (a *Adapter) fallbackBinding(tag string, reserved map[int]bool) (fallbackBinding, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fallbacks == nil {
		a.fallbacks = map[string]fallbackBinding{}
		b, err := os.ReadFile(a.configPath())
		if err == nil {
			var cfg struct {
				Inbounds []struct {
					Tag, Listen, Protocol string
					Port                  int
					Settings              struct{ Clients []struct{ ID, Email string } }
				}
			}
			if json.Unmarshal(b, &cfg) == nil {
				for _, in := range cfg.Inbounds {
					if in.Listen == "127.0.0.1" && in.Protocol == "vless" && in.Port > 0 && len(in.Settings.Clients) == 1 && in.Settings.Clients[0].Email == "" {
						a.fallbacks[in.Tag] = fallbackBinding{Port: in.Port, UUID: in.Settings.Clients[0].ID}
					}
				}
			}
		}
	}
	if binding, ok := a.fallbacks[tag]; ok && !reserved[binding.Port] {
		return binding, nil
	}
	for n := 0; n < 20; n++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fallbackBinding{}, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if reserved[port] {
			continue
		}
		binding := fallbackBinding{Port: port, UUID: pki.UUID()}
		a.fallbacks[tag] = binding
		return binding, nil
	}
	return fallbackBinding{}, fmt.Errorf("cannot allocate a private balancer fallback listener")
}
