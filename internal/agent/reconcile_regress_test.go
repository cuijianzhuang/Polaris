package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

// Regression tests for Apply with fake core adapters. The fakes are put in
// a.cores up front with the version the spec asks for, so ensureCore keeps
// them and nothing is installed.

// callLog records adapter calls across fakes, in order ("xray:reload").
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(s string) {
	l.mu.Lock()
	l.calls = append(l.calls, s)
	l.mu.Unlock()
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

type fakeCore struct {
	typ model.CoreType
	log *callLog
	// listen makes Start/Reload bind the core's TCP ports for real, failing
	// like a real core when another socket holds one.
	listen bool

	mu           sync.Mutex
	running      bool
	rendered     *model.NodeSpec
	failValidate bool
	lns          []net.Listener
	added        map[string][]string
	removed      map[string][]string
}

func newFake(t model.CoreType, log *callLog, listen bool) *fakeCore {
	return &fakeCore{typ: t, log: log, listen: listen, added: map[string][]string{}, removed: map[string][]string{}}
}

func (f *fakeCore) Type() model.CoreType             { return f.typ }
func (f *fakeCore) Capabilities(string) model.CapSet { return model.CapSet{} }
func (f *fakeCore) note(op string)                   { f.log.add(string(f.typ) + ":" + op) }
func (f *fakeCore) setFailValidate(v bool)           { f.mu.Lock(); f.failValidate = v; f.mu.Unlock() }
func (f *fakeCore) Health(context.Context) model.CoreStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return model.CoreStatus{Type: f.typ, Running: f.running}
}

func (f *fakeCore) Render(spec *model.NodeSpec) ([]byte, error) {
	s := coreSlice(spec, f.typ)
	f.mu.Lock()
	f.rendered = s
	f.mu.Unlock()
	return json.Marshal(s)
}

func (f *fakeCore) Validate([]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failValidate {
		return errors.New("fake validation failure")
	}
	return nil
}

func (f *fakeCore) bind() error {
	for _, l := range f.lns {
		l.Close()
	}
	f.lns = nil
	if !f.listen {
		return nil
	}
	for _, p := range portsOf(f.rendered) {
		if p.Proto != "tcp" {
			continue
		}
		l, err := net.Listen("tcp", net.JoinHostPort(p.Host, fmt.Sprint(p.Port)))
		if err != nil {
			for _, l := range f.lns {
				l.Close()
			}
			f.lns = nil
			return err
		}
		f.lns = append(f.lns, l)
	}
	return nil
}

func (f *fakeCore) Start(context.Context) error {
	f.note("start")
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.bind()
	f.running = err == nil
	return err
}

func (f *fakeCore) Reload(context.Context) error {
	f.note("reload")
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.bind()
	f.running = err == nil
	return err
}

func (f *fakeCore) Stop(context.Context) error {
	f.note("stop")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.lns {
		l.Close()
	}
	f.lns = nil
	f.running = false
	return nil
}

func (f *fakeCore) AddUsers(_ context.Context, tag string, us []model.User) error {
	f.note("add")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range us {
		f.added[tag] = append(f.added[tag], u.ID)
	}
	return nil
}

func (f *fakeCore) RemoveUsers(_ context.Context, tag string, ids []string) error {
	f.note("remove")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed[tag] = append(f.removed[tag], ids...)
	return nil
}

func (f *fakeCore) CollectTraffic(context.Context) ([]model.TrafficDelta, error) {
	f.note("collect")
	return nil, nil
}

func (f *fakeCore) OnlineIPs(context.Context) (map[string][]string, error) {
	return nil, core.ErrNotSupported
}

func (f *fakeCore) removedFrom(tag string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed[tag])
}

func (f *fakeCore) addedTo(tag string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.added[tag])
}

func newFakeAgent(t *testing.T, fakes ...*fakeCore) *Agent {
	dir := t.TempDir()
	a := &Agent{cfg: Config{DataDir: dir}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cores: map[model.CoreType]*managed{}}
	for _, f := range fakes {
		a.cores[f.typ] = &managed{spec: model.CoreSpec{Type: f.typ}, adapter: f,
			cfgPath: filepath.Join(dir, "run", string(f.typ)+".cfg")}
	}
	t.Cleanup(func() {
		for _, f := range fakes {
			f.Stop(context.Background())
		}
	})
	return a
}

