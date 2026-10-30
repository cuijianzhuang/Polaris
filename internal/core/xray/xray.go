// Package xray adapts Xray-core. Users are hot-updated through the gRPC
// HandlerService and traffic is read (and reset) from StatsService.
package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/encoding/protowire"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() {
	core.Register(model.CoreXray, func(o core.Options) core.CoreAdapter { return New(o) })
}

const (
	statsService   = "xray.app.stats.command.StatsService"
	handlerService = "xray.app.proxyman.command.HandlerService"
)

// Adapter implements core.CoreAdapter for Xray-core.
type Adapter struct {
	opt  core.Options
	proc *core.Process

	mu            sync.Mutex
	egressOrigins map[string]string
	last          *model.NodeSpec // spec of the most recent Render, used by hot updates
	api           *core.APIClient
	fallbacks     map[string]fallbackBinding
}

// New constructs an Xray adapter.
func New(o core.Options) *Adapter {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.APIListen == "" {
		o.APIListen = "127.0.0.1:10085"
	}
	a := &Adapter{opt: o}
	if o.BinPath != "" {
		// A SIGKILL cannot run diagnostic defers. All owned native processes
		// died with that agent; discard their private client files on restart.
		if o.WorkDir != "" {
			entries, _ := os.ReadDir(o.WorkDir)
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), "diagnostic-") {
					_ = os.RemoveAll(filepath.Join(o.WorkDir, entry.Name()))
				}
			}
		}
		a.proc = core.NewProcess("xray", o.BinPath, o.WorkDir, o.Logger, "run", "-c", a.configPath())
		a.proc.BindToParent = o.BindToParent
		a.proc.Env = []string{"XRAY_LOCATION_ASSET=" + filepath.Dir(o.BinPath)}
	}
	return a
}

func (a *Adapter) Type() model.CoreType { return model.CoreXray }

func (a *Adapter) configPath() string { return filepath.Join(a.opt.WorkDir, "xray.json") }

// Capabilities returns the matrix for a given Xray version.
func (a *Adapter) Capabilities(v string) model.CapSet {
	b := &model.CapBuilder{}
	streams := []model.Transport{model.TransTCP, model.TransWS, model.TransGRPC}
	if core.VersionAtLeast(v, "1.8.9") {
		streams = append(streams, model.TransHTTPUpgrade)
	}
	xhttp := core.VersionAtLeast(v, "24.9.30")
	if xhttp {
		streams = append(streams, model.TransXHTTP)
	}
	tlsable := streams
	b.Add([]model.Protocol{model.ProtoVLESS, model.ProtoVMess}, streams, []model.Security{model.SecNone})
	b.Add([]model.Protocol{model.ProtoVLESS, model.ProtoVMess, model.ProtoTrojan}, tlsable, []model.Security{model.SecTLS})
	realityT := []model.Transport{model.TransTCP, model.TransGRPC}
	if xhttp {
		realityT = append(realityT, model.TransXHTTP)
	}
	b.Add([]model.Protocol{model.ProtoVLESS, model.ProtoTrojan}, realityT, []model.Security{model.SecReality})
	b.Add([]model.Protocol{model.ProtoVLESS, model.ProtoVMess}, []model.Transport{model.TransKCP}, []model.Security{model.SecNone})
	b.Add([]model.Protocol{model.ProtoShadowsocks, model.ProtoSOCKS, model.ProtoHTTP},
		[]model.Transport{model.TransTCP}, []model.Security{model.SecNone})
	features := []string{model.FeatureProxyProtocol, model.FeatureTCPHTTPHeader}
	if core.VersionAtLeast(v, "26.5.9") {
		features = append(features, model.FeatureOutboundIP)
	}
	if core.VersionAtLeast(v, "25.9.11") {
		features = append(features, model.FeatureVLESSEncryption, model.FeatureRealityMLDSA65)
	}
	if core.VersionAtLeast(v, model.XrayPQRealityVersion) {
		features = append(features, model.FeatureRealityPQOnly)
	}
	return model.CapSet{
		Core:     model.CoreXray,
		Version:  v,
		Combos:   b.Combos(),
		Features: features,
		Flows:    []string{"xtls-rprx-vision"},
		HotUsers: true,
		RealIP:   true,
		Outbounds: []model.OutboundType{model.OutDirect, model.OutBlock, model.OutSOCKS, model.OutHTTP,
			model.OutShadowsocks, model.OutVLESS, model.OutVMess, model.OutTrojan, model.OutWireGuard,
			model.OutURLTest, model.OutSelector, model.OutBalancer},
		Balancers: []string{model.LBLeastPing, model.LBLeastLoad, model.LBRandom, model.LBRoundRobin, model.LBFallback},
	}
}

