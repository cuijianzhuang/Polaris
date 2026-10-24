package model

import "testing"

func TestRouteTraceTargets(t *testing.T) {
	for _, r := range []RouteTraceRequest{{}, {IPv4: "127.0.0.1"}, {IPv4: "10.0.0.1"}, {IPv4: "169.254.169.254"}, {IPv4: "224.0.0.1"}, {IPv4: "example.com"}, {IPv4: "1.1.1.1;id"}, {IPv6: "::1"}, {IPv6: "fe80::1%eth0"}, {IPv6: "fc00::1"}, {IPv6: "::ffff:1.1.1.1"}, {IPv4: "2606:4700::1111"}, {IPv6: "1.1.1.1"}} {
		if r.Validate() == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
	if err := (RouteTraceRequest{IPv4: "1.1.1.1", IPv6: "2606:4700:4700::1111"}).Validate(); err != nil {
		t.Fatal(err)
	}
}
