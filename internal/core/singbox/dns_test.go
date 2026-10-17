package singbox

import (
	"os"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
)

func TestRenderDNS(t *testing.T) {
	bin := os.Getenv("POLARIS_SINGBOX_BIN")
	a := New(core.Options{Version: "1.13.5", WorkDir: t.TempDir(), BinPath: bin})
	s := spec(t)
	s.DNS = coretest.DNS()
	s.DNS.Servers = append(s.DNS.Servers, coretest.DNS().Servers[0])
	s.DNS.Servers[len(s.DNS.Servers)-1].Tag, s.DNS.Servers[len(s.DNS.Servers)-1].Address = "dot", "tls://1.1.1.1"
	cfg, err := a.Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type": "fakeip"`, `"type": "hosts"`, `"domain_resolver": "dns-local"`, `"detour": "resi"`,
		`"final": "google"`, `"default_domain_resolver": "google"`, `"action": "resolve"`, `"type": "tls"`, `geosite-cn`} {
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("missing %s in:\n%s", want, cfg)
		}
	}
	if bin != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("sing-box rejected config: %v\n%s", err, cfg)
		}
	}
	// Legacy servers before 1.12.
	old := New(core.Options{Version: "1.11.0", WorkDir: t.TempDir()})
	cfg, err = old.Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"address": "https://dns.google:443/dns-query"`, `"address_resolver": "dns-local"`, `"inet4_range": "198.18.0.0/15"`} {
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("legacy: missing %s in:\n%s", want, cfg)
		}
	}
}
