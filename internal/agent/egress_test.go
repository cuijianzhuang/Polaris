package agent

import (
	"context"
	"github.com/cuijianzhuang/polaris/internal/model"
	"testing"
)

func TestOutboundIPRefusesBeforeApply(t *testing.T) {
	for _, tc := range []struct {
		core    model.CoreType
		version string
	}{{model.CoreXray, "26.3.27"}, {model.CoreXray, "26.4.17"}, {model.CoreXray, "26.4.25"}, {model.CoreSingbox, "1.11.0"}, {model.CoreMihomo, "1.19.15"}} {
		t.Run(string(tc.core)+tc.version, func(t *testing.T) {
			log := &callLog{}
			fake := newFake(tc.core, log, false)
			a := newFakeAgent(t, fake)
			in := vlessIn(tc.core, "in", freePortT(t))
			in.OutboundIPStrategy = model.OutboundIPv4Only
			r := a.Apply(context.Background(), &model.NodeSpec{Rev: 1, Cores: []model.CoreSpec{{Type: tc.core, Version: tc.version}}, Inbounds: []model.Inbound{in}})
			if r.ok || r.err == nil || a.appliedRev() != 0 || len(log.all()) != 0 {
				t.Fatalf("unsupported policy reached a running core: %+v %v", r, log.all())
			}
		})
	}
}
