package model

import (
	"encoding/json"
	"testing"
)

func TestSnellValidation(t *testing.T) {
	base := Inbound{Tag: "snell", Port: 1234, Core: CoreSingbox, Protocol: ProtocolSpec{Type: ProtoSnell, ServerKey: "secret-server-password"}}
	base.Normalize()
	if base.Protocol.SnellVersion != 5 || base.Network() != "tcp" {
		t.Fatal("incorrect Snell defaults")
	}
	if e := base.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, test := range []struct {
		name   string
		change func(*Inbound)
	}{
		{"core", func(in *Inbound) { in.Core = CoreMihomo }}, {"version", func(in *Inbound) { in.Protocol.SnellVersion = 4 }},
		{"transport", func(in *Inbound) { in.Transport.Type = TransWS }}, {"tls", func(in *Inbound) { in.Security.Type = SecTLS }},
		{"psk", func(in *Inbound) { in.Protocol.ServerKey = "short" }}, {"mode", func(in *Inbound) { in.Protocol.SnellMode = "default" }},
		{"obfs", func(in *Inbound) { in.Protocol.SnellObfs = "tls" }}, {"v6obfs", func(in *Inbound) { in.Protocol.SnellVersion = 6; in.Protocol.SnellObfs = "http" }},
		{"extra", func(in *Inbound) { in.Extra = json.RawMessage(`{"users":null}`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := base
			test.change(&in)
			if e := in.Validate(); e == nil {
				t.Fatal("invalid Snell accepted")
			}
		})
	}
}
