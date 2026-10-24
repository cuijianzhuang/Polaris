package agent

import (
	"context"
	"github.com/cuijianzhuang/polaris/internal/model"
	"testing"
)

func TestSnellApplyRejectsOldCoreBeforeMutation(t *testing.T) {
	log := &callLog{}
	a := newFakeAgent(t, newFake(model.CoreSingbox, log, false))
	spec := &model.NodeSpec{Cores: []model.CoreSpec{{Type: model.CoreSingbox, Version: "1.13.5"}}, Inbounds: []model.Inbound{{Tag: "snell", Port: 23456, Enabled: true, Core: model.CoreSingbox, Protocol: model.ProtocolSpec{Type: model.ProtoSnell}, Transport: model.TransportSpec{Type: model.TransTCP}, Security: model.SecuritySpec{Type: model.SecNone}}}}
	if result := a.Apply(context.Background(), spec); result.err == nil {
		t.Fatal("old core accepted Snell spec")
	}
	if len(log.all()) != 0 {
		t.Fatal("unsupported Snell mutated running core", log.all())
	}
}
