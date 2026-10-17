// Package mihomo adapts mihomo (Clash.Meta) as a server core. Inbounds become
// `listeners`, outbounds and routing become proxies / proxy-groups / rules.
// mihomo has no user API, so user changes reload the config through the
// external controller; traffic is derived from /connections (per inbound user).
package mihomo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func init() {
	core.Register(model.CoreMihomo, func(o core.Options) core.CoreAdapter { return New(o) })
}

// Adapter implements core.CoreAdapter for mihomo.
type Adapter struct {
	opt    core.Options
	proc   *core.Process
	secret string

	mu    sync.Mutex
	last  *model.NodeSpec
	conns map[string][2]int64 // connection id -> last seen up/down
}

func New(o core.Options) *Adapter {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.APIListen == "" {
		o.APIListen = "127.0.0.1:10088"
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	a := &Adapter{opt: o, secret: hex.EncodeToString(b), conns: map[string][2]int64{}}
	if o.BinPath != "" {
		a.proc = core.NewProcess("mihomo", o.BinPath, o.WorkDir, o.Logger, "-d", o.WorkDir, "-f", a.configPath())
	}
	return a
}

func (a *Adapter) Type() model.CoreType { return model.CoreMihomo }

func (a *Adapter) configPath() string { return filepath.Join(a.opt.WorkDir, "mihomo.yaml") }

func (a *Adapter) Capabilities(v string) model.CapSet {
	b := &model.CapBuilder{}
	b.Add([]model.Protocol{model.ProtoVLESS, model.ProtoVMess}, []model.Transport{model.TransTCP, model.TransWS, model.TransGRPC}, []model.Security{model.SecNone, model.SecTLS})
	b.Add([]model.Protocol{model.ProtoTrojan}, []model.Transport{model.TransTCP, model.TransWS, model.TransGRPC}, []model.Security{model.SecTLS})
	b.Add([]model.Protocol{model.ProtoVLESS, model.ProtoTrojan}, []model.Transport{model.TransTCP}, []model.Security{model.SecReality})
	if core.VersionAtLeast(v, "1.19.12") {
		b.Add([]model.Protocol{model.ProtoVLESS}, []model.Transport{model.TransXHTTP}, []model.Security{model.SecNone, model.SecTLS, model.SecReality})
	}
	b.Add([]model.Protocol{model.ProtoShadowsocks, model.ProtoSOCKS, model.ProtoHTTP}, []model.Transport{model.TransTCP}, []model.Security{model.SecNone})
	b.Add([]model.Protocol{model.ProtoHysteria2, model.ProtoTUIC}, []model.Transport{model.TransQUIC}, []model.Security{model.SecTLS})
	b.Add([]model.Protocol{model.ProtoAnyTLS}, []model.Transport{model.TransTCP}, []model.Security{model.SecTLS})
	return model.CapSet{
		Core: model.CoreMihomo, Version: v, Combos: b.Combos(), Flows: []string{"xtls-rprx-vision"},
		Outbounds: []model.OutboundType{model.OutDirect, model.OutBlock, model.OutSOCKS, model.OutHTTP, model.OutShadowsocks,
			model.OutVLESS, model.OutVMess, model.OutTrojan, model.OutHysteria2, model.OutWireGuard, model.OutURLTest, model.OutSelector, model.OutBalancer},
		Balancers: []string{model.LBLeastPing, model.LBRoundRobin, model.LBFallback, model.LBRandom},
	}
}

// Render produces the mihomo YAML.
func (a *Adapter) Render(spec *model.NodeSpec) ([]byte, error) {
	cfg := map[string]any{
		"mode": "rule", "log-level": "warning", "allow-lan": true, "ipv6": true,
		"external-controller": a.opt.APIListen, "secret": a.secret,
		"profile": map[string]any{"store-selected": false},
	}
	var listeners []any
	for _, in := range spec.InboundsFor(model.CoreMihomo) {
		l, err := renderListener(in)
		if err != nil {
			return nil, fmt.Errorf("inbound %s: %w", in.Tag, err)
		}
		listeners = append(listeners, l)
	}
	cfg["listeners"] = listeners
	proxies, groups, err := renderOutbounds(spec.Outbounds)
	if err != nil {
		return nil, err
	}
	if len(proxies) > 0 {
		cfg["proxies"] = proxies
	}
	if len(groups) > 0 {
		cfg["proxy-groups"] = groups
	}
	resolve := false
	if d := spec.DNS; d != nil {
		dns, hosts, err := renderDNS(d)
		if err != nil {
			return nil, err
		}
		cfg["dns"] = dns
		if hosts != nil {
			cfg["hosts"] = hosts
		}
		resolve = d.DomainStrategy == "IPIfNonMatch" || d.DomainStrategy == "IPOnDemand"
	}
	cfg["rules"] = renderRules(spec.Routes, resolve)
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.last = spec.Clone()
	a.mu.Unlock()
	return out, nil
}

func renderListener(in model.Inbound) (map[string]any, error) {
	l := map[string]any{"name": in.Tag, "listen": listenAddr(in.Listen), "port": in.Port}
	us := in.Users
	switch in.Protocol.Type {
	case model.ProtoVLESS:
		l["type"] = "vless"
		var users []any
		for _, u := range us {
			x := map[string]any{"username": u.ID, "uuid": u.UUID}
			if in.Protocol.Flow != "" {
				x["flow"] = in.Protocol.Flow
			}
			users = append(users, x)
		}
		l["users"] = nonNil(users)
	case model.ProtoVMess:
		l["type"] = "vmess"
		var users []any
		for _, u := range us {
			users = append(users, map[string]any{"username": u.ID, "uuid": u.UUID, "alterId": 0})
		}
		l["users"] = nonNil(users)
	case model.ProtoTrojan:
		l["type"] = "trojan"
		var users []any
		for _, u := range us {
			users = append(users, map[string]any{"username": u.ID, "password": u.Password})
		}
		l["users"] = nonNil(users)
	case model.ProtoAnyTLS:
		l["type"] = "anytls"
		m := map[string]string{}
		for _, u := range us {
			m[u.ID] = u.Password
		}
		l["users"] = m
	case model.ProtoHysteria2:
		l["type"] = "hysteria2"
		m := map[string]string{}
		for _, u := range us {
			m[u.ID] = u.Password
		}
		l["users"] = m
		if in.Protocol.UpMbps > 0 {
			l["up"] = in.Protocol.UpMbps
		}
		if in.Protocol.DownMbps > 0 {
			l["down"] = in.Protocol.DownMbps
		}
		if in.Protocol.Obfs != "" {
			l["obfs"], l["obfs-password"] = in.Protocol.Obfs, in.Protocol.ObfsPassword
		}
	case model.ProtoTUIC:
		l["type"] = "tuic"
		m := map[string]string{}
		for _, u := range us {
			m[u.UUID] = u.Password
		}
		l["users"] = m
		if in.Protocol.CongestionControl != "" {
			l["congestion-controller"] = in.Protocol.CongestionControl
		}
	case model.ProtoShadowsocks:
		// mihomo's shadowsocks listener is single-user: the 2022 server key
		// authenticates everyone, so per-user accounting is not possible.
		l["type"], l["cipher"], l["password"] = "shadowsocks", in.Protocol.Method, in.Protocol.ServerKey
	case model.ProtoSOCKS, model.ProtoHTTP:
		l["type"] = map[model.Protocol]string{model.ProtoSOCKS: "socks", model.ProtoHTTP: "http"}[in.Protocol.Type]
		var users []any
		for _, u := range us {
			users = append(users, map[string]any{"username": u.ID, "password": u.Password})
		}
		l["users"] = nonNil(users)
	default:
		return nil, fmt.Errorf("protocol %s not supported by mihomo", in.Protocol.Type)
	}
	switch in.Transport.Type {
	case model.TransWS:
		l["ws-path"] = orDefault(in.Transport.Path, "/")
	case model.TransGRPC:
		l["grpc-service-name"] = in.Transport.ServiceName
	case model.TransXHTTP:
		x := map[string]any{"path": orDefault(in.Transport.Path, "/")}
		if in.Transport.Host != "" {
			x["host"] = in.Transport.Host
		}
		if in.Transport.Mode != "" {
			x["mode"] = in.Transport.Mode
		}
		l["xhttp-config"] = x
	}
	switch in.Security.Type {
	case model.SecTLS:
		if in.Security.CertPEM == "" {
			return nil, fmt.Errorf("tls requires a certificate")
		}
		l["certificate"], l["private-key"] = in.Security.CertPEM, in.Security.KeyPEM
	case model.SecReality:
		l["reality-config"] = map[string]any{"dest": in.Security.RealityDest, "private-key": in.Security.RealityPrivateKey,
			"short-id": in.Security.RealityShortIDs, "server-names": in.Security.RealityServerNames}
	}
	if err := core.MergeExtra(l, in.Extra); err != nil {
		return nil, err
	}
	return l, nil
}

func listenAddr(s string) string {
	if s == "" || s == "::" {
		return "0.0.0.0"
	}
	return s
}

// renderOutbounds reuses the subscription renderer's Clash proxy mapping.
func renderOutbounds(obs []model.Outbound) ([]any, []any, error) {
	var proxies, groups []any
	for _, o := range obs {
		s, err := o.ParseSettings()
		if err != nil {
			return nil, nil, fmt.Errorf("outbound %s: %w", o.Tag, err)
		}
		switch o.Type {
		case model.OutDirect:
			groups = append(groups, map[string]any{"name": o.Tag, "type": "select", "proxies": []string{"DIRECT"}})
			continue
		case model.OutBlock:
			groups = append(groups, map[string]any{"name": o.Tag, "type": "select", "proxies": []string{"REJECT"}})
			continue
		case model.OutURLTest, model.OutSelector, model.OutBalancer:
			g := map[string]any{"name": o.Tag, "proxies": mapTags(s.Members), "url": orDefault(s.TestURL, "https://www.gstatic.com/generate_204"), "interval": 300}
			strategy := s.Strategy
			switch o.Type {
			case model.OutURLTest:
				strategy = model.LBLeastPing
			case model.OutSelector:
				strategy = "select"
			}
			switch strategy {
			case "select":
				g["type"] = "select"
			case model.LBFallback:
				g["type"] = "fallback"
			case model.LBRoundRobin:
				g["type"], g["strategy"] = "load-balance", "round-robin"
			case model.LBRandom:
				g["type"], g["strategy"] = "load-balance", "consistent-hashing"
			default:
				g["type"] = "url-test"
				if s.Tolerance > 0 {
					g["tolerance"] = s.Tolerance
				}
			}
			groups = append(groups, g)
			continue
		case model.OutWireGuard:
			p := map[string]any{"name": o.Tag, "type": "wireguard", "server": s.Server, "port": s.Port, "private-key": s.PrivateKey,
				"public-key": s.PeerPublicKey, "udp": true}
			for _, a := range s.LocalAddress {
				if strings.Contains(a, ":") {
					p["ipv6"] = strings.SplitN(a, "/", 2)[0]
				} else {
					p["ip"] = strings.SplitN(a, "/", 2)[0]
				}
			}
			if s.PreSharedKey != "" {
				p["pre-shared-key"] = s.PreSharedKey
			}
			if len(s.Reserved) > 0 {
				p["reserved"] = s.Reserved
			}
			if o.Detour != "" {
				p["dialer-proxy"] = mapTag(o.Detour)
			}
			proxies = append(proxies, p)
			continue
		}
		n := sub.Node{Name: o.Tag, Server: s.Server, Port: s.Port, UUID: s.UUID, Password: s.Password, Username: s.Username,
			Method: s.Method, Flow: s.Flow, UpMbps: s.UpMbps, DownMbps: s.DownMbps, ObfsPassword: s.ObfsPassword}
		switch o.Type {
		case model.OutSOCKS:
			n.Protocol = model.ProtoSOCKS
		case model.OutHTTP:
			n.Protocol = model.ProtoHTTP
		case model.OutShadowsocks:
			n.Protocol = model.ProtoShadowsocks
		case model.OutVLESS:
			n.Protocol = model.ProtoVLESS
		case model.OutVMess:
			n.Protocol = model.ProtoVMess
		case model.OutTrojan:
			n.Protocol = model.ProtoTrojan
		case model.OutHysteria2:
			n.Protocol = model.ProtoHysteria2
		default:
			return nil, nil, fmt.Errorf("outbound %s: type %s not supported by mihomo", o.Tag, o.Type)
		}
		if s.Transport != nil {
			n.Transport = *s.Transport
		}
		if s.Security != nil {
			n.TLS = *s.Security
		}
		p := sub.ClashProxy(n)
		if p == nil {
			return nil, nil, fmt.Errorf("outbound %s cannot be expressed in mihomo", o.Tag)
		}
		if o.Detour != "" {
			p["dialer-proxy"] = mapTag(o.Detour)
		}
		proxies = append(proxies, p)
	}
	return proxies, groups, nil
}

func mapTag(t string) string {
	switch t {
	case "direct":
		return "DIRECT"
	case "block":
		return "REJECT"
	}
	return t
}

func mapTags(ts []string) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = mapTag(t)
	}
	return out
}

