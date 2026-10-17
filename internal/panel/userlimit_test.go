package panel

import (
	"slices"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestUserSpeedLimits: a user's own rate wins per direction, otherwise the
// highest plan among their groups; Hysteria2-only users get none (it cannot
// route by user); rates reach the spec in kbit/s.
func TestUserSpeedLimits(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "n1", Address: "203.0.113.1"})
	s.st.DB.Create(&store.Inbound{ID: "x", NodeID: "n1", Tag: "x", Core: model.CoreXray, Port: 20001, Enabled: true})
	s.st.DB.Create(&store.Inbound{ID: "h", NodeID: "n1", Tag: "h", Core: model.CoreHysteria, Port: 20002, Enabled: true})
	group := func(name string, up, down int, inbounds ...string) string {
		code, out := c.do("POST", "/groups", map[string]any{"name": name, "inboundIds": inbounds, "speedUpMbps": up, "speedDownMbps": down})
		if code != 200 {
			t.Fatalf("group %s: %d %v", name, code, out)
		}
		return out["id"].(string)
	}
	basic := group("basic", 10, 20, "x")
	pro := group("pro", 5, 50, "x")
	hy := group("hy", 30, 30, "h")
	free := group("free", 0, 0, "x")
	add := func(id string, up, down int, groups ...string) {
		s.st.DB.Create(&store.User{ID: id, Username: id, UUID: id, Status: "active", SubToken: id, SpeedUpMbps: up, SpeedDownMbps: down})
		for _, g := range groups {
			s.st.DB.Create(&store.UserGroup{UserID: id, GroupID: g})
		}
	}
	add("both", 0, 0, basic, pro) // max per direction: up 10, down 50
	add("own", 100, 0, basic)     // own up 100, plan down 20
	add("hyonly", 0, 0, hy)       // Hysteria2 only: not limited
	add("none", 0, 0, free)       // no plan: not limited
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.UserLimit{
		{User: "both", UpKbps: 10000, DownKbps: 50000},
		{User: "own", UpKbps: 100000, DownKbps: 20000},
	}
	if !slices.Equal(spec.UserLimits, want) {
		t.Fatalf("limits %+v, want %+v", spec.UserLimits, want)
	}
	// Changing a group's plan reaches the users.
	if code, out := c.do("PUT", "/groups/"+pro, map[string]any{"name": "pro", "speedDownMbps": 8}); code != 200 {
		t.Fatalf("update group: %d %v", code, out)
	}
	spec, _ = s.BuildSpec("n1")
	if spec.UserLimits[0].DownKbps != 20000 {
		t.Fatalf("after plan change: %+v", spec.UserLimits)
	}
}
