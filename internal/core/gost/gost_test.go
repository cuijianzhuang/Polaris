package gost

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestRenderGost(t *testing.T) {
	a := New(core.Options{Version: "3.2.6", WorkDir: t.TempDir(), APIListen: "127.0.0.1:19999"})
	cfg, err := a.Render(&model.NodeSpec{Forwards: []model.ForwardRule{
		{Tag: "realm", Listen: 1, Target: "a:1"}, // realm rule: ignored here
		{Tag: "lb", Engine: model.EngineGost, Listen: 30443, Network: "tcp,udp", Target: "a.example:443", ExtraTargets: []string{"b.example:443"},
			Balance: "roundrobin", RateIn: 1024, RateOut: 2048, MaxConns: 100, AcceptProxy: true},
		{Tag: "cli", Engine: model.EngineGost, Listen: 30444, Network: "tcp", Target: "land.example:8443",
			TunnelRole: "client", TunnelType: "wss", TunnelHost: "cdn.example", TunnelPath: "/t", TunnelAuth: "pw"},
		{Tag: "srv", Engine: model.EngineGost, Listen: 8443, Network: "tcp,udp", Target: "127.0.0.1:443",
			TunnelRole: "server", TunnelType: "wss", TunnelPath: "/t", TunnelAuth: "pw"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	for _, want := range []string{"name: lb\n", "name: lb~udp", "strategy: round", "maxFails: 1", "$ 1024KB 2048KB", "$ 100",
		"proxyProtocol: 1", "type: relay", "chain: chain-cli", "password: pw", "path: /t", "addr: 127.0.0.1:19999"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "name: realm") {
		t.Error("realm rules must not reach gost")
	}
}

func TestParseMetrics(t *testing.T) {
	m := parseMetrics(strings.NewReader(`# HELP x
gost_service_transfer_input_bytes_total{host="h",service="fwd-a"} 100
gost_service_transfer_input_bytes_total{host="h",service="fwd-a~udp"} 5
gost_service_transfer_output_bytes_total{host="h",service="fwd-a"} 2.5e+03
`))
	if m["fwd-a"] != [2]int64{105, 2500} {
		t.Fatalf("got %v", m)
	}
}

func freePort(t *testing.T) int { return coretest.FreePort(t) }

func run(t *testing.T, bin string, spec *model.NodeSpec, api string) *Adapter {
	dir := t.TempDir()
	a := New(core.Options{Version: "3", BinPath: bin, WorkDir: dir, APIListen: api})
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/gost.yaml", cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop(context.Background()) })
	return a
}

func get(t *testing.T, port int) string {
	var last error
	for i := 0; i < 40; i++ {
		c := http.Client{Timeout: 3 * time.Second}
		resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return string(b)
		}
		last = err
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("no answer on :%d: %v", port, last)
	return ""
}

// Live: load-balanced forward, wss relay tunnel with a password, and per-rule
// traffic from the metrics endpoint (needs POLARIS_GOST_BIN).
func TestLiveGost(t *testing.T) {
	bin := os.Getenv("POLARIS_GOST_BIN")
	if bin == "" {
		t.Skip("POLARIS_GOST_BIN not set")
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello-gost") }))
	defer backend.Close()
	target := strings.TrimPrefix(backend.URL, "http://")
	dead := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	fwdPort, landPort, cliPort := freePort(t), freePort(t), freePort(t)
	api1, api2 := fmt.Sprintf("127.0.0.1:%d", freePort(t)), fmt.Sprintf("127.0.0.1:%d", freePort(t))
	relay := run(t, bin, &model.NodeSpec{Forwards: []model.ForwardRule{
		// Plain forward: a dead target first, failover must reach the backend.
		{Tag: "fwd-lb", Engine: model.EngineGost, Listen: fwdPort, Network: "tcp,udp", Target: dead, ExtraTargets: []string{target}, Failover: true,
			RateIn: 10240, RateOut: 10240, MaxConns: 50},
		// Relay end of a tunnel.
		{Tag: "fwd-cli", Engine: model.EngineGost, Listen: cliPort, Network: "tcp", Target: fmt.Sprintf("127.0.0.1:%d", landPort),
			TunnelRole: "client", TunnelType: "wss", TunnelPath: "/tun", TunnelSNI: "example.com", TunnelAuth: "s3cret"},
	}}, api1)
	run(t, bin, &model.NodeSpec{Forwards: []model.ForwardRule{
		{Tag: "fwd-land", Engine: model.EngineGost, Listen: landPort, Network: "tcp,udp", Target: target,
			TunnelRole: "server", TunnelType: "wss", TunnelPath: "/tun", TunnelAuth: "s3cret"},
	}}, api2)

	for i := 0; i < 3; i++ {
		if b := get(t, fwdPort); b != "hello-gost" {
			t.Fatalf("forward answered %q", b)
		}
	}
	if b := get(t, cliPort); b != "hello-gost" {
		t.Fatalf("tunnel answered %q", b)
	}
	time.Sleep(500 * time.Millisecond)
	ds, err := relay.CollectTraffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]model.TrafficDelta{}
	for _, d := range ds {
		got[d.UserID] = d
	}
	if d := got[core.TagPrefixInbound+"fwd-lb"]; d.Up == 0 || d.Down == 0 {
		t.Fatalf("no traffic for fwd-lb: %+v", ds)
	}
	// Deltas: a second collection without traffic reports nothing new.
	ds, _ = relay.CollectTraffic(context.Background())
	for _, d := range ds {
		if d.UserID == core.TagPrefixInbound+"fwd-lb" {
			t.Fatalf("counter not reset between collections: %+v", d)
		}
	}
}