// Render translates the Xray-hosted part of a NodeSpec into Xray JSON.
func (a *Adapter) Render(spec *model.NodeSpec) ([]byte, error) {
	original := spec
	egress, err := core.PlanEgress(spec, model.CoreXray, a.opt.Version, a.opt.Limit)
	if err != nil {
		return nil, err
	}
	spec = egress.Spec
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"api": map[string]any{
			"tag":      "api",
			"listen":   a.opt.APIListen,
			"services": []string{"HandlerService", "StatsService", "LoggerService", "RoutingService"},
		},
		"stats": map[string]any{},
		"policy": map[string]any{
			"levels": map[string]any{"0": map[string]any{
				"statsUserUplink": true, "statsUserDownlink": true, "statsUserOnline": true,
			}},
			"system": map[string]any{"statsInboundUplink": true, "statsInboundDownlink": true,
				"statsOutboundUplink": true, "statsOutboundDownlink": true},
		},
	}
	var inbounds []any
	for _, in := range spec.InboundsFor(model.CoreXray) {
		if in.Security.Type == model.SecReality && in.Security.RealityMinClientVer == "" &&
			core.VersionAtLeast(a.opt.Version, "26.7.11") && !core.VersionAtLeast(a.opt.Version, model.XrayPQRealityVersion) {
			// 26.7.x defaults minClientVer to 26.3.27, which refuses every
			// non-Xray client (sing-box, mihomo); keep them connectable.
			in.Security.RealityMinClientVer = "0.0.0"
		}
		ib, err := renderInbound(in, a.legacyKCP())
		if err != nil {
			return nil, fmt.Errorf("inbound %s: %w", in.Tag, err)
		}
		if sn, ok := ib["sniffing"].(map[string]any); ok && spec.DNS != nil && spec.DNS.FakeIP != nil {
			// Map fake addresses back to the name they were handed out for.
			if do, ok := sn["destOverride"].([]string); ok {
				sn["destOverride"] = append(append([]string(nil), do...), "fakedns")
			}
		}
		inbounds = append(inbounds, ib)
	}
	lim := core.PlanLimits(a.opt.Limit, spec, spec.InboundsFor(model.CoreXray))
	if lim != nil {
		// Limited users come back from the agent's limiter here, as "<id>#lim".
		var accts []any
		for _, binding := range lim.Bindings {
			u := binding.WireUser
			accts = append(accts, map[string]any{"user": u + model.LimitAlias, "pass": lim.Secret})
		}
		if lim.ReentryProtocol == "vless" {
			var clients []any
			for _, binding := range lim.Bindings {
				clients = append(clients, map[string]any{"id": model.LimitUUID(lim.Secret, binding.WireUser), "email": binding.WireUser + model.LimitAlias})
			}
			inbounds = append(inbounds, map[string]any{"tag": model.LimitInbound, "listen": lim.ReentryHost, "port": lim.ReentryPort, "protocol": "vless", "settings": map[string]any{"decryption": "none", "clients": clients}, "streamSettings": map[string]any{"network": "tcp", "security": "none"}, "sniffing": map[string]any{"enabled": true, "destOverride": []string{"http", "tls", "quic"}, "routeOnly": true}})
		} else {
			inbounds = append(inbounds, map[string]any{
				"tag": model.LimitInbound, "listen": lim.ReentryHost, "port": lim.ReentryPort, "protocol": "socks",
				"settings": map[string]any{"auth": "password", "udp": true, "ip": lim.ReentryHost, "accounts": accts},
				// Recover the domain for routing rules (the limiter forwards addresses as given).
				"sniffing": map[string]any{"enabled": true, "destOverride": []string{"http", "tls", "quic"}, "routeOnly": true},
			})
		}
	}
	cfg["inbounds"] = inbounds

	fallbacks, err := a.balancerFallbacks(spec)
	if err != nil {
		return nil, err
	}
	outs, balancers, observe, err := renderOutbounds(fallbacks.Outbounds, a.legacyKCP())
	if err != nil {
		return nil, err
	}
	outs = append(outs, fallbacks.Bridges...)
	cfg["inbounds"] = append(inbounds, fallbacks.Reentries...)
	rules, defTag := renderRules(core.AliasRoutes(spec), balancers)
	if _, isGroup := balancers[defTag]; isGroup {
		rules = append(rules, map[string]any{"type": "field", "network": "tcp,udp", "balancerTag": defTag})
	}
	if defTag != "" {
		outs = moveFirst(outs, defTag)
	}
	var limRules []any
	if lim != nil {
		for _, binding := range lim.Bindings {
			u := binding.WireUser
			tag := model.LimitOutboundPrefix + u
			outs = append(outs, map[string]any{"tag": tag, "protocol": "socks", "settings": map[string]any{
				"servers": []any{map[string]any{"address": lim.LimiterHost, "port": lim.LimiterPort,
					"users": []any{map[string]any{"user": u, "pass": lim.Secret}}}},
			}})
			r := map[string]any{"type": "field", "user": []string{binding.User}, "outboundTag": tag}
			if binding.Inbound != "" {
				r["inboundTag"] = []string{binding.Inbound}
			}
			limRules = append(limRules, r)
		}
	}
	cfg["outbounds"] = outs
	domainStrategy := "AsIs"
	if d := spec.DNS; d != nil {
		dns, fake, dnsRules, err := renderDNS(d, balancers)
		if err != nil {
			return nil, err
		}
		cfg["dns"] = dns
		if fake != nil {
			cfg["fakedns"] = fake
		}
		if len(dnsRules) > 0 {
			// After the api rule, before user rules.
			rules = append(append(rules[:1:1], dnsRules...), rules[1:]...)
		}
		if d.DomainStrategy != "" {
			domainStrategy = d.DomainStrategy
		}
	}
	if len(limRules) > 0 {
		// First after the api rule: a limited user's traffic detours through
		// the limiter, then meets every other rule as "<id>#lim".
		rules = append(append(rules[:1:1], limRules...), rules[1:]...)
	}
	if len(fallbacks.Rules) > 0 {
		rules = append(append(rules[:1:1], fallbacks.Rules...), rules[1:]...)
	}
	routing := map[string]any{"domainStrategy": domainStrategy, "rules": rules}
	if len(balancers) > 0 {
		var bl []any
		for _, b := range balancers {
			bl = append(bl, b)
		}
		routing["balancers"] = bl
	}
	cfg["routing"] = routing
	for k, v := range observe {
		cfg[k] = v
	}
	if g := a.geodata(spec.Geodata); g != nil {
		cfg["geodata"] = g
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.last = original.Clone()
	a.egressOrigins = egress.OutboundOrigins
	a.mu.Unlock()
	return out, nil
}

