// Package core defines the CoreAdapter contract every proxy core implements,
// plus shared helpers (process supervision, raw gRPC). Adding a core means
// adding one adapter; the panel, database and frontend stay unchanged.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// ErrNotSupported is returned by optional operations a core cannot perform.
// For AddUsers/RemoveUsers the agent falls back to a full Reload.
var ErrNotSupported = errors.New("operation not supported by this core")

// CoreAdapter translates the unified model into one core and drives its process.
type CoreAdapter interface {
	Type() model.CoreType
	Capabilities(version string) model.CapSet
	Render(spec *model.NodeSpec) ([]byte, error)
	Validate(cfg []byte) error

	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Reload(ctx context.Context) error

	AddUsers(ctx context.Context, inbound string, us []model.User) error
	RemoveUsers(ctx context.Context, inbound string, ids []string) error

	CollectTraffic(ctx context.Context) ([]model.TrafficDelta, error)
	OnlineIPs(ctx context.Context) (map[string][]string, error)
	Health(ctx context.Context) model.CoreStatus
}

// UserHook is implemented by adapters that authenticate users through an HTTP
// callback served by the agent (e.g. Hysteria2): the agent pushes the current
// user set and the core never needs a reload for user changes.
type UserHook interface {
	SetUsers(inbound string, us []model.User)
}

// Options configures an adapter instance on a node.
type Options struct {
	Version   string
	BinPath   string // path to the core executable
	WorkDir   string // directory holding config and runtime files
	APIListen string // loopback address for the core's management API
	// AuthURL is the agent-local callback for cores that support external auth.
	AuthURL string
	Logger  *slog.Logger
}

// Factory builds an adapter.
type Factory func(Options) CoreAdapter

var (
	mu        sync.RWMutex
	factories = map[model.CoreType]Factory{}
	// capability probes used by the panel, which has no binaries installed.
	capFuncs = map[model.CoreType]func(string) model.CapSet{}
)

// Register makes a core available. Called from adapter package init().
func Register(t model.CoreType, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	factories[t] = f
	capFuncs[t] = func(v string) model.CapSet { return f(Options{Version: v}).Capabilities(v) }
}

// New builds an adapter for the given core type.
func New(t model.CoreType, o Options) (CoreAdapter, error) {
	mu.RLock()
	f, ok := factories[t]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown core %q", t)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return f(o), nil
}

// Capabilities returns the capability set of a core version without a binary.
func Capabilities(t model.CoreType, version string) (model.CapSet, bool) {
	mu.RLock()
	f, ok := capFuncs[t]
	mu.RUnlock()
	if !ok {
		return model.CapSet{}, false
	}
	return f(version), true
}

// Types lists registered core types, sorted.
func Types() []model.CoreType {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]model.CoreType, 0, len(factories))
	for t := range factories {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
