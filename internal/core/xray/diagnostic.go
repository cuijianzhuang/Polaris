package xray

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/miekg/dns"
)

// RunRelayDiagnostic uses a new identity only in this running core. The agent
// serializes it with config application. It never changes the rendered config,
// last spec or panel user rows. A process without parent-death enforcement must
// not accept temporary identities.
func (a *Adapter) RunRelayDiagnostic(ctx context.Context, req model.RelayBusinessRequest) (result model.RelayBusinessResult, err error) {
	result = model.RelayBusinessResult{State: "unsupported", Checks: []model.BusinessCheck{}}
	if a.proc == nil || !a.proc.BindToParent || !core.ParentBoundSupported() {
		return result, fmt.Errorf("diagnostics require a Linux parent-bound Xray process")
	}
	in, err := a.inbound(req.InboundTag)
	if err != nil {
		return result, err
	}
	if in.Protocol.Type != model.ProtoVLESS {
		return result, fmt.Errorf("business diagnostics currently support Xray VLESS only")
	}
	if !a.proc.Running() {
		return result, fmt.Errorf("Xray is not running")
	}
	if req.EntryAddress == "" || req.EntryPort < 1 || req.EntryPort > 65535 {
		return result, fmt.Errorf("invalid entry address or port")
	}
	tcpTarget, err := url.Parse(req.TCPURL)
	if err != nil || tcpTarget.Hostname() == "" || tcpTarget.User != nil || (tcpTarget.Scheme != "http" && tcpTarget.Scheme != "https") {
		return result, fmt.Errorf("tcpUrl must be an HTTP(S) URL without credentials")
	}
	tcpPort := tcpTarget.Port()
	if tcpPort == "" {
		tcpPort = "80"
		if tcpTarget.Scheme == "https" {
			tcpPort = "443"
		}
	}
	tcpNum, e := strconv.Atoi(tcpPort)
	if e != nil || tcpNum < 1 || tcpNum > 65535 {
		return result, fmt.Errorf("invalid TCP target port")
	}
	udpHost, udpPort, e := net.SplitHostPort(req.UDPAddress)
	udpNum, e2 := strconv.Atoi(udpPort)
	if e != nil || e2 != nil || udpHost == "" || udpNum < 1 || udpNum > 65535 {
		return result, fmt.Errorf("udpAddress must be a DNS server host:port")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if req.ExpiresAt > 0 {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithDeadline(ctx, time.Unix(req.ExpiresAt, 0))
		defer deadlineCancel()
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	user := model.User{ID: model.DiagnosticUserPrefix + pki.Token(16), UUID: pki.UUID(), Password: pki.Password(24)}
	result.State = "failed"
	// Even an AddUsers RPC that times out may have reached the core. Always
	// attempt revocation; if it cannot be confirmed, stop and restart from the
	// stable on-disk configuration, which contains no diagnostic identity.
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 2*time.Second)
		removeErr := a.RemoveUsers(cleanup, in.Tag, []string{user.ID})
		done()
		if removeErr != nil {
			stopCtx, stop := context.WithCancel(context.Background())
			stop()
			if stopErr := a.Stop(stopCtx); stopErr == nil {
				result.CleanupVerified = true
				a.mu.Lock()
				if a.api != nil {
					_ = a.api.Close()
					a.api = nil
				}
				a.mu.Unlock()
				startCtx, startDone := context.WithTimeout(context.Background(), 5*time.Second)
				restartErr := a.Start(startCtx)
				startDone()
				if restartErr != nil {
					result.Checks = append(result.Checks, model.BusinessCheck{Kind: "core_recovery", State: "failed", Message: "临时身份已通过停止内核撤销，但正式配置重启失败"})
				}
			}
			result.State = "failed"
			result.BusinessVerified = false
			result.Checks = append(result.Checks, model.BusinessCheck{Kind: "credential_cleanup", State: "failed", Message: "热删除未确认，已执行内核停止恢复"})
			return
		}
		result.CleanupVerified = true
		result.Checks = append(result.Checks, model.BusinessCheck{Kind: "credential_cleanup", State: "passed"})
	}()
	if err = a.AddUsers(ctx, in.Tag, []model.User{user}); err != nil {
		return result, fmt.Errorf("temporary identity could not be installed")
	}
	dir, err := os.MkdirTemp(a.opt.WorkDir, "diagnostic-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	var client *core.Process
	defer func() {
		if client != nil {
			stopCtx, stop := context.WithCancel(context.Background())
			stop()
			_ = client.Stop(stopCtx)
		}
	}()
	// The client has only fixed-target loopback listeners and a single proxy
	// outbound, with no direct fallback. No SOCKS/open proxy is exposed.
	tcpLocal, err := diagnosticPort()
	if err != nil {
		return result, err
	}
	udpLocal, err := diagnosticPort()
	if err != nil {
		return result, err
	}
	cfg, err := a.diagnosticClient(in, req, user, tcpLocal, udpLocal, tcpTarget.Hostname(), tcpNum, udpHost, udpNum)
	if err != nil {
		return result, err
	}
	configPath := filepath.Join(dir, "client.json")
	if err = core.AtomicWrite(configPath, cfg, 0600); err != nil {
		return result, err
	}
	client = core.NewProcess("diagnostic-client", a.opt.BinPath, dir, a.opt.Logger, "run", "-c", configPath)
	client.BindToParent = true
	if err = client.Start(ctx); err != nil {
		return result, fmt.Errorf("diagnostic client could not start")
	}
	check := func(kind string, run func() error) {
		start := time.Now()
		e := run()
		state, message := "passed", ""
		if e != nil {
			state = "failed"
			message = "代理认证、目标连接或数据校验失败"
		}
		result.Checks = append(result.Checks, model.BusinessCheck{Kind: kind, State: state, Message: message, LatencyMs: time.Since(start).Milliseconds()})
	}
	check("proxy_tcp_business", func() error { return diagnosticHTTP(ctx, req.TCPURL, tcpLocal) })
	check("proxy_udp_business", func() error { return diagnosticDNS(ctx, udpLocal) })
	result.BusinessVerified = true
	for _, c := range result.Checks {
		if c.State != "passed" {
			result.BusinessVerified = false
		}
	}
	if result.BusinessVerified {
		result.State = "passed"
	}
	return result, nil
}

func diagnosticPort() (int, error) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return 0, e
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (a *Adapter) diagnosticClient(in *model.Inbound, req model.RelayBusinessRequest, user model.User, tcpLocal, udpLocal int, tcpHost string, tcpPort int, udpHost string, udpPort int) ([]byte, error) {
	t := in.Transport
	sec := &model.ClientTLS{Type: in.Security.Type, ServerName: in.Security.ServerName, ALPN: in.Security.ALPN}
	if sec.Type == model.SecReality {
		if len(in.Security.RealityServerNames) == 0 || len(in.Security.RealityShortIDs) == 0 {
			return nil, fmt.Errorf("REALITY settings incomplete")
		}
		sec.ServerName = in.Security.RealityServerNames[0]
		sec.PublicKey = in.Security.RealityPublicKey
		sec.ShortID = in.Security.RealityShortIDs[0]
		sec.Fingerprint = "chrome"
		sec.SpiderX = in.Security.RealitySpiderX
		sec.Mldsa65Verify = in.Security.RealityMldsa65Verify
	}
	if sec.Type == model.SecTLS && sec.ServerName == "" {
		sec.ServerName = orDefault(req.ServerName, req.EntryAddress)
	}
	settings, _ := json.Marshal(model.OutboundSettings{Server: req.EntryAddress, Port: req.EntryPort, UUID: user.UUID, Flow: in.Protocol.Flow, Encryption: in.Protocol.Encryption, Transport: &t, Security: sec})
	outs, _, _, err := renderOutbounds([]model.Outbound{{Tag: "diagnostic", Type: model.OutVLESS, Settings: settings}}, !core.VersionAtLeast(a.opt.Version, "26.5.9"))
	if err != nil {
		return nil, err
	}
	var proxy map[string]any
	for _, o := range outs {
		if o["tag"] == "diagnostic" {
			proxy = o
		}
	}
	if proxy == nil {
		return nil, fmt.Errorf("missing proxy outbound")
	}
	if sec.Type == model.SecTLS && in.Security.CertPEM != "" {
		stream := proxy["streamSettings"].(map[string]any)
		tlsSettings := stream["tlsSettings"].(map[string]any)
		tlsSettings["certificates"] = []any{map[string]any{"usage": "verify", "certificate": core.PEMLines(in.Security.CertPEM)}}
	}
	return json.Marshal(map[string]any{"log": map[string]any{"loglevel": "none"}, "inbounds": []any{
		map[string]any{"tag": "tcp", "listen": "127.0.0.1", "port": tcpLocal, "protocol": "dokodemo-door", "settings": map[string]any{"address": tcpHost, "port": tcpPort, "network": "tcp"}},
		map[string]any{"tag": "udp", "listen": "127.0.0.1", "port": udpLocal, "protocol": "dokodemo-door", "settings": map[string]any{"address": udpHost, "port": udpPort, "network": "udp"}},
	}, "outbounds": []any{proxy}})
}

func diagnosticHTTP(ctx context.Context, target string, port int) error {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return err
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	req.Header.Set("X-Polaris-Diagnostic", hex.EncodeToString(nonce))
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("HTTP status %d", res.StatusCode)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	return err
}

func diagnosticDNS(ctx context.Context, port int) error {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	name := hex.EncodeToString(nonce) + ".invalid."
	query := new(dns.Msg)
	query.SetQuestion(name, dns.TypeA)
	wire, err := query.Pack()
	if err != nil {
		return err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(4 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err = conn.Write(wire); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	var response dns.Msg
	if err = response.Unpack(buf[:n]); err != nil {
		return err
	}
	if !response.Response || response.Id != query.Id || len(response.Question) != 1 || !strings.EqualFold(response.Question[0].Name, name) || response.Question[0].Qtype != dns.TypeA || (response.Rcode != dns.RcodeSuccess && response.Rcode != dns.RcodeNameError) {
		return fmt.Errorf("invalid DNS reply")
	}
	return nil
}