// renderRules converts rules to mihomo syntax; several values of one kind are
// ORed, several kinds are ANDed. Unless resolve is set, IP rules don't look
// up domain destinations (no-resolve).
func renderRules(rules []model.RouteRule, resolve bool) []string {
	noResolve := ",no-resolve"
	if resolve {
		noResolve = ""
	}
	rs := append([]model.RouteRule(nil), rules...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Priority < rs[j].Priority })
	var out []string
	final := "DIRECT"
	for _, r := range rs {
		m := r.Match
		var groups [][]string
		add := func(kind string, vals []string, suffix string) {
			if len(vals) == 0 {
				return
			}
			var g []string
			for _, v := range vals {
				g = append(g, kind+","+v+suffix)
			}
			groups = append(groups, g)
		}
		add("IN-NAME", m.Inbound, "")
		add("IN-USER", m.Users, "")
		add("DOMAIN", m.Domain, "")
		add("DOMAIN-SUFFIX", m.DomainSuffix, "")
		add("DOMAIN-KEYWORD", m.DomainKeyword, "")
		add("GEOSITE", m.Geosite, "")
		var geoip []string
		for _, g := range m.GeoIP {
			if g == "private" {
				geoip = append(geoip, "LAN")
			} else {
				geoip = append(geoip, strings.ToUpper(g))
			}
		}
		add("GEOIP", geoip, noResolve)
		add("IP-CIDR", m.IPCIDR, noResolve)
		add("DST-PORT", m.Port, "")
		if m.Network != "" {
			add("NETWORK", strings.Split(m.Network, ","), "")
		}
		target := mapTag(r.Outbound)
		switch {
		case len(groups) == 0:
			if len(m.UserGroups) == 0 {
				final = target
			}
		case len(groups) == 1:
			for _, c := range groups[0] {
				out = append(out, c+","+target)
			}
		default:
			var parts []string
			for _, g := range groups {
				if len(g) == 1 {
					parts = append(parts, "("+g[0]+")")
				} else {
					var or []string
					for _, c := range g {
						or = append(or, "("+c+")")
					}
					parts = append(parts, "(OR,("+strings.Join(or, ",")+"))")
				}
			}
			out = append(out, "AND,("+strings.Join(parts, ",")+"),"+target)
		}
	}
	return append(out, "MATCH,"+final)
}

