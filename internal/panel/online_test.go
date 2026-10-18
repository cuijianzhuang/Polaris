package panel

import (
	"testing"
	"time"

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
	s.onlineMu.Lock()
	s.onlineIPs["u1"] = map[string]map[string]time.Time{
		"n1": {"1.1.1.1": now, "2.2.2.2": now.Add(-onlineIPWindow - time.Second)}, // second one is stale
		"n2": {"1.1.1.1": now.Add(-time.Second)},
	}
	s.onlineMu.Unlock()

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
