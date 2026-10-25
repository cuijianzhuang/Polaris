package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/xray"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func TestLiveSourceReferenceAuthBillingAndFailClosed(t *testing.T) {
	xbin := os.Getenv("POLARIS_XRAY_BIN")
	if xbin == "" {
		t.Skip("POLARIS_XRAY_BIN required")
	}
	providerPort := coretest.FreePort(t)
	provider := coretest.Inbound(model.CoreXray, "provider", providerPort, model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{Type: model.TransTCP}, model.SecuritySpec{Type: model.SecNone})
	provider.Listen = "127.0.0.1"
	provider.Sniffing.Enabled = false
	provider.Users = []model.User{{ID: "provider-account", UUID: "11111111-1111-4111-8111-111111111111"}}
	var sampleMu sync.RWMutex
	var sample func()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "source-resource-success")
		w.(http.Flusher).Flush()
		sampleMu.RLock()
		if sample != nil {
			sample()
		}
		sampleMu.RUnlock()
	}))
	defer backend.Close()
	write := func(t *testing.T, a core.CoreAdapter, dir string, spec *model.NodeSpec) {
		t.Helper()
		b, e := a.Render(spec)
		if e != nil {
			t.Fatal(e)
		}
		path := map[model.CoreType]string{model.CoreXray: "xray.json", model.CoreSingbox: "singbox.json", model.CoreMihomo: "mihomo.yaml"}[a.Type()]
		if a.Type() == model.CoreXray {
			var cfg map[string]any
			json.Unmarshal(b, &cfg)
			for _, raw := range cfg["outbounds"].([]any) {
				o := raw.(map[string]any)
				if o["protocol"] == "freedom" {
					o["settings"] = map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.0/8"}}}}
				}
			}
			b, _ = json.Marshal(cfg)
		}
		if e = a.Validate(b); e != nil {
			t.Fatal(e)
		}
		if e = core.AtomicWrite(filepath.Join(dir, path), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	pd := t.TempDir()
	pa := xray.New(core.Options{Version: "26.3.27", BinPath: xbin, WorkDir: pd, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
	write(t, pa, pd, &model.NodeSpec{Inbounds: []model.Inbound{provider}})
	if e := pa.Start(t.Context()); e != nil {
		t.Fatal(e)
	}
	defer pa.Stop(context.Background())
	for _, tc := range []struct {
		typ          model.CoreType
		env, version string
	}{{model.CoreXray, "POLARIS_XRAY_BIN", "26.3.27"}, {model.CoreSingbox, "POLARIS_SINGBOX_BIN", "1.14.4"}, {model.CoreMihomo, "POLARIS_MIHOMO_BIN", "1.19.14"}} {
		t.Run(string(tc.typ), func(t *testing.T) {
			bin := os.Getenv(tc.env)
			if bin == "" {
				t.Skip(tc.env + " required")
			}
			s := newTestServer(t)
			src := store.SubSource{ID: "source", Name: "source", Kind: "manual", Enabled: true}
			s.st.DB.Create(&src)
			n := sub.Node{Name: "Provider", Protocol: model.ProtoVLESS, Server: "127.0.0.1", Port: providerPort, UUID: "11111111-1111-4111-8111-111111111111", Transport: model.TransportSpec{Type: model.TransTCP}, TLS: model.ClientTLS{Type: model.SecNone}}
			ns, e := s.commitSourceRefresh(&src, []sub.Node{n}, store.SubTraffic{})
			if e != nil {
				t.Fatal(e)
			}
			s.st.DB.Create(&store.Node{ID: "entry", Name: "entry", DefaultCore: tc.typ})
			s.st.DB.Create(&store.Core{ID: "core", NodeID: "entry", Type: tc.typ, Version: tc.version})
			port := coretest.FreePort(t)
			in := store.Inbound{ID: "entry-in", NodeID: "entry", Core: tc.typ, Tag: "entry-in", Port: port, Listen: "127.0.0.1", Enabled: true, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone}), OutboundTag: "supplier"}
			if tc.typ == model.CoreMihomo {
				in.Extra = json.RawMessage(`{"allow-insecure":true}`)
			} // local plaintext client fixture only
			s.st.DB.Create(&in)
			user := coretest.Users()[0]
			s.st.DB.Create(&store.User{ID: user.ID, Username: user.ID, UUID: user.UUID, Password: user.Password, Status: "active", SubToken: "source-test-token"})
			s.st.DB.Create(&store.UserAccess{UserID: user.ID, Kind: "inbound", TargetID: in.ID})
			o := store.Outbound{ID: "ref"}
			if e = s.storeOutbound("entry", &o, &outboundReq{Tag: "supplier", Type: model.OutSourceRef, SourceNodeID: ns[0].ResourceID}); e != nil {
				t.Fatal(e)
			}
			dir := t.TempDir()
			a, e := core.New(tc.typ, core.Options{Version: tc.version, BinPath: bin, WorkDir: dir, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
			if e != nil {
				t.Fatal(e)
			}
			build := func() {
				t.Helper()
				spec, e := s.BuildSpec("entry")
				if e != nil {
					t.Fatal(e)
				}
				write(t, a, dir, spec)
			}
			build()
			if e = a.Start(t.Context()); e != nil {
				t.Fatal(e)
			}
			defer a.Stop(context.Background())
			sampled := make(chan []model.TrafficDelta, 8)
			sampleMu.Lock()
			sample = func() {
				time.Sleep(20 * time.Millisecond)
				ds, _ := a.CollectTraffic(t.Context())
				select {
				case sampled <- ds:
				default:
				}
			}
			sampleMu.Unlock()
			defer func() { sampleMu.Lock(); sample = nil; sampleMu.Unlock() }()
			target := strings.TrimPrefix(backend.URL, "http://")
			for i := 0; i < 5; i++ {
				body, e := coretest.FetchVLESS(port, target)
				if e == nil && body == "source-resource-success" {
					break
				}
				if i == 4 {
					if logs, ok := a.(interface{ Logs(int) []string }); ok {
						t.Log(logs.Logs(20))
					}
					t.Log(pa.Logs(20))
					t.Fatalf("proxy: %s %v entry=%+v provider=%+v", body, e, a.Health(t.Context()), pa.Health(t.Context()))
				}
				time.Sleep(50 * time.Millisecond)
			}
			var stats []model.TrafficDelta
			select {
			case stats = <-sampled:
			case <-time.After(time.Second):
				t.Fatal("no live traffic sample")
			}
			var up, down int64
			for _, d := range stats {
				if strings.HasPrefix(d.UserID, "inbound>>>") || strings.HasPrefix(d.UserID, "outbound>>>") {
					continue
				}
				if d.UserID != user.ID {
					t.Fatal("supplier identity entered user billing", d)
				}
				up += d.Up
				down += d.Down
			}
			if up == 0 || down == 0 {
				t.Fatalf("real user traffic missing: %+v", stats)
			}
			// Wrong upstream credentials must fail even though the HTTP target is
			// reachable directly from the entry node.
			n.UUID = "22222222-2222-4222-8222-222222222222"
			if ns, e = s.commitSourceRefresh(&src, []sub.Node{n}, store.SubTraffic{}); e != nil {
				t.Fatal(e)
			}
			build()
			if e = a.Reload(t.Context()); e != nil {
				t.Fatal(e)
			}
			time.Sleep(300 * time.Millisecond)
			if !a.Health(t.Context()).Running {
				if logs, ok := a.(interface{ Logs(int) []string }); ok {
					t.Log(logs.Logs(20))
				}
				t.Fatal("core exited during reload")
			}
			if body, e := coretest.FetchVLESS(port, target); e == nil {
				t.Fatal("wrong upstream credential escaped via direct", body)
			}
			n.UUID = "11111111-1111-4111-8111-111111111111"
			if _, e = s.commitSourceRefresh(&src, []sub.Node{n}, store.SubTraffic{}); e != nil {
				t.Fatal(e)
			}
			build()
			if e = a.Reload(t.Context()); e != nil {
				t.Fatal(e)
			}
			time.Sleep(300 * time.Millisecond)
			if !a.Health(t.Context()).Running {
				if logs, ok := a.(interface{ Logs(int) []string }); ok {
					t.Log(logs.Logs(20))
				}
				t.Fatal("core exited during reload")
			}
			for attempt := 0; attempt < 20; attempt++ {
				body, e := coretest.FetchVLESS(port, target)
				if e == nil && body == "source-resource-success" {
					break
				}
				if attempt == 19 {
					t.Fatal("credential repair did not recover", body, e)
				}
				time.Sleep(50 * time.Millisecond)
			}
			if _, e = s.commitSourceRefresh(&src, nil, store.SubTraffic{}); e != nil {
				t.Fatal(e)
			}
			build()
			if e = a.Reload(t.Context()); e != nil {
				t.Fatal(e)
			}
			time.Sleep(300 * time.Millisecond)
			if !a.Health(t.Context()).Running {
				if logs, ok := a.(interface{ Logs(int) []string }); ok {
					t.Log(logs.Logs(20))
				}
				t.Fatal("core exited during reload")
			}
			if body, e := coretest.FetchVLESS(port, target); e == nil {
				t.Fatal("removed resource escaped via direct", body)
			}
			t.Logf("%s: authenticated relay, user traffic %d/%d, wrong credentials, rotation and removed-resource blocking verified", tc.typ, up, down)
		})
	}
}
