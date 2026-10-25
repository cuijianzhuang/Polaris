package singbox

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/miekg/dns"
)

func diagnosticFixture(t *testing.T) (*Adapter, model.Inbound, []byte) {
	t.Helper()
	bin := os.Getenv("POLARIS_SINGBOX_BIN")
	if bin == "" {
		t.Skip("POLARIS_SINGBOX_BIN not set")
	}
	if !core.ParentBoundSupported() {
		t.Skip("Linux parent-death enforcement required")
	}
	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dest.EnableHTTP2 = true
	dest.StartTLS()
	t.Cleanup(dest.Close)
	in := coretest.Inbound(model.CoreSingbox, "landing", coretest.FreePort(t), model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"}, model.TransportSpec{}, coretest.Reality(t))
	in.Listen = "127.0.0.1"
	in.Security.RealityDest = strings.TrimPrefix(dest.URL, "https://")
	if destination := os.Getenv("POLARIS_DIAGNOSTIC_REALITY_DEST"); destination != "" {
		in.Security.RealityDest = destination
	}
	a := New(core.Options{Version: "1.13.5", BinPath: bin, WorkDir: t.TempDir(), APIListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(coretest.FreePort(t))), BindToParent: true})
	if !a.stats {
		t.Fatal("test binary must include with_v2ray_api")
	}
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}})
	if err != nil {
		t.Fatal(err)
	}
	if err = core.AtomicWrite(a.configPath(), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, in, cfg
}

// A real intermediate TCP listener: every native VLESS TCP/XUDP connection
// must traverse this entry rather than dialing the landing listener directly.
func diagnosticRelay(t *testing.T, target int) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			incoming, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer incoming.Close()
				outgoing, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(target)), time.Second)
				if err != nil {
					return
				}
				defer outgoing.Close()
				go func() { io.Copy(outgoing, incoming); outgoing.Close() }()
				io.Copy(incoming, outgoing)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func diagnosticUDP(t *testing.T, valid bool) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(q)
		r.Rcode = dns.RcodeNameError
		if !valid {
			r.Question[0].Name = "wrong.invalid."
		}
		w.WriteMsg(r)
	})}
	go s.ActivateAndServe()
	t.Cleanup(func() { s.Shutdown() })
	return pc.LocalAddr().String()
}

