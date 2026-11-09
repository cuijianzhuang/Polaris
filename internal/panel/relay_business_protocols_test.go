package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestExtendedBusinessDiagnosticsRequireMatchingAgent(t *testing.T) {
	for _, typ := range []model.CoreType{model.CoreXray, model.CoreSingbox} {
		protocols := []model.Protocol{model.ProtoVMess, model.ProtoTrojan}
		if typ == model.CoreSingbox {
			protocols = append(protocols, model.ProtoAnyTLS, model.ProtoHysteria2, model.ProtoTUIC)
		}
		for _, protocol := range protocols {
			t.Run(string(typ)+"/"+string(protocol), func(t *testing.T) {
				transport := model.TransTCP
				if protocol == model.ProtoHysteria2 || protocol == model.ProtoTUIC {
					transport = model.TransQUIC
				}
				in := store.Inbound{NodeID: "land", Core: typ, Protocol: store.NewJSON(model.ProtocolSpec{Type: protocol}), Transport: store.NewJSON(model.TransportSpec{Type: transport}), Security: store.NewJSON(coretest.TLS(t, "test.example"))}
				legacy := []string{model.RelayBusinessFeature, model.SingboxRelayBusinessFeature}
				landing := store.Node{AgentFeatures: store.NewJSON(legacy)}
				installed := store.Core{NodeID: "land", Type: typ, RunningVersion: "1.13.5"}
				if support := relayBusinessAvailability(in, landing, installed); support.Supported {
					t.Fatal("old agent offered new protocol", support)
				}
				landing.AgentFeatures = store.NewJSON(append(legacy, model.ExtendedRelayBusinessFeature))
				if support := relayBusinessAvailability(in, landing, installed); !support.Supported || support.RequiresRestart != (typ == model.CoreSingbox) {
					t.Fatal("extended capability not reflected", support)
				}
				in.Extra = []byte(`{"listen_port":1234}`)
				if support := relayBusinessAvailability(in, landing, installed); support.Supported {
					t.Fatal("native override admitted", support)
				}
			})
		}
	}
}
