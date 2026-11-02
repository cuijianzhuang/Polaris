// Package cloudflared supervises local-JSON and remote-token Cloudflare Tunnels.
package cloudflared

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"gopkg.in/yaml.v3"
)

func init() {
	core.Register(model.CoreCloudflared, func(o core.Options) core.CoreAdapter { return New(o) })
}

type child struct {
	spec     model.TunnelSpec
	config   string
	proc     *core.Process
	metrics  string
	ready    bool
	next     time.Time
	failures int
	reasonMu sync.Mutex
	reason   string
}
type Adapter struct {
	opt      core.Options
	op       sync.Mutex
	mu       sync.Mutex
	children map[string]*child
	cancel   context.CancelFunc
	done     chan struct{}
}

func New(o core.Options) *Adapter {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Adapter{opt: o, children: map[string]*child{}}
}
func (a *Adapter) Type() model.CoreType { return model.CoreCloudflared }
func (a *Adapter) Capabilities(v string) model.CapSet {
	return model.CapSet{Core: model.CoreCloudflared, Version: v}
}
func CredentialPath(work string, t model.TunnelSpec) string {
	ext := ".json"
	if t.CredentialMode == "remote" {
		ext = ".token"
	}
	return filepath.Join(work, "credentials", t.ProfileID, t.CredentialVersion+ext)
}
func (a *Adapter) configPath(t model.TunnelSpec) string {
	b, _ := json.Marshal(t)
	return filepath.Join(a.opt.WorkDir, "configs", t.ProfileID, fmt.Sprintf("%x.yml", sha256.Sum256(b)))
}
func (a *Adapter) Render(spec *model.NodeSpec) ([]byte, error) {
	if len(spec.Tunnels) > 32 {
		return nil, fmt.Errorf("at most 32 managed tunnels per node")
	}
	tunnels := append([]model.TunnelSpec{}, spec.Tunnels...)
	sort.Slice(tunnels, func(i, j int) bool { return tunnels[i].ProfileID < tunnels[j].ProfileID })
	seen := map[string]bool{}
	for _, t := range tunnels {
		if err := t.Validate(); err != nil {
			return nil, err
		}
		if t.CredentialMode == "remote" && !core.VersionAtLeast(a.opt.Version, model.RemoteTunnelMinVersion) {
			return nil, fmt.Errorf("远程 Tunnel Token 需要 cloudflared %s 或更新版本", model.RemoteTunnelMinVersion)
		}
		if seen[t.ProfileID] || seen[t.ID] {
			return nil, fmt.Errorf("duplicate managed tunnel")
		}
		seen[t.ProfileID] = true
		seen[t.ID] = true
		raw, err := os.ReadFile(CredentialPath(a.opt.WorkDir, t))
		if err != nil {
			return nil, fmt.Errorf("Tunnel 私有凭据文件不存在")
		}
		if _, err := model.NormalizeManagedTunnelCredential(raw, t.ID, t.CredentialMode); err != nil {
			return nil, fmt.Errorf("Tunnel 私有凭据无效")
		}
		credential, err := filepath.Abs(CredentialPath(a.opt.WorkDir, t))
		if err != nil {
			return nil, err
		}
		if t.CredentialMode == "remote" {
			// An explicit, empty local config prevents discovering a user's
			// unrelated config.yml. No local ingress or secret goes into it.
			b, _ := yaml.Marshal(map[string]string{"tunnel": t.ID})
			if err := os.MkdirAll(filepath.Dir(a.configPath(t)), 0700); err != nil {
				return nil, err
			}
			if err := core.AtomicWrite(a.configPath(t), b, 0600); err != nil {
				return nil, err
			}
			continue
		}
		cfg := struct {
			Tunnel     string              `yaml:"tunnel"`
			Credential string              `yaml:"credentials-file"`
			Ingress    []model.TunnelRoute `yaml:"ingress"`
		}{t.ID, credential, t.Routes}
		b, err := yaml.Marshal(cfg)
		if err != nil {
			return nil, fmt.Errorf("生成 Tunnel 回源配置失败")
		}
		if err := os.MkdirAll(filepath.Dir(a.configPath(t)), 0700); err != nil {
			return nil, err
		}
		if err := core.AtomicWrite(a.configPath(t), b, 0600); err != nil {
			return nil, err
		}
	}
	return json.Marshal(tunnels)
}
func (a *Adapter) Validate(cfg []byte) error {
	var tunnels []model.TunnelSpec
	if json.Unmarshal(cfg, &tunnels) != nil || len(tunnels) > 32 {
		return fmt.Errorf("invalid cloudflared manifest")
	}
	for _, t := range tunnels {
		if err := t.Validate(); err != nil {
			return err
		}
		if t.CredentialMode == "remote" {
			raw, err := os.ReadFile(CredentialPath(a.opt.WorkDir, t))
			if err != nil {
				return fmt.Errorf("Tunnel Token 文件不存在")
			}
			if _, err := model.NormalizeRemoteTunnelToken(raw, t.ID); err != nil {
				return fmt.Errorf("Tunnel Token 无效")
			}
			continue // Remote ingress is fetched by cloudflared, not locally validated.
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		// Never return tool output: even an invalid binary/config must not echo a key.
		command := exec.CommandContext(ctx, a.opt.BinPath, "tunnel", "--config", a.configPath(t), "ingress", "validate")
		command.Env = managedEnvironment()
		err := command.Run()
		cancel()
		if err != nil {
			return fmt.Errorf("cloudflared ingress 配置校验失败")
		}
	}
	return nil
}
func (a *Adapter) stopMonitor() {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.cancel = nil
	a.done = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
func (a *Adapter) stopChildren(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, c := range a.children {
		if err := c.proc.Stop(ctx); err != nil {
			return fmt.Errorf("停止 cloudflared 失败")
		}
		delete(a.children, id)
	}
	return nil
}
func (a *Adapter) makeChild(t model.TunnelSpec) (*child, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("分配 Tunnel 状态端口失败")
	}
	metrics := l.Addr().String()
	_ = l.Close()
	c := &child{spec: t, config: a.configPath(t), metrics: metrics}
	c.proc = core.NewProcess("cloudflared", a.opt.BinPath, a.opt.WorkDir, a.opt.Logger, "--no-autoupdate", "tunnel", "--config", c.config, "--metrics", metrics, "run")
	if t.CredentialMode == "remote" {
		path, err := filepath.Abs(CredentialPath(a.opt.WorkDir, t))
		if err != nil {
			return nil, err
		}
		c.proc.Args = append(c.proc.Args, "--token-file", path)
	}
	// Do not inherit cloudflared-specific credential, token or log-file
	// overrides. Keep the session proxy and CA environment untouched.
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "TUNNEL_") {
			c.proc.UnsetEnv = append(c.proc.UnsetEnv, name)
		}
	}
	c.proc.BindToParent = a.opt.BindToParent
	c.proc.Filter = func(line string) bool {
		low := strings.ToLower(line)
		reason := ""
		switch {
		case strings.Contains(low, "unauthorized") || strings.Contains(low, "invalid tunnel secret") || strings.Contains(low, "authentication failed"):
			reason = "Cloudflare 拒绝凭据，请核对隧道授权"
		case strings.Contains(low, "failed to dial") || strings.Contains(low, "timeout") || strings.Contains(low, "failed to resolve"):
			reason = "连接 Cloudflare 边缘失败，请检查出站网络"
		}
		if reason != "" {
			c.reasonMu.Lock()
			c.reason = reason
			c.reasonMu.Unlock()
		}
		return true // Child stdout/stderr can include credentials. Retain none of it.
	}
	return c, nil
}

