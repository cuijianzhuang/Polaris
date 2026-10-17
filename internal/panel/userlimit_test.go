package panel

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"

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

func TestNodeLimitedAndSpeedNote(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "n1", Address: "203.0.113.1"})
	s.st.DB.Create(&store.Inbound{ID: "x", NodeID: "n1", Tag: "x", Core: model.CoreXray, Port: 20001, Enabled: true})
	s.st.DB.Create(&store.Inbound{ID: "h", NodeID: "n1", Tag: "h", Core: model.CoreHysteria, Port: 20002, Enabled: true})
	s.st.DB.Create(&store.User{ID: "a", Username: "alice", UUID: "a", Status: "active", SubToken: "a", SpeedDownMbps: 50})
	s.st.DB.Create(&store.User{ID: "b", Username: "bob", UUID: "b", Status: "active", SubToken: "b", SpeedUpMbps: 10})
	s.st.DB.Create(&store.UserAccess{UserID: "a", Kind: "inbound", TargetID: "x"})
	s.st.DB.Create(&store.UserAccess{UserID: "b", Kind: "inbound", TargetID: "h"})

	s.handleHeartbeat("n1", &polarisv1.Heartbeat{Limited: []*polarisv1.LimitedUser{
		{UserId: "b", UpKbps: 10000, UpBps: 1_000_000},
		{UserId: "a", DownKbps: 50000, DownBps: 47_000_000},
	}})
	code, out := c.doList("GET", "/nodes/n1/limited")
	if code != 200 || len(out) != 2 {
		t.Fatalf("limited: %d %v", code, out)
	}
	first := out[0].(map[string]any)
	if first["username"] != "alice" || first["saturated"] != true || out[1].(map[string]any)["saturated"] != false {
		t.Fatalf("rows %v", out)
	}
	ua, ub := store.User{}, store.User{}
	s.st.DB.First(&ua, "id = ?", "a")
	s.st.DB.First(&ub, "id = ?", "b")
	if got := s.speedNote(&ua); got != "↑ 不限 · ↓ 50 Mbps" {
		t.Errorf("alice note %q", got)
	}
	if got := s.speedNote(&ub); got != "" {
		t.Errorf("Hysteria2-only user should get no note, got %q", got)
	}
}

// doList is apiClient.do for endpoints that answer with a JSON array.
func (a *apiClient) doList(method, path string) (int, []any) {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+a.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}
