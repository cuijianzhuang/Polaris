package xray

import (
	"os"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestRenderDNS(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_BIN")
	a := New(core.Options{Version: "26.3.27", WorkDir: t.TempDir(), BinPath: bin})
	obs, routes := coretest.Chain()
	in := coretest.Inbound(model.CoreXray, "vless", 24443, model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{}, model.SecuritySpec{})
	spec := &model.NodeSpec{Inbounds: []model.Inbound{in}, Outbounds: obs, Routes: routes, DNS: coretest.DNS()}
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"fakedns"`, `"geosite:cn"`, `"keyword:baidu"`, `"https://dns.google:443/dns-query"`,
		`"tag": "dns-google"`, `"queryStrategy": "UseIP"`, `"domainStrategy": "IPIfNonMatch"`, `"panel.internal": "10.0.0.2"`} {
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("missing %s in:\n%s", want, cfg)
		}
	}
	if bin != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("xray rejected config: %v\n%s", err, cfg)
		}
	}
	spec.DNS.Servers[0].Address = "tls://1.1.1.1"
	if _, err := a.Render(spec); err == nil || !strings.Contains(err.Error(), "DoT") {
		t.Fatalf("DoT should be refused on Xray, got %v", err)
	}
}
