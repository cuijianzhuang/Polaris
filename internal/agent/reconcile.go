package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

// managed is one running core on this host.
type managed struct {
	spec    model.CoreSpec
	adapter core.CoreAdapter
	cfgPath string
	// applied is the spec this core last converged to. It can differ from
	// Agent.current when another core failed in the same apply.
	applied *model.NodeSpec
}

// Hysteria uses base+10.. (one stats port per inbound).
var apiOffsets = map[model.CoreType]int{model.CoreXray: 0, model.CoreSingbox: 1, model.CoreMihomo: 3, model.CoreCaddy: 4, model.CoreHysteria: 10}

func (a *Agent) apiListen(t model.CoreType) string {
	off, ok := apiOffsets[t]
	if !ok {
		return ""
	}
	return "127.0.0.1:" + strconv.Itoa(a.cfg.APIPortBase+off)
}

var cfgNames = map[model.CoreType]string{
	model.CoreXray: "xray.json", model.CoreSingbox: "singbox.json", model.CoreHysteria: "hysteria.json",
	model.CoreForwarder: "realm.toml", model.CoreMihomo: "mihomo.yaml", model.CoreCaddy: "Caddyfile",
}

// applyResult describes one reconcile.
type applyResult struct {
	ok         bool
	method     string
	err        error
	rolledBack bool
}

// Apply converges the host onto spec. Only one apply runs at a time.
//
//   - users-only changes go through AddUsers/RemoveUsers (falling back to a
//     reload when the core cannot hot-update);
//   - structural changes are rendered, validated, atomically swapped in and
//     reloaded; if the core is unhealthy afterwards the previous config is
//     restored and the failure reported.
func (a *Agent) Apply(ctx context.Context, spec *model.NodeSpec) applyResult {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	prev := a.current
	// Refuse up front, leaving running cores untouched, when another program
	// holds a port the new spec needs. Ports our cores hold come from what
	// each one actually runs.
	held := []*model.NodeSpec{prev}
	for _, m := range a.cores {
		held = append(held, m.applied)
	}
	if msgs := portConflicts(held, spec); len(msgs) > 0 {
		return applyResult{ok: false, method: "reload", err: errors.New(strings.Join(msgs, "; "))}
	}
	method := "hot"
	wanted := map[model.CoreType]model.CoreSpec{}
	for _, cs := range spec.Cores {
		wanted[cs.Type] = cs
	}
	// Implicit forwarder / caddy when relay forwards or CDN fronts exist.
	if len(spec.Forwards) > 0 {
		if _, ok := wanted[model.CoreForwarder]; !ok {
			wanted[model.CoreForwarder] = model.CoreSpec{Type: model.CoreForwarder}
		}
	}
	if len(spec.Fronts) > 0 {
		if _, ok := wanted[model.CoreCaddy]; !ok {
			wanted[model.CoreCaddy] = model.CoreSpec{Type: model.CoreCaddy}
		}
	}
	// Stop cores that are no longer wanted.
	for t, m := range a.cores {
		if _, ok := wanted[t]; !ok {
			a.collectFrom(ctx, m)
			_ = m.adapter.Stop(ctx)
			a.setCore(t, nil)
			method = "reload"
		}
	}
	types := make([]model.CoreType, 0, len(wanted))
	for t := range wanted {
		types = append(types, t)
	}
	// Cores that take over a port from another core go last, so the port is
	// released before it is bound again (an inbound moved between cores).
	gains := map[model.CoreType]bool{}
	for _, t := range types {
		var had *model.NodeSpec
		if m := a.cores[t]; m != nil {
			had = m.applied
		}
		gains[t] = takesPorts(coreSlice(had, t), coreSlice(spec, t))
	}
	sort.Slice(types, func(i, j int) bool {
		if gains[types[i]] != gains[types[j]] {
			return !gains[types[i]]
		}
		return types[i] < types[j]
	})

	var errs []string
	rolledBack := false
	for _, t := range types {
		cs := wanted[t]
		active := len(spec.InboundsFor(t)) > 0 || (t == model.CoreForwarder && len(spec.Forwards) > 0) ||
			(t == model.CoreCaddy && len(spec.Fronts) > 0)
		m, old, err := a.ensureCore(ctx, cs, active)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t, err))
			continue
		}
		if m == nil {
			continue // installed, nothing to serve
		}
		corePrev := m.applied
		if corePrev == nil && old == nil {
			corePrev = prev
		}
		meth, rb, err := a.applyCore(ctx, m, corePrev, spec, active)
		if meth == "reload" {
			method = "reload"
		}
		rolledBack = rolledBack || rb
		if err != nil && old != nil {
			// A version switch failed: go back to the version that worked.
			err = a.revertVersion(ctx, m, old, err)
			rolledBack = true
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t, err))
		} else {
			m.applied = spec
		}
	}
	if len(errs) > 0 {
		return applyResult{ok: false, method: method, err: errors.New(strings.Join(errs, "; ")), rolledBack: rolledBack}
	}
	a.current = spec
	a.rev.Store(spec.Rev)
	a.saveSpec(spec)
	a.syncFirewall(ctx, spec)
	return applyResult{ok: true, method: method}
}

