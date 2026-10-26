package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
)

// RunRelayDiagnostic replaces the shared core with a parent-bound, leased
// process. sing-box has no VLESS user API: SIGHUP would persist the diagnostic
// credential in its restart configuration. Never modify that stable file.
// The Agent holds applyMu throughout this operation, including recovery.
func (a *Adapter) RunRelayDiagnostic(ctx context.Context, req model.RelayBusinessRequest) (result model.RelayBusinessResult, err error) {
	result = model.RelayBusinessResult{State: "unsupported", Checks: []model.BusinessCheck{}}
	if a.proc == nil || !a.proc.BindToParent || !core.ParentBoundSupported() || !a.stats {
		return result, fmt.Errorf("sing-box diagnostics require Linux parent-death enforcement and with_v2ray_api")
	}
	if a.opt.Version == "" || a.opt.Version == "latest" || strings.Contains(a.opt.Version, "-") || !core.VersionAtLeast(a.opt.Version, model.SingboxRelayBusinessMinVersion) {
		return result, fmt.Errorf("sing-box diagnostics require a known stable version >= %s", model.SingboxRelayBusinessMinVersion)
	}
	a.mu.Lock()
	var in *model.Inbound
	if a.last != nil {
		for _, candidate := range a.last.InboundsFor(model.CoreSingbox) {
			if candidate.Tag == req.InboundTag {
				copy := candidate
				in = &copy
				break
			}
		}
	}
	a.mu.Unlock()
	if in == nil || !model.SingboxRelayBusinessInbound(*in) {
		return result, fmt.Errorf("sing-box diagnostics support VLESS TCP REALITY/Vision without inbound overrides only")
	}
	if !a.proc.Running() {
		return result, fmt.Errorf("sing-box is not running")
	}
	if req.EntryAddress == "" || req.EntryPort < 1 || req.EntryPort > 65535 {
		return result, fmt.Errorf("invalid relay entry")
	}
	expires := time.Unix(req.ExpiresAt, 0)
	if !expires.After(time.Now()) || expires.After(time.Now().Add(30*time.Second)) {
		return result, fmt.Errorf("invalid diagnostic lease")
	}
	ctx, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	target, err := url.Parse(req.TCPURL)
	if err != nil || target.Hostname() == "" || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return result, fmt.Errorf("tcpUrl must be an HTTP(S) URL without credentials")
	}
	tcpPort := target.Port()
	if tcpPort == "" {
		tcpPort = "80"
		if target.Scheme == "https" {
			tcpPort = "443"
		}
	}
	tcpNum, e := strconv.Atoi(tcpPort)
	udpHost, udpPort, e2 := net.SplitHostPort(req.UDPAddress)
	udpNum, e3 := strconv.Atoi(udpPort)
	if e != nil || e2 != nil || e3 != nil || tcpNum < 1 || tcpNum > 65535 || udpHost == "" || udpNum < 1 || udpNum > 65535 {
		return result, fmt.Errorf("invalid diagnostic target port")
	}
	stable, err := os.ReadFile(a.configPath())
	if err != nil {
		return result, fmt.Errorf("cannot read stable sing-box configuration")
	}
	user := model.User{ID: model.DiagnosticUserPrefix + pki.Token(16), UUID: pki.UUID()}
	serverConfig, err := diagnosticServer(stable, *in, user)
	if err != nil {
		return result, err
	}
	dir, err := os.MkdirTemp(a.opt.WorkDir, "diagnostic-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	tcpLocal, err := core.DiagnosticPort()
	if err != nil {
		return result, err
	}
	udpLocal, err := core.DiagnosticPort()
	if err != nil {
		return result, err
	}
	clientConfig, err := diagnosticClient(*in, req, user, tcpLocal, udpLocal, target.Hostname(), tcpNum, udpHost, udpNum)
	if err != nil {
		return result, err
	}
	serverPath, clientPath := filepath.Join(dir, "server.json"), filepath.Join(dir, "client.json")
	if err = core.AtomicWrite(serverPath, serverConfig, 0600); err != nil {
		return result, err
	}
	if err = core.AtomicWrite(clientPath, clientConfig, 0600); err != nil {
		return result, err
	}
	for _, path := range []string{serverPath, clientPath} {
		if core.RunCheck(a.opt.BinPath, "check", "-c", path) != nil {
			return result, fmt.Errorf("sing-box rejected diagnostic configuration")
		}
	}
	if err = ctx.Err(); err != nil {
		return result, err // Do not interrupt the shared core for an expired job.
	}
	server := core.NewProcess("sing-box-diagnostic-server", a.opt.BinPath, a.opt.WorkDir, a.opt.Logger, "run", "-c", serverPath, "-D", a.opt.WorkDir)
	client := core.NewProcess("sing-box-diagnostic-client", a.opt.BinPath, dir, a.opt.Logger, "run", "-c", clientPath)
	server.BindToParent, client.BindToParent = true, true
	server.Deadline, client.Deadline = expires, expires
	server.Filter = a.online.line
	result.State = "failed"
	// Stop uses an already-canceled context to SIGKILL and wait, rather than
	// leaving an accepted identity alive during a graceful shutdown timeout.
	kill := func(process *core.Process) {
		stopCtx, done := context.WithCancel(context.Background())
		done()
		_ = process.Stop(stopCtx)
	}
	var stopLease func() bool
	leaseDone := make(chan struct{})
	tested := false
	defer func() {
		kill(client)
		// Preserve ordinary-user deltas before destroying the temporary core.
		// Diagnostic counters travel only in this result, never to billing.
		var up, down int64
		statsCtx, statsDone := context.WithTimeout(context.Background(), time.Second)
		deltas, statsErr := a.collectTraffic(statsCtx)
		statsDone()
		a.mu.Lock()
		for _, delta := range deltas {
			if delta.UserID == user.ID {
				up, down = up+delta.Up, down+delta.Down
			} else {
				a.diagnosticTraffic = append(a.diagnosticTraffic, delta)
			}
		}
		a.mu.Unlock()
		kill(server)
		if stopLease != nil && !stopLease() {
			<-leaseDone // Join before recovery; the lease also covers stats draining.
		}
		result.CleanupVerified = !server.Running() && !client.Running()
		cleanupState := "failed"
		if result.CleanupVerified {
			cleanupState = "passed"
		}
		result.Checks = append(result.Checks, model.BusinessCheck{Kind: "credential_cleanup", State: cleanupState})
		if tested {
			state := "failed"
			if statsErr == nil && up > 0 && down > 0 {
				state = "passed"
			}
			result.Checks = append(result.Checks, model.BusinessCheck{Kind: "proxy_authentication", State: state},
				model.BusinessCheck{Kind: "proxy_transfer", State: state, UpBytes: up, DownBytes: down})
		}
		// Reopen only the stable configuration, even on cancellation or errors.
		recoverCtx, recoverDone := context.WithTimeout(context.Background(), 4*time.Second)
		recoverErr := a.Start(recoverCtx)
		recoverDone()
		if recoverErr != nil {
			result.Checks = append(result.Checks, model.BusinessCheck{Kind: "core_recovery", State: "failed", Message: "临时身份已撤销，但正式配置恢复失败"})
		}
		result.BusinessVerified = tested && err == nil && ctx.Err() == nil && result.CleanupVerified
		for _, check := range result.Checks {
			result.BusinessVerified = result.BusinessVerified && check.State == "passed"
		}
		if result.BusinessVerified {
			result.State = "passed"
		}
	}()
	if err = a.Stop(ctx); err != nil {
		return result, fmt.Errorf("could not stop stable sing-box process")
	}
	if err = server.Start(ctx); err != nil {
		return result, fmt.Errorf("temporary sing-box server could not start")
	}
	stopLease = context.AfterFunc(ctx, func() {
		defer close(leaseDone)
		kill(server)
	})
	if err = client.Start(ctx); err != nil {
		return result, fmt.Errorf("temporary sing-box client could not start")
	}
	tested = true
	for _, probe := range []struct {
		kind string
		run  func() error
	}{
		{"proxy_tcp_business", func() error { return core.DiagnosticHTTP(ctx, req.TCPURL, tcpLocal) }},
		{"proxy_udp_business", func() error { return core.DiagnosticDNS(ctx, udpLocal) }},
	} {
		start := time.Now()
		state, message := "passed", ""
		if probe.run() != nil {
			state, message = "failed", "代理认证、目标连接或数据校验失败"
		}
		result.Checks = append(result.Checks, model.BusinessCheck{Kind: probe.kind, State: state, Message: message, LatencyMs: time.Since(start).Milliseconds()})
	}
	return result, nil
}

