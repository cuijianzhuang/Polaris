// Package agent runs on every node: it keeps a stream to the panel, converges
// the host onto the desired NodeSpec, supervises cores and reports traffic.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cuijianzhuang/polaris/internal/realityscan"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/api/gen/polaris/v1/polarisv1connect"
	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"

	_ "github.com/cuijianzhuang/polaris/internal/core/caddy"
	_ "github.com/cuijianzhuang/polaris/internal/core/forwarder"
	_ "github.com/cuijianzhuang/polaris/internal/core/gost"
	_ "github.com/cuijianzhuang/polaris/internal/core/hysteria"
	_ "github.com/cuijianzhuang/polaris/internal/core/mihomo"
	_ "github.com/cuijianzhuang/polaris/internal/core/singbox"
	_ "github.com/cuijianzhuang/polaris/internal/core/xray"
)

// Version is set at build time.
var Version = "dev"

// Config configures the agent.
type Config struct {
	PanelURL   string // https://panel:9443
	DataDir    string // /var/lib/polaris
	AuthListen string // loopback address for core auth callbacks
	Mirror     string // optional GitHub download mirror
	// APIPortBase is the first loopback port for core management APIs
	// (xray = base, sing-box = base+1, hysteria = base+2). Default 10085.
	APIPortBase int
	// Intervals (defaults: 30s heartbeat, 10s traffic, 60s online IPs).
	Heartbeat time.Duration
	Traffic   time.Duration
	OnlineIPs time.Duration
	// AllowUnverified permits core installs when no digest is published.
	AllowUnverified bool
	// DisableIPDetect turns off public IP discovery (DDNS); IPEcho overrides the echo services.
	DisableIPDetect  bool
	IPEcho4, IPEcho6 []string
}

// Agent is the node daemon.
type Agent struct {
	cfg Config
	id  *Identity
	log *slog.Logger

	collectWarnMu sync.Mutex
	collectWarned map[string]time.Time // last warning per core type
	client        polarisv1connect.AgentServiceClient
	installer     *Installer
	sys           sysSampler
	ips           *ipDetector

	applyMu sync.Mutex
	current *model.NodeSpec
	// rev mirrors current.Rev for heartbeats, which must never wait for an
	// apply (installing a core can take minutes).
	rev   atomic.Int64
	cores map[model.CoreType]*managed // written under applyMu and coresMu
	// coresMu lets heartbeats, online-IP reports and auth callbacks read
	// the cores without waiting for an apply to finish.
	coresMu sync.RWMutex

	queueMu sync.Mutex
	queue   []*polarisv1.TrafficBatch

	sendMu  sync.Mutex
	stream  *connect.BidiStreamForClient[polarisv1.AgentMessage, polarisv1.PanelMessage]
	wantRev chan int64

	acme acmeHTTP  // ACME HTTP-01 challenge server (panel-driven)
	fw   *firewall // host firewall rules for the spec's ports

	restartMu sync.Mutex
	restart   bool // set by a self-update: Run returns ErrRestart
	cancel    context.CancelFunc
}

// New creates an agent from saved credentials.
func New(cfg Config, log *slog.Logger) (*Agent, error) {
	if cfg.AuthListen == "" {
		cfg.AuthListen = "127.0.0.1:10090"
	}
	if cfg.APIPortBase == 0 {
		cfg.APIPortBase = 10085
	}
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = 30 * time.Second
	}
	if cfg.Traffic == 0 {
		cfg.Traffic = 10 * time.Second
	}
	if cfg.OnlineIPs == 0 {
		cfg.OnlineIPs = 60 * time.Second
	}
	a := &Agent{cfg: cfg, log: log, cores: map[model.CoreType]*managed{}, wantRev: make(chan int64, 1)}
	id, err := LoadIdentity(a.idDir())
	if err != nil {
		return nil, fmt.Errorf("not enrolled (run with --token first): %w", err)
	}
	a.id = id
	a.client = polarisv1connect.NewAgentServiceClient(httpClient(id.tlsConfig()), cfg.PanelURL, connect.WithGRPC())
	a.fw = newFirewall(cfg.DataDir)
	a.installer = &Installer{Dir: filepath.Join(cfg.DataDir, "cores"), Mirror: cfg.Mirror, AllowUnverified: cfg.AllowUnverified}
	if !cfg.DisableIPDetect {
		a.ips = newIPDetector(cfg.IPEcho4, cfg.IPEcho6, 0)
	}
	a.loadQueue()
	return a, nil
}