// withFallbacks adds resolved fallbacks (entries the panel could not resolve
// carry no Dest and are skipped).
func withFallbacks(settings map[string]any, in model.Inbound) map[string]any {
	if !in.FallbackCapable() {
		return settings
	}
	var fbs []any
	for _, f := range in.Protocol.Fallbacks {
		if f.Dest == "" {
			continue
		}
		fb := map[string]any{"dest": f.Dest}
		if f.Path != "" {
			fb["path"] = f.Path
		}
		if f.ALPN != "" {
			fb["alpn"] = f.ALPN
		}
		if f.Xver > 0 {
			fb["xver"] = f.Xver
		}
		fbs = append(fbs, fb)
	}
	if len(fbs) > 0 {
		settings["fallbacks"] = fbs
	}
	return settings
}

func renderInbound(in model.Inbound, legacyKCP bool) (map[string]any, error) {
	// Header presence is not proof of a trusted peer. Only a protected
	// loopback listener may accept forwarding metadata from its local proxy.
	if ip := net.ParseIP(in.Listen); ip == nil || !ip.IsLoopback() {
		in.Transport.TrustedForwardedFor = nil
	}
	ib := map[string]any{
		"tag":      in.Tag,
		"listen":   in.Listen,
		"port":     in.Port,
		"protocol": string(in.Protocol.Type),
	}
	var clients []any
	switch in.Protocol.Type {
	case model.ProtoVLESS:
		for _, u := range in.Users {
			c := map[string]any{"id": u.UUID, "email": u.ID, "level": u.Level}
			if in.Protocol.Flow != "" {
				c["flow"] = in.Protocol.Flow
			}
			clients = append(clients, c)
		}
		vs := map[string]any{"clients": nonNil(clients), "decryption": orDefault(in.Protocol.Decryption, "none")}
		if len(in.Protocol.TestSeed) == 4 {
			vs["testseed"] = in.Protocol.TestSeed
		}
		ib["settings"] = withFallbacks(vs, in)
	case model.ProtoVMess:
		for _, u := range in.Users {
			clients = append(clients, map[string]any{"id": u.UUID, "email": u.ID, "level": u.Level})
		}
		ib["settings"] = map[string]any{"clients": nonNil(clients)}
	case model.ProtoTrojan:
		for _, u := range in.Users {
			clients = append(clients, map[string]any{"password": u.Password, "email": u.ID, "level": u.Level})
		}
		ib["settings"] = withFallbacks(map[string]any{"clients": nonNil(clients)}, in)
	case model.ProtoShadowsocks:
		m := in.Protocol.Method
		s := map[string]any{"method": m, "network": "tcp,udp"}
		if model.SS2022KeyLen(m) > 0 {
			s["password"] = in.Protocol.ServerKey
			for _, u := range in.Users {
				clients = append(clients, map[string]any{"password": u.SSKey(m), "email": u.ID, "level": u.Level})
			}
		} else {
			for _, u := range in.Users {
				clients = append(clients, map[string]any{"method": m, "password": u.SSKey(m), "email": u.ID, "level": u.Level})
			}
		}
		s["clients"] = nonNil(clients)
		ib["settings"] = s
	case model.ProtoSOCKS, model.ProtoHTTP:
		var accts []any
		for _, u := range in.Users {
			accts = append(accts, map[string]any{"user": u.ID, "pass": u.Password})
		}
		s := map[string]any{"accounts": nonNil(accts)}
		if in.Protocol.Type == model.ProtoSOCKS {
			s["auth"] = "password"
			s["udp"] = in.Protocol.UDP
		}
		ib["settings"] = s
	default:
		return nil, fmt.Errorf("protocol %s not supported by xray", in.Protocol.Type)
	}

	stream, err := renderStream(in.Transport, in.Security, true, legacyKCP)
	if err != nil {
		return nil, err
	}
	if stream != nil {
		ib["streamSettings"] = stream
	}
	if in.Sniffing.Enabled {
		do := in.Sniffing.DestOverride
		if len(do) == 0 {
			do = []string{"http", "tls", "quic"}
		}
		sn := map[string]any{"enabled": true, "destOverride": do, "routeOnly": in.Sniffing.RouteOnly}
		if in.Sniffing.MetadataOnly {
			sn["metadataOnly"] = true
		}
		if len(in.Sniffing.IPsExcluded) > 0 {
			sn["ipsExcluded"] = in.Sniffing.IPsExcluded
		}
		if len(in.Sniffing.DomainsExcluded) > 0 {
			sn["domainsExcluded"] = in.Sniffing.DomainsExcluded
		}
		ib["sniffing"] = sn
	}
	if err := core.MergeExtra(ib, in.Extra); err != nil {
		return nil, fmt.Errorf("extra: %w", err)
	}
	return ib, nil
}

