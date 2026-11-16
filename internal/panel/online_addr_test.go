package panel

import (
	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
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

func TestNodeSourcesExcludedFromOnlineLimit(t *testing.T) {
	s := newTestServer(t)
	s.st.SetSetting("ip_ban_minutes", "10")
	u := store.User{ID: store.NewID(), Username: "relay-client", UUID: store.NewID(), Password: "p", SubToken: store.NewID(), Status: "active", IPLimit: 1}
	n := store.Node{ID: store.NewID(), Name: "relay", Address: "203.0.113.1", PublicIPv6: "2001:db8::1"}
	for _, v := range []any{&u, &n} {
		if err := s.st.DB.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	s.nodeUserSets.mu.Lock()
	s.nodeUserSets.m = map[string]nodeUsersEntry{n.ID: {set: map[string]bool{u.ID: true}, own: map[string]bool{u.ID: true}}}
	s.nodeUserSets.mu.Unlock()
	if _, err := s.ownNodeAddresses(); err != nil {
		t.Fatal(err)
	}
	// A newly observed public address must be excluded immediately.
	s.observePublicIP(&n, "203.0.113.2", "")
	report := func(ips ...string) {
		s.handleOnlineIPs(n.ID, &polarisv1.OnlineIPs{Users: []*polarisv1.UserIPs{{UserId: u.ID, Ips: ips}}})
	}
	report("203.0.113.1", "203.0.113.2", "2001:db8::1", "198.51.100.1")
	var got store.User
	if err := s.st.DB.First(&got, "id = ?", u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.BannedUntil != nil {
		t.Fatal("relay addresses caused a ban")
	}
	users := []store.User{got}
	s.withOnline(users)
	if users[0].OnlineIPCount == nil || *users[0].OnlineIPCount != 1 {
		t.Fatalf("wrong count: %+v", users[0])
	}
	excluded := 0
	for _, d := range users[0].OnlineDevices {
		if d.Excluded == "node" {
			excluded++
		}
	}
	if excluded != 3 {
		t.Fatalf("want 3 excluded node addresses, got %d", excluded)
	}
	report("198.51.100.1", "198.51.100.2")
	if err := s.st.DB.First(&got, "id = ?", u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.BannedUntil == nil {
		t.Fatal("two actual clients did not trigger the limit")
	}
}