func vlessIn(c model.CoreType, tag string, port int, users ...model.User) model.Inbound {
	return model.Inbound{Tag: tag, Core: c, Listen: "127.0.0.1", Port: port, Enabled: true,
		Protocol: model.ProtocolSpec{Type: model.ProtoVLESS}, Users: users}
}

func nodeSpec(rev int64, ins ...model.Inbound) *model.NodeSpec {
	return &model.NodeSpec{NodeID: "n1", Rev: rev,
		Cores:    []model.CoreSpec{{Type: model.CoreSingbox}, {Type: model.CoreXray}},
		Inbounds: ins}
}

var (
	ru1 = model.User{ID: "u1", UUID: "1"}
	ru2 = model.User{ID: "u2", UUID: "2"}
)

// A user hot-added on one core during an apply that failed on another core
// must be hot-removed later: the core's previous state is what it applied,
// not the last fully successful spec.
func TestApplyPartialFailureUserRemoval(t *testing.T) {
	t.Parallel()
	log := &callLog{}
	xr, sb := newFake(model.CoreXray, log, false), newFake(model.CoreSingbox, log, false)
	a := newFakeAgent(t, xr, sb)
	ctx := context.Background()
	px, ps, ps2 := freePortT(t), freePortT(t), freePortT(t)

	if r := a.Apply(ctx, nodeSpec(1, vlessIn(model.CoreXray, "xa", px, ru1), vlessIn(model.CoreSingbox, "sb", ps, ru1))); !r.ok {
		t.Fatalf("rev1: %v", r.err)
	}
	sb.setFailValidate(true)
	if r := a.Apply(ctx, nodeSpec(2, vlessIn(model.CoreXray, "xa", px, ru1, ru2), vlessIn(model.CoreSingbox, "sb", ps2, ru1))); r.ok {
		t.Fatal("rev2 should fail on singbox")
	}
	if got := xr.addedTo("xa"); !slices.Equal(got, []string{"u2"}) {
		t.Fatalf("rev2: xray should hot-add u2, added %v", got)
	}
	sb.setFailValidate(false)
	if r := a.Apply(ctx, nodeSpec(3, vlessIn(model.CoreXray, "xa", px, ru1), vlessIn(model.CoreSingbox, "sb", ps, ru1))); !r.ok {
		t.Fatalf("rev3: %v", r.err)
	}
	if got := xr.removedFrom("xa"); !slices.Equal(got, []string{"u2"}) {
		t.Fatalf("rev3: xray must remove u2 it still serves, removed %v (calls %v)", got, log.all())
	}
}

// A port opened on one core during an apply that failed on another core is
// ours, not "another program's", on the next apply.
func TestApplyPartialFailureOwnPort(t *testing.T) {
	t.Parallel()
	log := &callLog{}
	xr, sb := newFake(model.CoreXray, log, true), newFake(model.CoreSingbox, log, true)
	a := newFakeAgent(t, xr, sb)
	ctx := context.Background()
	px, pnew, ps, ps2 := freePortT(t), freePortT(t), freePortT(t), freePortT(t)

	if r := a.Apply(ctx, nodeSpec(1, vlessIn(model.CoreXray, "xa", px), vlessIn(model.CoreSingbox, "sb", ps))); !r.ok {
		t.Fatalf("rev1: %v", r.err)
	}
	sb.setFailValidate(true)
	rev2 := nodeSpec(2, vlessIn(model.CoreXray, "xa", px), vlessIn(model.CoreXray, "xb", pnew), vlessIn(model.CoreSingbox, "sb", ps2))
	if r := a.Apply(ctx, rev2); r.ok {
		t.Fatal("rev2 should fail on singbox")
	}
	// xray really listens on the new port now.
	if !portBusy(portUse{Proto: "tcp", Host: "127.0.0.1", Port: pnew}) {
		t.Fatal("fake xray does not hold the new port")
	}
	sb.setFailValidate(false)
	r := a.Apply(ctx, nodeSpec(3, vlessIn(model.CoreXray, "xa", px), vlessIn(model.CoreXray, "xb", pnew), vlessIn(model.CoreSingbox, "sb", ps)))
	if !r.ok {
		t.Fatalf("rev3 refused: %v", r.err)
	}
}

