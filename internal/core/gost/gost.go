// Package gost runs port forwards with GOST v3: load balancing with native
// failover, relay tunnels (ws / wss / tls / mwss / mtls / kcp / quic / grpc)
// carrying TCP and UDP, rate and connection limits, and per-rule traffic
// counters from its Prometheus metrics.
package gost

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() {
	core.Register(model.CoreGost, func(o core.Options) core.CoreAdapter { return New(o) })
}

type Adapter struct {
	opt  core.Options
	proc *core.Process
	mu   sync.Mutex
	n    int
	last map[string][2]int64 // cumulative counters per rule tag at the last collection
}

func New(o core.Options) *Adapter {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	a := &Adapter{opt: o, last: map[string][2]int64{}}
	if o.BinPath != "" {
		a.proc = core.NewProcess("gost", o.BinPath, o.WorkDir, o.Logger, "-C", filepath.Join(o.WorkDir, "gost.yaml"))
	}
	return a
}

func (a *Adapter) Type() model.CoreType { return model.CoreGost }

func (a *Adapter) Capabilities(v string) model.CapSet {
	return model.CapSet{Core: model.CoreGost, Version: v}
}

type node struct {
	Name      string         `yaml:"name"`
	Addr      string         `yaml:"addr"`
	Connector *typed         `yaml:"connector,omitempty"`
	Dialer    *typed         `yaml:"dialer,omitempty"`
	Metadata  map[string]any `yaml:"metadata,omitempty"`
}

type typed struct {
	Type     string         `yaml:"type"`
	Chain    string         `yaml:"chain,omitempty"`
	Auth     *auth          `yaml:"auth,omitempty"`
	TLS      map[string]any `yaml:"tls,omitempty"`
	Metadata map[string]any `yaml:"metadata,omitempty"`
}

type auth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type selector struct {
	Strategy    string `yaml:"strategy"`
	MaxFails    int    `yaml:"maxFails,omitempty"`
	FailTimeout string `yaml:"failTimeout,omitempty"`
}

type forwarder struct {
	Nodes    []node    `yaml:"nodes"`
	Selector *selector `yaml:"selector,omitempty"`
}

type service struct {
	Name      string         `yaml:"name"`
	Addr      string         `yaml:"addr"`
	Limiter   string         `yaml:"limiter,omitempty"`
	CLimiter  string         `yaml:"climiter,omitempty"`
	Handler   typed          `yaml:"handler"`
	Listener  typed          `yaml:"listener"`
	Forwarder *forwarder     `yaml:"forwarder,omitempty"`
	Metadata  map[string]any `yaml:"metadata,omitempty"`
}

type hop struct {
	Name     string    `yaml:"name"`
	Selector *selector `yaml:"selector,omitempty"`
	Nodes    []node    `yaml:"nodes"`
}

type chain struct {
	Name string `yaml:"name"`
	Hops []hop  `yaml:"hops"`
}

type limiter struct {
	Name   string   `yaml:"name"`
	Limits []string `yaml:"limits"`
}

type config struct {
	Services  []service      `yaml:"services"`
	Chains    []chain        `yaml:"chains,omitempty"`
	Limiters  []limiter      `yaml:"limiters,omitempty"`
	CLimiters []limiter      `yaml:"climiters,omitempty"`
	Metrics   map[string]any `yaml:"metrics,omitempty"`
	Log       map[string]any `yaml:"log"`
}

// Tunnel types GOST understands as listener / dialer.
var tunnelTypes = map[string]bool{"ws": true, "wss": true, "tls": true, "mwss": true, "mtls": true, "kcp": true, "quic": true, "grpc": true}

func strategy(balance string) string {
	switch balance {
	case "roundrobin":
		return "round"
	case "iphash":
		return "hash"
	}
	return "fifo" // primary first, the others take over when it fails
}

