// Package mihomo reserves the slot for a mihomo (Clash.Meta) server adapter.
// It is registered so the panel can list it, but advertises no capabilities
// until the adapter is implemented; inbounds can never be assigned to it.
package mihomo

import (
	"context"
	"errors"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() {
	core.Register(model.CoreMihomo, func(o core.Options) core.CoreAdapter { return &Adapter{opt: o} })
}

var errTODO = errors.New("mihomo adapter is not implemented yet")

type Adapter struct{ opt core.Options }

func (a *Adapter) Type() model.CoreType { return model.CoreMihomo }
func (a *Adapter) Capabilities(v string) model.CapSet {
	return model.CapSet{Core: model.CoreMihomo, Version: v}
}
func (a *Adapter) Render(*model.NodeSpec) ([]byte, error)                       { return nil, errTODO }
func (a *Adapter) Validate([]byte) error                                        { return errTODO }
func (a *Adapter) Start(context.Context) error                                  { return errTODO }
func (a *Adapter) Stop(context.Context) error                                   { return nil }
func (a *Adapter) Reload(context.Context) error                                 { return errTODO }
func (a *Adapter) AddUsers(context.Context, string, []model.User) error         { return core.ErrNotSupported }
func (a *Adapter) RemoveUsers(context.Context, string, []string) error          { return core.ErrNotSupported }
func (a *Adapter) CollectTraffic(context.Context) ([]model.TrafficDelta, error) { return nil, nil }
func (a *Adapter) OnlineIPs(context.Context) (map[string][]string, error) {
	return nil, core.ErrNotSupported
}
func (a *Adapter) Health(context.Context) model.CoreStatus {
	return model.CoreStatus{Type: model.CoreMihomo, Version: a.opt.Version, Error: errTODO.Error()}
}