// renderStream builds streamSettings for an inbound (server=true) or outbound.
// legacyKCP reports whether mKCP header/seed are still accepted (removed in
// Xray 26 in favour of finalmask).
func (a *Adapter) legacyKCP() bool { return !core.VersionAtLeast(a.opt.Version, "26.0.0") }

func renderStream(t model.TransportSpec, sec model.SecuritySpec, server, legacyKCP bool) (map[string]any, error) {
	st := map[string]any{}
	switch t.Type {
	case "", model.TransTCP:
		st["network"] = "tcp"
		if t.HeaderType == "http" {
			// HTTP request/response camouflage; clients send the same header
			// (headerType=http in share links).
			req := map[string]any{"path": []string{orDefault(t.Path, "/")}}
			if t.Host != "" {
				req["headers"] = map[string]any{"Host": strings.Split(t.Host, ",")}
			}
			st["tcpSettings"] = map[string]any{"header": map[string]any{"type": "http", "request": req}}
		}
	case model.TransWS:
		st["network"] = "ws"
		st["wsSettings"] = pathHost(t)
	case model.TransHTTPUpgrade:
		st["network"] = "httpupgrade"
		st["httpupgradeSettings"] = pathHost(t)
	case model.TransXHTTP:
		st["network"] = "xhttp"
		x := pathHost(t)
		if t.Mode != "" {
			x["mode"] = t.Mode
		}
		st["xhttpSettings"] = x
	case model.TransGRPC:
		st["network"] = "grpc"
		st["grpcSettings"] = map[string]any{"serviceName": t.ServiceName}
	case model.TransKCP:
		st["network"] = "kcp"
		k := map[string]any{}
		if legacyKCP {
			if t.HeaderType != "" {
				k["header"] = map[string]any{"type": t.HeaderType}
			}
			if t.Seed != "" {
				k["seed"] = t.Seed
			}
		}
		st["kcpSettings"] = k
	default:
		return nil, fmt.Errorf("transport %s not supported by xray", t.Type)
	}
	if server && t.AcceptProxyProtocol {
		key := map[model.Transport]string{"": "tcpSettings", model.TransTCP: "tcpSettings", model.TransWS: "wsSettings",
			model.TransHTTPUpgrade: "httpupgradeSettings"}[t.Type]
		if key == "" {
			return nil, fmt.Errorf("proxy protocol is only supported on raw / ws / httpupgrade")
		}
		ts, _ := st[key].(map[string]any)
		if ts == nil {
			ts = map[string]any{}
		}
		ts["acceptProxyProtocol"] = true
		st[key] = ts
	}
	if server {
		switch t.Type {
		case model.TransWS, model.TransHTTPUpgrade, model.TransXHTTP, model.TransGRPC:
			trusted := t.TrustedForwardedFor
			if len(trusted) == 0 {
				// Xray treats an absent/empty list as trust ALL X-Forwarded-For.
				// A nonempty list containing an impossible HTTP header name
				// disables that fallback. Real-core tests cover this behavior.
				trusted = []string{""}
			}
			st["sockopt"] = map[string]any{"trustedXForwardedFor": trusted}
		}
	}
	switch sec.Type {
	case "", model.SecNone:
	case model.SecTLS:
		st["security"] = "tls"
		tls := map[string]any{}
		if sec.ServerName != "" {
			tls["serverName"] = sec.ServerName
		}
		if len(sec.ALPN) > 0 {
			tls["alpn"] = sec.ALPN
		}
		if server {
			if sec.CertPEM == "" || sec.KeyPEM == "" {
				return nil, fmt.Errorf("tls requires a certificate")
			}
			tls["certificates"] = []any{map[string]any{
				"certificate": core.PEMLines(sec.CertPEM),
				"key":         core.PEMLines(sec.KeyPEM),
			}}
		} else {
			if sec.Fingerprint != "" {
				tls["fingerprint"] = sec.Fingerprint
			}
			if sec.Insecure {
				tls["allowInsecure"] = true
			}
		}
		st["tlsSettings"] = tls
	case model.SecReality:
		st["security"] = "reality"
		if server {
			r := map[string]any{
				"show":        false,
				"dest":        sec.RealityDest,
				"xver":        0,
				"serverNames": sec.RealityServerNames,
				"privateKey":  sec.RealityPrivateKey,
				"shortIds":    nonNilStr(sec.RealityShortIDs),
			}
			if sec.RealityMaxTimeDiff > 0 {
				r["maxTimeDiff"] = sec.RealityMaxTimeDiff
			}
			if sec.RealityMinClientVer != "" {
				r["minClientVer"] = sec.RealityMinClientVer
			}
			if sec.RealityMaxClientVer != "" {
				r["maxClientVer"] = sec.RealityMaxClientVer
			}
			if sec.RealityMldsa65Seed != "" {
				r["mldsa65Seed"] = sec.RealityMldsa65Seed
			}
			if l := sec.RealityLimitUpload; l != nil && *l != (model.LimitFallback{}) {
				r["limitFallbackUpload"] = limitFallback(l)
			}
			if l := sec.RealityLimitDownload; l != nil && *l != (model.LimitFallback{}) {
				r["limitFallbackDownload"] = limitFallback(l)
			}
			st["realitySettings"] = r
		} else {
			r := map[string]any{
				"serverName":  sec.ServerName,
				"publicKey":   sec.RealityPublicKey,
				"fingerprint": orDefault(sec.Fingerprint, "chrome"),
			}
			if len(sec.RealityShortIDs) > 0 {
				r["shortId"] = sec.RealityShortIDs[0]
			}
			if sec.RealitySpiderX != "" {
				r["spiderX"] = sec.RealitySpiderX
			}
			if sec.RealityMldsa65Verify != "" {
				r["mldsa65Verify"] = sec.RealityMldsa65Verify
			}
			st["realitySettings"] = r
		}
	default:
		return nil, fmt.Errorf("security %s not supported", sec.Type)
	}
	return st, nil
}

