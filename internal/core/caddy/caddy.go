// Package caddy runs Caddy as a TLS front so several CDN inbounds share one
// port (443): Caddy terminates TLS and routes by path to loopback inbounds.
package caddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() {
	core.Register(model.CoreCaddy, func(o core.Options) core.CoreAdapter { return New(o) })
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
	if o.APIListen == "" {
		o.APIListen = "127.0.0.1:10089"
	}
	a := &Adapter{opt: o}
	if o.BinPath != "" {
		a.proc = core.NewProcess("caddy", o.BinPath, o.WorkDir, o.Logger, "run", "--config", a.configPath(), "--adapter", "caddyfile")
		a.proc.BindToParent = o.BindToParent
		a.proc.Env = []string{"XDG_DATA_HOME=" + o.WorkDir, "XDG_CONFIG_HOME=" + o.WorkDir, "HOME=" + o.WorkDir}
	}
	return a
}

func (a *Adapter) Type() model.CoreType { return model.CoreCaddy }

func (a *Adapter) configPath() string { return filepath.Join(a.opt.WorkDir, "Caddyfile") }

func (a *Adapter) Capabilities(v string) model.CapSet {
	return model.CapSet{Core: model.CoreCaddy, Version: v}
}

// Render writes certificates next to the config and returns the Caddyfile.
func (a *Adapter) Render(spec *model.NodeSpec) ([]byte, error) {
	type site struct {
		domain string
		port   int
		cert   string
		sum    string // digest of the certificate and key
		rules  []model.FrontRule
	}
	sites := map[string]*site{}
	var keys []string
	for _, f := range spec.Fronts {
		key := fmt.Sprintf("%s:%d", f.Domain, f.Port)
		st, ok := sites[key]
		if !ok {
			st = &site{domain: f.Domain, port: f.Port}
			sites[key] = st
			keys = append(keys, key)
		}
		if f.CertPEM != "" && st.cert == "" {
			name := strings.NewReplacer("*", "_", ":", "_").Replace(f.Domain)
			st.cert = name
			sum := sha256.Sum256([]byte(f.CertPEM + f.KeyPEM))
			st.sum = hex.EncodeToString(sum[:8])
			if a.opt.WorkDir != "" {
				dir := filepath.Join(a.opt.WorkDir, "certs")
				if err := core.AtomicWrite(filepath.Join(dir, name+".crt"), []byte(f.CertPEM), 0o600); err != nil {
					return nil, err
				}
				if err := core.AtomicWrite(filepath.Join(dir, name+".key"), []byte(f.KeyPEM), 0o600); err != nil {
					return nil, err
				}
			}
		}
		st.rules = append(st.rules, f)
	}
	sort.Strings(keys)
	var b strings.Builder
	// Trust the visitor header only from an actual Cloudflare socket peer.
	// Ignore client-supplied XFF chains and replace forwarding metadata below.
	fmt.Fprintf(&b, "{\n\tadmin %s\n\tauto_https off\n\tpersist_config off\n\tservers {\n\t\ttrusted_proxies static %s\n\t\ttrusted_proxies_strict\n\t\tclient_ip_headers CF-Connecting-IP\n\t}\n\tlog {\n\t\tlevel WARN\n\t}\n}\n",
		a.opt.APIListen, strings.Join(model.CloudflareRanges, " "))
	for _, k := range keys {
		st := sites[k]
		fmt.Fprintf(&b, "\nhttps://%s:%d {\n", st.domain, st.port)
		if st.cert == "" {
			return nil, fmt.Errorf("front %s has no certificate", k)
		}
		dir := filepath.Join(a.opt.WorkDir, "certs")
		fmt.Fprintf(&b, "\ttls %s %s\n", filepath.Join(dir, st.cert+".crt"), filepath.Join(dir, st.cert+".key"))
		// The files are referenced by path: a digest of their content makes a
		// renewed certificate change the config, so Caddy reloads it.
		fmt.Fprintf(&b, "\t# cert %s\n", st.sum)
		// Longest paths first so a short prefix never shadows a longer one.
		sort.SliceStable(st.rules, func(i, j int) bool { return len(st.rules[i].Path) > len(st.rules[j].Path) })
		for i, r := range st.rules {
			p := "/" + strings.Trim(r.Path, "/")
			upstream := r.Upstream
			if r.H2C {
				upstream = "h2c://" + upstream
			}
			fmt.Fprintf(&b, "\t@r%d path %s %s/*\n", i, p, p)
			fmt.Fprintf(&b, "\thandle @r%d {\n\t\treverse_proxy %s {\n\t\t\tflush_interval -1\n\t\t\theader_up -CF-Connecting-IP\n\t\t\theader_up X-Polaris-Client-IP {http.vars.client_ip}\n\t\t\theader_up X-Forwarded-For {http.vars.client_ip}\n\t\t}\n\t}\n", i, upstream)
		}
		b.WriteString("\thandle {\n\t\trespond 404\n\t}\n}\n")
	}
	a.mu.Lock()
	a.n = len(spec.Fronts)
	a.mu.Unlock()
	return []byte(b.String()), nil
}

// Validate runs `caddy validate`.
func (a *Adapter) Validate(cfg []byte) error {
	if a.opt.BinPath == "" {
		return fmt.Errorf("caddy binary not configured")
	}
	f, err := core.WriteTemp(a.opt.WorkDir, "Caddyfile-check-*", cfg)
	if err != nil {
		return err
	}
	defer os.Remove(f)
	return core.RunCheck(a.opt.BinPath, "validate", "--config", f, "--adapter", "caddyfile")
}

func (a *Adapter) Start(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("caddy binary not configured")
	}
	return a.proc.Start(ctx)
}

func (a *Adapter) Stop(ctx context.Context) error {
	if a.proc == nil {
		return nil
	}
	return a.proc.Stop(ctx)
}

// Reload pushes the Caddyfile to the admin API (graceful), restarting on failure.
func (a *Adapter) Reload(ctx context.Context) error {
	if a.proc == nil {
		return fmt.Errorf("caddy binary not configured")
	}
	if !a.proc.Running() {
		return a.proc.Start(ctx)
	}
	cfg, err := os.ReadFile(a.configPath())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+a.opt.APIListen+"/load", bytes.NewReader(cfg))
	req.Header.Set("Content-Type", "text/caddyfile")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
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
	st := model.CoreStatus{Type: model.CoreCaddy, Version: a.opt.Version}
	if a.proc != nil {
		st.Running, st.PID, st.StartedAt = a.proc.Running(), a.proc.PID(), a.proc.StartedAt()
		if err := a.proc.LastError(); err != nil && !st.Running {
			st.Error = err.Error()
		}
	}
	a.mu.Lock()
	st.Inbounds = a.n
	a.mu.Unlock()
	return st
}

// Logs returns recent process output.
func (a *Adapter) Logs(n int) []string {
	if a.proc == nil {
		return nil
	}
	return a.proc.Logs(n)
}