// ensureCore installs the binary and (re)creates the adapter on version
// change. On a switch it also returns the previous core, which keeps
// running until the new binary is installed, for revertVersion.
func (a *Agent) ensureCore(ctx context.Context, cs model.CoreSpec, active bool) (*managed, *managed, error) {
	m := a.cores[cs.Type]
	if m != nil && m.spec.Version == cs.Version {
		return m, nil, nil
	}
	if !active && m == nil {
		// Install ahead of time so the first inbound starts instantly.
		if _, err := a.installer.Ensure(ctx, cs); err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	}
	bin, err := a.installer.Ensure(ctx, cs)
	if err != nil {
		if m != nil {
			return nil, nil, fmt.Errorf("安装 %s 失败，继续运行 %s：%w", cs.Version, m.spec.Version, err)
		}
		return nil, nil, err
	}
	work := filepath.Join(a.cfg.DataDir, "run", string(cs.Type))
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, nil, err
	}
	ad, err := core.New(cs.Type, core.Options{
		Version: cs.Version, BinPath: bin, WorkDir: work, APIListen: a.apiListen(cs.Type),
		AuthURL: "http://" + a.cfg.AuthListen + "/auth/" + string(cs.Type), Logger: a.log,
	})
	if err != nil {
		return nil, nil, err
	}
	if m != nil {
		a.collectFrom(ctx, m)
		_ = m.adapter.Stop(ctx) // version switch
	}
	nm := &managed{spec: cs, adapter: ad, cfgPath: filepath.Join(work, cfgNames[cs.Type])}
	a.setCore(cs.Type, nm)
	return nm, m, nil
}

// revertVersion undoes a failed version switch: the new core is stopped,
// "current" points at the old version again and the old core is restarted
// with the configuration it was running (applyCore already restored it).
func (a *Agent) revertVersion(ctx context.Context, failed, old *managed, cause error) error {
	t := failed.spec.Type
	a.log.Error("core version switch failed, reverting", "core", t, "from", failed.spec.Version, "to", old.spec.Version, "err", cause)
	_ = failed.adapter.Stop(ctx)
	a.setCore(t, old)
	if _, err := a.installer.Ensure(ctx, old.spec); err != nil {
		a.log.Warn("relink previous core version", "core", t, "err", err)
	}
	if err := old.adapter.Start(ctx); err == nil {
		err = waitHealthy(ctx, old.adapter)
		if err != nil {
			return fmt.Errorf("升级到 %s 失败（%v），回退到 %s 后仍未正常运行：%w", failed.spec.Version, cause, old.spec.Version, err)
		}
	} else {
		return fmt.Errorf("升级到 %s 失败（%v），回退到 %s 时启动失败：%w", failed.spec.Version, cause, old.spec.Version, err)
	}
	return fmt.Errorf("升级到 %s 失败，已回退到 %s：%w", failed.spec.Version, old.spec.Version, cause)
}

func (a *Agent) applyCore(ctx context.Context, m *managed, prev, next *model.NodeSpec, active bool) (method string, rolledBack bool, err error) {
	t := m.spec.Type
	if !active {
		a.collectFrom(ctx, m)
		return "reload", false, m.adapter.Stop(ctx)
	}
	cfg, err := m.adapter.Render(next)
	if err != nil {
		return "reload", false, fmt.Errorf("render: %w", err)
	}
	old, _ := os.ReadFile(m.cfgPath)
	running := m.adapter.Health(ctx).Running
	if running && prev != nil && string(old) != "" {
		if adds, removes, ok := userOnlyDiff(prev, next, t); ok {
			if len(adds) == 0 && len(removes) == 0 && string(old) == string(cfg) {
				return "hot", false, nil
			}
			if err := m.adapter.Validate(cfg); err != nil {
				return "hot", false, fmt.Errorf("validate: %w", err)
			}
			if err := a.hotUpdate(ctx, m, adds, removes); err == nil {
				// Keep the file in sync so a later restart has the same users.
				return "hot", false, core.AtomicWrite(m.cfgPath, cfg, 0o600)
			} else if !errors.Is(err, core.ErrNotSupported) {
				a.log.Warn("hot update failed, reloading", "core", t, "err", err)
			}
		}
	}
	if string(old) == string(cfg) && running {
		return "hot", false, nil
	}
	if err := m.adapter.Validate(cfg); err != nil {
		return "reload", false, fmt.Errorf("validate: %w", err)
	}
	if err := core.AtomicWrite(m.cfgPath, cfg, 0o600); err != nil {
		return "reload", false, err
	}
	if running {
		a.collectFrom(ctx, m) // a reload resets the core's counters
		err = m.adapter.Reload(ctx)
	} else {
		err = m.adapter.Start(ctx)
	}
	if err == nil {
		err = waitHealthy(ctx, m.adapter)
	}
	if err != nil && len(old) > 0 {
		a.log.Error("core unhealthy after apply, rolling back", "core", t, "err", err)
		_ = core.AtomicWrite(m.cfgPath, old, 0o600)
		if prev != nil {
			_, _ = m.adapter.Render(prev) // restore adapter state for hot updates
		}
		if rerr := m.adapter.Reload(ctx); rerr != nil {
			_ = m.adapter.Start(ctx)
		}
		return "reload", true, err
	}
	return "reload", false, err
}