func limitFallback(l *model.LimitFallback) map[string]any {
	return map[string]any{"afterBytes": l.AfterBytes, "bytesPerSec": l.BytesPerSec, "burstBytesPerSec": l.BurstBytesPerSec}
}

func pathHost(t model.TransportSpec) map[string]any {
	m := map[string]any{"path": orDefault(t.Path, "/")}
	if t.Host != "" {
		m["host"] = t.Host
	}
	return m
}

func renderOutbounds(obs []model.Outbound, legacyKCP bool) (outs []map[string]any, balancers map[string]map[string]any, observe map[string]any, err error) {
	outs = []map[string]any{
		{"tag": "direct", "protocol": "freedom"},
		{"tag": "block", "protocol": "blackhole"},
	}
	balancers = map[string]map[string]any{}
	var probe, burst []string
	probeURL := "https://www.gstatic.com/generate_204"
	probeInterval := time.Duration(0)
	for _, o := range obs {
		s, perr := o.ParseSettings()
		if perr != nil {
			return nil, nil, nil, fmt.Errorf("outbound %s: %w", o.Tag, perr)
		}
		var ob map[string]any
		switch o.Type {
		case model.OutDirect:
			ob = map[string]any{"protocol": "freedom"}
		case model.OutBlock:
			ob = map[string]any{"protocol": "blackhole"}
		case model.OutSOCKS, model.OutHTTP:
			srv := map[string]any{"address": s.Server, "port": s.Port}
			if s.Username != "" {
				srv["users"] = []any{map[string]any{"user": s.Username, "pass": s.Password}}
			}
			ob = map[string]any{"protocol": string(o.Type), "settings": map[string]any{"servers": []any{srv}}}
		case model.OutShadowsocks:
			ob = map[string]any{"protocol": "shadowsocks", "settings": map[string]any{"servers": []any{
				map[string]any{"address": s.Server, "port": s.Port, "method": s.Method, "password": s.Password}}}}
		case model.OutVLESS:
			u := map[string]any{"id": s.UUID, "encryption": orDefault(s.Encryption, "none")}
			if s.Flow != "" {
				u["flow"] = s.Flow
			}
			ob = map[string]any{"protocol": "vless", "settings": map[string]any{"vnext": []any{
				map[string]any{"address": s.Server, "port": s.Port, "users": []any{u}}}}}
		case model.OutVMess:
			ob = map[string]any{"protocol": "vmess", "settings": map[string]any{"vnext": []any{
				map[string]any{"address": s.Server, "port": s.Port, "users": []any{map[string]any{"id": s.UUID, "security": "auto"}}}}}}
		case model.OutTrojan:
			ob = map[string]any{"protocol": "trojan", "settings": map[string]any{"servers": []any{
				map[string]any{"address": s.Server, "port": s.Port, "password": s.Password}}}}
		case model.OutWireGuard:
			peer := map[string]any{"publicKey": s.PeerPublicKey, "endpoint": net.JoinHostPort(strings.Trim(s.Server, "[]"), strconv.Itoa(s.Port))}
			if s.PreSharedKey != "" {
				peer["preSharedKey"] = s.PreSharedKey
			}
			w := map[string]any{"secretKey": s.PrivateKey, "address": s.LocalAddress, "peers": []any{peer}}
			if len(s.Reserved) > 0 {
				w["reserved"] = s.Reserved
			}
			if s.MTU > 0 {
				w["mtu"] = s.MTU
			}
			ob = map[string]any{"protocol": "wireguard", "settings": w}
		case model.OutURLTest, model.OutSelector, model.OutBalancer:
			strategy := s.Strategy
			switch o.Type {
			case model.OutURLTest:
				strategy = model.LBLeastPing
			case model.OutSelector:
				strategy = model.LBRandom
				if len(s.Members) > 0 {
					// Manual choice: pin to the first member; per-group choice is
					// expressed through routing rules.
					s.Members = s.Members[:1]
				}
			}
			if strategy == "" || strategy == model.LBFallback {
				// Xray has no ordered fallback; leastPing over healthy members
				// plus fallbackTag gives the same failover behaviour.
				strategy = model.LBLeastPing
			}
			b := map[string]any{"tag": o.Tag, "selector": s.Members, "strategy": map[string]any{"type": strategy}}
			if s.Fallback != "" {
				b["fallbackTag"] = s.Fallback
			}
			balancers[o.Tag] = b
			switch strategy {
			case model.LBLeastPing:
				probe = append(probe, s.Members...)
			case model.LBLeastLoad:
				burst = append(burst, s.Members...)
			case model.LBRandom, model.LBRoundRobin:
				if s.Fallback != "" {
					probe = append(probe, s.Members...)
				}
			}
			if s.TestURL != "" {
				probeURL = s.TestURL
			}
			if d, e := time.ParseDuration(orDefault(s.Interval, "1m")); e == nil && d > 0 && (probeInterval == 0 || d < probeInterval) {
				probeInterval = d
			}
			continue
		default:
			return nil, nil, nil, fmt.Errorf("outbound %s: type %s not supported by xray", o.Tag, o.Type)
		}
		ob["tag"] = o.Tag
		if s.Transport != nil || s.Security != nil {
			t := model.TransportSpec{Type: model.TransTCP}
			if s.Transport != nil {
				t = *s.Transport
			}
			var sec model.SecuritySpec
			if s.Security != nil {
				sec = clientSec(*s.Security)
			}
			st, serr := renderStream(t, sec, false, legacyKCP)
			if serr != nil {
				return nil, nil, nil, fmt.Errorf("outbound %s: %w", o.Tag, serr)
			}
			ob["streamSettings"] = st
		}
		if o.Detour != "" {
			st, _ := ob["streamSettings"].(map[string]any)
			if st == nil {
				st = map[string]any{}
				ob["streamSettings"] = st
			}
			st["sockopt"] = map[string]any{"dialerProxy": o.Detour}
		}
		applyOutboundIP(ob, o)
		outs = append(outs, ob)
	}
	// Xray strategies consume one Observatory feature. Registering both types
	// makes some groups read a report that never probes their members. Burst
	// reports include the ordinary Alive/Delay fields, so share them when any
	// group needs leastLoad.
	if len(burst) > 0 {
		burst = append(burst, probe...)
		probe = nil
	}
	if len(probe) > 0 || len(burst) > 0 {
		observe = map[string]any{}
		if len(probe) > 0 {
			observe["observatory"] = map[string]any{"subjectSelector": dedupe(probe), "probeUrl": probeURL, "probeInterval": probeInterval.String()}
		}
		if len(burst) > 0 {
			observe["burstObservatory"] = map[string]any{"subjectSelector": dedupe(burst),
				"pingConfig": map[string]any{"destination": probeURL, "interval": probeInterval.String(), "sampling": 3, "timeout": "5s"}}
		}
	}
	return outs, balancers, observe, nil
}

