package singbox

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestLiveSingboxExtendedDiagnosticProtocols(t *testing.T) {
	bin := diagnosticBinary(t)
	for _, tc := range []struct {
		name      string
		protocol  model.Protocol
		transport model.TransportSpec
	}{
		{"vless-tls", model.ProtoVLESS, model.TransportSpec{Type: model.TransTCP}},
		{"vless-ws", model.ProtoVLESS, model.TransportSpec{Type: model.TransWS, Path: "/probe"}},
		{"vmess-ws", model.ProtoVMess, model.TransportSpec{Type: model.TransWS, Path: "/probe"}},
		{"trojan-grpc", model.ProtoTrojan, model.TransportSpec{Type: model.TransGRPC, ServiceName: "probe"}},
		{"anytls", model.ProtoAnyTLS, model.TransportSpec{Type: model.TransTCP}},
		{"hysteria2", model.ProtoHysteria2, model.TransportSpec{Type: model.TransQUIC}},
		{"tuic", model.ProtoTUIC, model.TransportSpec{Type: model.TransQUIC}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(core.Options{Version: "1.13.5", BinPath: bin, WorkDir: t.TempDir(), APIListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(coretest.FreePort(t))), BindToParent: true})
			in := coretest.Inbound(model.CoreSingbox, "extended", coretest.FreePort(t), model.ProtocolSpec{Type: tc.protocol}, tc.transport, coretest.TLS(t, "diagnostic.example"))
			in.Listen = "127.0.0.1"
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
			defer a.Stop(context.Background())
			var captured atomic.Value
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths, _ := filepath.Glob(filepath.Join(a.opt.WorkDir, "diagnostic-*", "client.json"))
				if len(paths) == 1 {
					b, _ := os.ReadFile(paths[0])
					captured.Store(b)
				}
				if r.Header.Get("X-Polaris-Diagnostic") == "" {
					http.Error(w, "missing nonce", 400)
					return
				}
				io.WriteString(w, "authenticated native diagnostic payload")
			}))
			defer backend.Close()
			entry := in.Port
			if tc.transport.Type != model.TransQUIC {
				entry = diagnosticRelay(t, in.Port)
			}
			r, err := a.RunRelayDiagnostic(t.Context(), model.RelayBusinessRequest{ExpiresAt: time.Now().Add(15 * time.Second).Unix(), InboundTag: in.Tag, EntryAddress: "127.0.0.1", EntryPort: entry, TCPURL: backend.URL, UDPAddress: diagnosticUDP(t, true)})
			if err != nil || !r.BusinessVerified || !r.CleanupVerified {
				t.Fatalf("result %+v err %v logs %v", r, err, a.Logs(20))
			}
			for _, d := range a.diagnosticTraffic {
				if model.IsDiagnosticCounter(d.UserID) {
					t.Fatal("diagnostic user leaked into billing", d)
				}
			}
			actual, _ := os.ReadFile(a.configPath())
			if string(actual) != string(cfg) {
				t.Fatal("stable configuration changed")
			}
			client, ok := captured.Load().([]byte)
			if !ok {
				t.Fatal("real authenticated request did not reach target")
			}
			if err = diagnosticReplay(t, a, client, backend.URL); err == nil {
				t.Fatal("temporary credential remained valid after cleanup")
			}
			var doc map[string]any
			json.Unmarshal(client, &doc)
			out := doc["outbounds"].([]any)[0].(map[string]any)
			ordinary := in.Users[0]
			switch tc.protocol {
			case model.ProtoVLESS, model.ProtoVMess:
				out["uuid"] = ordinary.UUID
			case model.ProtoTUIC:
				out["uuid"], out["password"] = ordinary.UUID, ordinary.Password
			default:
				out["password"] = ordinary.Password
			}
			regular, _ := json.Marshal(doc)
			if err = diagnosticReplay(t, a, regular, backend.URL); err != nil {
				t.Fatal("ordinary user's credentials failed after diagnostic recovery", err)
			}
		})
	}
}
