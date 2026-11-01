package panel

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A user's online IPs follow the live reports: fresh ones show with the
// nodes they reached and the client model, stale ones disappear, and what
// was stored last time is ignored.
func TestWithOnline(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "hk-1"})
	s.st.DB.Create(&store.Node{ID: "n2", Name: "sg-1"})
	s.st.DB.Create(&store.UserDevice{ID: "d1", UserID: "u1", HWID: "h", Model: "iPhone", OS: "iOS", IP: "1.1.1.1", LastSeen: time.Now()})
	now := time.Now()
	m := memOnly(s) // seeds per-IP times directly
	m.mu.Lock()
	m.ips["u1"] = map[string]map[string]time.Time{
		"n1": {"1.1.1.1": now, "2.2.2.2": now.Add(-onlineIPWindow - time.Second)}, // second one is stale
		"n2": {"1.1.1.1": now.Add(-time.Second)},
	}
	m.mu.Unlock()

	us := []store.User{
		{ID: "u1", OnlineIPs: store.NewJSON([]string{"9.9.9.9"})}, // stored list from long ago
		{ID: "u2", OnlineIPs: store.NewJSON([]string{"8.8.8.8"})}, // nothing live
	}
	s.withOnline(us)

	d := us[0].OnlineDevices
	if len(d) != 1 || d[0].IP != "1.1.1.1" || len(d[0].Nodes) != 2 || d[0].Nodes[0] != "hk-1" || d[0].Nodes[1] != "sg-1" || d[0].Model != "iPhone" {
		t.Fatalf("u1 devices: %+v", d)
	}
	if ips := us[0].OnlineIPs.V; len(ips) != 1 || ips[0] != "1.1.1.1" {
		t.Fatalf("u1 ips: %v", ips)
	}
	if len(us[1].OnlineDevices) != 0 || len(us[1].OnlineIPs.V) != 0 || us[1].OnlineDevices == nil {
		t.Fatalf("u2 should show nothing (and an empty list, not null): %+v %v", us[1].OnlineDevices, us[1].OnlineIPs.V)
	}
}

func TestOnlineIPAvailabilityAndCounts(t *testing.T) {
	s := newTestServer(t)
	s.st.SetSetting("ip_limit_trusted", "203.0.113.20/32")
	now := time.Now()
	m := memOnly(s)
	m.ips = map[string]map[string]map[string]time.Time{
		"known":   {"n1": {"203.0.113.10:1234": now}, "n2": {"::ffff:203.0.113.10": now, "2001:db8::1": now, "invalid": now}},
		"trusted": {"n1": {"203.0.113.20": now}},
		"proxy":   {"n1": {"104.16.1.1": now}},
		"partial": {"n1": {"104.16.1.1": now, "203.0.113.10": now}},
		"expired": {"n1": {"203.0.113.30": now.Add(-onlineIPWindow - time.Second)}},
	}
	users := []store.User{{ID: "known"}, {ID: "trusted"}, {ID: "proxy"}, {ID: "partial"}, {ID: "expired", OnlineAt: &now}, {ID: "unknown", OnlineAt: &now, OnlineIPs: store.NewJSON([]string{"9.9.9.9"})}, {ID: "inactive"}}
	s.withOnline(users)
	for i, want := range []string{"available", "available", "unavailable", "partial", "expired", "unavailable", "none"} {
		if users[i].OnlineIPState != want {
			t.Fatalf("%s: state %s, want %s", users[i].ID, users[i].OnlineIPState, want)
		}
	}
	if u := users[0]; u.OnlineIPCount == nil || *u.OnlineIPCount != 2 || len(u.OnlineIPs.V) != 2 || u.OnlineAt == nil || !u.OnlineAt.Equal(now) {
		t.Fatalf("canonical fresh sources: %+v", u)
	}
	if users[1].OnlineIPCount == nil || *users[1].OnlineIPCount != 0 || users[1].OnlineDevices[0].Excluded != "trusted" {
		t.Fatal("trusted IP was counted")
	}
	if users[2].OnlineIPCount != nil || users[2].OnlineDevices[0].Excluded != "cdn" {
		t.Fatal("CDN edge mistaken for a client IP")
	}
	if users[3].OnlineIPCount == nil || *users[3].OnlineIPCount != 1 {
		t.Fatal("partial count is not the known lower bound")
	}
	if users[4].OnlineIPSeenAt == nil || len(users[4].OnlineIPs.V) != 0 || users[4].OnlineIPCount != nil {
		t.Fatal("expired source revived")
	}
	if len(users[5].OnlineIPs.V) != 0 || users[5].OnlineIPSeenAt != nil {
		t.Fatal("historical database list used as live evidence")
	}
	b, err := json.Marshal(users[1])
	if err != nil || !strings.Contains(string(b), `"onlineIpCount":0`) {
		t.Fatalf("known zero count omitted: %s %v", b, err)
	}
}

func TestOnlineIPReportsNormalizeAndEmptyDoesNotMarkActive(t *testing.T) {
	s := newTestServer(t)
	s.st.SetSetting("ip_ban_minutes", "5")
	encoded, err := json.Marshal(model.NodeSpec{Inbounds: []model.Inbound{{Tag: "a", Users: []model.User{{ID: "u"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{&store.Node{ID: "n", DesiredRev: 1, AppliedRev: 1}, &store.User{ID: "u", Username: "online", IPLimit: 1}, &store.ConfigRevision{NodeID: "n", Rev: 1, Spec: s.st.Box.Seal(string(encoded)), Status: "applied"}} {
		if err := s.st.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	report := func(ips []string) {
		s.handleOnlineIPs("n", &polarisv1.OnlineIPs{Users: []*polarisv1.UserIPs{nil, {UserId: "u", Ips: ips}}})
	}
	report([]string{"203.0.113.8", "::ffff:203.0.113.8", "203.0.113.8:1234", "bad"})
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", "u").Error; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(u.OnlineIPs.V, []string{"203.0.113.8"}) || u.BannedUntil != nil || u.OnlineAt == nil {
		t.Fatalf("duplicate source falsely counted or banned: %+v", u)
	}
	report([]string{"bad", "0.0.0.0", "224.0.0.1"})
	view := []store.User{{ID: "u"}}
	s.withOnline(view)
	if len(view[0].OnlineIPs.V) != 1 {
		t.Fatal("malformed report cleared a valid snapshot")
	}
	past := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	if err := s.st.DB.Model(&u).Update("online_at", past).Error; err != nil {
		t.Fatal(err)
	}
	report(nil)
	u = store.User{}
	s.st.DB.First(&u, "id = ?", "u")
	if u.OnlineAt == nil || !u.OnlineAt.Equal(past) || len(u.OnlineIPs.V) != 0 {
		t.Fatal("empty report marked an idle user active", u.OnlineAt, u.OnlineIPs.V)
	}
	view = []store.User{u}
	s.withOnline(view)
	if view[0].OnlineIPState != "none" {
		t.Fatal("empty snapshot shown online", view[0].OnlineIPState)
	}
}
