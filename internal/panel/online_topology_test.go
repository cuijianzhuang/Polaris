package panel

import (
	"slices"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestOnlineRelayTopologyDoesNotHideClientOrInventActivity(t *testing.T) {
	s := newTestServer(t)
	nodes := []store.Node{
		{ID: "entry", Name: "Entry", Address: "203.0.113.1", RelayFor: store.NewJSON([]string{"landing", "idle"})},
		{ID: "landing", Name: "Landing", Address: "203.0.113.2"},
		{ID: "idle", Name: "Idle", Address: "203.0.113.3"},
	}
	for i := range nodes {
		if err := s.st.DB.Create(&nodes[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	m := memOnly(s)
	m.ips = map[string]map[string]map[string]time.Time{
		"client":     {"entry": {"198.51.100.1": now}, "landing": {"203.0.113.1": now}, "idle": {"198.51.100.99": now.Add(-onlineIPWindow - time.Second)}},
		"relay-only": {"landing": {"203.0.113.1": now}},
		"forwarded":  {"entry": {"198.51.100.1": now}, "landing": {"198.51.100.1": now}},
	}
	users := []store.User{{ID: "client"}, {ID: "relay-only"}, {ID: "forwarded"}}
	s.withOnline(users)
	u := users[0]
	if u.OnlineIPState != "partial" || u.OnlineIPCount == nil || *u.OnlineIPCount != 1 {
		t.Fatalf("client hidden by topology: %+v", u)
	}
	if len(u.OnlineNodes) != 2 || u.OnlineNodes[0].ID != "entry" || u.OnlineNodes[1].ID != "landing" {
		t.Fatalf("stale/configured nodes invented as active: %+v", u.OnlineNodes)
	}
	if len(u.OnlineNodes[0].RelayTargets) != 2 {
		t.Fatalf("missing manual targets: %+v", u.OnlineNodes)
	}
	client := u.OnlineDevices[0]
	if client.IP != "198.51.100.1" || client.Excluded != "" || !slices.Equal(client.Nodes, []string{"Entry"}) {
		t.Fatalf("wrong client evidence: %+v", client)
	}
	relay := u.OnlineDevices[1]
	if relay.IP != "203.0.113.1" || relay.Excluded != "node" || len(relay.RelayNodes) != 1 || relay.RelayNodes[0].ID != "entry" || !slices.Equal(relay.Nodes, []string{"Landing"}) {
		t.Fatalf("wrong relay evidence: %+v", relay)
	}
	if users[1].OnlineIPState != "unavailable" || users[1].OnlineIPCount != nil || len(users[1].OnlineNodes) != 1 {
		t.Fatalf("relay-only user lost connection evidence: %+v", users[1])
	}
	if users[2].OnlineIPCount == nil || *users[2].OnlineIPCount != 1 || users[2].OnlineDevices[0].Excluded != "" || len(users[2].OnlineDevices[0].Nodes) != 2 {
		t.Fatalf("forwarded client falsely classified as relay: %+v", users[2])
	}
}

func TestOnlineTopologyCombinesAdjacentConfiguredPaths(t *testing.T) {
	nodes := []store.Node{
		{ID: "a", Name: "A", Address: "[2001:db8::1]:443", RelayFor: store.NewJSON([]string{"b", "missing", "a"})},
		{ID: "b", Name: "B", PublicIPv4: "203.0.113.1"}, {ID: "c", Name: "C"},
	}
	chains := []store.Chain{
		{Hops: store.NewJSON([]store.ChainHop{{NodeID: "a"}, {NodeID: "b"}, {NodeID: "c"}})},
		{Disabled: true, Hops: store.NewJSON([]store.ChainHop{{NodeID: "c"}, {NodeID: "a"}})},
	}
	topology := buildOnlineTopology(nodes, chains)
	if len(topology.targets["a"]) != 1 || topology.targets["a"][0].ID != "b" || len(topology.targets["b"]) != 1 || topology.targets["b"][0].ID != "c" || len(topology.targets["c"]) != 0 {
		t.Fatalf("wrong directed adjacent topology: %+v", topology.targets)
	}
	if len(topology.addressOwners["2001:db8::1"]) != 1 || topology.addressOwners["2001:db8::1"][0].ID != "a" {
		t.Fatal("IPv6 address not mapped to owner")
	}
}

func TestRelayMetadataNeverExemptsRealClientsFromLimit(t *testing.T) {
	s := newTestServer(t)
	s.st.SetSetting("ip_ban_minutes", "10")
	u := store.User{ID: "u", Username: "u", Status: "active", IPLimit: 1}
	entry := store.Node{ID: "entry", Name: "Entry", RelayFor: store.NewJSON([]string{"landing"})}
	landing := store.Node{ID: "landing", Name: "Landing"}
	for _, row := range []any{&u, &entry, &landing} {
		if err := s.st.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	s.nodeUserSets.mu.Lock()
	s.nodeUserSets.m = map[string]nodeUsersEntry{
		"entry":   {set: map[string]bool{"u": true}, own: map[string]bool{"u": true}},
		"landing": {set: map[string]bool{"u": true}, own: map[string]bool{"u": true}},
	}
	s.nodeUserSets.mu.Unlock()
	report := func(node, ip string) {
		s.handleOnlineIPs(node, &polarisv1.OnlineIPs{Users: []*polarisv1.UserIPs{{UserId: "u", Ips: []string{ip}}}})
	}
	report("landing", "198.51.100.2")
	report("entry", "198.51.100.1")
	var got store.User
	if err := s.st.DB.First(&got, "id = ?", "u").Error; err != nil {
		t.Fatal(err)
	}
	view := []store.User{got}
	s.withOnline(view)
	if got.BannedUntil == nil || view[0].OnlineIPCount == nil || *view[0].OnlineIPCount != 2 {
		t.Fatalf("manual relay configuration bypassed real client limit: %+v", view[0])
	}
}