func waitHealthy(ctx context.Context, ad core.CoreAdapter) error {
	time.Sleep(1200 * time.Millisecond)
	st := ad.Health(ctx)
	if !st.Running {
		if st.Error != "" {
			return fmt.Errorf("core not running: %s", st.Error)
		}
		return errors.New("core not running")
	}
	return nil
}

func (a *Agent) hotUpdate(ctx context.Context, m *managed, adds map[string][]model.User, removes map[string][]string) error {
	for tag, ids := range removes {
		if err := m.adapter.RemoveUsers(ctx, tag, ids); err != nil {
			return err
		}
	}
	for tag, us := range adds {
		if err := m.adapter.AddUsers(ctx, tag, us); err != nil {
			return err
		}
	}
	return nil
}

// userOnlyDiff reports whether the core's part of two specs differs only in
// inbound user lists, and returns the per-inbound additions/removals.
func userOnlyDiff(prev, next *model.NodeSpec, t model.CoreType) (map[string][]model.User, map[string][]string, bool) {
	strip := func(s *model.NodeSpec) ([]byte, map[string][]model.User) {
		users := map[string][]model.User{}
		var ins []model.Inbound
		for _, in := range s.InboundsFor(t) {
			users[in.Tag] = in.Users
			in.Users = nil
			ins = append(ins, in)
		}
		b, _ := json.Marshal(struct {
			In  []model.Inbound
			Out []model.Outbound
			R   []model.RouteRule
			F   []model.ForwardRule
			Fr  []model.FrontRule
		}{ins, s.Outbounds, s.Routes, s.Forwards, s.Fronts})
		return b, users
	}
	pb, pu := strip(prev)
	nb, nu := strip(next)
	if string(pb) != string(nb) {
		return nil, nil, false
	}
	adds := map[string][]model.User{}
	removes := map[string][]string{}
	for tag, nus := range nu {
		old := map[string]model.User{}
		for _, u := range pu[tag] {
			old[u.ID] = u
		}
		cur := map[string]bool{}
		for _, u := range nus {
			cur[u.ID] = true
			o, ok := old[u.ID]
			if !ok {
				adds[tag] = append(adds[tag], u)
			} else if o != u { // credentials changed: re-add
				removes[tag] = append(removes[tag], u.ID)
				adds[tag] = append(adds[tag], u)
			}
		}
		for id := range old {
			if !cur[id] {
				removes[tag] = append(removes[tag], id)
			}
		}
	}
	return adds, removes, true
}

func (a *Agent) specPath() string { return filepath.Join(a.idDir(), "applied-spec.json") }

func (a *Agent) saveSpec(spec *model.NodeSpec) {
	b, err := json.Marshal(spec)
	if err == nil {
		_ = core.AtomicWrite(a.specPath(), b, 0o600)
	}
}

// loadSpec restores the last applied spec so cores keep serving while the
// panel is unreachable.
func (a *Agent) loadSpec() *model.NodeSpec {
	b, err := os.ReadFile(a.specPath())
	if err != nil {
		return nil
	}
	var s model.NodeSpec
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

// syncFirewall opens / closes host firewall ports for the applied spec. It
// never fails the apply: a firewall problem is logged, the cores keep running.
func (a *Agent) syncFirewall(ctx context.Context, spec *model.NodeSpec) {
	if a.fw == nil {
		return
	}
	summary, err := a.fw.Sync(ctx, spec, spec.AutoFirewall)
	if summary != "" {
		a.log.Info("firewall updated", "changes", summary)
	}
	if err != nil {
		a.log.Warn("firewall update incomplete", "err", err)
	}
}

// coreSlice is the part of a spec one core serves (its sockets).
func coreSlice(s *model.NodeSpec, t model.CoreType) *model.NodeSpec {
	if s == nil {
		return nil
	}
	out := &model.NodeSpec{Inbounds: s.InboundsFor(t)}
	switch t {
	case model.CoreForwarder:
		out.Forwards = s.Forwards
	case model.CoreCaddy:
		out.Fronts = s.Fronts
	}
	return out
}

// takesPorts reports whether a core needs a port it does not hold yet.
func takesPorts(had, want *model.NodeSpec) bool {
	mine := map[string]bool{}
	for _, p := range portsOf(had) {
		mine[p.key()] = true
	}
	for _, p := range portsOf(want) {
		if !mine[p.key()] {
			return true
		}
	}
	return false
}