func diagnosticServer(stable []byte, in model.Inbound, user model.User) ([]byte, error) {
	var doc struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if json.Unmarshal(stable, &doc) != nil || strings.Contains(string(stable), model.DiagnosticUserPrefix) {
		return nil, fmt.Errorf("invalid stable sing-box configuration")
	}
	var cfg map[string]any
	if json.Unmarshal(stable, &cfg) != nil {
		return nil, fmt.Errorf("invalid stable sing-box configuration")
	}
	found := false
	for _, inbound := range doc.Inbounds {
		if inbound["tag"] != in.Tag {
			continue
		}
		// Refuse stale/uncommitted adapter metadata or native overrides.
		tls, _ := inbound["tls"].(map[string]any)
		reality, _ := tls["reality"].(map[string]any)
		if found || inbound["type"] != "vless" || inbound["transport"] != nil || inbound["listen_port"] != float64(in.Port) || reality["private_key"] != in.Security.RealityPrivateKey {
			return nil, fmt.Errorf("active sing-box inbound does not match diagnostic scope")
		}
		users, _ := inbound["users"].([]any)
		inbound["users"] = append(users, map[string]any{"name": user.ID, "uuid": user.UUID, "flow": in.Protocol.Flow})
		found = true
	}
	if !found {
		return nil, fmt.Errorf("sing-box inbound not found")
	}
	cfg["inbounds"] = doc.Inbounds
	experimental, _ := cfg["experimental"].(map[string]any)
	api, _ := experimental["v2ray_api"].(map[string]any)
	stats, _ := api["stats"].(map[string]any)
	if stats == nil || stats["enabled"] != true {
		return nil, fmt.Errorf("sing-box per-user statistics are not enabled")
	}
	users, _ := stats["users"].([]any)
	stats["users"] = append(users, user.ID)
	return json.Marshal(cfg)
}