func managedEnvironment() []string {
	out := []string{}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(name, "TUNNEL_") {
			out = append(out, value)
		}
	}
	return out
}
func (a *Adapter) apply(ctx context.Context) error {
	a.stopMonitor()
	raw, err := os.ReadFile(filepath.Join(a.opt.WorkDir, "tunnels.json"))
	if err != nil {
		return fmt.Errorf("cloudflared manifest missing")
	}
	var tunnels []model.TunnelSpec
	if json.Unmarshal(raw, &tunnels) != nil {
		return fmt.Errorf("invalid cloudflared manifest")
	}
	wanted := map[string]bool{}
	for _, t := range tunnels {
		if err := t.Validate(); err != nil {
			return err
		}
		wanted[t.ProfileID] = true
	}
	a.mu.Lock()
	for id, c := range a.children {
		if !wanted[id] {
			_ = c.proc.Stop(ctx)
			delete(a.children, id)
		}
	}
	for _, t := range tunnels {
		old := a.children[t.ProfileID]
		cfg := a.configPath(t)
		if old != nil && old.config == cfg && old.proc.Running() {
			continue
		}
		if old != nil {
			_ = old.proc.Stop(ctx)
			delete(a.children, t.ProfileID)
		}
		c, makeErr := a.makeChild(t)
		if makeErr == nil {
			makeErr = c.proc.Start(ctx)
		}
		if makeErr != nil {
			a.mu.Unlock()
			_ = a.stopChildren(context.Background())
			return fmt.Errorf("cloudflared 启动失败，请检查凭据、配置和出站网络")
		}
		a.children[t.ProfileID] = c
	}
	monitor, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.cancel = cancel
	a.done = done
	a.mu.Unlock()
	go func() { defer close(done); a.watch(monitor) }()
	return nil
}
func (a *Adapter) Start(ctx context.Context) error {
	a.op.Lock()
	defer a.op.Unlock()
	return a.apply(ctx)
}
func (a *Adapter) Reload(ctx context.Context) error {
	a.op.Lock()
	defer a.op.Unlock()
	return a.apply(ctx)
}
func (a *Adapter) Stop(ctx context.Context) error {
	a.op.Lock()
	defer a.op.Unlock()
	a.stopMonitor()
	return a.stopChildren(ctx)
}

