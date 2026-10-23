package xray

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestLiveRelayBusinessDiagnostic(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" {
		t.Skip("POLARIS_XRAY_BIN not set")
	}
	for _, security := range []string{"none", "tls", "tls-default-sni", "vision", "reality"} {
		t.Run(security, func(t *testing.T) {
			dir := t.TempDir()
			a := New(core.Options{Version: "26.3.27", BinPath: bin, WorkDir: dir, APIListen: net.JoinHostPort("127.0.0.1", itoa(freePort(t))), BindToParent: true})
			port := freePort(t)
			in := coretest.Inbound(model.CoreXray, "entry", port, model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{}, model.SecuritySpec{})
			in.Listen = "127.0.0.1"
			if security != "none" {
				in.Security = coretest.TLS(t, "test.example")
				in.Security.ServerName = "test.example"
			}
			if security == "reality" {
				dest := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				dest.EnableHTTP2 = true
				dest.StartTLS()
				defer dest.Close()
				in.Security = coretest.Reality(t)
				in.Security.RealityDest = strings.TrimPrefix(dest.URL, "https://")
				in.Protocol.Flow = "xtls-rprx-vision"
			}
			if security == "tls-default-sni" {
				in.Security.ServerName = ""
			}
			if security == "vision" {
				in.Protocol.Flow = "xtls-rprx-vision"
			}
			spec := &model.NodeSpec{Inbounds: []model.Inbound{in}}
			cfg, err := a.Render(spec)
			if err != nil {
				t.Fatal(err)
			}
			// Fixtures are local; permit them explicitly on recent native versions.
			var doc map[string]any
			json.Unmarshal(cfg, &doc)
			for _, v := range doc["outbounds"].([]any) {
				o := v.(map[string]any)
				if o["protocol"] == "freedom" {
					o["settings"] = map[string]any{"ipsBlocked": []string{"224.0.0.0/4"}, "finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.0/8"}}}}
				}
			}
			cfg, _ = json.Marshal(doc)
			if err = core.AtomicWrite(a.configPath(), cfg, 0600); err != nil {
				t.Fatal(err)
			}
			if err = a.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer a.Stop(context.Background())
			var credential atomic.Value
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths, _ := filepath.Glob(filepath.Join(dir, "diagnostic-*", "client.json"))
				if len(paths) == 1 {
					b, _ := os.ReadFile(paths[0])
					var conf map[string]any
					if json.Unmarshal(b, &conf) == nil {
						credential.Store(conf["outbounds"].([]any)[0].(map[string]any)["settings"].(map[string]any)["vnext"].([]any)[0].(map[string]any)["users"].([]any)[0].(map[string]any)["id"].(string))
					}
				}
				if r.Header.Get("X-Polaris-Diagnostic") == "" {
					http.Error(w, "missing nonce", 400)
					return
				}
				io.WriteString(w, "business-response")
			}))
			defer backend.Close()
			udp, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer udp.Close()
			ds := &dns.Server{PacketConn: udp, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
				m := new(dns.Msg)
				m.SetReply(r)
				m.Rcode = dns.RcodeNameError
				w.WriteMsg(m)
			})}
			go ds.ActivateAndServe()
			defer ds.Shutdown()
			req := model.RelayBusinessRequest{EntryAddress: "127.0.0.1", EntryPort: port, InboundTag: in.Tag, ServerName: "test.example", TCPURL: backend.URL, UDPAddress: udp.LocalAddr().String()}
			pid := a.proc.PID()
			result, err := a.RunRelayDiagnostic(t.Context(), req)
			if err != nil || !result.BusinessVerified || !result.CleanupVerified || result.State != "passed" {
				t.Fatalf("diagnostic %+v %v", result, err)
			}
			if a.proc.PID() != pid {
				t.Fatal("successful diagnostic restarted the shared core")
			}
			saved, _ := os.ReadFile(a.configPath())
			if string(saved) != string(cfg) {
				t.Fatal("diagnostic modified stable config")
			}
			dirs, _ := filepath.Glob(filepath.Join(dir, "diagnostic-*"))
			if len(dirs) != 0 {
				t.Fatal("temporary files survived", dirs)
			}
			deltas, err := a.CollectTraffic(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, d := range deltas {
				if model.IsDiagnosticCounter(d.UserID) && d.Up+d.Down > 0 {
					found = true
				}
				if d.UserID == coretest.Users()[0].ID && d.Up+d.Down != 0 {
					t.Fatal("real user was charged", d)
				}
			}
			if !found {
				t.Fatal("native core did not record isolated diagnostic identity")
			}
			if security == "none" {
				id, _ := credential.Load().(string)
				if id == "" {
					t.Fatal("client credential was not observed")
				}
				if _, err := fetchDiagnosticVLESS(in.Port, id, backend.Listener.Addr().String()); err == nil {
					t.Fatal("temporary identity remained valid after cleanup")
				}
				blackhole, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				req.UDPAddress = blackhole.LocalAddr().String()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				blocked, err := a.RunRelayDiagnostic(ctx, req)
				cancel()
				blackhole.Close()
				if err != nil || blocked.BusinessVerified || !blocked.CleanupVerified {
					t.Fatal("UDP outage accepted", blocked, err)
				}
				if blocked.Checks[0].State != "passed" || blocked.Checks[1].State != "failed" {
					t.Fatal("UDP failure not distinguished from TCP", blocked)
				}
				req.UDPAddress = udp.LocalAddr().String()
			}
			// An open TCP port without the VLESS server must not verify business.
			wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			defer wrong.Close()
			req.EntryPort = wrong.Listener.Addr().(*net.TCPAddr).Port
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			result, _ = a.RunRelayDiagnostic(ctx, req)
			cancel()
			if result.BusinessVerified || !result.CleanupVerified {
				t.Fatal("open port mistaken for business success", result)
			}
		})
	}
}

