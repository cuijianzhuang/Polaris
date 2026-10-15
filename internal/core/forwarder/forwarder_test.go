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