func clientSec(c model.ClientTLS) model.SecuritySpec {
	s := model.SecuritySpec{Type: c.Type, ServerName: c.ServerName, ALPN: c.ALPN, Fingerprint: c.Fingerprint, Insecure: c.Insecure,
		RealityPublicKey: c.PublicKey, RealitySpiderX: c.SpiderX, RealityMldsa65Verify: c.Mldsa65Verify}
	if c.ShortID != "" {
		s.RealityShortIDs = []string{c.ShortID}
	}
	return s
}

// renderRules converts route rules (sorted by priority). A rule with an empty
// match selects the default outbound, which in Xray is the first outbound.
func renderRules(rules []model.RouteRule, balancers map[string]map[string]any) ([]any, string) {
	rs := append([]model.RouteRule(nil), rules...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Priority < rs[j].Priority })
	out := []any{map[string]any{"type": "field", "inboundTag": []string{"api"}, "outboundTag": "api"}}
	def := ""
	for _, r := range rs {
		m := r.Match
		x := map[string]any{"type": "field"}
		var domains, ips []string
		for _, d := range m.Domain {
			domains = append(domains, "full:"+d)
		}
		for _, d := range m.DomainSuffix {
			domains = append(domains, "domain:"+d)
		}
		for _, d := range m.DomainKeyword {
			domains = append(domains, "keyword:"+d)
		}
		for _, g := range m.Geosite {
			domains = append(domains, "geosite:"+g)
		}
		for _, g := range m.GeoIP {
			ips = append(ips, "geoip:"+g)
		}
		ips = append(ips, m.IPCIDR...)
		if len(domains) > 0 {
			x["domain"] = domains
		}
		if len(ips) > 0 {
			x["ip"] = ips
		}
		if len(m.Inbound) > 0 {
			x["inboundTag"] = m.Inbound
		}
		if len(m.Users) > 0 {
			x["user"] = m.Users
		}
		if len(m.Port) > 0 {
			x["port"] = strings.Join(m.Port, ",")
		}
		if m.Network != "" {
			x["network"] = m.Network
		}
		if len(x) == 1 {
			if len(m.UserGroups) > 0 {
				continue // group resolved to no users: rule matches nobody
			}
			def = r.Outbound
			continue
		}
		if _, ok := balancers[r.Outbound]; ok {
			x["balancerTag"] = r.Outbound
		} else {
			x["outboundTag"] = r.Outbound
		}
		out = append(out, x)
	}
	return out, def
}

