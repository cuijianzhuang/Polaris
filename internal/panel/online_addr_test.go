package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestNodeOwnAddresses(t *testing.T) {
	nodes := []store.Node{
		{Address: "1.2.3.4"},
		{Address: "node.example.com", PublicIPv4: "5.6.7.8"},
		{Address: "[2001:db8::1]:8443", PublicIPv6: "2001:db8::2"},
	}
	set := nodeOwnAddresses(nodes)
	for _, want := range []string{"1.2.3.4", "5.6.7.8", "2001:db8::1", "2001:db8::2"} {
		if !set.contains(want) {
			t.Errorf("node address %s missing from the set", want)
		}
	}
	// A hostname is not a literal, and a subscriber address is not in the set.
	for _, notWant := range []string{"node.example.com", "9.9.9.9", "2001:db8::99"} {
		if set.contains(notWant) {
			t.Errorf("%s should not be treated as a node address", notWant)
		}
	}
}

func TestGeoText(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"中国", "广东", "深圳"}, "中国 · 广东 · 深圳"},
		{[]string{"中国", "", "深圳"}, "中国 · 深圳"},
		{[]string{"新加坡", "新加坡", ""}, "新加坡"},
		{[]string{"", "", ""}, ""},
	}
	for _, c := range cases {
		if got := geoText(c.in...); got != c.want {
			t.Errorf("geoText(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Without any database the address is still listed, with a state saying why
// there is no location, rather than being dropped.
func TestOnlineDeviceGeoWithoutDatabase(t *testing.T) {
	s := newTestServer(t)
	geo, network, state := s.describeIP("203.0.113.9")
	if geo != "" || network != "" {
		t.Fatalf("geo=%q network=%q without a database, want empty", geo, network)
	}
	if state != "unavailable" {
		t.Fatalf("state = %q, want unavailable", state)
	}
}
