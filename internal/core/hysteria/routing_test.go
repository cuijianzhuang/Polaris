package hysteria

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func routingSpec(t *testing.T, port int) *model.NodeSpec {
	in := coretest.Inbound(model.CoreHysteria, "hy", port, model.ProtocolSpec{Type: model.ProtoHysteria2}, model.TransportSpec{}, coretest.TLS(t, "h.example.com"))
	in.Listen = "127.0.0.1"
	return &model.NodeSpec{
		Inbounds: []model.Inbound{in},
		Outbounds: []model.Outbound{
			{Tag: "warp", Type: model.OutSOCKS, Settings: coretest.Settings(model.OutboundSettings{Server: "127.0.0.1", Port: 40000, Username: "a", Password: "b"})},
			{Tag: "corp", Type: model.OutHTTP, Settings: coretest.Settings(model.OutboundSettings{Server: "proxy.example.com", Port: 8443, Security: &model.ClientTLS{Type: model.SecTLS}})},
			{Tag: "land", Type: model.OutVLESS, Settings: coretest.Settings(model.OutboundSettings{Server: "198.51.100.2", Port: 443, UUID: "11111111-2222-4333-8444-555555555555"})},
			{Tag: "chained", Type: model.OutSOCKS, Detour: "warp", Settings: coretest.Settings(model.OutboundSettings{Server: "10.0.0.9", Port: 1080})},
		},
		Routes: []model.RouteRule{
			{Priority: 1, Match: model.RouteMatch{IPCIDR: []string{"10.0.0.0/8"}}, Outbound: "block"},
			{Priority: 2, Match: model.RouteMatch{DomainSuffix: []string{"openai.com"}, Domain: []string{"claude.ai"}}, Outbound: "warp"},
			{Priority: 3, Match: model.RouteMatch{Network: "udp", Port: []string{"443"}}, Outbound: "block"},
			{Priority: 4, Match: model.RouteMatch{DomainSuffix: []string{"intra.example.com"}, Network: "tcp"}, Outbound: "corp"},
			{Priority: 5, Match: model.RouteMatch{DomainSuffix: []string{"netflix.com"}}, Outbound: "land"},                       // VLESS: not carried
			{Priority: 6, Match: model.RouteMatch{Users: []string{"u1"}}, Outbound: "warp"},                                       // per-user: not expressible
			{Priority: 7, Match: model.RouteMatch{Inbound: []string{"other"}, DomainSuffix: []string{"x.com"}}, Outbound: "warp"}, // other inbound
			{Priority: 8, Match: model.RouteMatch{DomainKeyword: []string{"ads"}}, Outbound: "block"},                             // keyword only
			{Priority: 9, Match: model.RouteMatch{DomainSuffix: []string{"y.com"}}, Outbound: "chained"},                          // chained: not carried
		},
	}
}

func TestRenderRouting(t *testing.T) {
	spec := routingSpec(t, 26445)
	outs, acl := renderRouting(spec, "hy", "46")
	want := []string{
		"reject(10.0.0.0/8)",
		"warp(claude.ai)", "warp(suffix:openai.com)",
		"reject(all, udp/443)",
		"corp(suffix:intra.example.com, tcp)",
	}
	if !slices.Equal(acl, want) {
		t.Fatalf("acl:\n%s\nwant:\n%s", strings.Join(acl, "\n"), strings.Join(want, "\n"))
	}
	b, _ := json.Marshal(outs)
	for _, w := range []string{`"name":"direct"`, `"mode":"46"`, `"type":"socks5"`, `"username":"a"`, `"url":"https://proxy.example.com:8443"`} {
		if !strings.Contains(string(b), w) {
			t.Fatalf("outbounds missing %s: %s", w, b)
		}
	}
	if strings.Contains(string(b), "land") || strings.Contains(string(b), "chained") {
		t.Fatalf("unsupported outbounds rendered: %s", b)
	}
	if first, _ := outs[0].(map[string]any); first["name"] != "direct" {
		t.Fatalf("direct must be first (Hysteria's default): %s", b)
	}
	// Nothing to route and no IP mode: the config stays as before.
	if o, a := renderRouting(&model.NodeSpec{}, "hy", ""); o != nil || a != nil {
		t.Fatal("empty spec should add nothing")
	}
}

// TestLiveRouting checks a real Hysteria accepts the outbounds and ACL.
func TestLiveRouting(t *testing.T) {
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
	if _, err := a.Render(routingSpec(t, free())); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(a.Logs(20), "\n"))
	}
	defer a.Stop(ctx)
	time.Sleep(800 * time.Millisecond)
	if st := a.Health(ctx); !st.Running {
		t.Fatalf("not running %+v\n%s", st, strings.Join(a.Logs(20), "\n"))
	}
}