func moveFirst(outs []map[string]any, tag string) []map[string]any {
	for i, o := range outs {
		if o["tag"] == tag {
			res := []map[string]any{o}
			res = append(res, outs[:i]...)
			return append(res, outs[i+1:]...)
		}
	}
	return outs
}

// Validate runs `xray run -test` on the config.
func (a *Adapter) Validate(cfg []byte) error {
	if a.opt.BinPath == "" {
		return fmt.Errorf("xray binary not configured")
	}
	f, err := core.WriteTemp(a.opt.WorkDir, "xray-check-*.json", cfg)
	if err != nil {
		return err
	}
	defer os.Remove(f)
	return core.RunCheck(a.opt.BinPath, "run", "-test", "-c", f)
}

func (a *Adapter) Start(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("xray binary not configured")
	}
	return a.proc.Start(ctx)
}

func (a *Adapter) Stop(ctx context.Context) error {
	if a.proc == nil {
		return nil
	}
	return a.proc.Stop(ctx)
}

// Reload restarts Xray: it has no in-place config reload.
func (a *Adapter) Reload(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("xray binary not configured")
	}
	return a.proc.Restart(ctx)
}

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

func (a *Adapter) inbound(tag string) (*model.Inbound, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == nil {
		return nil, fmt.Errorf("no spec rendered yet")
	}
	for i := range a.last.Inbounds {
		if a.last.Inbounds[i].Tag == tag {
			return &a.last.Inbounds[i], nil
		}
	}
	return nil, fmt.Errorf("inbound %s not found", tag)
}

// AddUsers adds users through HandlerService.AlterInbound without a restart.
func (a *Adapter) AddUsers(ctx context.Context, tag string, us []model.User) error {
	in, err := a.inbound(tag)
	if err != nil {
		return err
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	for _, u := range us {
		acct, err := account(in, u)
		if err != nil {
			return err
		}
		var user []byte
		user = core.PBVarint(user, 1, uint64(u.Level))
		user = core.PBString(user, 2, u.ID)
		user = core.PBBytes(user, 3, acct)
		op := core.PBBytes(nil, 1, user)
		if err := a.alter(ctx, c, tag, "xray.app.proxyman.command.AddUserOperation", op); err != nil {
			return fmt.Errorf("add user %s: %w", u.ID, err)
		}
	}
	return nil
}

// RemoveUsers removes users by email (= panel user id).
func (a *Adapter) RemoveUsers(ctx context.Context, tag string, ids []string) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	for _, id := range ids {
		op := core.PBString(nil, 1, id)
		if err := a.alter(ctx, c, tag, "xray.app.proxyman.command.RemoveUserOperation", op); err != nil {
			return fmt.Errorf("remove user %s: %w", id, err)
		}
	}
	return nil
}

func (a *Adapter) alter(ctx context.Context, c *core.APIClient, tag, opType string, op []byte) error {
	var req []byte
	req = core.PBString(req, 1, tag)
	req = core.PBBytes(req, 2, core.PBTyped(opType, op))
	_, err := c.Invoke(ctx, "/"+handlerService+"/AlterInbound", req)
	return err
}