// A change to CDN fronts only must reload Caddy, not count as a users-only
// (hot) change.
func TestApplyFrontsOnlyReloadsCaddy(t *testing.T) {
	t.Parallel()
	log := &callLog{}
	cd := newFake(model.CoreCaddy, log, false)
	a := newFakeAgent(t, cd)
	ctx := context.Background()
	port := freePortT(t)
	fronts := func(rev int64, path string) *model.NodeSpec {
		return &model.NodeSpec{NodeID: "n1", Rev: rev, Fronts: []model.FrontRule{
			{Domain: "cdn.example.com", Port: port, Path: path, Upstream: "127.0.0.1:1"}}}
	}
	if r := a.Apply(ctx, fronts(1, "/a")); !r.ok {
		t.Fatalf("rev1: %v", r.err)
	}
	r := a.Apply(ctx, fronts(2, "/b"))
	if !r.ok {
		t.Fatalf("rev2: %v", r.err)
	}
	if r.method != "reload" || !slices.Contains(log.all(), "caddy:reload") {
		t.Fatalf("fronts change: method=%s calls=%v, want a caddy reload", r.method, log.all())
	}
}

// An inbound moved from xray to sing-box on the same port: xray (which only
// loses the port) must be applied before sing-box binds it.
func TestApplyOrderPortMove(t *testing.T) {
	t.Parallel()
	log := &callLog{}
	xr, sb := newFake(model.CoreXray, log, true), newFake(model.CoreSingbox, log, true)
	a := newFakeAgent(t, xr, sb)
	ctx := context.Background()
	p, keep, ps := freePortT(t), freePortT(t), freePortT(t)

	if r := a.Apply(ctx, nodeSpec(1, vlessIn(model.CoreXray, "moved", p), vlessIn(model.CoreXray, "keep", keep), vlessIn(model.CoreSingbox, "sb", ps))); !r.ok {
		t.Fatalf("rev1: %v", r.err)
	}
	n := len(log.all())
	r := a.Apply(ctx, nodeSpec(2, vlessIn(model.CoreSingbox, "moved", p), vlessIn(model.CoreXray, "keep", keep), vlessIn(model.CoreSingbox, "sb", ps)))
	calls := log.all()[n:]
	if !r.ok {
		t.Fatalf("move: %v (calls %v)", r.err, calls)
	}
	xi, si := slices.Index(calls, "xray:reload"), slices.Index(calls, "singbox:reload")
	if xi < 0 || si < 0 || xi > si {
		t.Fatalf("xray must reload before singbox: %v", calls)
	}
}

// Counters live in core memory: they are read right before a reload or stop.
func TestApplyCollectsBeforeReloadAndStop(t *testing.T) {
	t.Parallel()
	log := &callLog{}
	xr, sb := newFake(model.CoreXray, log, false), newFake(model.CoreSingbox, log, false)
	a := newFakeAgent(t, xr, sb)
	ctx := context.Background()
	p1, p2, ps := freePortT(t), freePortT(t), freePortT(t)

	if r := a.Apply(ctx, nodeSpec(1, vlessIn(model.CoreXray, "xa", p1), vlessIn(model.CoreSingbox, "sb", ps))); !r.ok {
		t.Fatalf("rev1: %v", r.err)
	}
	// Structural change on xray: reload.
	if r := a.Apply(ctx, nodeSpec(2, vlessIn(model.CoreXray, "xa", p2), vlessIn(model.CoreSingbox, "sb", ps))); !r.ok {
		t.Fatalf("rev2: %v", r.err)
	}
	// xray no longer wanted at all: stop.
	rev3 := &model.NodeSpec{NodeID: "n1", Rev: 3, Cores: []model.CoreSpec{{Type: model.CoreSingbox}},
		Inbounds: []model.Inbound{vlessIn(model.CoreSingbox, "sb", ps)}}
	if r := a.Apply(ctx, rev3); !r.ok {
		t.Fatalf("rev3: %v", r.err)
	}
	calls := log.all()
	var checked []string
	for i, c := range calls {
		typ, op, _ := strings.Cut(c, ":")
		if op != "reload" && op != "stop" {
			continue
		}
		if i == 0 || calls[i-1] != typ+":collect" {
			t.Fatalf("%s not preceded by %s:collect: %v", c, typ, calls)
		}
		checked = append(checked, c)
	}
	if !slices.Contains(checked, "xray:reload") || !slices.Contains(checked, "xray:stop") {
		t.Fatalf("expected an xray reload and stop: %v", calls)
	}
}
