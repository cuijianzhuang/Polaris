package hysteria

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestRenderDNS(t *testing.T) {
	a := New(core.Options{WorkDir: t.TempDir()})
	in := coretest.Inbound(model.CoreHysteria, "hy", 26443, model.ProtocolSpec{Type: model.ProtoHysteria2}, model.TransportSpec{}, coretest.TLS(t, "h.example.com"))
	spec := &model.NodeSpec{Inbounds: []model.Inbound{in}, DNS: coretest.DNS()}
	out, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]struct{ Config map[string]any }
	json.Unmarshal(out, &m)
	cfg := m["hy"].Config
	r, _ := json.Marshal(cfg["resolver"])
	if !strings.Contains(string(r), `"type":"https"`) || !strings.Contains(string(r), `"addr":"dns.google:443"`) || !strings.Contains(string(r), `"sni":"dns.google"`) {
		t.Fatalf("resolver: %s", r)
	}
	if o, _ := json.Marshal(cfg["outbounds"]); !strings.Contains(string(o), `"mode":"46"`) {
		t.Fatalf("outbounds: %s", o)
	}
	// A system or DoQ default keeps the system resolver.
	spec.DNS.Servers = []model.DNSServer{{Tag: "q", Address: "quic://94.140.14.14"}}
	out, _ = a.Render(spec)
	m = nil
	json.Unmarshal(out, &m)
	if _, ok := m["hy"].Config["resolver"]; ok {
		t.Fatal("DoQ is not supported by Hysteria")
	}
}

// TestLiveDNS checks a real Hysteria accepts the resolver and outbound.
func TestLiveDNS(t *testing.T) {
	bin := os.Getenv("POLARIS_HYSTERIA_BIN")
	if bin == "" {
		t.Skip("POLARIS_HYSTERIA_BIN not set")
	}
	free := func() int {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	a := New(core.Options{BinPath: bin, WorkDir: t.TempDir(), APIListen: fmt.Sprintf("127.0.0.1:%d", free())})
	auth := httptest.NewServer(a)
	defer auth.Close()
	a.opt.AuthURL = auth.URL + "/auth/hysteria"
	in := coretest.Inbound(model.CoreHysteria, "hy", free(), model.ProtocolSpec{Type: model.ProtoHysteria2}, model.TransportSpec{}, coretest.TLS(t, "h.example.com"))
	in.Listen = "127.0.0.1"
	for _, addr := range []string{"https://dns.google/dns-query", "tls://1.1.1.1", "tcp://8.8.8.8", "8.8.8.8"} {
		d := coretest.DNS()
		d.Servers = []model.DNSServer{{Tag: "d", Address: addr}}
		if _, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}, DNS: d}); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := a.Reload(ctx); err != nil {
			t.Fatalf("%s: %v\n%s", addr, err, strings.Join(a.Logs(20), "\n"))
		}
		time.Sleep(700 * time.Millisecond)
		if st := a.Health(ctx); !st.Running {
			t.Fatalf("%s: not running %+v\n%s", addr, st, strings.Join(a.Logs(20), "\n"))
		}
	}
	a.Stop(context.Background())
}
