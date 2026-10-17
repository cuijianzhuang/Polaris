package mihomo

import (
	"os"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestRenderDNS(t *testing.T) {
	bin := os.Getenv("POLARIS_MIHOMO_BIN")
	a := New(core.Options{Version: "1.19.15", WorkDir: t.TempDir(), BinPath: bin})
	obs, routes := coretest.Chain()
	in := coretest.Inbound(model.CoreMihomo, "vmess", 25444, model.ProtocolSpec{Type: model.ProtoVMess}, model.TransportSpec{}, model.SecuritySpec{})
	spec := &model.NodeSpec{Inbounds: []model.Inbound{in}, Outbounds: obs, Routes: routes, DNS: coretest.DNS()}
	spec.DNS.Servers = append(spec.DNS.Servers, model.DNSServer{Tag: "dot", Address: "tls://1.1.1.1", Domains: []string{"cloudflare.com"}})
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"enhanced-mode: fake-ip", "fake-ip-filter-mode: whitelist", "geosite:cn:", "+.corp.example.com:",
		"https://dns.google:443/dns-query#resi", "default-nameserver:", "tls://1.1.1.1:853", "panel.internal: 10.0.0.2"} {
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("missing %s in:\n%s", want, cfg)
		}
	}
	if strings.Contains(string(cfg), "no-resolve") {
		t.Fatal("IPIfNonMatch should let IP rules resolve domains")
	}
	if bin != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("mihomo rejected config: %v\n%s", err, cfg)
		}
	}
}