func diagnosticReplay(t *testing.T, a *Adapter, cfg []byte, target string) error {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "client.json")
	if err := os.WriteFile(path, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	p := core.NewProcess("credential-replay", a.opt.BinPath, dir, a.opt.Logger, "run", "-c", path)
	p.BindToParent = true
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())
	var doc struct {
		Inbounds []struct {
			Port int `json:"listen_port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(cfg, &doc); err != nil || len(doc.Inbounds) == 0 {
		t.Fatal("invalid captured client")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	return core.DiagnosticHTTP(ctx, target, doc.Inbounds[0].Port)
}

func TestLiveSingboxRelayDiagnostic(t *testing.T) {
	for _, scenario := range []string{"success", "bad-entry", "bad-udp", "untrusted-https", "disconnect", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			a, in, stable := diagnosticFixture(t)
			var captured atomic.Value
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ordinaryPort := coretest.FreePort(t)
			backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ordinary" {
					io.WriteString(w, "independent ordinary-user traffic")
					return
				}
				paths, _ := filepath.Glob(filepath.Join(a.opt.WorkDir, "diagnostic-*", "client.json"))
				if len(paths) == 1 {
					cfg, _ := os.ReadFile(paths[0])
					captured.Store(cfg)
				}
				if r.Header.Get("X-Polaris-Diagnostic") == "" {
					http.Error(w, "nonce missing", 400)
					return
				}
				if scenario == "disconnect" {
					cancel()
				}
				if scenario == "deadline" {
					<-r.Context().Done()
					return
				}
				if scenario == "success" {
					if err := core.DiagnosticHTTP(r.Context(), "http://"+r.Host+"/ordinary", ordinaryPort); err != nil {
						http.Error(w, "ordinary user failed during diagnostic", 500)
						return
					}
				}
				io.WriteString(w, strings.Repeat("business payload", 200))
			}))
			if scenario == "untrusted-https" {
				backend.StartTLS()
			} else {
				backend.Start()
			}
			defer backend.Close()
			req := model.RelayBusinessRequest{Core: model.CoreSingbox, ExpiresAt: time.Now().Add(15 * time.Second).Unix(), InboundTag: in.Tag, EntryAddress: "127.0.0.1", EntryPort: diagnosticRelay(t, in.Port), TCPURL: backend.URL, UDPAddress: diagnosticUDP(t, scenario != "bad-udp")}
			if scenario == "success" {
				host, port, _ := net.SplitHostPort(backend.Listener.Addr().String())
				num, _ := strconv.Atoi(port)
				cfg, e := diagnosticClient(in, req, coretest.Users()[0], ordinaryPort, coretest.FreePort(t), host, num, "127.0.0.1", 53)
				if e != nil {
					t.Fatal(e)
				}
				path := filepath.Join(t.TempDir(), "ordinary-client.json")
				if e = os.WriteFile(path, cfg, 0600); e != nil {
					t.Fatal(e)
				}
				p := core.NewProcess("ordinary-client", a.opt.BinPath, filepath.Dir(path), a.opt.Logger, "run", "-c", path)
				p.BindToParent = true
				if e = p.Start(t.Context()); e != nil {
					t.Fatal(e)
				}
				defer p.Stop(context.Background())
			}
			if scenario == "bad-entry" {
				req.EntryPort = coretest.FreePort(t)
			}
			if scenario == "deadline" {
				req.ExpiresAt = time.Now().Add(4 * time.Second).Unix()
			}
			r, err := a.RunRelayDiagnostic(ctx, req)
			if err != nil || !r.CleanupVerified || r.BusinessVerified != (scenario == "success") {
				t.Fatalf("result %+v error %v", r, err)
			}
			if !a.proc.Running() {
				t.Fatal("regular process not restored")
			}
			if scenario == "success" {
				ordinary := false
				for _, delta := range a.diagnosticTraffic {
					if model.IsDiagnosticCounter(delta.UserID) {
						t.Fatal("diagnostic traffic leaked into billing buffer")
					}
					if delta.UserID == coretest.Users()[0].ID && delta.Up > 0 && delta.Down > 0 {
						ordinary = true
					}
				}
				if !ordinary {
					t.Fatal("ordinary traffic was lost during diagnostic process replacement")
				}
			}
			onDisk, _ := os.ReadFile(a.configPath())
			if string(onDisk) != string(stable) {
				t.Fatal("diagnostic identity changed stable configuration")
			}
			paths, _ := filepath.Glob(filepath.Join(a.opt.WorkDir, "diagnostic-*"))
			if len(paths) != 0 {
				t.Fatal("temporary credentials left on disk")
			}
			if cfg, ok := captured.Load().([]byte); ok {
				if diagnosticReplay(t, a, cfg, backend.URL) == nil {
					t.Fatal("temporary identity still authenticates after cleanup")
				}
				// The same real path must still accept a normal user.
				var doc map[string]any
				json.Unmarshal(cfg, &doc)
				doc["outbounds"].([]any)[0].(map[string]any)["uuid"] = coretest.Users()[0].UUID
				normal, _ := json.Marshal(doc)
				if diagnosticReplay(t, a, normal, backend.URL) != nil && scenario != "disconnect" && scenario != "deadline" {
					t.Fatal("ordinary user failed after cleanup")
				}
			}
			if scenario == "success" {
				if captured.Load() == nil {
					t.Fatal("real backend was not reached")
				}
				var transfer bool
				for _, c := range r.Checks {
					if c.Kind == "proxy_transfer" && c.UpBytes > 0 && c.DownBytes > 0 {
						transfer = true
					}
				}
				if !transfer {
					t.Fatal("no independently metered diagnostic traffic")
				}
			}
		})
	}
}

// Use a fresh process so SSL_CERT_FILE is read before the system trust pool is
// cached. Verification stays enabled; untrusted TLS is covered separately.
func TestLiveSingboxDiagnosticHTTPS(t *testing.T) {
	if target := os.Getenv("POLARIS_DIAGNOSTIC_HTTPS_TARGET"); target != "" {
		a, in, _ := diagnosticFixture(t)
		r, err := a.RunRelayDiagnostic(t.Context(), model.RelayBusinessRequest{ExpiresAt: time.Now().Add(15 * time.Second).Unix(), InboundTag: in.Tag, EntryAddress: "127.0.0.1", EntryPort: diagnosticRelay(t, in.Port), TCPURL: target, UDPAddress: diagnosticUDP(t, true)})
		if err != nil || !r.BusinessVerified || !r.CleanupVerified {
			t.Fatalf("HTTPS result %+v error %v", r, err)
		}
		return
	}
	if os.Getenv("POLARIS_SINGBOX_BIN") == "" {
		t.Skip("POLARIS_SINGBOX_BIN not set")
	}
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Polaris-Diagnostic") == "" {
			http.Error(w, "nonce missing", 400)
			return
		}
		io.WriteString(w, strings.Repeat("HTTPS business payload", 100))
	}))
	defer backend.Close()
	trust := filepath.Join(t.TempDir(), "trusted-backend.pem")
	if err := os.WriteFile(trust, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestLiveSingboxDiagnosticHTTPS$", "-test.v")
	cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+trust, "POLARIS_DIAGNOSTIC_HTTPS_TARGET="+backend.URL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("verified TLS diagnostic: %v\n%s", err, out)
	}
}

func TestDiagnosticRefusesUnboundSingbox(t *testing.T) {
	a := New(core.Options{})
	if _, err := a.RunRelayDiagnostic(t.Context(), model.RelayBusinessRequest{}); err == nil {
		t.Fatal("temporary user accepted without parent-death enforcement")
	}
}

func TestLiveSingboxDiagnosticOwner(t *testing.T) {
	ownerDir := os.Getenv("POLARIS_DIAGNOSTIC_OWNER_DIR")
	if ownerDir == "" {
		return
	}
	a, in, _ := diagnosticFixture(t)
	state, _ := json.Marshal(struct {
		WorkDir   string
		APIListen string
		Inbound   model.Inbound
	}{a.opt.WorkDir, a.opt.APIListen, in})
	if err := core.AtomicWrite(filepath.Join(ownerDir, "owner.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = a.RunRelayDiagnostic(t.Context(), model.RelayBusinessRequest{ExpiresAt: time.Now().Add(15 * time.Second).Unix(), InboundTag: in.Tag, EntryAddress: "127.0.0.1", EntryPort: in.Port, TCPURL: os.Getenv("POLARIS_DIAGNOSTIC_OWNER_TARGET"), UDPAddress: "127.0.0.1:53"})
	t.Fatal("owner should have been killed with an active diagnostic")
}

func TestLiveSingboxDiagnosticCrashRevokesIdentity(t *testing.T) {
	if os.Getenv("POLARIS_SINGBOX_BIN") == "" || !core.ParentBoundSupported() {
		t.Skip("native sing-box and Linux required")
	}
	entered := make(chan struct{}, 1)
	var blocked atomic.Bool
	blocked.Store(true)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blocked.Load() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "ordinary user after crash recovery")
	}))
	defer backend.Close()
	dir := t.TempDir()
	destination := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	destination.EnableHTTP2 = true
	destination.StartTLS()
	defer destination.Close()
	exe, _ := os.Executable()
	owner := exec.Command(exe, "-test.run=^TestLiveSingboxDiagnosticOwner$", "-test.v")
	owner.Env = append(os.Environ(), "POLARIS_DIAGNOSTIC_OWNER_DIR="+dir, "POLARIS_DIAGNOSTIC_OWNER_TARGET="+backend.URL, "POLARIS_DIAGNOSTIC_REALITY_DEST="+strings.TrimPrefix(destination.URL, "https://"))
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer owner.Process.Kill()
	select {
	case <-entered: // A real request authenticated with the temporary UUID.
	case <-time.After(10 * time.Second):
		owner.Process.Kill()
		owner.Wait()
		t.Fatal("diagnostic owner did not reach the real backend")
	}
	var state struct {
		WorkDir   string
		APIListen string
		Inbound   model.Inbound
	}
	b, err := os.ReadFile(filepath.Join(dir, "owner.json"))
	if err != nil || json.Unmarshal(b, &state) != nil {
		t.Fatal("missing owner state")
	}
	paths, _ := filepath.Glob(filepath.Join(state.WorkDir, "diagnostic-*", "client.json"))
	if len(paths) != 1 {
		t.Fatal("no active temporary client")
	}
	captured, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	owner.Wait()
	// Do not credit the failure of a dead relay: verify the native landing
	// listener itself closes when the owner is SIGKILLed.
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(state.Inbound.Port))
	closed := false
	for until := time.Now().Add(2 * time.Second); time.Now().Before(until); {
		c, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			closed = true
			break
		}
		c.Close()
		time.Sleep(10 * time.Millisecond)
	}
	if !closed {
		t.Fatal("diagnostic listener survived Agent SIGKILL")
	}
	// Restarting an Agent must use the stable file and remove private orphans.
	a := New(core.Options{Version: "1.13.5", BinPath: os.Getenv("POLARIS_SINGBOX_BIN"), WorkDir: state.WorkDir, APIListen: state.APIListen, BindToParent: true})
	defer os.RemoveAll(state.WorkDir)
	if _, err = a.Render(&model.NodeSpec{Inbounds: []model.Inbound{state.Inbound}}); err != nil {
		t.Fatal(err)
	}
	if err = a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	orphans, _ := filepath.Glob(filepath.Join(state.WorkDir, "diagnostic-*"))
	if len(orphans) != 0 {
		t.Fatal("private diagnostic artifacts survived Agent restart")
	}
	blocked.Store(false)
	if diagnosticReplay(t, a, captured, backend.URL) == nil {
		t.Fatal("temporary identity resurrected after Agent restart")
	}
	var client map[string]any
	json.Unmarshal(captured, &client)
	client["outbounds"].([]any)[0].(map[string]any)["uuid"] = coretest.Users()[0].UUID
	normal, _ := json.Marshal(client)
	if err = diagnosticReplay(t, a, normal, backend.URL); err != nil {
		t.Fatal("ordinary identity did not recover after Agent restart")
	}
}
