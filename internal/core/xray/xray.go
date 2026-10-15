// Package xray adapts Xray-core. Users are hot-updated through the gRPC
// HandlerService and traffic is read (and reset) from StatsService.
package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

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

	mu   sync.Mutex
	last *model.NodeSpec // spec of the most recent Render, used by hot updates
	api  *core.APIClient
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
		a.proc = core.NewProcess("xray", o.BinPath, o.WorkDir, o.Logger, "run", "-c", a.configPath())
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
	return model.CapSet{
		Core:     model.CoreXray,
		Version:  v,
		Combos:   b.Combos(),
		Flows:    []string{"xtls-rprx-vision"},
		HotUsers: true,
		RealIP:   true,
		Outbounds: []model.OutboundType{model.OutDirect, model.OutBlock, model.OutSOCKS, model.OutHTTP,
			model.OutShadowsocks, model.OutVLESS, model.OutVMess, model.OutTrojan, model.OutWireGuard,
			model.OutURLTest, model.OutSelector},
	}
}

// Render translates the Xray-hosted part of a NodeSpec into Xray JSON.
func (a *Adapter) Render(spec *model.NodeSpec) ([]byte, error) {
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"api": map[string]any{
			"tag":      "api",
			"listen":   a.opt.APIListen,
			"services": []string{"HandlerService", "StatsService", "LoggerService"},
		},
		"stats": map[string]any{},
		"policy": map[string]any{
			"levels": map[string]any{"0": map[string]any{
				"statsUserUplink": true, "statsUserDownlink": true, "statsUserOnline": true,
			}},
			"system": map[string]any{"statsInboundUplink": true, "statsInboundDownlink": true},
		},
	}
	var inbounds []any
	for _, in := range spec.InboundsFor(model.CoreXray) {
		ib, err := renderInbound(in, a.legacyKCP())
		if err != nil {
			return nil, fmt.Errorf("inbound %s: %w", in.Tag, err)
		}
		inbounds = append(inbounds, ib)
	}
	cfg["inbounds"] = inbounds

	outs, balancers, observe, err := renderOutbounds(spec.Outbounds, a.legacyKCP())
	if err != nil {
		return nil, err
	}
	rules, defTag := renderRules(spec.Routes, balancers)
	if defTag != "" {
		outs = moveFirst(outs, defTag)
	}
	cfg["outbounds"] = outs
	routing := map[string]any{"domainStrategy": "AsIs", "rules": rules}
	if len(balancers) > 0 {
		var bl []any
		for _, b := range balancers {
			bl = append(bl, b)
		}
		routing["balancers"] = bl
	}
	cfg["routing"] = routing
	if observe != nil {
		cfg["observatory"] = observe
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

func renderInbound(in model.Inbound, legacyKCP bool) (map[string]any, error) {
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
		ib["settings"] = map[string]any{"clients": nonNil(clients), "decryption": "none"}
	case model.ProtoVMess:
		for _, u := range in.Users {
			clients = append(clients, map[string]any{"id": u.UUID, "email": u.ID, "level": u.Level})
		}
		ib["settings"] = map[string]any{"clients": nonNil(clients)}
	case model.ProtoTrojan:
		for _, u := range in.Users {
			clients = append(clients, map[string]any{"password": u.Password, "email": u.ID, "level": u.Level})
		}
		ib["settings"] = map[string]any{"clients": nonNil(clients)}
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
		ib["sniffing"] = map[string]any{"enabled": true, "destOverride": do, "routeOnly": in.Sniffing.RouteOnly}
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
			st["realitySettings"] = map[string]any{
				"show":        false,
				"dest":        sec.RealityDest,
				"xver":        0,
				"serverNames": sec.RealityServerNames,
				"privateKey":  sec.RealityPrivateKey,
				"shortIds":    nonNilStr(sec.RealityShortIDs),
			}
		} else {
			r := map[string]any{
				"serverName":  sec.ServerName,
				"publicKey":   sec.RealityPublicKey,
				"fingerprint": orDefault(sec.Fingerprint, "chrome"),
			}
			if len(sec.RealityShortIDs) > 0 {
				r["shortId"] = sec.RealityShortIDs[0]
			}
			st["realitySettings"] = r
		}
	default:
		return nil, fmt.Errorf("security %s not supported", sec.Type)
	}
	return st, nil
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
	var probe []string
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
			u := map[string]any{"id": s.UUID, "encryption": "none"}
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
			peer := map[string]any{"publicKey": s.PeerPublicKey, "endpoint": fmt.Sprintf("%s:%d", s.Server, s.Port)}
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
		case model.OutURLTest, model.OutSelector:
			strategy := "leastPing"
			if o.Type == model.OutSelector {
				strategy = "random"
				if len(s.Members) > 0 {
					// Manual choice: pin to the first member; per-group choice is
					// expressed through routing rules.
					s.Members = s.Members[:1]
				}
			}
			balancers[o.Tag] = map[string]any{"tag": o.Tag, "selector": s.Members, "strategy": map[string]any{"type": strategy}}
			if o.Type == model.OutURLTest {
				probe = append(probe, s.Members...)
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
		outs = append(outs, ob)
	}
	if len(probe) > 0 {
		observe = map[string]any{"subjectSelector": dedupe(probe), "probeUrl": "https://www.gstatic.com/generate_204", "probeInterval": "60s"}
	}
	return outs, balancers, observe, nil
}

func clientSec(c model.ClientTLS) model.SecuritySpec {
	s := model.SecuritySpec{Type: c.Type, ServerName: c.ServerName, ALPN: c.ALPN, Fingerprint: c.Fingerprint, Insecure: c.Insecure,
		RealityPublicKey: c.PublicKey}
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
	stats, err := c.QueryStats(ctx, statsService, "user>>>", true)
	if err != nil {
		return nil, err
	}
	var out []model.TrafficDelta
	for id, v := range core.UserTraffic(stats) {
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
