package core

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func egressSpec(p model.OutboundIPStrategy) *model.NodeSpec {
	return &model.NodeSpec{Inbounds: []model.Inbound{{Tag: "v4", Core: model.CoreXray, Enabled: true, OutboundIPStrategy: p, Users: []model.User{{ID: "alice"}}}, {Tag: "auto", Core: model.CoreXray, Enabled: true}}, Outbounds: []model.Outbound{
		{Tag: "server", Type: model.OutSOCKS, Settings: json.RawMessage(`{"server":"dual.test","port":1080}`)},
		{Tag: "backup", Type: model.OutBalancer, Settings: json.RawMessage(`{"members":["server"],"strategy":"roundRobin"}`)},
		{Tag: "primary", Type: model.OutBalancer, Settings: json.RawMessage(`{"members":["server"],"strategy":"roundRobin","fallback":"backup"}`)},
	}, Routes: []model.RouteRule{
		{Priority: 0, Match: model.RouteMatch{UserGroups: []string{"empty"}}, Outbound: "block"},
		{Priority: 1, Match: model.RouteMatch{Inbound: []string{"auto"}}, Outbound: "block"},
		{Priority: 2, Match: model.RouteMatch{Domain: []string{"full:example.test"}}, Outbound: "primary"},
		{Priority: 3, Outbound: "block"}, {Priority: 4, Outbound: "direct"},
	}}
}
func TestEgressIsolationAndLimitContext(t *testing.T) {
	s := egressSpec(model.OutboundPreferIPv4)
	s.UserLimits = []model.UserLimit{{User: "alice", UpKbps: 100}}
	before, _ := json.Marshal(s)
	if _, err := PlanEgress(s, model.CoreXray, "26.5.9", nil); err == nil {
		t.Fatal("old limiter allowed")
	}
	p, err := PlanEgress(s, model.CoreXray, "26.5.9", &model.LimitWire{Contexts: true, ReentryProtocol: "vless"})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("mutated input")
	}
	if len(p.Spec.Outbounds) <= len(s.Outbounds) {
		t.Fatal("no isolated graph")
	}
	var reentry, udp bool
	for _, r := range p.Spec.Routes[:len(p.Spec.Routes)-len(s.Routes)] {
		if len(r.Match.UserGroups) > 0 {
			t.Fatal("empty group widened")
		}
		if reflect.DeepEqual(r.Match.Inbound, []string{"auto"}) {
			t.Fatal("other inbound changed")
		}
		if reflect.DeepEqual(r.Match.Inbound, []string{model.LimitInbound}) {
			reentry = true
			if !reflect.DeepEqual(r.Match.Users, []string{model.ScopedLimitUser("alice", "v4") + model.LimitAlias}) {
				t.Fatal(r.Match)
			}
		}
		udp = udp || r.Match.Network == "udp"
	}
	if !udp || !reentry {
		t.Fatal("missing UDP or limiter route")
	}
	original := map[string]bool{}
	for _, o := range s.Outbounds {
		original[o.Tag] = true
	}
	var groups int
	for _, o := range p.Spec.Outbounds[len(s.Outbounds):] {
		for tag := range original {
			if strings.HasPrefix(o.Tag, tag) {
				t.Fatal("private outbound captured by original prefix")
			}
		}
		v, _ := o.ParseSettings()
		if o.Type.IsGroup() {
			groups++
			for _, m := range v.Members {
				if original[m] {
					t.Fatal("member escaped scope")
				}
			}
			if original[v.Fallback] {
				t.Fatal("fallback escaped scope")
			}
		} else if !o.DialIPStrategy.IsAuto() && o.DialIPStrategy != model.OutboundPreferIPv4 {
			t.Fatal(o)
		}
	}
	if groups < 4 {
		t.Fatal("TCP/UDP fallback graphs missing", groups)
	}
}
func TestEgressRejectsUnsupportedAndLiteralHop(t *testing.T) {
	for _, tc := range []struct {
		typ     model.CoreType
		version string
	}{{model.CoreMihomo, "99.0.0"}, {model.CoreXray, "26.3.27"}, {model.CoreSingbox, "1.11.0"}} {
		if err := CheckOutboundIP(tc.typ, tc.version, model.OutboundIPv4Only); err == nil {
			t.Fatal(tc)
		}
	}
	s := egressSpec(model.OutboundIPv4Only)
	s.Outbounds[0].Settings = json.RawMessage(`{"server":"::1","port":1080}`)
	p, err := PlanEgress(s, model.CoreXray, "26.5.9", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range p.Spec.Outbounds[len(s.Outbounds):] {
		if o.Type == model.OutSOCKS {
			t.Fatal("wrong-family literal proxy hop allowed")
		}
	}
	s.DNS = &model.DNSSpec{Strategy: "ipv4_only"}
	if _, err = PlanEgress(s, model.CoreXray, "26.5.9", nil); err == nil {
		t.Fatal("conflicting DNS allowed")
	}
}
func TestSingStrictGroupGuard(t *testing.T) {
	s := egressSpec(model.OutboundIPv6Only)
	s.Inbounds[0].Core = model.CoreSingbox
	s.Routes = []model.RouteRule{{Outbound: "g"}}
	s.Outbounds = []model.Outbound{{Tag: "g", Type: model.OutSelector, Settings: json.RawMessage(`{"members":["direct","block"]}`)}}
	p, err := PlanEgress(s, model.CoreSingbox, "1.13.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Spec.Routes[0].Match.NativeIPVersion != 4 || p.Spec.Routes[0].Outbound != "block" {
		t.Fatal("group bypassed strict literal guard", p.Spec.Routes)
	}
	s.Outbounds[0].Settings = json.RawMessage(`{"members":["direct","p"]}`)
	s.Outbounds = append(s.Outbounds, model.Outbound{Tag: "p", Type: model.OutSOCKS, Settings: json.RawMessage(`{"server":"dual.test","port":1080}`)})
	if _, err := PlanEgress(s, model.CoreSingbox, "1.13.5", nil); err == nil {
		t.Fatal("unsafe mixed group allowed")
	}
}

func TestEgressTrafficOrigins(t *testing.T) {
	got := EgressTraffic(map[string][2]int64{"alice": {10, 20}, "outbound>>>private1": {30, 40}, "outbound>>>private2": {50, 60}, "outbound>>>proxy": {1, 2}, "inbound>>>entry": {10, 20}}, map[string]string{"private1": "proxy", "private2": "proxy"})
	want := map[string][2]int64{"alice": {10, 20}, "outbound>>>proxy": {81, 102}, "inbound>>>entry": {10, 20}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}
