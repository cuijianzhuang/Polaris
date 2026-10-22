package singbox

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

// TestWARPOutbound: the same WARP outbound and rule on sing-box (a
// WireGuard endpoint plus a geosite rule set).
func TestWARPOutbound(t *testing.T) {
	in := coretest.Inbound(model.CoreSingbox, "vless", 2102, model.ProtocolSpec{Type: model.ProtoVLESS},
		model.TransportSpec{Type: model.TransTCP}, model.SecuritySpec{})
	set, _ := json.Marshal(model.OutboundSettings{Server: "162.159.192.1", Port: 2408,
		PrivateKey: "aGVsbG9oZWxsb2hlbGxvaGVsbG9oZWxsb2hlbGxvMDE=", PeerPublicKey: "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=",
		LocalAddress: []string{"172.16.0.2/32", "2606:4700:110:8a36::1/128"}, Reserved: []int{1, 2, 3}, MTU: 1280})
	sp := &model.NodeSpec{Inbounds: []model.Inbound{in},
		Outbounds: []model.Outbound{{ID: "o1", Tag: "warp", Type: model.OutWireGuard, Settings: set}},
		Routes:    []model.RouteRule{{ID: "r1", Match: model.RouteMatch{Geosite: []string{"openai"}}, Outbound: "warp", Priority: 25}}}
	a := New(core.Options{Version: "1.12.12", WorkDir: t.TempDir(), BinPath: os.Getenv("POLARIS_SINGBOX_BIN")})
	cfg, err := a.Render(sp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	for _, want := range []string{`"type": "wireguard"`, `"reserved"`, `geosite-openai`, `"outbound": "warp"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s:\n%s", want, s)
		}
	}
	if os.Getenv("POLARIS_SINGBOX_BIN") != "" {
		if err := a.Validate(cfg); err != nil {
			t.Fatalf("sing-box rejected the WARP config: %v\n%s", err, cfg)
		}
	}
}
