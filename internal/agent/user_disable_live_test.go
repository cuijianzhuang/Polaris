package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/singbox"
	"github.com/cuijianzhuang/polaris/internal/core/xray"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestLiveDisableUserKeepsOtherUsers(t *testing.T) {
	for _, typ := range []model.CoreType{model.CoreSingbox, model.CoreXray} {
		t.Run(string(typ), func(t *testing.T) {
			bin := os.Getenv(map[model.CoreType]string{model.CoreSingbox: "POLARIS_SINGBOX_BIN", model.CoreXray: "POLARIS_XRAY_BIN"}[typ])
			if bin == "" {
				t.Skip("core binary not configured")
			}
			protocols := []model.Protocol{model.ProtoVLESS, model.ProtoSOCKS, model.ProtoHTTP, model.ProtoShadowsocks}
			if typ == model.CoreSingbox {
				protocols = append(protocols, model.ProtoAnyTLS, model.ProtoHysteria2, model.ProtoTUIC)
			}
			version := map[model.CoreType]string{model.CoreSingbox: "1.13.5", model.CoreXray: "26.3.27"}[typ]
			for _, protocol := range protocols {
				t.Run(string(protocol), func(t *testing.T) {
					a := newFakeAgent(t)
					work := filepath.Join(a.cfg.DataDir, "run", string(typ))
					if err := os.MkdirAll(work, 0700); err != nil {
						t.Fatal(err)
					}
					opts := core.Options{Version: version, BinPath: bin, WorkDir: work, APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t)), Logger: a.log}
					var adapter core.CoreAdapter
					if typ == model.CoreSingbox {
						adapter = singbox.New(opts)
					} else {
						adapter = &localFixtureCore{CoreAdapter: xray.New(opts)}
					}
					t.Cleanup(func() { _ = adapter.Stop(t.Context()) })
					a.cores[typ] = &managed{spec: model.CoreSpec{Type: typ, Version: version}, adapter: adapter, cfgPath: filepath.Join(work, string(typ)+".json")}
					shared := coretest.Inbound(typ, "shared", coretest.FreePort(t), model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{}, model.SecuritySpec{})
					shared.Listen = "127.0.0.1"
					shared.Sniffing.Enabled = false
					security := model.SecuritySpec{}
					if protocol == model.ProtoAnyTLS || protocol == model.ProtoHysteria2 || protocol == model.ProtoTUIC {
						security = coretest.TLS(t, "localhost")
					}
					proto := model.ProtocolSpec{Type: protocol}
					if protocol == model.ProtoShadowsocks {
						proto.Method = "2022-blake3-aes-128-gcm"
						proto.ServerKey = "Ar9iPjxjvKbx8PmqXbX5Sg=="
					}
					private := coretest.Inbound(typ, "private", coretest.FreePort(t), proto, model.TransportSpec{}, security)
					private.Listen = "127.0.0.1"
					private.Users = shared.Users[:1]
					spec := &model.NodeSpec{NodeID: "n1", Rev: 1, Cores: []model.CoreSpec{{Type: typ, Version: version}}, Inbounds: []model.Inbound{shared, private}}
					if result := a.Apply(t.Context(), spec); !result.ok {
						t.Fatal("initial apply", result.err)
					}
					target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "still-authorized") }))
					defer target.Close()
					address := target.Listener.Addr().String()
					if got, err := coretest.FetchVLESSUser(shared.Port, address, shared.Users[1].UUID); err != nil || got != "still-authorized" {
						t.Fatal("initial user unavailable", err)
					}
					next := spec.Clone()
					next.Rev = 2
					next.Inbounds[0].Users = next.Inbounds[0].Users[1:]
					next.Inbounds[1].Users = nil
					if result := a.Apply(t.Context(), next); !result.ok {
						t.Fatal("disable apply", result.err)
					}
					if got, err := coretest.FetchVLESSUser(shared.Port, address, shared.Users[0].UUID); err == nil {
						t.Fatal("disabled user still authenticated", got)
					}
					if got, err := coretest.FetchVLESSUser(shared.Port, address, shared.Users[1].UUID); err != nil || got != "still-authorized" {
						t.Fatal("disabled one user also broke the other user", err)
					}
					restore := spec.Clone()
					restore.Rev = 3
					if result := a.Apply(t.Context(), restore); !result.ok {
						t.Fatal("re-enable failed", result.err)
					}
					for _, user := range shared.Users {
						if got, err := coretest.FetchVLESSUser(shared.Port, address, user.UUID); err != nil || got != "still-authorized" {
							t.Fatal("re-enable broke authentication", user.ID, err)
						}
					}
				})
			}
		})
	}
}

// Only allow the test HTTP fixture through Xray's private-target safeguard.
// Keep production rendering and policy unchanged.
type localFixtureCore struct{ core.CoreAdapter }

func (a *localFixtureCore) Render(spec *model.NodeSpec) ([]byte, error) {
	cfg, err := a.CoreAdapter.Render(spec)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(cfg, &v); err != nil {
		return nil, err
	}
	for _, raw := range v["outbounds"].([]any) {
		o := raw.(map[string]any)
		if o["protocol"] == "freedom" {
			o["settings"] = map[string]any{"finalRules": []any{map[string]any{"action": "allow"}}}
		}
	}
	return json.Marshal(v)
}
