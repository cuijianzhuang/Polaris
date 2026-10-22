package mihomo

import (
	"fmt"
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestBalancerFallbackRender(t *testing.T) {
	obs := []model.Outbound{
		{Tag: "p1", Type: model.OutHTTP, Settings: coretest.Settings(model.OutboundSettings{Server: "127.0.0.1", Port: 1080})},
		{Tag: "backup", Type: model.OutBalancer, Settings: coretest.Settings(model.OutboundSettings{Members: []string{"p1"}})},
		{Tag: "primary", Type: model.OutBalancer, Settings: coretest.Settings(model.OutboundSettings{Members: []string{"p1"}, Strategy: model.LBRoundRobin, Fallback: "backup", Interval: "5s"})},
	}
	a := New(core.Options{WorkDir: t.TempDir(), BinPath: os.Getenv("POLARIS_MIHOMO_BIN")})
	cfg, err := a.Render(&model.NodeSpec{Outbounds: obs})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(cfg, &m); err != nil {
		t.Fatal(err)
	}
	var primary, inner map[string]any
	for _, raw := range m["proxy-groups"].([]any) {
		g := raw.(map[string]any)
		if g["name"] == "primary" {
			primary = g
		}
		if g["name"] == "polaris-primary-primary" {
			inner = g
		}
	}
	if primary == nil || inner == nil || primary["type"] != "fallback" || inner["type"] != "load-balance" || inner["strategy"] != "round-robin" {
		t.Fatalf("group strategy lost: %s", cfg)
	}
	proxies := primary["proxies"].([]any)
	if len(proxies) != 2 || proxies[0] != inner["name"] || proxies[1] != "backup" || primary["lazy"] != false || primary["interval"] != 5 {
		t.Fatalf("priority/probes: %s", cfg)
	}
	if a.opt.BinPath != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("validate: %v\n%s", err, cfg)
		}
	}
	obs[2].Settings = coretest.Settings(model.OutboundSettings{Members: []string{"p1"}, Fallback: "block"})
	if _, err := a.Render(&model.NodeSpec{Outbounds: obs}); err == nil {
		t.Fatal("unsupported block fallback accepted")
	}
}

func TestLiveBalancerFallback(t *testing.T) {
	bin := os.Getenv("POLARIS_MIHOMO_BIN")
	if bin == "" {
		t.Skip("POLARIS_MIHOMO_BIN not set")
	}
	a := New(core.Options{Version: "1.19.15", BinPath: bin, WorkDir: t.TempDir(), APIListen: fmt.Sprintf("127.0.0.1:%d", coretest.FreePort(t))})
	coretest.LiveBalancerFallback(t, a, model.CoreMihomo, a.configPath(), model.LBRoundRobin)
}
