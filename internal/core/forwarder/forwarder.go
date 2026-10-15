// Package forwarder runs L4 port forwards on relay nodes using realm. It never
// decrypts traffic, so REALITY and Hysteria2 pass through untouched; users are
// metered on the landing inbound.
package forwarder

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() {
	core.Register(model.CoreForwarder, func(o core.Options) core.CoreAdapter { return New(o) })
}

type Adapter struct {
	opt  core.Options
	proc *core.Process
	mu   sync.Mutex
	n    int
}

func New(o core.Options) *Adapter {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	a := &Adapter{opt: o}
	if o.BinPath != "" {
		a.proc = core.NewProcess("realm", o.BinPath, o.WorkDir, o.Logger, "-c", filepath.Join(o.WorkDir, "realm.toml"))
	}
	return a
}

func (a *Adapter) Type() model.CoreType { return model.CoreForwarder }

func (a *Adapter) Capabilities(v string) model.CapSet {
	return model.CapSet{Core: model.CoreForwarder, Version: v}
}

// Render writes a realm TOML config from the spec's forward rules.
func (a *Adapter) Render(spec *model.NodeSpec) ([]byte, error) {
	var b strings.Builder
	b.WriteString("[log]\nlevel = \"warn\"\n\n[network]\nno_tcp = false\nuse_udp = true\n\n")
	for _, f := range spec.Forwards {
		if f.Listen <= 0 || f.Target == "" {
			return nil, fmt.Errorf("forward %s: listen and target are required", f.Tag)
		}
		fmt.Fprintf(&b, "[[endpoints]]\nlisten = \"[::]:%d\"\nremote = %q\n", f.Listen, f.Target)
		if f.Network == "tcp" {
			b.WriteString("network = { use_udp = false }\n")
		} else if f.Network == "udp" {
			b.WriteString("network = { no_tcp = true, use_udp = true }\n")
		}
		b.WriteString("\n")
	}
	a.mu.Lock()
	a.n = len(spec.Forwards)
	a.mu.Unlock()
	return []byte(b.String()), nil
}

func (a *Adapter) Validate(cfg []byte) error {
	if len(cfg) == 0 {
		return fmt.Errorf("empty config")
	}
	return nil
}

func (a *Adapter) Start(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("realm binary not configured")
	}
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
		return fmt.Errorf("realm binary not configured")
	}
	return a.proc.Restart(ctx)
}

func (a *Adapter) AddUsers(context.Context, string, []model.User) error { return core.ErrNotSupported }
func (a *Adapter) RemoveUsers(context.Context, string, []string) error  { return core.ErrNotSupported }
func (a *Adapter) CollectTraffic(context.Context) ([]model.TrafficDelta, error) {
	return nil, nil
}
func (a *Adapter) OnlineIPs(context.Context) (map[string][]string, error) {
	return nil, core.ErrNotSupported
}

func (a *Adapter) Health(context.Context) model.CoreStatus {
	st := model.CoreStatus{Type: model.CoreForwarder, Version: a.opt.Version}
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