func itoa(p int) string { return fmt.Sprint(p) }

func TestDiagnosticClientNeverHasDirectFallback(t *testing.T) {
	a := New(core.Options{})
	in := &model.Inbound{Protocol: model.ProtocolSpec{Type: model.ProtoVLESS}, Transport: model.TransportSpec{Type: model.TransTCP}, Security: model.SecuritySpec{Type: model.SecNone}}
	b, err := a.diagnosticClient(in, model.RelayBusinessRequest{EntryAddress: "entry.example", EntryPort: 443}, model.User{UUID: "11111111-1111-4111-8111-111111111111"}, 10000, 10001, "target.example", 443, "1.1.1.1", 53)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(b, &doc)
	outs := doc["outbounds"].([]any)
	if len(outs) != 1 || outs[0].(map[string]any)["protocol"] != "vless" || strings.Contains(string(b), "freedom") {
		t.Fatal("client can bypass proxy")
	}
	if _, err := a.RunRelayDiagnostic(t.Context(), model.RelayBusinessRequest{}); err == nil {
		t.Fatal("unbound process accepted a diagnostic identity")
	}
}

func TestLiveDiagnosticCleanupRecovery(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" {
		t.Skip("POLARIS_XRAY_BIN not set")
	}
	dir := t.TempDir()
	a := New(core.Options{Version: "26.3.27", BinPath: bin, WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", freePort(t)), BindToParent: true})
	in := coretest.Inbound(model.CoreXray, "entry", freePort(t), model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{}, model.SecuritySpec{})
	in.Listen = "127.0.0.1"
	cfg, e := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}})
	if e != nil {
		t.Fatal(e)
	}
	var native map[string]any
	json.Unmarshal(cfg, &native)
	for _, v := range native["outbounds"].([]any) {
		o := v.(map[string]any)
		if o["protocol"] == "freedom" {
			o["settings"] = map[string]any{"ipsBlocked": []string{"224.0.0.0/4"}, "finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.0/8"}}}}
		}
	}
	cfg, _ = json.Marshal(native)
	if e = core.AtomicWrite(a.configPath(), cfg, 0600); e != nil {
		t.Fatal(e)
	}
	if e = a.Start(t.Context()); e != nil {
		t.Fatal(e)
	}
	defer a.Stop(context.Background())
	var uuid atomic.Value
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths, _ := filepath.Glob(filepath.Join(dir, "diagnostic-*", "client.json"))
		if len(paths) == 1 {
			b, _ := os.ReadFile(paths[0])
			var c map[string]any
			if json.Unmarshal(b, &c) == nil {
				uuid.Store(c["outbounds"].([]any)[0].(map[string]any)["settings"].(map[string]any)["vnext"].([]any)[0].(map[string]any)["users"].([]any)[0].(map[string]any)["id"].(string))
			}
		}
		// Deliberately make management unavailable after successful authentication.
		// Proxy data still works; cleanup must stop the core rather than leave the
		// hot identity installed, and recover using an entirely fresh API client.
		a.mu.Lock()
		if a.api != nil {
			a.api.Close()
		}
		a.mu.Unlock()
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	pc, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ds := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) { m := new(dns.Msg); m.SetReply(r); w.WriteMsg(m) })}
	go ds.ActivateAndServe()
	defer ds.Shutdown()
	pid := a.proc.PID()
	result, e := a.RunRelayDiagnostic(t.Context(), model.RelayBusinessRequest{InboundTag: in.Tag, EntryAddress: "127.0.0.1", EntryPort: in.Port, TCPURL: backend.URL, UDPAddress: pc.LocalAddr().String()})
	uuidValue, _ := uuid.Load().(string)
	if e != nil || result.BusinessVerified || !result.CleanupVerified || a.proc.PID() == pid || uuidValue == "" {
		t.Fatal("failed cleanup did not restart safely", result, e)
	}
	if _, e := fetchDiagnosticVLESS(in.Port, uuidValue, backend.Listener.Addr().String()); e == nil {
		t.Fatal("temporary identity survived recovery")
	}
	if _, e := fetchDiagnosticVLESS(in.Port, coretest.Users()[0].UUID, backend.Listener.Addr().String()); e != nil {
		t.Fatal("stable user not restored", e)
	}
}

