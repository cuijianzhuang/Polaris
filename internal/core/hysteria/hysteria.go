// Package hysteria adapts the official Hysteria2 server. Users are
// authenticated through an HTTP callback served by the agent, so user changes
// only touch agent memory and never reload the core. Each inbound runs as its
// own server process with its own traffic-stats port.
package hysteria

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func init() {
	core.Register(model.CoreHysteria, func(o core.Options) core.CoreAdapter { return New(o) })
}

type instance struct {
	tag    string
	api    string
	cfg    string
	proc   *core.Process
	hash   string
	active bool
}

// Adapter implements core.CoreAdapter and http.Handler (the auth callback).
type Adapter struct {
	opt    core.Options
	secret string

	mu    sync.RWMutex
	last  *model.NodeSpec
	users map[string]map[string]string // inbound tag -> password -> user id
	insts map[string]*instance

	cmu     sync.Mutex
	clients map[string][]client // user id -> where its connections authenticated from
}

func New(o core.Options) *Adapter {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.APIListen == "" {
		o.APIListen = "127.0.0.1:10095"
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return &Adapter{opt: o, secret: hex.EncodeToString(b), users: map[string]map[string]string{}, insts: map[string]*instance{}}
}

func (a *Adapter) Type() model.CoreType { return model.CoreHysteria }

func (a *Adapter) Capabilities(v string) model.CapSet {
	return model.CapSet{
		Core:      model.CoreHysteria,
		Version:   v,
		Combos:    []model.Combo{{Protocol: model.ProtoHysteria2, Transport: model.TransQUIC, Security: model.SecTLS}},
		HotUsers:  true,
		Outbounds: hyOutbounds,
	}
}

// statsAddr gives inbound i its own loopback stats port (base + i).
func (a *Adapter) statsAddr(i int) string {
	host, port, err := net.SplitHostPort(a.opt.APIListen)
	if err != nil {
		return a.opt.APIListen
	}
	p, _ := strconv.Atoi(port)
	return net.JoinHostPort(host, strconv.Itoa(p+i))
}

func safeName(tag string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, tag)
}

func listenHost(l string) string {
	if l == "" || l == "::" || l == "0.0.0.0" {
		return ""
	}
	if strings.Contains(l, ":") {
		return "[" + l + "]"
	}
	return l
}

