package forwarder

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestRender(t *testing.T) {
	a := New(core.Options{})
	cfg, err := a.Render(&model.NodeSpec{Forwards: []model.ForwardRule{
		{Tag: "c1", Listen: 30443, Network: "tcp", Target: "198.51.100.2:443"},
		{Tag: "c2", Listen: 30444, Network: "udp", Target: "198.51.100.2:8443"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	if !strings.Contains(s, `listen = "[::]:30443"`) || !strings.Contains(s, `remote = "198.51.100.2:8443"`) || !strings.Contains(s, "no_tcp = true") {
		t.Fatalf("%s", s)
	}
}

func TestRenderForwardOptions(t *testing.T) {
	a := New(core.Options{Version: "2.7.0", WorkDir: t.TempDir()})
	cfg, err := a.Render(&model.NodeSpec{Forwards: []model.ForwardRule{{
		Tag: "lb", Listen: 30443, Network: "tcp", Target: "a.example:443", ExtraTargets: []string{"b.example:443"},
		Balance: "roundrobin", Weights: []int{2, 1}, Through: "10.0.0.2",
		RemoteTransport: "ws;host=a;path=/x;tls;sni=a;insecure", SendProxy: 2,
	}, {Tag: "u", ListenIP: "1.2.3.4", Listen: 30053, Network: "udp", Target: "8.8.8.8:53", RemoteTransport: "ws;host=a;path=/"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	for _, want := range []string{`extra_remotes = ["b.example:443"]`, `balance = "roundrobin: 2, 1"`, `through = "10.0.0.2"`,
		`remote_transport = "ws;host=a;path=/x;tls;sni=a;insecure"`, `network = { use_udp = false, send_proxy = true, send_proxy_version = 2 }`,
		`listen = "1.2.3.4:30053"`, `network = { no_tcp = true, use_udp = true }`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in\n%s", want, s)
		}
	}
	if strings.Count(s, "remote_transport") != 1 {
		t.Error("UDP forwards must not get a transport")
	}
}