func fetchDiagnosticVLESS(port int, id, target string) (string, error) {
	host, p, err := net.SplitHostPort(target)
	if err != nil {
		return "", err
	}
	destPort, _ := strconv.Atoi(p)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	uuid, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil {
		return "", err
	}
	header := append([]byte{0}, uuid...)
	header = append(header, 0, 1, byte(destPort>>8), byte(destPort), 1)
	header = append(header, net.ParseIP(host).To4()...)
	header = append(header, []byte("GET / HTTP/1.1\r\nHost: "+target+"\r\nX-Polaris-Diagnostic: test\r\nConnection: close\r\n\r\n")...)
	if _, err = conn.Write(header); err != nil {
		return "", err
	}
	var response [2]byte
	if _, err = io.ReadFull(conn, response[:]); err != nil {
		return "", err
	}
	if _, err = io.CopyN(io.Discard, conn, int64(response[1])); err != nil {
		return "", err
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1024))
	return string(b), err
}

func TestStartupRemovesDiagnosticArtifacts(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "diagnostic-orphan")
	if e := os.Mkdir(orphan, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(orphan, "client.json"), []byte("expired-test-secret"), 0600); e != nil {
		t.Fatal(e)
	}
	stable := filepath.Join(dir, "xray.json")
	if e := os.WriteFile(stable, []byte("stable"), 0600); e != nil {
		t.Fatal(e)
	}
	New(core.Options{BinPath: "/not-started", WorkDir: dir})
	if _, e := os.Stat(orphan); !os.IsNotExist(e) {
		t.Fatal("orphaned client files retained", e)
	}
	if b, e := os.ReadFile(stable); e != nil || string(b) != "stable" {
		t.Fatal("stable config was modified", e)
	}
}