// account encodes the protocol-specific Account TypedMessage for a user.
func account(in *model.Inbound, u model.User) ([]byte, error) {
	var b []byte
	switch in.Protocol.Type {
	case model.ProtoVLESS:
		b = core.PBString(b, 1, u.UUID)
		b = core.PBString(b, 2, in.Protocol.Flow)
		b = core.PBString(b, 3, "none")
		if len(in.Protocol.TestSeed) == 4 {
			var packed []byte
			for _, v := range in.Protocol.TestSeed {
				packed = protowire.AppendVarint(packed, uint64(v))
			}
			b = core.PBBytes(b, 9, packed) // repeated uint32 testseed
		}
		return core.PBTyped("xray.proxy.vless.Account", b), nil
	case model.ProtoVMess:
		b = core.PBString(b, 1, u.UUID)
		return core.PBTyped("xray.proxy.vmess.Account", b), nil
	case model.ProtoTrojan:
		b = core.PBString(b, 1, u.Password)
		return core.PBTyped("xray.proxy.trojan.Account", b), nil
	case model.ProtoShadowsocks:
		if model.SS2022KeyLen(in.Protocol.Method) == 0 {
			return nil, core.ErrNotSupported
		}
		b = core.PBString(b, 1, u.SSKey(in.Protocol.Method))
		return core.PBTyped("xray.proxy.shadowsocks_2022.Account", b), nil
	}
	return nil, core.ErrNotSupported
}

// CollectTraffic reads and resets per-user counters.
func (a *Adapter) CollectTraffic(ctx context.Context) ([]model.TrafficDelta, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	stats, err := c.QueryStats(ctx, statsService, "", true) // users, inbounds and outbounds
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	// Internal bridge counters are transport overhead, not public inbounds or
	// user egresses. Only suppress tags absent from the requested node spec.
	internal := map[string]bool{}
	for tag := range a.fallbacks {
		internal[tag] = true
	}
	if a.last != nil {
		for _, in := range a.last.Inbounds {
			delete(internal, in.Tag)
		}
		for _, ob := range a.last.Outbounds {
			delete(internal, ob.Tag)
		}
	}
	origins := a.egressOrigins
	a.mu.Unlock()
	var out []model.TrafficDelta
	for id, v := range core.EgressTraffic(core.AllTraffic(stats), origins) {
		if kind, tag, ok := strings.Cut(id, ">>>"); ok && (kind == "inbound" || kind == "outbound") && internal[tag] {
			continue
		}
		if v[0] == 0 && v[1] == 0 {
			continue
		}
		out = append(out, model.TrafficDelta{UserID: id, Up: v[0], Down: v[1]})
	}
	return out, nil
}

// OnlineIPs lists source IPs per user (requires statsUserOnline, Xray >= 25).
func (a *Adapter) OnlineIPs(ctx context.Context) (map[string][]string, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	resp, err := c.Invoke(ctx, "/"+statsService+"/GetAllOnlineUsers", nil)
	if err != nil {
		if core.IsUnimplemented(err) {
			return nil, core.ErrNotSupported
		}
		return nil, err
	}
	var names []string
	_ = core.PBRange(resp, func(n protoNum, t protoType, b []byte, _ uint64) {
		if n == 1 && t == bytesType {
			names = append(names, string(b))
		}
	})
	out := map[string][]string{}
	for _, name := range names {
		req := core.PBString(nil, 1, name)
		r, err := c.Invoke(ctx, "/"+statsService+"/GetStatsOnlineIpList", req)
		if err != nil {
			continue
		}
		user := strings.TrimSuffix(strings.TrimPrefix(name, "user>>>"), ">>>online")
		_ = core.PBRange(r, func(n protoNum, t protoType, b []byte, _ uint64) {
			if n != 2 || t != bytesType {
				return
			}
			_ = core.PBRange(b, func(n2 protoNum, t2 protoType, k []byte, _ uint64) {
				if n2 == 1 && t2 == bytesType {
					out[user] = append(out[user], string(k))
				}
			})
		})
	}
	return out, nil
}

func (a *Adapter) Health(ctx context.Context) model.CoreStatus {
	st := model.CoreStatus{Type: model.CoreXray, Version: a.opt.Version}
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
		st.Inbounds = len(a.last.InboundsFor(model.CoreXray))
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

func nonNil(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

func nonNilStr(v []string) []string {
	if v == nil {
		return []string{""}
	}
	return v
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// geodata renders the auto-update block (Xray 26.5.9+). Xray refuses
// assets whose file is missing, so only files present next to the binary
// are listed (the agent downloads missing ones before applying).
func (a *Adapter) geodata(g *model.GeodataSpec) map[string]any {
	if g == nil || g.Cron == "" || !core.VersionAtLeast(a.opt.Version, "26.5.9") {
		return nil
	}
	var assets []any
	for _, as := range g.Assets {
		if a.opt.BinPath != "" {
			if _, err := os.Stat(filepath.Join(filepath.Dir(a.opt.BinPath), as.File)); err != nil {
				continue
			}
		}
		assets = append(assets, map[string]any{"url": as.URL, "file": as.File})
	}
	if len(assets) == 0 {
		return nil
	}
	out := map[string]any{"cron": g.Cron, "assets": assets}
	if g.Outbound != "" {
		out["outbound"] = g.Outbound
	}
	return out
}