// Validate runs `mihomo -t`.
func (a *Adapter) Validate(cfg []byte) error {
	if a.opt.BinPath == "" {
		return fmt.Errorf("mihomo binary not configured")
	}
	f, err := core.WriteTemp(a.opt.WorkDir, "mihomo-check-*.yaml", cfg)
	if err != nil {
		return err
	}
	defer os.Remove(f)
	return core.RunCheck(a.opt.BinPath, "-t", "-d", filepath.Dir(f), "-f", f)
}

func (a *Adapter) Start(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("mihomo binary not configured")
	}
	return a.proc.Start(ctx)
}

func (a *Adapter) Stop(ctx context.Context) error {
	if a.proc == nil {
		return nil
	}
	return a.proc.Stop(ctx)
}

func (a *Adapter) api(ctx context.Context, method, path string, body any, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+a.opt.APIListen+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mihomo api %s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Reload reapplies the config file through the controller, restarting when
// the controller is unreachable (or its secret changed with the new file).
func (a *Adapter) Reload(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("mihomo binary not configured")
	}
	if !a.proc.Running() {
		return a.proc.Start(ctx)
	}
	if err := a.api(ctx, http.MethodPut, "/configs?force=true", map[string]string{"path": a.configPath()}, nil); err != nil {
		return a.proc.Restart(ctx)
	}
	return nil
}

