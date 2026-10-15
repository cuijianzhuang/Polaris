package panel

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func userIDs(us []store.User) map[string]bool {
	m := map[string]bool{}
	for _, u := range us {
		m[u.ID] = true
	}
	return m
}

func TestDirectNodeAndInboundBindings(t *testing.T) {
	s, _, c := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "tokyo"})
	db.Create(&store.Node{ID: "n2", Name: "hk"})
	db.Create(&store.Inbound{ID: "i1", NodeID: "n1", Tag: "a", Port: 1001, Enabled: true})
	db.Create(&store.Inbound{ID: "i2", NodeID: "n1", Tag: "b", Port: 1002, Enabled: true})
	db.Create(&store.Inbound{ID: "i3", NodeID: "n2", Tag: "c", Port: 1003, Enabled: true})
	db.Create(&store.Group{ID: "g1", Name: "vip"})
	db.Create(&store.GroupInbound{GroupID: "g1", InboundID: "i3"})

	// alice: whole node n1; bob: one inbound on n2; carol: group only.
	code, alice := c.do("POST", "/users", map[string]any{"username": "alice", "nodeIds": []string{"n1"}})
	if code != 201 {
		t.Fatalf("create alice %d %v", code, alice)
	}
	_, bob := c.do("POST", "/users", map[string]any{"username": "bob", "inboundIds": []string{"i3"}})
	_, carol := c.do("POST", "/users", map[string]any{"username": "carol", "groupIds": []string{"g1"}})
	a, b, cr := alice["id"].(string), bob["id"].(string), carol["id"].(string)

	got, err := s.inboundUsers([]string{"i1", "i2", "i3"})
	if err != nil {
		t.Fatal(err)
	}
	if !userIDs(got["i1"])[a] || !userIDs(got["i2"])[a] || userIDs(got["i3"])[a] {
		t.Fatalf("node binding: %v", got)
	}
	if u := userIDs(got["i3"]); !u[b] || !u[cr] || len(u) != 2 {
		t.Fatalf("inbound + group binding on i3: %v", u)
	}
	// An inbound added later to a bound node is covered automatically.
	db.Create(&store.Inbound{ID: "i4", NodeID: "n1", Tag: "d", Port: 1004, Enabled: true})
	got, _ = s.inboundUsers([]string{"i4"})
	if !userIDs(got["i4"])[a] {
		t.Fatal("node binding should include new inbounds")
	}
	if ns := s.nodesForUsers(a); len(ns) != 1 || ns[0] != "n1" {
		t.Fatalf("nodesForUsers(alice) = %v", ns)
	}

	// The user list reports bindings; updates without nodeIds keep them.
	_, u := c.do("GET", "/users/"+a, nil)
	if ids := u["nodeIds"].([]any); len(ids) != 1 || ids[0] != "n1" {
		t.Fatalf("nodeIds: %v", u["nodeIds"])
	}
	c.do("PUT", "/users/"+a, map[string]any{"username": "alice", "remark": "x"})
	if got, _ = s.inboundUsers([]string{"i1"}); !userIDs(got["i1"])[a] {
		t.Fatal("update without nodeIds dropped the binding")
	}

	// Node view: via + inbound counts; bind / unbind from the node page.
	var rows []nodeUserRow
	getJSON(t, c, "/nodes/n2/users", &rows)
	via := map[string][]string{}
	for _, r := range rows {
		via[r.Username] = r.Via
	}
	if len(rows) != 2 || via["bob"][0] != "inbound" || via["carol"][0] != "group:vip" {
		t.Fatalf("node users: %+v", rows)
	}
	if code, _ := c.do("POST", "/nodes/n2/users", map[string]any{"userIds": []string{a}}); code != 204 {
		t.Fatalf("bind %d", code)
	}
	if got, _ = s.inboundUsers([]string{"i3"}); !userIDs(got["i3"])[a] {
		t.Fatal("bind from node page failed")
	}
	c.do("POST", "/nodes/n2/users", map[string]any{"userIds": []string{a}, "detach": true})
	if got, _ = s.inboundUsers([]string{"i3"}); userIDs(got["i3"])[a] {
		t.Fatal("unbind from node page failed")
	}

	// Bulk attach / detach.
	if code, out := c.do("POST", "/users/bulk", map[string]any{"action": "attach-nodes", "ids": []string{b, cr}, "nodeIds": []string{"n1"}}); code != 200 {
		t.Fatalf("bulk %d %v", code, out)
	}
	if got, _ = s.inboundUsers([]string{"i2"}); !userIDs(got["i2"])[b] || !userIDs(got["i2"])[cr] {
		t.Fatal("bulk attach-nodes")
	}
	c.do("POST", "/users/bulk", map[string]any{"action": "detach-nodes", "ids": []string{b, cr}, "nodeIds": []string{"n1"}})
	if got, _ = s.inboundUsers([]string{"i2"}); userIDs(got["i2"])[b] {
		t.Fatal("bulk detach-nodes")
	}

	// Deleting the node removes its bindings.
	c.do("DELETE", "/nodes/n1", nil)
	var n int64
	db.Model(&store.UserAccess{}).Where("target_id IN ?", []string{"n1", "i1", "i2", "i4"}).Count(&n)
	if n != 0 {
		t.Fatalf("%d bindings left after node delete", n)
	}
}

func getJSON(t *testing.T, c *apiClient, path string, v any) {
	t.Helper()
	req, _ := http.NewRequest("GET", c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// Bulk actions other than create send no template (as the UI does), and
// delete must not deadlock on SQLite's single connection.
func TestBulkActionsWithoutTemplate(t *testing.T) {
	s, _, c := newAdminClient(t)
	_, u := c.do("POST", "/users", map[string]any{"username": "dave"})
	id := u["id"].(string)
	for _, action := range []string{"disable", "enable", "reset-traffic", "delete"} {
		if code, out := c.do("POST", "/users/bulk", map[string]any{"action": action, "ids": []string{id}}); code != 200 {
			t.Fatalf("%s: %d %v", action, code, out)
		}
	}
	var n int64
	s.st.DB.Model(&store.User{}).Where("id = ?", id).Count(&n)
	if n != 0 {
		t.Fatal("bulk delete did not delete")
	}
}
