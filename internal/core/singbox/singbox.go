// Package singbox adapts sing-box (>= 1.11 config format). sing-box has no
// public user-management API, so user changes fall back to a SIGHUP reload.
// Per-user traffic comes from the v2ray_api stats service, which requires a
// build with the with_v2ray_api tag. Official builds lack it but ship the
// Clash API, which still yields inbound / outbound totals (see clashapi.go).
package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() {
	core.Register(model.CoreSingbox, func(o core.Options) core.CoreAdapter { return New(o) })
}

var statsServices = []string{"experimental.v2rayapi.StatsService", "v2ray.core.app.stats.command.StatsService"}

// Adapter implements core.CoreAdapter for sing-box.
type Adapter struct {
	opt  core.Options
	proc *core.Process

	mu      sync.Mutex
	last    *model.NodeSpec
	api     *core.APIClient
	service string
	seen    map[string][2]int64 // cumulative counters at last collection
	stats   bool                // binary built with with_v2ray_api
	secret  string              // Clash API secret (builds without v2ray_api)
	conns   map[string][2]int64 // Clash API per-connection counters at last poll
}

// New constructs a sing-box adapter.
func New(o core.Options) *Adapter {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.APIListen == "" {
		o.APIListen = "127.0.0.1:10086"
	}
	a := &Adapter{opt: o, seen: map[string][2]int64{}, stats: true, conns: map[string][2]int64{}}
	if o.BinPath != "" {
		a.proc = core.NewProcess("sing-box", o.BinPath, o.WorkDir, o.Logger, "run", "-c", a.configPath(), "-D", o.WorkDir)
		a.stats = hasBuildTag(o.BinPath, "with_v2ray_api")
		if !a.stats {
			o.Logger.Warn("sing-box built without with_v2ray_api: per-user traffic is unavailable, counting inbound/outbound totals via the Clash API", "bin", o.BinPath)
			a.secret = clashSecret(o.WorkDir)
		}
	}
	return a
}

// hasBuildTag inspects `sing-box version` output for a build tag.
func hasBuildTag(bin, tag string) bool {
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), tag)
}

func (a *Adapter) Type() model.CoreType { return model.CoreSingbox }

func (a *Adapter) configPath() string { return filepath.Join(a.opt.WorkDir, "singbox.json") }

// Capabilities returns the matrix for a sing-box version.
func (a *Adapter) Capabilities(v string) model.CapSet {
	b := &model.CapBuilder{}
	streams := []model.Transport{model.TransTCP, model.TransWS, model.TransGRPC, model.TransHTTPUpgrade}
	b.Add([]model.Protocol{model.ProtoVLESS, model.ProtoVMess, model.ProtoTrojan}, streams, []model.Security{model.SecNone, model.SecTLS})
	b.Add([]model.Protocol{model.ProtoVLESS, model.ProtoTrojan}, []model.Transport{model.TransTCP, model.TransGRPC}, []model.Security{model.SecReality})
	b.Add([]model.Protocol{model.ProtoShadowsocks, model.ProtoShadowTLS, model.ProtoSOCKS, model.ProtoHTTP},
		[]model.Transport{model.TransTCP}, []model.Security{model.SecNone})
	b.Add([]model.Protocol{model.ProtoHysteria2, model.ProtoTUIC}, []model.Transport{model.TransQUIC}, []model.Security{model.SecTLS})
	b.Add([]model.Protocol{model.ProtoNaive}, []model.Transport{model.TransTCP}, []model.Security{model.SecTLS})
	if core.VersionAtLeast(v, "1.12.0") {
		b.Add([]model.Protocol{model.ProtoAnyTLS}, []model.Transport{model.TransTCP}, []model.Security{model.SecTLS})
	}
	return model.CapSet{
		Core:     model.CoreSingbox,
		Version:  v,
		Combos:   b.Combos(),
		Flows:    []string{"xtls-rprx-vision"},
		HotUsers: false,
		RealIP:   false,
		Outbounds: []model.OutboundType{model.OutDirect, model.OutBlock, model.OutSOCKS, model.OutHTTP,
			model.OutShadowsocks, model.OutVLESS, model.OutVMess, model.OutTrojan, model.OutHysteria2,
			model.OutWireGuard, model.OutURLTest, model.OutSelector, model.OutBalancer},
		Balancers: []string{model.LBLeastPing, model.LBFallback},
	}
}

