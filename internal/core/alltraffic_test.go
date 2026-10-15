package core

import "testing"

func TestAllTraffic(t *testing.T) {
	m := AllTraffic([]Stat{
		{Name: "user>>>u1>>>traffic>>>uplink", Value: 1},
		{Name: "inbound>>>vless>>>traffic>>>downlink", Value: 5},
		{Name: "outbound>>>direct>>>traffic>>>uplink", Value: 7},
		{Name: "inbound>>>api>>>traffic>>>uplink", Value: 9},
	})
	if m["u1"][0] != 1 || m["inbound>>>vless"][1] != 5 || m["outbound>>>direct"][0] != 7 {
		t.Fatalf("got %v", m)
	}
	if _, ok := m["inbound>>>api"]; ok {
		t.Fatal("api handler counted")
	}
}
