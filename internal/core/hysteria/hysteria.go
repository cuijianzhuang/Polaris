// Package hysteria adapts the official Hysteria2 server. Users are
// authenticated through an HTTP callback served by the agent, so user changes
// only touch agent memory and never reload the core.
package hysteria

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() {
	core.Register(model.CoreHysteria, func(o core.Options) core.CoreAdapter { return New(o) })
}

// Adapter implements core.CoreAdapter and http.Handler (the auth callback).
// One Hysteria process serves exactly one inbound.
type Adapter struct {
	opt    core.Options
	proc   *core.Process
	secret string

	mu    sync.RWMutex
	last  *model.NodeSpec
	users map[string]string // password -> user id
}

func New(o core.Options) *Adapter {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.APIListen == "" {
		o.APIListen = "127.0.0.1:10087"
	}
	a := &Adapter{opt: o, users: map[string]string{}, secret: randomSecret()}
	if o.BinPath != "" {
		a.proc = core.NewProcess("hysteria", o.BinPath, o.WorkDir, o.Logger, "server", "-c", filepath.Join(o.WorkDir, "hysteria.json"))
	}
	return a
}

func (a *Adapter) Type() model.CoreType { return model.CoreHysteria }

func (a *Adapter) Capabilities(v string) model.CapSet {
	return model.CapSet{
		Core:     model.CoreHysteria,
		Version:  v,
		Combos:   []model.Combo{{Protocol: model.ProtoHysteria2, Transport: model.TransQUIC, Security: model.SecTLS}},
		HotUsers: true,
	}
}

// Render emits the server config (Hysteria reads JSON as well as YAML). TLS
// material is written next to it because Hysteria only takes file paths.
func (a *Adapter) Render(spec *model.NodeSpec) ([]byte, error) {
	ins := spec.InboundsFor(model.CoreHysteria)
	if len(ins) == 0 {
		return []byte("{}"), nil
	}
	if len(ins) > 1 {
		return nil, fmt.Errorf("hysteria core serves one inbound, got %d", len(ins))
	}
	in := ins[0]
	if in.Security.CertPEM == "" {
		return nil, fmt.Errorf("inbound %s: tls requires a certificate", in.Tag)
	}
	cert := filepath.Join(a.opt.WorkDir, "hysteria.crt")
	key := filepath.Join(a.opt.WorkDir, "hysteria.key")
	if a.opt.WorkDir != "" {
		if err := core.AtomicWrite(cert, []byte(in.Security.CertPEM), 0o600); err != nil {
			return nil, err
		}
		if err := core.AtomicWrite(key, []byte(in.Security.KeyPEM), 0o600); err != nil {
			return nil, err
		}
	}
	cfg := map[string]any{
		"listen": fmt.Sprintf(":%d", in.Port),
		"tls":    map[string]any{"cert": cert, "key": key},
		"auth": map[string]any{
			"type": "http",
			"http": map[string]any{"url": a.opt.AuthURL},
		},
		"trafficStats": map[string]any{"listen": a.opt.APIListen, "secret": a.secret},
	}
	if in.Protocol.UpMbps > 0 || in.Protocol.DownMbps > 0 {
		cfg["bandwidth"] = map[string]any{
			"up":   fmt.Sprintf("%d mbps", in.Protocol.UpMbps),
			"down": fmt.Sprintf("%d mbps", in.Protocol.DownMbps),
		}
	}
	if in.Protocol.Obfs == "salamander" {
		cfg["obfs"] = map[string]any{"type": "salamander", "salamander": map[string]any{"password": in.Protocol.ObfsPassword}}
	}
	if err := core.MergeExtra(cfg, in.Extra); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.last = spec.Clone()
	a.users = map[string]string{}
	for _, u := range in.Users {
		a.users[u.Password] = u.ID
	}
	a.mu.Unlock()
	return json.MarshalIndent(cfg, "", "  ")
}

// ServeHTTP answers Hysteria's auth callback.
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Addr string `json:"addr"`
		Auth string `json:"auth"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	a.mu.RLock()
	id, ok := a.users[req.Auth]
	a.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "id": id})
}

// SetUsers replaces the authorised users.
func (a *Adapter) SetUsers(_ string, us []model.User) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.users = map[string]string{}
	for _, u := range us {
		a.users[u.Password] = u.ID
	}
}

func (a *Adapter) Validate(cfg []byte) error {
	var m map[string]any
	return json.Unmarshal(cfg, &m)
}

func (a *Adapter) Start(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("hysteria binary not configured")
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
		return fmt.Errorf("hysteria binary not configured")
	}
	return a.proc.Restart(ctx)
}

// AddUsers / RemoveUsers only update the in-memory auth table.
func (a *Adapter) AddUsers(_ context.Context, _ string, us []model.User) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range us {
		a.users[u.Password] = u.ID
	}
	return nil
}

func (a *Adapter) RemoveUsers(_ context.Context, _ string, ids []string) error {
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for pw, id := range a.users {
		if drop[id] {
			delete(a.users, pw)
		}
	}
	return nil
}

func (a *Adapter) get(ctx context.Context, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.opt.APIListen+path, nil)
	req.Header.Set("Authorization", a.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hysteria api %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// CollectTraffic reads and clears the trafficStats counters. tx is bytes sent
// to the client (download), rx bytes received from it (upload).
func (a *Adapter) CollectTraffic(ctx context.Context) ([]model.TrafficDelta, error) {
	var m map[string]struct {
		TX int64 `json:"tx"`
		RX int64 `json:"rx"`
	}
	if err := a.get(ctx, "/traffic?clear=1", &m); err != nil {
		return nil, err
	}
	var out []model.TrafficDelta
	for id, v := range m {
		out = append(out, model.TrafficDelta{UserID: id, Up: v.RX, Down: v.TX})
	}
	return out, nil
}

func (a *Adapter) OnlineIPs(context.Context) (map[string][]string, error) {
	return nil, core.ErrNotSupported
}

func (a *Adapter) Health(context.Context) model.CoreStatus {
	st := model.CoreStatus{Type: model.CoreHysteria, Version: a.opt.Version}
	if a.proc != nil {
		st.Running = a.proc.Running()
		st.PID = a.proc.PID()
		st.StartedAt = a.proc.StartedAt()
	}
	a.mu.RLock()
	if a.last != nil {
		st.Inbounds = len(a.last.InboundsFor(model.CoreHysteria))
	}
	a.mu.RUnlock()
	return st
}

func randomSecret() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
