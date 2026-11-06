package singbox

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestUnsupportedOutboundTransportCannotBecomeTCP(t *testing.T) {
	for _, transport := range []model.Transport{model.TransXHTTP, model.TransKCP} {
		t.Run(string(transport), func(t *testing.T) {
			a := New(core.Options{Version: "1.13.5", WorkDir: t.TempDir()})
			o := model.Outbound{Tag: "relay-next", Type: model.OutVLESS, Settings: coretest.Settings(model.OutboundSettings{Server: "example.com", Port: 443, UUID: coretest.Users()[0].UUID, Transport: &model.TransportSpec{Type: transport, Path: "/relay"}})}
			if _, err := a.Render(&model.NodeSpec{Outbounds: []model.Outbound{o}}); err == nil || !strings.Contains(err.Error(), string(transport)) {
				t.Fatal("unsupported stream silently rendered as TCP", err)
			}
		})
	}
}