// Run blocks until ctx is cancelled. After a self-update it stops the cores
// and returns ErrRestart.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.restartMu.Lock()
	a.cancel = cancel
	a.restartMu.Unlock()
	a.log.Info("agent starting", "node", a.id.NodeID, "panel", a.cfg.PanelURL, "version", Version)
	// Serve immediately from the last applied spec, even if the panel is down.
	if spec := a.loadSpec(); spec != nil {
		if r := a.Apply(ctx, spec); !r.ok {
			a.log.Error("restore last spec", "err", r.err)
		}
	}
	go a.serveAuth(ctx)
	go a.trafficLoop(ctx)
	go a.reconcileLoop(ctx)

	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		a.log.Warn("panel stream closed, reconnecting", "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
	a.shutdown()
	a.restartMu.Lock()
	defer a.restartMu.Unlock()
	if a.restart {
		return ErrRestart
	}
	return nil
}

func (a *Agent) shutdown() {
	a.acme.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.collect(ctx)
	a.flushTraffic(ctx)
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	for _, m := range a.cores {
		_ = m.adapter.Stop(ctx)
	}
}

// session runs one bidi stream until it breaks.
func (a *Agent) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := a.client.Connect(ctx)
	a.sendMu.Lock()
	a.stream = stream
	a.sendMu.Unlock()
	defer func() {
		a.sendMu.Lock()
		a.stream = nil
		a.sendMu.Unlock()
	}()
	// Receive does not observe ctx on its own: close both halves on cancel.
	go func() {
		<-ctx.Done()
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
	}()
	if err := a.sendHeartbeat(); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(a.cfg.Heartbeat)
		defer t.Stop()
		ipT := time.NewTicker(a.cfg.OnlineIPs)
		defer ipT.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := a.sendHeartbeat(); err != nil {
					cancel()
					return
				}
			case <-ipT.C:
				a.sendOnlineIPs(ctx)
				a.requestRev(0) // safety net: converge even if a push was missed
			}
		}
	}()
	for {
		msg, err := stream.Receive()
		if err != nil {
			return err
		}
		switch m := msg.Msg.(type) {
		case *polarisv1.PanelMessage_SpecChanged:
			a.requestRev(m.SpecChanged.Rev)
		case *polarisv1.PanelMessage_Command:
			go a.handleCommand(ctx, m.Command)
		}
	}
}

func (a *Agent) send(m *polarisv1.AgentMessage) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if a.stream == nil {
		return errors.New("not connected")
	}
	return a.stream.Send(m)
}

func (a *Agent) appliedRev() int64 { return a.rev.Load() }

func (a *Agent) sendHeartbeat() error {
	hb := &polarisv1.Heartbeat{AppliedRev: a.appliedRev(), AgentVersion: Version, System: a.sys.Sample()}
	if a.ips != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		hb.PublicIpv4, hb.PublicIpv6 = a.ips.Get(ctx)
		cancel()
	}
	for t, m := range a.coreSnapshot() {
		st := m.adapter.Health(context.Background())
		cs := &polarisv1.CoreState{Type: string(t), Version: m.spec.Version, Running: st.Running, Pid: int32(st.PID),
			Error: st.Error, Inbounds: int32(st.Inbounds), InstalledVersions: a.installer.Installed(t)}
		if !st.StartedAt.IsZero() {
			cs.StartedAt = st.StartedAt.Unix()
		}
		hb.Cores = append(hb.Cores, cs)
	}
	return a.send(&polarisv1.AgentMessage{Msg: &polarisv1.AgentMessage_Heartbeat{Heartbeat: hb}})
}