// Render translates the sing-box-hosted part of a NodeSpec into sing-box JSON.
func (a *Adapter) Render(spec *model.NodeSpec) ([]byte, error) {
	ins := spec.InboundsFor(model.CoreSingbox)
	var inbounds []any
	var users []string
	seenUser := map[string]bool{}
	var sniffTags []string
	for _, in := range ins {
		ib, err := renderInbound(in)
		if err != nil {
			return nil, fmt.Errorf("inbound %s: %w", in.Tag, err)
		}
		inbounds = append(inbounds, ib)
		for _, u := range in.Users {
			if !seenUser[u.ID] {
				seenUser[u.ID] = true
				users = append(users, u.ID)
			}
		}
		if in.Sniffing.Enabled {
			sniffTags = append(sniffTags, in.Tag)
		}
	}
	sort.Strings(users)

	outs, endpoints, err := renderOutbounds(spec.Outbounds)
	if err != nil {
		return nil, err
	}
	lim := core.PlanLimits(a.opt.Limit, spec, ins)
	if lim != nil {
		// Limited users come back from the agent's limiter here, as "<id>#lim".
		var us []any
		for _, u := range lim.Users {
			us = append(us, map[string]any{"username": u + model.LimitAlias, "password": lim.Secret})
		}
		inbounds = append(inbounds, map[string]any{"type": "socks", "tag": model.LimitInbound,
			"listen": lim.ReentryHost, "listen_port": lim.ReentryPort, "users": us})
		for _, u := range lim.Users {
			outs = append(outs, map[string]any{"type": "socks", "tag": model.LimitOutboundPrefix + u, "server": lim.LimiterHost,
				"server_port": lim.LimiterPort, "version": "5", "username": u, "password": lim.Secret})
		}
	}
	rules, ruleSets, final := renderRules(core.AliasRoutes(spec))
	if len(sniffTags) > 0 {
		rules = append([]any{map[string]any{"inbound": sniffTags, "action": "sniff"}}, rules...)
	}
	route := map[string]any{"rules": nonNil(rules), "final": orDefault(final, "direct")}
	var dns map[string]any
	if d := spec.DNS; d != nil {
		var sets []any
		var resolver string
		dns, sets, resolver, err = renderDNS(d, a.opt.Version)
		if err != nil {
			return nil, err
		}
		ruleSets = mergeRuleSets(ruleSets, sets)
		if resolver != "" && core.VersionAtLeast(a.opt.Version, "1.12.0") {
			route["default_domain_resolver"] = resolver
		}
		if (d.DomainStrategy == "IPIfNonMatch" || d.DomainStrategy == "IPOnDemand") && core.VersionAtLeast(a.opt.Version, "1.11.0") {
			// Resolve before matching so IP rules see the destination address.
			at := 0
			if len(sniffTags) > 0 {
				at = 1
			}
			rules = append(append(rules[:at:at], map[string]any{"action": "resolve"}), rules[at:]...)
			route["rules"] = rules
		}
	}
	if lim != nil {
		// After the sniff rule: a limited user's traffic detours through the
		// limiter, comes back as "<id>#lim" (sniffed again) and then meets
		// every other rule.
		at := 0
		if len(sniffTags) > 0 {
			at = 1
		}
		add := []any{}
		for _, u := range lim.Users {
			add = append(add, map[string]any{"auth_user": []string{u}, "outbound": model.LimitOutboundPrefix + u})
		}
		add = append(add, map[string]any{"inbound": []string{model.LimitInbound}, "action": "sniff"})
		rules = append(append(append([]any(nil), rules[:at]...), add...), rules[at:]...)
		route["rules"] = rules
	}
	if len(ruleSets) > 0 {
		route["rule_set"] = ruleSets
	}
	cfg := map[string]any{
		"log":       map[string]any{"level": "warn", "timestamp": true},
		"inbounds":  nonNil(inbounds),
		"outbounds": outs,
		"route":     route,
	}
	if dns != nil {
		cfg["dns"] = dns
	}
	if !a.stats && a.secret != "" {
		cfg["experimental"] = map[string]any{
			"clash_api": map[string]any{"external_controller": a.opt.APIListen, "secret": a.secret},
		}
	}
	if a.stats {
		cfg["experimental"] = map[string]any{
			"v2ray_api": map[string]any{
				"listen": a.opt.APIListen,
				"stats": map[string]any{"enabled": true, "users": nonNilStr(users),
					"inbounds": nonNilStr(tagsOf(inbounds)), "outbounds": nonNilStr(tagsOf(outs))},
			},
		}
	}
	if len(endpoints) > 0 {
		cfg["endpoints"] = endpoints
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.last = spec.Clone()
	a.mu.Unlock()
	return out, nil
}

func renderInbound(in model.Inbound) (map[string]any, error) {
	ib := map[string]any{
		"type":        string(in.Protocol.Type),
		"tag":         in.Tag,
		"listen":      in.Listen,
		"listen_port": in.Port,
	}
	var users []any
	switch in.Protocol.Type {
	case model.ProtoVLESS:
		for _, u := range in.Users {
			x := map[string]any{"name": u.ID, "uuid": u.UUID}
			if in.Protocol.Flow != "" {
				x["flow"] = in.Protocol.Flow
			}
			users = append(users, x)
		}
	case model.ProtoVMess:
		for _, u := range in.Users {
			users = append(users, map[string]any{"name": u.ID, "uuid": u.UUID, "alterId": 0})
		}
	case model.ProtoTrojan, model.ProtoHysteria2, model.ProtoAnyTLS, model.ProtoShadowTLS:
		for _, u := range in.Users {
			users = append(users, map[string]any{"name": u.ID, "password": u.Password})
		}
	case model.ProtoTUIC:
		for _, u := range in.Users {
			users = append(users, map[string]any{"name": u.ID, "uuid": u.UUID, "password": u.Password})
		}
	case model.ProtoShadowsocks:
		m := in.Protocol.Method
		if model.SS2022KeyLen(m) == 0 {
			return nil, fmt.Errorf("sing-box multi-user shadowsocks requires a 2022 cipher")
		}
		ib["method"] = m
		ib["password"] = in.Protocol.ServerKey
		for _, u := range in.Users {
			users = append(users, map[string]any{"name": u.ID, "password": u.SSKey(m)})
		}
	case model.ProtoNaive, model.ProtoSOCKS, model.ProtoHTTP:
		for _, u := range in.Users {
			users = append(users, map[string]any{"username": u.ID, "password": u.Password})
		}
	default:
		return nil, fmt.Errorf("protocol %s not supported by sing-box", in.Protocol.Type)
	}
	ib["users"] = nonNil(users)

	switch in.Protocol.Type {
	case model.ProtoHysteria2:
		if in.Protocol.UpMbps > 0 {
			ib["up_mbps"] = in.Protocol.UpMbps
		}
		if in.Protocol.DownMbps > 0 {
			ib["down_mbps"] = in.Protocol.DownMbps
		}
		if in.Protocol.Obfs != "" {
			ib["obfs"] = map[string]any{"type": in.Protocol.Obfs, "password": in.Protocol.ObfsPassword}
		}
	case model.ProtoTUIC:
		ib["congestion_control"] = orDefault(in.Protocol.CongestionControl, "bbr")
	case model.ProtoShadowTLS:
		host, port, err := splitHostPort(in.Protocol.HandshakeServer, 443)
		if err != nil {
			return nil, fmt.Errorf("shadowtls handshake: %w", err)
		}
		ib["version"] = 3
		ib["handshake"] = map[string]any{"server": host, "server_port": port}
		if in.Protocol.Detour != "" {
			ib["detour"] = in.Protocol.Detour
		}
	}

	if t := renderTransport(in.Transport); t != nil {
		ib["transport"] = t
	}
	tls, err := renderServerTLS(in.Security)
	if err != nil {
		return nil, err
	}
	if tls != nil {
		if _, ok := tls["alpn"]; !ok && in.Protocol.Type == model.ProtoTUIC {
			tls["alpn"] = []string{"h3"} // TUIC clients always offer h3
		}
		ib["tls"] = tls
	}
	if err := core.MergeExtra(ib, in.Extra); err != nil {
		return nil, fmt.Errorf("extra: %w", err)
	}
	return ib, nil
}

func renderTransport(t model.TransportSpec) map[string]any {
	switch t.Type {
	case model.TransWS:
		m := map[string]any{"type": "ws", "path": orDefault(t.Path, "/")}
		if t.Host != "" {
			m["headers"] = map[string]any{"Host": t.Host}
		}
		return m
	case model.TransHTTPUpgrade:
		m := map[string]any{"type": "httpupgrade", "path": orDefault(t.Path, "/")}
		if t.Host != "" {
			m["host"] = t.Host
		}
		return m
	case model.TransGRPC:
		return map[string]any{"type": "grpc", "service_name": t.ServiceName}
	}
	return nil
}

func renderServerTLS(sec model.SecuritySpec) (map[string]any, error) {
	switch sec.Type {
	case "", model.SecNone:
		return nil, nil
	case model.SecTLS:
		if sec.CertPEM == "" || sec.KeyPEM == "" {
			return nil, fmt.Errorf("tls requires a certificate")
		}
		t := map[string]any{
			"enabled":     true,
			"certificate": core.PEMLines(sec.CertPEM),
			"key":         core.PEMLines(sec.KeyPEM),
		}
		if sec.ServerName != "" {
			t["server_name"] = sec.ServerName
		}
		if len(sec.ALPN) > 0 {
			t["alpn"] = sec.ALPN
		}
		return t, nil
	case model.SecReality:
		host, port, err := splitHostPort(sec.RealityDest, 443)
		if err != nil {
			return nil, fmt.Errorf("reality dest: %w", err)
		}
		sn := ""
		if len(sec.RealityServerNames) > 0 {
			sn = sec.RealityServerNames[0]
		}
		return map[string]any{
			"enabled":     true,
			"server_name": sn,
			"reality": map[string]any{
				"enabled":     true,
				"handshake":   map[string]any{"server": host, "server_port": port},
				"private_key": sec.RealityPrivateKey,
				"short_id":    nonNilStr(sec.RealityShortIDs),
			},
		}, nil
	}
	return nil, fmt.Errorf("security %s not supported", sec.Type)
}

func renderClientTLS(c *model.ClientTLS) map[string]any {
	if c == nil || c.Type == "" || c.Type == model.SecNone {
		return nil
	}
	t := map[string]any{"enabled": true}
	if c.ServerName != "" {
		t["server_name"] = c.ServerName
	}
	if len(c.ALPN) > 0 {
		t["alpn"] = c.ALPN
	}
	if c.Insecure {
		t["insecure"] = true
	}
	fp := c.Fingerprint
	if c.Type == model.SecReality && fp == "" {
		fp = "chrome"
	}
	if fp != "" {
		t["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	if c.Type == model.SecReality {
		t["reality"] = map[string]any{"enabled": true, "public_key": c.PublicKey, "short_id": c.ShortID}
	}
	return t
}

func renderOutbounds(obs []model.Outbound) ([]any, []any, error) {
	outs := []any{map[string]any{"type": "direct", "tag": "direct"}}
	var endpoints []any
	for _, o := range obs {
		s, err := o.ParseSettings()
		if err != nil {
			return nil, nil, fmt.Errorf("outbound %s: %w", o.Tag, err)
		}
		ob := map[string]any{"tag": o.Tag}
		switch o.Type {
		case model.OutDirect:
			ob["type"] = "direct"
		case model.OutBlock:
			continue // rendered as a reject rule action
		case model.OutSOCKS, model.OutHTTP:
			ob["type"] = string(o.Type)
			ob["server"], ob["server_port"] = s.Server, s.Port
			if s.Username != "" {
				ob["username"], ob["password"] = s.Username, s.Password
			}
		case model.OutShadowsocks:
			ob["type"] = "shadowsocks"
			ob["server"], ob["server_port"] = s.Server, s.Port
			ob["method"], ob["password"] = s.Method, s.Password
		case model.OutVLESS:
			ob["type"] = "vless"
			ob["server"], ob["server_port"], ob["uuid"] = s.Server, s.Port, s.UUID
			if s.Flow != "" {
				ob["flow"] = s.Flow
			}
		case model.OutVMess:
			ob["type"] = "vmess"
			ob["server"], ob["server_port"], ob["uuid"], ob["security"] = s.Server, s.Port, s.UUID, "auto"
		case model.OutTrojan:
			ob["type"] = "trojan"
			ob["server"], ob["server_port"], ob["password"] = s.Server, s.Port, s.Password
		case model.OutHysteria2:
			ob["type"] = "hysteria2"
			ob["server"], ob["server_port"], ob["password"] = s.Server, s.Port, s.Password
			if s.UpMbps > 0 {
				ob["up_mbps"] = s.UpMbps
			}
			if s.DownMbps > 0 {
				ob["down_mbps"] = s.DownMbps
			}
			if s.ObfsPassword != "" {
				ob["obfs"] = map[string]any{"type": "salamander", "password": s.ObfsPassword}
			}
		case model.OutWireGuard:
			peer := map[string]any{
				"address": s.Server, "port": s.Port, "public_key": s.PeerPublicKey,
				"allowed_ips": []string{"0.0.0.0/0", "::/0"},
			}
			if s.PreSharedKey != "" {
				peer["pre_shared_key"] = s.PreSharedKey
			}
			if len(s.Reserved) > 0 {
				peer["reserved"] = s.Reserved
			}
			ep := map[string]any{"type": "wireguard", "tag": o.Tag, "address": s.LocalAddress,
				"private_key": s.PrivateKey, "peers": []any{peer}}
			if s.MTU > 0 {
				ep["mtu"] = s.MTU
			}
			if o.Detour != "" {
				ep["detour"] = o.Detour
			}
			endpoints = append(endpoints, ep)
			continue
		case model.OutURLTest:
			ob["type"] = "urltest"
			ob["outbounds"] = s.Members
			ob["url"] = orDefault(s.TestURL, "https://www.gstatic.com/generate_204")
			ob["interval"] = orDefault(s.Interval, "1m")
		case model.OutSelector:
			ob["type"] = "selector"
			ob["outbounds"] = s.Members
			if len(s.Members) > 0 {
				ob["default"] = s.Members[0]
			}
		case model.OutBalancer:
			switch s.Strategy {
			case model.LBRandom, model.LBRoundRobin:
				return nil, nil, fmt.Errorf("outbound %s: sing-box has no %s balancer", o.Tag, s.Strategy)
			}
			ob["type"] = "urltest"
			ob["outbounds"] = s.Members
			ob["url"] = orDefault(s.TestURL, "https://www.gstatic.com/generate_204")
			ob["interval"] = orDefault(s.Interval, "1m")
			tol := s.Tolerance
			if s.Strategy == model.LBFallback && tol == 0 {
				tol = 30000 // stay on the current member until it fails
			}
			if tol > 0 {
				ob["tolerance"] = tol
			}
		default:
			return nil, nil, fmt.Errorf("outbound %s: type %s not supported by sing-box", o.Tag, o.Type)
		}
		if s.Transport != nil {
			if t := renderTransport(*s.Transport); t != nil {
				ob["transport"] = t
			}
		}
		if t := renderClientTLS(s.Security); t != nil {
			ob["tls"] = t
		}
		if o.Detour != "" {
			ob["detour"] = o.Detour
		}
		outs = append(outs, ob)
	}
	return outs, endpoints, nil
}

// mergeRuleSets appends rule sets whose tag is not there yet.
func mergeRuleSets(have, more []any) []any {
	seen := map[string]bool{}
	for _, r := range have {
		seen[r.(map[string]any)["tag"].(string)] = true
	}
	for _, r := range more {
		if t := r.(map[string]any)["tag"].(string); !seen[t] {
			seen[t] = true
			have = append(have, r)
		}
	}
	return have
}

func renderRules(rules []model.RouteRule) (out []any, ruleSets []any, final string) {
	rs := append([]model.RouteRule(nil), rules...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Priority < rs[j].Priority })
	seenSet := map[string]bool{}
	addSet := func(tag, url string) {
		if seenSet[tag] {
			return
		}
		seenSet[tag] = true
		ruleSets = append(ruleSets, map[string]any{"type": "remote", "tag": tag, "format": "binary", "url": url})
	}
	for _, r := range rs {
		m := r.Match
		x := map[string]any{}
		setIf := func(k string, v []string) {
			if len(v) > 0 {
				x[k] = v
			}
		}
		setIf("inbound", m.Inbound)
		setIf("auth_user", m.Users)
		setIf("domain", m.Domain)
		setIf("domain_suffix", m.DomainSuffix)
		setIf("domain_keyword", m.DomainKeyword)
		var cidrs = append([]string(nil), m.IPCIDR...)
		var sets []string
		for _, g := range m.Geosite {
			tag := "geosite-" + g
			addSet(tag, "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/"+tag+".srs")
			sets = append(sets, tag)
		}
		for _, g := range m.GeoIP {
			if g == "private" {
				x["ip_is_private"] = true
				continue
			}
			tag := "geoip-" + g
			addSet(tag, "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/"+tag+".srs")
			sets = append(sets, tag)
		}
		setIf("ip_cidr", cidrs)
		setIf("rule_set", sets)
		var ports []int
		var ranges []string
		for _, p := range m.Port {
			if strings.Contains(p, "-") {
				ranges = append(ranges, strings.Replace(p, "-", ":", 1))
			} else if n, err := strconv.Atoi(p); err == nil {
				ports = append(ports, n)
			}
		}
		if len(ports) > 0 {
			x["port"] = ports
		}
		setIf("port_range", ranges)
		if m.Network != "" {
			x["network"] = strings.Split(m.Network, ",")
		}
		if len(x) == 0 {
			if len(m.UserGroups) > 0 {
				continue
			}
			final = r.Outbound
			continue
		}
		if r.Outbound == "block" {
			x["action"] = "reject"
		} else {
			x["outbound"] = r.Outbound
		}
		out = append(out, x)
	}
	if final == "block" {
		out = append(out, map[string]any{"action": "reject"})
		final = ""
	}
	return out, ruleSets, final
}

// Validate runs `sing-box check`.
func (a *Adapter) Validate(cfg []byte) error {
	if a.opt.BinPath == "" {
		return fmt.Errorf("sing-box binary not configured")
	}
	f, err := core.WriteTemp(a.opt.WorkDir, "singbox-check-*.json", cfg)
	if err != nil {
		return err
	}
	defer os.Remove(f)
	return core.RunCheck(a.opt.BinPath, "check", "-c", f)
}

// resetCounters forgets the cumulative counters: a (re)started or reloaded
// sing-box counts from zero again.
func (a *Adapter) resetCounters() {
	a.mu.Lock()
	a.seen = map[string][2]int64{}
	a.mu.Unlock()
}

func (a *Adapter) Start(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("sing-box binary not configured")
	}
	a.resetCounters()
	return a.proc.Start(ctx)
}

