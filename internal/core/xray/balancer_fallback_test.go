package xray

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestBalancerFallbackRender(t *testing.T) {
	obs := []model.Outbound{
		{Tag: "proxy", Type: model.OutSOCKS, Settings: coretest.Settings(model.OutboundSettings{Server: "127.0.0.1", Port: 1080})},
		{Tag: "backup", Type: model.OutBalancer, Settings: coretest.Settings(model.OutboundSettings{Members: []string{"proxy"}, Strategy: model.LBRoundRobin, Fallback: "block"})},
		{Tag: "primary", Type: model.OutBalancer, Settings: coretest.Settings(model.OutboundSettings{Members: []string{"proxy"}, Strategy: model.LBRandom, Fallback: "backup", Interval: "5s"})},
	}
	a := New(core.Options{Version: "26.3.27", WorkDir: t.TempDir(), BinPath: os.Getenv("POLARIS_XRAY_BIN")})
	spec := &model.NodeSpec{Outbounds: obs, Routes: []model.RouteRule{{Outbound: "primary"}}}
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(cfg, &m)
	routing := m["routing"].(map[string]any)
	bs := routing["balancers"].([]any)
	fb := ""
	for _, raw := range bs {
		b := raw.(map[string]any)
		if b["tag"] == "primary" {
			fb = b["fallbackTag"].(string)
		}
	}
	if fb == "" || fb == "backup" {
		t.Fatalf("fallback must address a handler: %s", cfg)
	}
	rules := routing["rules"].([]any)
	if rules[1].(map[string]any)["balancerTag"] != "backup" || rules[len(rules)-1].(map[string]any)["balancerTag"] != "primary" {
		t.Fatalf("bridge/default rules: %s", cfg)
	}
	if m["observatory"].(map[string]any)["probeInterval"] != "5s" {
		t.Fatalf("random/roundRobin need health probes: %s", cfg)
	}
	original, _ := obs[2].ParseSettings()
	if original.Fallback != "backup" {
		t.Fatal("render mutated persisted settings")
	}
	if a.opt.BinPath != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("validate: %v\n%s", err, cfg)
		}
	}
	// UUID and private port must stay stable through ordinary renders and
	// agent restarts, so an unrelated routing edit does not restart the core.
	first := a.fallbacks[fb]
	if first.Port == 0 || first.UUID == "" {
		t.Fatal("missing authenticated bridge binding")
	}
	if _, err := a.Render(spec); err != nil {
		t.Fatal(err)
	}
	if a.fallbacks[fb] != first {
		t.Fatal("binding changed on render")
	}
	if err := os.WriteFile(a.configPath(), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := New(a.opt)
	if _, err := restarted.Render(spec); err != nil {
		t.Fatal(err)
	}
	if restarted.fallbacks[fb] != first {
		t.Fatal("binding changed on agent restart")
	}
	// Reallocate if a newly configured public inbound claims the old port.
	spec.Inbounds = []model.Inbound{{Tag: "new-listener", Port: first.Port}}
	if _, err := a.Render(spec); err != nil {
		t.Fatal(err)
	}
	if a.fallbacks[fb].Port == first.Port {
		t.Fatal("private bridge conflicts with a configured listener")
	}
	// A corrupt stored cycle is rejected even when the API was bypassed.
	obs[1].Settings = coretest.Settings(model.OutboundSettings{Members: []string{"proxy"}, Fallback: "primary"})
	if _, err := a.Render(spec); err == nil {
		t.Fatal("cycle rendered")
	}
}

func TestLiveBalancerFallback(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" {
		t.Skip("POLARIS_XRAY_BIN not set")
	}
	for _, strategy := range []string{model.LBRoundRobin, model.LBRandom, model.LBLeastPing, model.LBLeastLoad} {
		t.Run(strategy, func(t *testing.T) {
			a := New(core.Options{Version: "26.3.27", BinPath: bin, WorkDir: t.TempDir(), APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
			coretest.LiveBalancerFallback(t, a, model.CoreXray, a.configPath(), strategy)
		})
	}
}