func tunnelMeta(f *model.ForwardRule) map[string]any {
	m := map[string]any{}
	switch f.TunnelType {
	case "ws", "wss", "mwss":
		if f.TunnelPath != "" {
			m["path"] = f.TunnelPath
		}
		if f.TunnelHost != "" {
			m["host"] = f.TunnelHost
		}
	case "grpc":
		if f.TunnelPath != "" {
			m["path"] = f.TunnelPath
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// Render writes the GOST config for the spec's gost forward rules.
func (a *Adapter) Render(spec *model.NodeSpec) ([]byte, error) {
	cfg := config{Log: map[string]any{"level": "warn"}}
	n := 0
	for _, fr := range spec.Forwards {
		if fr.Engine != model.EngineGost {
			continue
		}
		f := fr
		if f.Listen <= 0 || f.Target == "" {
			return nil, fmt.Errorf("forward %s: listen and target are required", f.Tag)
		}
		if f.TunnelRole != "" && !tunnelTypes[f.TunnelType] {
			return nil, fmt.Errorf("forward %s: unknown tunnel type %q", f.Tag, f.TunnelType)
		}
		n++
		host := f.ListenIP
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			host = "[" + host + "]"
		}
		addr := host + ":" + strconv.Itoa(f.Listen)
		targets := append([]string{f.Target}, f.ExtraTargets...)
		sel := &selector{Strategy: strategy(f.Balance)}
		if f.Failover || len(targets) > 1 {
			sel.MaxFails, sel.FailTimeout = 1, "30s"
		}
		var nodes []node
		for i, t := range targets {
			nodes = append(nodes, node{Name: fmt.Sprintf("t%d", i), Addr: t})
		}
		var lim, clim string
		if f.RateIn > 0 || f.RateOut > 0 {
			lim = "lim-" + f.Tag
			rate := func(v int) string {
				if v <= 0 {
					return "0"
				}
				return strconv.Itoa(v) + "KB"
			}
			cfg.Limiters = append(cfg.Limiters, limiter{Name: lim, Limits: []string{"$ " + rate(f.RateIn) + " " + rate(f.RateOut)}})
		}
		if f.MaxConns > 0 {
			clim = "clim-" + f.Tag
			cfg.CLimiters = append(cfg.CLimiters, limiter{Name: clim, Limits: []string{"$ " + strconv.Itoa(f.MaxConns)}})
		}
		var meta map[string]any
		if f.AcceptProxy {
			meta = map[string]any{"proxyProtocol": 1}
		}
		var ath *auth
		if f.TunnelAuth != "" {
			ath = &auth{Username: "polaris", Password: f.TunnelAuth}
		}
		switch f.TunnelRole {
		case "server":
			// The relay pins the targets, whatever the client asks for, and
			// carries both TCP and UDP.
			cfg.Services = append(cfg.Services, service{Name: f.Tag, Addr: addr, Limiter: lim, CLimiter: clim, Metadata: meta,
				Handler:   typed{Type: "relay", Auth: ath},
				Listener:  typed{Type: f.TunnelType, Metadata: tunnelMeta(&f)},
				Forwarder: &forwarder{Nodes: nodes, Selector: sel}})
		case "client":
			ch := "chain-" + f.Tag
			var hops []node
			for i, t := range targets {
				d := &typed{Type: f.TunnelType, Metadata: tunnelMeta(&f)}
				if f.TunnelSNI != "" {
					d.TLS = map[string]any{"serverName": f.TunnelSNI}
				}
				hops = append(hops, node{Name: fmt.Sprintf("r%d", i), Addr: t, Connector: &typed{Type: "relay", Auth: ath}, Dialer: d})
			}
			cfg.Chains = append(cfg.Chains, chain{Name: ch, Hops: []hop{{Name: "h0", Selector: sel, Nodes: hops}}})
			// The landing relay decides the final target; this one is a placeholder.
			fwd := &forwarder{Nodes: []node{{Name: "t0", Addr: "127.0.0.1:1"}}}
			for _, nw := range networks(f.Network) {
				cfg.Services = append(cfg.Services, service{Name: svcName(f.Tag, nw), Addr: addr, Limiter: lim, CLimiter: clim, Metadata: meta,
					Handler: typed{Type: nw, Chain: ch}, Listener: typed{Type: nw}, Forwarder: fwd})
			}
		default:
			for _, nw := range networks(f.Network) {
				cfg.Services = append(cfg.Services, service{Name: svcName(f.Tag, nw), Addr: addr, Limiter: lim, CLimiter: clim, Metadata: meta,
					Handler: typed{Type: nw}, Listener: typed{Type: nw}, Forwarder: &forwarder{Nodes: nodes, Selector: sel}})
			}
		}
	}
	if a.opt.APIListen != "" {
		cfg.Metrics = map[string]any{"addr": a.opt.APIListen, "path": "/metrics"}
	}
	a.mu.Lock()
	a.n = n
	a.mu.Unlock()
	if cfg.Services == nil {
		cfg.Services = []service{}
	}
	return yaml.Marshal(cfg)
}

func networks(n string) []string {
	switch n {
	case "tcp":
		return []string{"tcp"}
	case "udp":
		return []string{"udp"}
	}
	return []string{"tcp", "udp"}
}

// svcName keeps the rule tag for TCP and adds a suffix for UDP; traffic is
// summed per tag.
func svcName(tag, nw string) string {
	if nw == "udp" {
		return tag + "~udp"
	}
	return tag
}

func (a *Adapter) Validate(cfg []byte) error {
	if len(cfg) == 0 {
		return fmt.Errorf("empty config")
	}
	var c config
	return yaml.Unmarshal(cfg, &c)
}

func (a *Adapter) Start(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("gost binary not configured")
	}
	a.mu.Lock()
	a.last = map[string][2]int64{} // counters restart with the process
	a.mu.Unlock()
	return a.proc.Start(ctx)
}

func (a *Adapter) Stop(ctx context.Context) error {
	if a.proc == nil {
		return nil
	}
	return a.proc.Stop(ctx)
}

func (a *Adapter) Reload(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("gost binary not configured")
	}
	a.mu.Lock()
	a.last = map[string][2]int64{}
	a.mu.Unlock()
	return a.proc.Restart(ctx)
}

