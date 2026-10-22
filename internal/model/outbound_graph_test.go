package model

import (
	"encoding/json"
	"testing"
)

func TestOutboundGraph(t *testing.T) {
	group := func(tag, fallback string, members ...string) Outbound {
		b, _ := json.Marshal(OutboundSettings{Members: members, Fallback: fallback})
		return Outbound{Tag: tag, Type: OutBalancer, Settings: b}
	}
	leaf := Outbound{Tag: "proxy", Type: OutSOCKS}
	for _, tc := range []struct {
		name string
		obs  []Outbound
		ok   bool
	}{
		{"chain", []Outbound{leaf, group("a", "b", "proxy"), group("b", "block", "direct")}, true},
		{"self", []Outbound{leaf, group("a", "a", "proxy")}, false},
		{"two groups", []Outbound{leaf, group("a", "b", "proxy"), group("b", "a", "proxy")}, false},
		{"missing target", []Outbound{leaf, group("a", "missing", "proxy")}, false},
		{"missing member", []Outbound{group("a", "direct", "missing")}, false},
		{"duplicate member", []Outbound{leaf, group("a", "direct", "proxy", "proxy")}, false},
		{"fallback is member", []Outbound{leaf, group("a", "proxy", "proxy")}, false},
		{"no members", []Outbound{group("a", "direct")}, false},
		{"duplicate tag", []Outbound{leaf, leaf}, false},
		{"mixed edges", []Outbound{{Tag: "proxy", Type: OutSOCKS, Detour: "a"}, group("a", "b", "direct"), group("b", "block", "proxy")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateOutboundGraph(tc.obs); (err == nil) != tc.ok {
				t.Fatalf("ok=%v, err=%v", tc.ok, err)
			}
		})
	}
}
