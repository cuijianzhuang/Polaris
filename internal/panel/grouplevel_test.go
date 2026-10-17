package panel

import (
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestGroupLevels checks tiered node access: a group of level L reaches every
// node of level 1..L, a group can also bind whole nodes, and untiered nodes
// (level 0) are only reachable through explicit grants.
func TestGroupLevels(t *testing.T) {
	s, _, c := newAdminClient(t)
	for i, lvl := range []int{1, 2, 0} {
		id := fmt.Sprintf("n%d", i+1)
		s.st.DB.Create(&store.Node{ID: id, Name: id, Address: "203.0.113.1"})
		s.st.DB.Create(&store.Inbound{ID: "in-" + id, NodeID: id, Tag: "t-" + id, Core: model.CoreXray, Port: 20000 + i, Enabled: true})
		if lvl > 0 {
			if code, out := c.do("PUT", "/nodes/"+id, map[string]any{"name": id, "address": "203.0.113.1", "level": lvl}); code != 200 {
				t.Fatalf("set node level %d %v", code, out)
			}
		}
	}
	mkGroup := func(name string, body map[string]any) string {
		body["name"] = name
		code, out := c.do("POST", "/groups", body)
		if code != 200 {
			t.Fatalf("group %s: %d %v", name, code, out)
		}
		return out["id"].(string)
	}
	basic := mkGroup("basic", map[string]any{"level": 1})
	vip := mkGroup("vip", map[string]any{"level": 2})
	special := mkGroup("special", map[string]any{"nodeIds": []string{"n3"}})
	for u, g := range map[string]string{"ub": basic, "uv": vip, "us": special} {
		s.st.DB.Create(&store.User{ID: u, Username: u, UUID: u, Status: "active", SubToken: u})
		s.st.DB.Create(&store.UserGroup{UserID: u, GroupID: g})
	}
	reach := func(user string) []string {
		var ids []string
		s.st.DB.Raw("SELECT DISTINCT inbound_id FROM ("+store.GrantsSQL+") g WHERE user_id = ?", user).Scan(&ids)
		sort.Strings(ids)
		return ids
	}
	want := map[string][]string{"ub": {"in-n1"}, "uv": {"in-n1", "in-n2"}, "us": {"in-n3"}}
	for u, w := range want {
		if got := reach(u); !slices.Equal(got, w) {
			t.Errorf("%s reaches %v, want %v", u, got, w)
		}
	}
	// The node spec carries the users who may connect.
	spec, err := s.BuildSpec("n2")
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, u := range spec.Inbounds[0].Users {
		users = append(users, u.ID)
	}
	if !slices.Equal(users, []string{"uv"}) {
		t.Fatalf("n2 users %v, want only the vip user", users)
	}
	// Lowering a node's level opens it to the lower tier.
	c.do("PUT", "/nodes/n2", map[string]any{"name": "n2", "address": "203.0.113.1", "level": 1})
	if got := reach("ub"); !slices.Equal(got, []string{"in-n1", "in-n2"}) {
		t.Fatalf("after lowering n2: %v", got)
	}
	// Groups report their level and nodes; deleting a node drops the binding.
	var g store.Group
	s.st.DB.First(&g, "id = ?", vip)
	if g.Level != 2 {
		t.Fatalf("vip level %d", g.Level)
	}
	if code, _ := c.do("POST", "/groups", map[string]any{"name": "bad", "level": 100}); code != 400 {
		t.Fatal("level out of range accepted")
	}
	if code, _ := c.do("DELETE", "/nodes/n3", nil); code >= 300 {
		t.Fatalf("delete node %d", code)
	}
	var n int64
	s.st.DB.Model(&store.GroupNode{}).Where("node_id = ?", "n3").Count(&n)
	if n != 0 {
		t.Fatal("group binding to a deleted node kept")
	}
}