// requestRev asks the reconcile loop for a revision without ever blocking
// (it runs on the stream and heartbeat goroutines): a pending request is
// replaced by the newer one.
func (a *Agent) requestRev(rev int64) {
	for {
		select {
		case a.wantRev <- rev:
			return
		default:
		}
		select {
		case <-a.wantRev:
		default:
		}
	}
}

// coreSnapshot copies the core map for readers outside an apply.
func (a *Agent) coreSnapshot() map[model.CoreType]*managed {
	a.coresMu.RLock()
	defer a.coresMu.RUnlock()
	out := make(map[model.CoreType]*managed, len(a.cores))
	for t, m := range a.cores {
		out[t] = m
	}
	return out
}

// setCore replaces (m != nil) or removes a core. Called with applyMu held.
func (a *Agent) setCore(t model.CoreType, m *managed) {
	a.coresMu.Lock()
	defer a.coresMu.Unlock()
	if m == nil {
		delete(a.cores, t)
	} else {
		a.cores[t] = m
	}
}

// reconcileLoop pulls and applies specs as revisions are announced.
func (a *Agent) reconcileLoop(ctx context.Context) {
	failed := int64(-1)
	for {
		var rev int64
		select {
		case <-ctx.Done():
			return
		case rev = <-a.wantRev:
		}
		if rev != 0 && rev == a.appliedRev() {
			continue
		}
		resp, err := a.client.GetSpec(ctx, connect.NewRequest(&polarisv1.GetSpecRequest{Rev: rev}))
		if err != nil {
			a.log.Error("fetch spec", "rev", rev, "err", err)
			continue
		}
		var spec model.NodeSpec
		if err := json.Unmarshal(resp.Msg.SpecJson, &spec); err != nil {
			a.log.Error("decode spec", "err", err)
			continue
		}
		spec.Rev = resp.Msg.Rev
		if spec.Rev == a.appliedRev() {
			continue
		}
		if spec.Rev == failed {
			continue // do not retry a revision that already failed; wait for a new one
		}
		r := a.Apply(ctx, &spec)
		res := &polarisv1.ApplyResult{Rev: spec.Rev, Ok: r.ok, Method: r.method, RolledBack: r.rolledBack}
		if r.err != nil {
			res.Error = r.err.Error()
			failed = spec.Rev
			a.log.Error("apply spec", "rev", spec.Rev, "err", r.err, "rolled_back", r.rolledBack)
		} else {
			a.log.Info("applied spec", "rev", spec.Rev, "method", r.method)
		}
		_ = a.send(&polarisv1.AgentMessage{Msg: &polarisv1.AgentMessage_ApplyResult{ApplyResult: res}})
		_ = a.sendHeartbeat()
	}
}

// --- traffic ---

func (a *Agent) queuePath() string { return filepath.Join(a.idDir(), "traffic-queue.json") }

func (a *Agent) loadQueue() {
	b, err := os.ReadFile(a.queuePath())
	if err != nil {
		return
	}
	var q []*polarisv1.TrafficBatch
	if json.Unmarshal(b, &q) == nil {
		a.queue = q
	}
}

func (a *Agent) saveQueue() {
	b, _ := json.Marshal(a.queue)
	_ = core.AtomicWrite(a.queuePath(), b, 0o600)
}

// trafficLoop collects every interval and uploads; batches survive restarts
// and panel outages on disk until the panel acknowledges them.
func (a *Agent) trafficLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.Traffic)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.collect(ctx)
			a.flushTraffic(ctx)
		}
	}
}