// Render writes one config (plus TLS files) per inbound and returns their
// combined JSON, which the agent stores as the core's config fingerprint.
func (a *Adapter) Render(spec *model.NodeSpec) ([]byte, error) {
	ins := spec.InboundsFor(model.CoreHysteria)
	sort.Slice(ins, func(i, j int) bool { return ins[i].Tag < ins[j].Tag })
	combined := map[string]any{}
	users := map[string]map[string]string{}
	next := map[string]*instance{}
	for i, in := range ins {
		if in.Security.CertPEM == "" {
			return nil, fmt.Errorf("inbound %s: tls requires a certificate", in.Tag)
		}
		name := safeName(in.Tag)
		cert := filepath.Join(a.opt.WorkDir, "hy-"+name+".crt")
		key := filepath.Join(a.opt.WorkDir, "hy-"+name+".key")
		if a.opt.WorkDir != "" {
			if err := core.AtomicWrite(cert, []byte(in.Security.CertPEM), 0o600); err != nil {
				return nil, err
			}
			if err := core.AtomicWrite(key, []byte(in.Security.KeyPEM), 0o600); err != nil {
				return nil, err
			}
		}
		auth := a.opt.AuthURL
		if auth != "" {
			sep := "?"
			if strings.Contains(auth, "?") {
				sep = "&"
			}
			auth += sep + "tag=" + url.QueryEscape(in.Tag)
		}
		stats := a.statsAddr(i)
		cfg := map[string]any{
			"listen":       fmt.Sprintf("%s:%d", listenHost(in.Listen), in.Port),
			"tls":          map[string]any{"cert": cert, "key": key},
			"auth":         map[string]any{"type": "http", "http": map[string]any{"url": auth}},
			"trafficStats": map[string]any{"listen": stats, "secret": a.secret},
		}
		if in.Protocol.UpMbps > 0 || in.Protocol.DownMbps > 0 {
			cfg["bandwidth"] = map[string]any{"up": fmt.Sprintf("%d mbps", in.Protocol.UpMbps), "down": fmt.Sprintf("%d mbps", in.Protocol.DownMbps)}
		}
		if in.Protocol.Obfs == "salamander" {
			cfg["obfs"] = map[string]any{"type": "salamander", "salamander": map[string]any{"password": in.Protocol.ObfsPassword}}
		}
		mode := ""
		if d := spec.DNS; d != nil {
			if r := renderResolver(d); r != nil {
				cfg["resolver"] = r
			}
			mode = directMode(d.Strategy)
		}
		if outs, acl := renderRouting(spec, in.Tag, mode); outs != nil {
			cfg["outbounds"] = outs
			if len(acl) > 0 {
				// geoip / geosite lists are fetched by Hysteria into its working directory.
				cfg["acl"] = map[string]any{"inline": acl, "geoUpdateInterval": "168h"}
			}
		}
		if err := core.MergeExtra(cfg, in.Extra); err != nil {
			return nil, err
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		path := filepath.Join(a.opt.WorkDir, "hy-"+name+".json")
		if a.opt.WorkDir != "" {
			if err := core.AtomicWrite(path, b, 0o600); err != nil {
				return nil, err
			}
		}
		// The config refers to the cert files by path: put a digest of their
		// content in the comparison so a renewal restarts the instance.
		sum := sha256.Sum256([]byte(in.Security.CertPEM + in.Security.KeyPEM))
		certSum := hex.EncodeToString(sum[:8])
		combined[in.Tag] = map[string]any{"config": cfg, "certSha256": certSum}
		m := map[string]string{}
		for _, u := range in.Users {
			m[u.Password] = u.ID
		}
		users[in.Tag] = m
		next[in.Tag] = &instance{tag: in.Tag, api: stats, cfg: path, hash: string(b) + certSum}
	}
	a.mu.Lock()
	a.last = spec.Clone()
	a.users = users
	for tag, nx := range next {
		if cur, ok := a.insts[tag]; ok {
			cur.api, cur.cfg = nx.api, nx.cfg
			if cur.hash != nx.hash {
				cur.hash = nx.hash
				cur.active = false // config changed: restart on Reload
			}
			next[tag] = cur
		}
	}
	for tag, cur := range a.insts {
		if _, ok := next[tag]; !ok && cur.proc != nil {
			go cur.proc.Stop(context.Background())
		}
	}
	a.insts = next
	a.mu.Unlock()
	return json.MarshalIndent(combined, "", "  ")
}

// ServeHTTP answers Hysteria's auth callback (?tag= selects the inbound).
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Addr string `json:"addr"`
		Auth string `json:"auth"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tag := r.URL.Query().Get("tag")
	a.mu.RLock()
	id, ok := "", false
	if tag != "" {
		id, ok = a.users[tag][req.Auth]
	} else {
		for _, m := range a.users {
			if id, ok = m[req.Auth]; ok {
				break
			}
		}
	}
	a.mu.RUnlock()
	if ok {
		a.noteClient(id, req.Addr)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "id": id})
}

// SetUsers replaces the authorised users of an inbound.
func (a *Adapter) SetUsers(tag string, us []model.User) {
	m := map[string]string{}
	for _, u := range us {
		m[u.Password] = u.ID
	}
	a.mu.Lock()
	a.users[tag] = m
	a.mu.Unlock()
}

func (a *Adapter) Validate(cfg []byte) error {
	var m map[string]any
	return json.Unmarshal(cfg, &m)
}

// ensure starts stopped instances and restarts those whose config changed.
func (a *Adapter) ensure(ctx context.Context) error {
	if a.opt.BinPath == "" {
		return fmt.Errorf("hysteria binary not configured")
	}
	a.mu.Lock()
	insts := make([]*instance, 0, len(a.insts))
	for _, in := range a.insts {
		insts = append(insts, in)
	}
	a.mu.Unlock()
	var errs []string
	for _, in := range insts {
		if in.proc == nil {
			in.proc = core.NewProcess("hysteria["+in.tag+"]", a.opt.BinPath, a.opt.WorkDir, a.opt.Logger, "server", "-c", in.cfg)
		}
		var err error
		switch {
		case !in.proc.Running():
			err = in.proc.Start(ctx)
		case !in.active:
			err = in.proc.Restart(ctx)
		}
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		in.active = true
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (a *Adapter) Start(ctx context.Context) error  { return a.ensure(ctx) }
func (a *Adapter) Reload(ctx context.Context) error { return a.ensure(ctx) }

func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	insts := a.insts
	a.mu.Unlock()
	for _, in := range insts {
		if in.proc != nil {
			_ = in.proc.Stop(ctx)
		}
	}
	return nil
}

// AddUsers / RemoveUsers only update the in-memory auth table.
func (a *Adapter) AddUsers(_ context.Context, tag string, us []model.User) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.users[tag] == nil {
		a.users[tag] = map[string]string{}
	}
	for _, u := range us {
		a.users[tag][u.Password] = u.ID
	}
	return nil
}

func (a *Adapter) RemoveUsers(_ context.Context, tag string, ids []string) error {
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for pw, id := range a.users[tag] {
		if drop[id] {
			delete(a.users[tag], pw)
		}
	}
	return nil
}

func (a *Adapter) get(ctx context.Context, addr, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
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

// CollectTraffic reads and clears every instance's counters. tx is bytes
// sent to the client (download), rx bytes received from it (upload).
func (a *Adapter) CollectTraffic(ctx context.Context) ([]model.TrafficDelta, error) {
	a.mu.RLock()
	var addrs []string
	for _, in := range a.insts {
		addrs = append(addrs, in.api)
	}
	a.mu.RUnlock()
	var all [][]model.TrafficDelta
	var lastErr error
	for _, addr := range addrs {
		var m map[string]struct {
			TX int64 `json:"tx"`
			RX int64 `json:"rx"`
		}
		if err := a.get(ctx, addr, "/traffic?clear=1", &m); err != nil {
			lastErr = err
			continue
		}
		var ds []model.TrafficDelta
		for id, v := range m {
			ds = append(ds, model.TrafficDelta{UserID: id, Up: v.RX, Down: v.TX})
		}
		all = append(all, ds)
	}
	if len(all) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return model.MergeDeltas(all...), nil
}

func (a *Adapter) Health(context.Context) model.CoreStatus {
	st := model.CoreStatus{Type: model.CoreHysteria, Version: a.opt.Version}
	a.mu.RLock()
	defer a.mu.RUnlock()
	running, total := 0, len(a.insts)
	for _, in := range a.insts {
		if in.proc != nil && in.proc.Running() {
			running++
			if st.PID == 0 {
				st.PID, st.StartedAt = in.proc.PID(), in.proc.StartedAt()
			}
		} else if in.proc != nil {
			if err := in.proc.LastError(); err != nil {
				st.Error = in.tag + ": " + err.Error()
			}
		}
	}
	st.Running = total > 0 && running == total
	st.Inbounds = total
	return st
}

// Logs returns recent output from every instance.
func (a *Adapter) Logs(n int) []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []string
	for _, in := range a.insts {
		if in.proc != nil {
			for _, l := range in.proc.Logs(n) {
				out = append(out, "["+in.tag+"] "+l)
			}
		}
	}
	return out
}