func (a *Adapter) AddUsers(context.Context, string, []model.User) error { return core.ErrNotSupported }
func (a *Adapter) RemoveUsers(context.Context, string, []string) error  { return core.ErrNotSupported }

type connection struct {
	ID       string `json:"id"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
	Metadata struct {
		InboundUser string `json:"inboundUser"`
		InboundName string `json:"inboundName"`
		SourceIP    string `json:"sourceIP"`
	} `json:"metadata"`
}

func (a *Adapter) connections(ctx context.Context) ([]connection, error) {
	var resp struct {
		Connections []connection `json:"connections"`
	}
	err := a.api(ctx, http.MethodGet, "/connections", nil, &resp)
	return resp.Connections, err
}

// CollectTraffic diffs per-connection byte counters by inbound user. Bytes
// a connection moves between its last poll and closing are not counted.
func (a *Adapter) CollectTraffic(ctx context.Context) ([]model.TrafficDelta, error) {
	conns, err := a.connections(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	per := map[string][2]int64{}
	seen := map[string][2]int64{}
	for _, c := range conns {
		prev := a.conns[c.ID]
		seen[c.ID] = [2]int64{c.Upload, c.Download}
		if c.Metadata.InboundUser == "" {
			continue
		}
		v := per[c.Metadata.InboundUser]
		v[0] += c.Upload - prev[0]
		v[1] += c.Download - prev[1]
		per[c.Metadata.InboundUser] = v
	}
	a.conns = seen
	var out []model.TrafficDelta
	for id, v := range per {
		out = append(out, model.TrafficDelta{UserID: id, Up: v[0], Down: v[1]})
	}
	return out, nil
}

func (a *Adapter) OnlineIPs(ctx context.Context) (map[string][]string, error) {
	conns, err := a.connections(ctx)
	if err != nil {
		return nil, err
	}
	set := map[string]map[string]bool{}
	for _, c := range conns {
		if u := c.Metadata.InboundUser; u != "" && c.Metadata.SourceIP != "" {
			if set[u] == nil {
				set[u] = map[string]bool{}
			}
			set[u][c.Metadata.SourceIP] = true
		}
	}
	out := map[string][]string{}
	for u, ips := range set {
		for ip := range ips {
			out[u] = append(out[u], ip)
		}
	}
	return out, nil
}

func (a *Adapter) Health(context.Context) model.CoreStatus {
	st := model.CoreStatus{Type: model.CoreMihomo, Version: a.opt.Version}
	if a.proc != nil {
		st.Running, st.PID, st.StartedAt = a.proc.Running(), a.proc.PID(), a.proc.StartedAt()
		if err := a.proc.LastError(); err != nil && !st.Running {
			st.Error = err.Error()
		}
	}
	a.mu.Lock()
	if a.last != nil {
		st.Inbounds = len(a.last.InboundsFor(model.CoreMihomo))
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

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