func (a *Agent) collect(ctx context.Context) {
	a.applyMu.Lock()
	ms := make([]*managed, 0, len(a.cores))
	for _, m := range a.cores {
		ms = append(ms, m)
	}
	a.collectFrom(ctx, ms...)
	a.applyMu.Unlock()
}

// collectFrom reads and queues the counters of some cores. Called with
// applyMu held, also right before a core is stopped or reloaded: counters
// live in core memory and would otherwise be lost.
func (a *Agent) collectFrom(ctx context.Context, ms ...*managed) {
	var all [][]model.TrafficDelta
	for _, m := range ms {
		ds, err := m.adapter.CollectTraffic(ctx)
		if err != nil {
			if !errors.Is(err, core.ErrNotSupported) {
				a.warnCollect(string(m.spec.Type), err)
			}
			continue
		}
		all = append(all, ds)
	}
	merged := model.MergeDeltas(all...)
	if len(merged) == 0 {
		return
	}
	b := &polarisv1.TrafficBatch{BatchId: store.NewID(), CollectedAt: time.Now().Unix()}
	for _, d := range merged {
		b.Users = append(b.Users, &polarisv1.UserTraffic{UserId: d.UserID, Up: d.Up, Down: d.Down})
	}
	a.queueMu.Lock()
	a.queue = append(a.queue, b)
	if len(a.queue) > 50000 { // ~5 days at 10s; drop oldest beyond that
		a.queue = a.queue[len(a.queue)-50000:]
	}
	a.saveQueue()
	a.queueMu.Unlock()
}

// warnCollect logs a traffic collection failure at most every 10 minutes
// per core: lost counters are silent otherwise, and user traffic goes missing.
func (a *Agent) warnCollect(coreType string, err error) {
	a.collectWarnMu.Lock()
	defer a.collectWarnMu.Unlock()
	if a.collectWarned == nil {
		a.collectWarned = map[string]time.Time{}
	}
	if time.Since(a.collectWarned[coreType]) < 10*time.Minute {
		a.log.Debug("collect traffic", "core", coreType, "err", err)
		return
	}
	a.collectWarned[coreType] = time.Now()
	a.log.Warn("collect traffic failed; user traffic from this core is not counted", "core", coreType, "err", err)
}

func (a *Agent) flushTraffic(ctx context.Context) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	for len(a.queue) > 0 {
		n := len(a.queue)
		if n > 200 {
			n = 200
		}
		resp, err := a.client.ReportTraffic(ctx, connect.NewRequest(&polarisv1.ReportTrafficRequest{Batches: a.queue[:n]}))
		if err != nil {
			return
		}
		ok := map[string]bool{}
		for _, id := range resp.Msg.AcceptedBatchIds {
			ok[id] = true
		}
		var rest []*polarisv1.TrafficBatch
		for _, b := range a.queue {
			if !ok[b.BatchId] {
				rest = append(rest, b)
			}
		}
		if len(rest) == len(a.queue) {
			return
		}
		a.queue = rest
		a.saveQueue()
	}
}

func (a *Agent) sendOnlineIPs(ctx context.Context) {
	merged := map[string]map[string]bool{}
	for _, m := range a.coreSnapshot() {
		ips, err := m.adapter.OnlineIPs(ctx)
		if err != nil {
			continue
		}
		for u, list := range ips {
			if merged[u] == nil {
				merged[u] = map[string]bool{}
			}
			for _, ip := range list {
				merged[u][ip] = true
			}
		}
	}
	if len(merged) == 0 {
		return
	}
	msg := &polarisv1.OnlineIPs{}
	for u, set := range merged {
		x := &polarisv1.UserIPs{UserId: u}
		for ip := range set {
			x.Ips = append(x.Ips, ip)
		}
		msg.Users = append(msg.Users, x)
	}
	_ = a.send(&polarisv1.AgentMessage{Msg: &polarisv1.AgentMessage_OnlineIps{OnlineIps: msg}})
}