func (a *Adapter) AddUsers(context.Context, string, []model.User) error { return core.ErrNotSupported }
func (a *Adapter) RemoveUsers(context.Context, string, []string) error  { return core.ErrNotSupported }

// CollectTraffic turns GOST's cumulative per-service byte counters into
// per-rule deltas ("inbound>>>tag", like inbound tag counters).
func (a *Adapter) CollectTraffic(ctx context.Context) ([]model.TrafficDelta, error) {
	if a.opt.APIListen == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.opt.APIListen+"/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	cur := parseMetrics(resp.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []model.TrafficDelta
	for tag, v := range cur {
		old := a.last[tag]
		up, down := v[0]-old[0], v[1]-old[1]
		if up < 0 || down < 0 { // restarted
			up, down = v[0], v[1]
		}
		if up > 0 || down > 0 {
			out = append(out, model.TrafficDelta{UserID: core.TagPrefixInbound + tag, Up: up, Down: down})
		}
	}
	a.last = cur
	return out, nil
}

// parseMetrics sums gost_service_transfer_{input,output}_bytes_total per rule
// tag (input = client upload).
func parseMetrics(r io.Reader) map[string][2]int64 {
	out := map[string][2]int64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		var idx int
		switch {
		case strings.HasPrefix(line, "gost_service_transfer_input_bytes_total{"):
			idx = 0
		case strings.HasPrefix(line, "gost_service_transfer_output_bytes_total{"):
			idx = 1
		default:
			continue
		}
		i := strings.Index(line, `service="`)
		if i < 0 {
			continue
		}
		rest := line[i+9:]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			continue
		}
		tag, _, _ := strings.Cut(rest[:j], "~")
		fs := strings.Fields(line)
		v, err := strconv.ParseFloat(fs[len(fs)-1], 64)
		if err != nil {
			continue
		}
		c := out[tag]
		c[idx] += int64(v)
		out[tag] = c
	}
	return out
}

func (a *Adapter) OnlineIPs(context.Context) (map[string][]string, error) {
	return nil, core.ErrNotSupported
}

func (a *Adapter) Health(context.Context) model.CoreStatus {
	st := model.CoreStatus{Type: model.CoreGost, Version: a.opt.Version}
	if a.proc != nil {
		st.Running = a.proc.Running()
		st.PID = a.proc.PID()
		st.StartedAt = a.proc.StartedAt()
	}
	a.mu.Lock()
	st.Inbounds = a.n
	a.mu.Unlock()
	return st
}