var readyClient = &http.Client{Timeout: 400 * time.Millisecond, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}

func (a *Adapter) watch(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		a.mu.Lock()
		children := make([]*child, 0, len(a.children))
		for _, c := range a.children {
			children = append(children, c)
		}
		a.mu.Unlock()
		for _, c := range children {
			if ctx.Err() != nil {
				break
			}
			if !c.proc.Running() && time.Now().After(c.next) {
				err := c.proc.Start(ctx)
				c.failures++
				delay := 5 * time.Second * time.Duration(1<<min(c.failures, 4))
				c.next = time.Now().Add(delay)
				if err == nil {
					c.failures = 0
				}
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+c.metrics+"/ready", nil)
			resp, err := readyClient.Do(req)
			a.mu.Lock()
			c.ready = err == nil && resp.StatusCode == 200 && c.proc.Running()
			a.mu.Unlock()
			if resp != nil {
				resp.Body.Close()
			}
			if c.ready {
				c.reasonMu.Lock()
				c.reason = ""
				c.reasonMu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (a *Adapter) Health(context.Context) model.CoreStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := model.CoreStatus{Type: model.CoreCloudflared, Version: a.opt.Version, Running: len(a.children) > 0, Inbounds: len(a.children)}
	ready := 0
	for _, c := range a.children {
		st.Running = st.Running && c.proc.Running()
		if c.ready {
			ready++
		}
		if st.PID == 0 {
			st.PID = c.proc.PID()
			st.StartedAt = c.proc.StartedAt()
		}
		c.reasonMu.Lock()
		if c.reason != "" {
			st.Error = c.reason
		}
		c.reasonMu.Unlock()
	}
	if ready < len(a.children) && st.Error == "" {
		st.Error = fmt.Sprintf("Tunnel 就绪 %d/%d；等待 Cloudflare 连接", ready, len(a.children))
	}
	return st
}
func (a *Adapter) Logs(int) []string {
	st := a.Health(context.Background())
	if st.Error != "" {
		return []string{st.Error}
	}
	return nil
}
func (a *Adapter) AddUsers(context.Context, string, []model.User) error         { return core.ErrNotSupported }
func (a *Adapter) RemoveUsers(context.Context, string, []string) error          { return core.ErrNotSupported }
func (a *Adapter) CollectTraffic(context.Context) ([]model.TrafficDelta, error) { return nil, nil }
func (a *Adapter) OnlineIPs(context.Context) (map[string][]string, error) {
	return nil, core.ErrNotSupported
}