// --- commands (structured only; no arbitrary execution) ---

func (a *Agent) handleCommand(ctx context.Context, c *polarisv1.Command) {
	res := &polarisv1.CommandResult{Id: c.Id, Ok: true}
	fail := func(err error) { res.Ok, res.Error = false, err.Error() }
	switch c.Kind {
	case "restart_core":
		a.applyMu.Lock() // held across the restart: no apply may interleave
		m := a.cores[model.CoreType(c.Args["type"])]
		if m == nil {
			a.applyMu.Unlock()
			fail(errors.New("core is not running on this node"))
			break
		}
		a.collectFrom(ctx, m)
		if err := m.adapter.Stop(ctx); err != nil {
			fail(err)
		} else if err := m.adapter.Start(ctx); err != nil {
			fail(err)
		}
		a.applyMu.Unlock()
	case "geodata_update":
		out, err := a.updateGeodata(ctx, c.Args)
		res.Output = out
		if err != nil {
			fail(err)
		}
	case "busy_ports":
		t, u := busyPorts()
		b, _ := json.Marshal(map[string][]int{"tcp": t, "udp": u})
		res.Output = string(b)
	case "reality_scan":
		rs, err := realityscan.Scan(ctx, c.Args["target"])
		if err != nil {
			fail(err)
			break
		}
		b, _ := json.Marshal(rs)
		res.Output = string(b)
	case "core_logs":
		m := a.coreSnapshot()[model.CoreType(c.Args["type"])]
		n, _ := strconv.Atoi(c.Args["lines"])
		if n <= 0 || n > 2000 {
			n = 200
		}
		if m == nil {
			fail(errors.New("core is not running on this node"))
			break
		}
		if l, ok := m.adapter.(interface{ Logs(int) []string }); ok {
			res.Output = strings.Join(l.Logs(n), "\n")
		}
	case "acme_http":
		if err := a.handleACMEHTTP(c.Args); err != nil {
			fail(err)
		}
	case "upgrade_agent":
		out, err := a.selfUpdate(ctx, c.Args)
		if err != nil {
			fail(err)
			break
		}
		res.Output = out
		if !strings.HasPrefix(out, "already") {
			a.log.Info("agent binary updated, restarting", "change", out)
			_ = a.send(&polarisv1.AgentMessage{Msg: &polarisv1.AgentMessage_CommandResult{CommandResult: res}})
			time.Sleep(300 * time.Millisecond) // let the result reach the panel
			a.requestRestart()
			return
		}
	case "probe":
		ms, err := probe(ctx, c.Args)
		if err != nil {
			fail(err)
		} else {
			res.Output = strconv.Itoa(ms)
		}
	default:
		fail(fmt.Errorf("unknown command %q", c.Kind))
	}
	_ = a.send(&polarisv1.AgentMessage{Msg: &polarisv1.AgentMessage_CommandResult{CommandResult: res}})
}

func probe(ctx context.Context, args map[string]string) (int, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if addr := args["tcp"]; addr != "" {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return 0, err
		}
		c.Close()
		return int(time.Since(start).Milliseconds()), nil
	}
	if u := args["http"]; u != "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return 0, fmt.Errorf("http %s", resp.Status)
		}
		return int(time.Since(start).Milliseconds()), nil
	}
	return 0, errors.New("probe needs tcp or http target")
}

// serveAuth exposes HTTP auth callbacks for cores that support them (Hysteria2).
func (a *Agent) serveAuth(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		t := model.CoreType(strings.TrimPrefix(r.URL.Path, "/auth/"))
		m := a.coreSnapshot()[t]
		if m == nil {
			http.NotFound(w, r)
			return
		}
		if h, ok := m.adapter.(http.Handler); ok {
			h.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
	srv := &http.Server{Addr: a.cfg.AuthListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.log.Error("auth callback server", "err", err)
	}
}