func diagnosticClient(in model.Inbound, req model.RelayBusinessRequest, user model.User, tcpLocal, udpLocal int, tcpHost string, tcpPort int, udpHost string, udpPort int) ([]byte, error) {
	sec := in.Security
	if sec.RealityPublicKey == "" || len(sec.RealityServerNames) == 0 || len(sec.RealityShortIDs) == 0 {
		return nil, fmt.Errorf("incomplete REALITY configuration")
	}
	tls := renderClientTLS(&model.ClientTLS{Type: model.SecReality, ServerName: sec.RealityServerNames[0], PublicKey: sec.RealityPublicKey, ShortID: sec.RealityShortIDs[0], Fingerprint: "chrome"})
	return json.Marshal(map[string]any{
		"log": map[string]any{"disabled": true},
		"inbounds": []any{
			map[string]any{"type": "direct", "tag": "tcp", "listen": "127.0.0.1", "listen_port": tcpLocal, "network": "tcp", "override_address": tcpHost, "override_port": tcpPort},
			map[string]any{"type": "direct", "tag": "udp", "listen": "127.0.0.1", "listen_port": udpLocal, "network": "udp", "override_address": udpHost, "override_port": udpPort},
		},
		"outbounds": []any{map[string]any{"type": "vless", "tag": "diagnostic", "server": req.EntryAddress, "server_port": req.EntryPort, "uuid": user.UUID, "flow": in.Protocol.Flow, "tls": tls, "packet_encoding": "xudp"}},
		"route":     map[string]any{"final": "diagnostic"},
	})
}