func (a *Adapter) Stop(ctx context.Context) error {
	if a.proc == nil {
		return nil
	}
	a.resetCounters()
	return a.proc.Stop(ctx)
}

// Reload asks sing-box to re-read its config via SIGHUP, starting it if needed.
func (a *Adapter) Reload(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("sing-box binary not configured")
	}
	a.resetCounters()
	if !a.proc.Running() {
		return a.proc.Start(ctx)
	}
	return a.proc.Signal(syscall.SIGHUP)
}

func (a *Adapter) AddUsers(context.Context, string, []model.User) error { return core.ErrNotSupported }
func (a *Adapter) RemoveUsers(context.Context, string, []string) error  { return core.ErrNotSupported }

func (a *Adapter) client() (*core.APIClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.api != nil {
		return a.api, nil
	}
	c, err := core.DialAPI(a.opt.APIListen)
	if err != nil {
		return nil, err
	}
	a.api = c
	return c, nil
}

// CollectTraffic reads cumulative counters and returns the difference to the
// previous read. A counter that went backwards means the core restarted, in
// which case the current value is the delta.
func (a *Adapter) CollectTraffic(ctx context.Context) ([]model.TrafficDelta, error) {
	if !a.stats {
		if a.secret == "" {
			return nil, core.ErrNotSupported
		}
		return a.clashTraffic(ctx)
	}
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	var stats []core.Stat
	a.mu.Lock()
	svc := a.service
	a.mu.Unlock()
	candidates := statsServices
	if svc != "" {
		candidates = []string{svc}
	}
	for _, s := range candidates {
		stats, err = c.QueryStats(ctx, s, "", false) // users, inbounds and outbounds
		if err == nil {
			svc = s
			break
		}
		if !core.IsUnimplemented(err) {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	cur := core.AllTraffic(stats)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.service = svc
	var out []model.TrafficDelta
	for id, v := range cur {
		prev := a.seen[id]
		d := model.TrafficDelta{UserID: id, Up: v[0] - prev[0], Down: v[1] - prev[1]}
		if d.Up < 0 || d.Down < 0 {
			d.Up, d.Down = v[0], v[1]
		}
		if d.Up != 0 || d.Down != 0 {
			out = append(out, d)
		}
	}
	a.seen = cur
	return out, nil
}

func (a *Adapter) OnlineIPs(context.Context) (map[string][]string, error) {
	return nil, core.ErrNotSupported
}

func (a *Adapter) Health(ctx context.Context) model.CoreStatus {
	st := model.CoreStatus{Type: model.CoreSingbox, Version: a.opt.Version}
	if a.proc != nil {
		st.Running = a.proc.Running()
		st.PID = a.proc.PID()
		st.StartedAt = a.proc.StartedAt()
		if err := a.proc.LastError(); err != nil && !st.Running {
			st.Error = err.Error()
		}
	}
	a.mu.Lock()
	if a.last != nil {
		st.Inbounds = len(a.last.InboundsFor(model.CoreSingbox))
	}
	a.mu.Unlock()
	return st
}

// Logs returns recent process output.
func (a *Adapter) Logs(n int) []string {
	if a.proc == nil {
		return nil
	}
	return a.proc.Logs(n)
}

func splitHostPort(s string, def int) (string, int, error) {
	if s == "" {
		return "", 0, fmt.Errorf("empty address")
	}
	host, p, err := net.SplitHostPort(s)
	if err != nil {
		return s, def, nil
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, err
	}
	return host, n, nil
}

func nonNil(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

func nonNilStr(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// tagsOf lists the tags of rendered inbounds / outbounds (for per-tag stats).
func tagsOf(items []any) []string {
	var tags []string
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if t, _ := m["tag"].(string); t != "" {
				tags = append(tags, t)
			}
		}
	}
	return tags
}
