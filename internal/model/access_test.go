package model

import "testing"

func TestAccessTransition(t *testing.T) {
	old := []AccessRule{{Protocol: "tcp", Port: 443, Sources: []string{"198.51.100.0/24", "2001:db8::/32"}}, {Protocol: "udp", Port: 8443, Sources: []string{"198.51.100.8/32"}}}
	next := []AccessRule{{Protocol: "tcp", Port: 443, Sources: []string{"198.51.100.9/32", "2001:db8:1::/48", "203.0.113.1/32"}}}
	got, e := TransitionalAccess(old, next)
	if e != nil || len(got) != 2 || len(got[0].Sources) != 2 || got[0].Sources[0] != "198.51.100.9/32" {
		t.Fatalf("transition widened sources %+v %v", got, e)
	}
	next[0].Sources = []string{"203.0.113.1/32"}
	got, e = TransitionalAccess(old, next)
	if e != nil || len(got[0].Sources) != 0 {
		t.Fatalf("disjoint change must deny until commit %+v %v", got, e)
	}
	for _, rules := range [][]AccessRule{{{Protocol: "tcp;drop", Port: 443}}, {{Protocol: "tcp", Port: 0}}, {{Protocol: "tcp", Port: 443, Sources: []string{"1.2.3.4; accept"}}}} {
		if _, e := NormalizeAccessRules(rules); e == nil {
			t.Fatal("invalid rule accepted", rules)
		}
	}
}
