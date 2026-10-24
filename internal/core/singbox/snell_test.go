package singbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func snellSpec(t *testing.T) *model.NodeSpec {
	in := coretest.Inbound(model.CoreSingbox, "snell", coretest.FreePort(t), model.ProtocolSpec{Type: model.ProtoSnell, SnellVersion: 5, ServerKey: "server-psk-not-a-user-key"}, model.TransportSpec{}, model.SecuritySpec{})
	in.Listen = "127.0.0.1"
	in.Sniffing.Enabled = false
	return &model.NodeSpec{Inbounds: []model.Inbound{in}}
}

func TestSnellCapabilityAndFailClosed(t *testing.T) {
	a := New(core.Options{Version: "1.14.4"})
	s := snellSpec(t)
	if a.Capabilities("1.13.5").SupportsProtocol(model.ProtoSnell) || !a.Capabilities("1.14.0").SupportsProtocol(model.ProtoSnell) {
		t.Fatal("wrong Snell version gate")
	}
	if _, err := New(core.Options{Version: "1.13.5"}).Render(s); err == nil {
		t.Fatal("old core accepted Snell")
	}
	a.stats = false
	if _, err := a.Render(s); err == nil {
		t.Fatal("unmetered core accepted Snell")
	}
	a.stats = true
	b, err := a.Render(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"userkey": "pw-one-123456"`, `"name": "u2"`, `"version": 5`, `"psk": "server-psk-not-a-user-key"`} {
		if !strings.Contains(string(b), want) {
			t.Fatal("missing", want)
		}
	}
	for _, extra := range []string{`{"users":[]}`, `{"UsErS":[]}`, `{"type":"socks"}`, `{"psk":"shared"}`, `{"tag":"wrong"}`, `{"detour":"bypass"}`} {
		s.Inbounds[0].Extra = json.RawMessage(extra)
		if _, err := a.Render(s); err == nil {
			t.Fatal("auth bypass accepted", extra)
		}
	}
	s.Inbounds[0].Extra = nil
	s.Inbounds[0].Users[1].Password = s.Inbounds[0].Users[0].Password
	if _, err := a.Render(s); err == nil {
		t.Fatal("shared user keys accepted")
	}
	s.Inbounds[0].Users = nil
	b, err = a.Render(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"type": "snell"`) {
		t.Fatal("empty users enabled shared PSK mode")
	}
}
