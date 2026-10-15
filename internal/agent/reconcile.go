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
}

var apiOffsets = map[model.CoreType]int{model.CoreXray: 0, model.CoreSingbox: 1, model.CoreHysteria: 2}

func (a *Agent) apiListen(t model.CoreType) string {
	off, ok := apiOffsets[t]
	if !ok {
		return ""
	}
	return "127.0.0.1:" + strconv.Itoa(a.cfg.APIPortBase+off)
}

var cfgNames = map[model.CoreType]string{
	model.CoreXray: "xray.json", model.CoreSingbox: "singbox.json", model.CoreHysteria: "hysteria.json",
	model.CoreForwarder: "realm.toml",
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
	method := "hot"
	wanted := map[model.CoreType]model.CoreSpec{}
	for _, cs := range spec.Cores {
		wanted[cs.Type] = cs
	}
	// Implicit forwarder when relay forwards exist.
	if len(spec.Forwards) > 0 {
		if _, ok := wanted[model.CoreForwarder]; !ok {
			wanted[model.CoreForwarder] = model.CoreSpec{Type: model.CoreForwarder}
		}
	}
	// Stop cores that are no longer wanted.
	for t, m := range a.cores {
		if _, ok := wanted[t]; !ok {
			_ = m.adapter.Stop(ctx)
			delete(a.cores, t)
			method = "reload"
		}
	}
	types := make([]model.CoreType, 0, len(wanted))
	for t := range wanted {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })

	var errs []string
	rolledBack := false
	for _, t := range types {
		cs := wanted[t]
		if t == model.CoreMihomo {
			continue // reserved slot, nothing to run yet
		}
		active := len(spec.InboundsFor(t)) > 0 || (t == model.CoreForwarder && len(spec.Forwards) > 0)
		m, err := a.ensureCore(ctx, cs, active)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t, err))
			continue
		}
		if m == nil {
			continue // installed, nothing to serve
		}
		meth, rb, err := a.applyCore(ctx, m, prev, spec, active)
		if meth == "reload" {
			method = "reload"
		}
		rolledBack = rolledBack || rb
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t, err))
		}
	}
	if len(errs) > 0 {
		return applyResult{ok: false, method: method, err: errors.New(strings.Join(errs, "; ")), rolledBack: rolledBack}
	}
	a.current = spec
	a.saveSpec(spec)
	return applyResult{ok: true, method: method}
}

// ensureCore installs the binary and (re)creates the adapter on version change.
func (a *Agent) ensureCore(ctx context.Context, cs model.CoreSpec, active bool) (*managed, error) {
	m := a.cores[cs.Type]
	if m != nil && m.spec.Version == cs.Version {
		return m, nil
	}
	if !active && m == nil {
		// Install ahead of time so the first inbound starts instantly.
		if _, err := a.installer.Ensure(ctx, cs); err != nil {
			return nil, err
		}
		return nil, nil
	}
	bin, err := a.installer.Ensure(ctx, cs)
	if err != nil {
		return nil, err
	}
	work := filepath.Join(a.cfg.DataDir, "run", string(cs.Type))
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	ad, err := core.New(cs.Type, core.Options{
		Version: cs.Version, BinPath: bin, WorkDir: work, APIListen: a.apiListen(cs.Type),
		AuthURL: "http://" + a.cfg.AuthListen + "/auth/" + string(cs.Type), Logger: a.log,
	})
	if err != nil {
		return nil, err
	}
	if m != nil {
		_ = m.adapter.Stop(ctx) // version switch
	}
	nm := &managed{spec: cs, adapter: ad, cfgPath: filepath.Join(work, cfgNames[cs.Type])}
	a.cores[cs.Type] = nm
	return nm, nil
}

func (a *Agent) applyCore(ctx context.Context, m *managed, prev, next *model.NodeSpec, active bool) (method string, rolledBack bool, err error) {
	t := m.spec.Type
	if !active {
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
		}{ins, s.Outbounds, s.Routes, s.Forwards})
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
