package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestPlans: a plan decides the servers (through its level) and sets the
// account's limits when applied; renewing extends, switching starts over
// with the traffic cleared, expiry hands the user to the fallback plan, and
// plan membership survives edits to the user's ordinary groups.
func TestPlans(t *testing.T) {
	s, srv, c := newAdminClient(t)
	for i, lvl := range []int{1, 2} {
		id := []string{"n1", "n2"}[i]
		s.st.DB.Create(&store.Node{ID: id, Name: id, Address: "203.0.113.1", Level: lvl})
		s.st.DB.Create(&store.Inbound{ID: "in-" + id, NodeID: id, Tag: "t-" + id, Core: model.CoreXray, Port: 21000 + i, Enabled: true})
	}
	mkPlan := func(body map[string]any) string {
		code, out := c.do("POST", "/plans", body)
		if code != 200 {
			t.Fatalf("plan %v: %d %v", body["name"], code, out)
		}
		return out["id"].(string)
	}
	free := mkPlan(map[string]any{"name": "免费版", "level": 1, "trafficLimit": 10 << 30, "resetPolicy": "monthly", "resetDay": 1})
	pro := mkPlan(map[string]any{"name": "专业版", "level": 2, "trafficLimit": 100 << 30, "days": 30, "deviceLimit": 3,
		"fallbackId": free, "price": "¥15 / 月"})
	if code, _ := c.do("PUT", "/plans/"+free, map[string]any{"name": "免费版", "level": 1, "fallbackId": free}); code != 400 {
		t.Fatal("a plan falling back to itself was accepted")
	}
	if code, _ := c.do("POST", "/plans", map[string]any{"name": "坏", "fallbackId": "nope"}); code != 400 {
		t.Fatal("unknown fallback accepted")
	}

	reach := func(user string) []string {
		var ids []string
		s.st.DB.Raw("SELECT DISTINCT inbound_id FROM ("+store.GrantsSQL+") g WHERE user_id = ?", user).Scan(&ids)
		sort.Strings(ids)
		return ids
	}
	get := func(id string) store.User {
		var u store.User
		s.st.DB.First(&u, "id = ?", id)
		return u
	}

	// A user created on a plan joins its group and no default group.
	code, out := c.do("POST", "/users", map[string]any{"username": "alice", "planId": pro, "trafficLimit": 100 << 30})
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	uid := out["id"].(string)
	if got := reach(uid); !slices.Equal(got, []string{"in-n1", "in-n2"}) {
		t.Fatalf("pro reaches %v", got)
	}
	if code, _ := c.do("POST", "/users", map[string]any{"username": "bob", "planId": "nope"}); code != 400 {
		t.Fatalf("unknown plan on create: %d", code)
	}
	// The user's ordinary groups leave the plan out; editing them keeps it.
	_, g := c.do("POST", "/groups", map[string]any{"name": "extra"})
	if code, out := c.do("PUT", "/users/"+uid, map[string]any{"username": "alice", "groupIds": []string{g["id"].(string), free}}); code != 200 {
		t.Fatalf("update %d %v", code, out)
	}
	_, out = c.do("GET", "/users/"+uid, nil)
	if out["planId"] != pro || len(out["groupIds"].([]any)) != 1 || out["groupIds"].([]any)[0] != g["id"] {
		t.Fatalf("after edit: plan %v groups %v", out["planId"], out["groupIds"])
	}
	if got := reach(uid); !slices.Equal(got, []string{"in-n1", "in-n2"}) {
		t.Fatalf("plan lost on edit: %v", got)
	}

	// A plugin sells plans with users:plan; the same order applies once.
	_, tok := c.do("POST", "/tokens", map[string]any{"name": "shop", "scopes": []string{"users:plan"}})
	post := func(path string, body any, key string) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+s.cfg.APIBase()+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok["secret"].(string))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	if code, m := post("/users/"+uid+"/plan", map[string]any{"planId": pro}, "order-1"); code != 200 || m["planId"] != pro {
		t.Fatalf("start pro %d %v", code, m)
	}
	first := get(uid)
	if first.ExpireAt == nil || first.TrafficLimit != 100<<30 || first.DeviceLimit != 3 {
		t.Fatalf("pro limits not applied: %+v", first)
	}
	s.st.DB.Model(&store.User{}).Where("id = ?", uid).Updates(map[string]any{"used_up": 1 << 30, "used_down": 2 << 30})
	for i := 0; i < 3; i++ {
		if code, m := post("/users/"+uid+"/plan", map[string]any{"planId": pro}, "order-2"); code != 200 {
			t.Fatalf("renew #%d %d %v", i, code, m)
		}
	}
	renewed := get(uid)
	if d := renewed.ExpireAt.Sub(*first.ExpireAt); d < 30*24*time.Hour-time.Minute || d > 30*24*time.Hour+time.Minute {
		t.Fatalf("renew moved expiry by %v, want 30 days once", d)
	}
	if renewed.Used() != 3<<30 {
		t.Fatalf("renewing an active plan cleared traffic: %d", renewed.Used())
	}
	if code, _ := post("/users/"+uid+"/renew", map[string]any{"days": 1}, ""); code != 403 {
		t.Fatalf("users:plan reached renew: %d", code)
	}

	// Switching plans starts over: traffic cleared, the new reach and limits.
	if code, m := post("/users/"+uid+"/plan", map[string]any{"planId": free, "mode": "switch"}, ""); code != 200 {
		t.Fatalf("switch %d %v", code, m)
	}
	u := get(uid)
	if u.PlanID != free || u.Used() != 0 || u.ExpireAt != nil || u.TrafficLimit != 10<<30 || u.ResetPolicy != "monthly" || u.DeviceLimit != 0 {
		t.Fatalf("after switch: %+v", u)
	}
	if got := reach(uid); !slices.Equal(got, []string{"in-n1"}) {
		t.Fatalf("free reaches %v", got)
	}

	// Expiry hands the user to the fallback plan instead of stopping it.
	c.do("POST", "/users/"+uid+"/plan", map[string]any{"planId": pro})
	past := time.Now().Add(-time.Hour)
	s.st.DB.Model(&store.User{}).Where("id = ?", uid).Updates(map[string]any{"expire_at": past, "used_up": 5 << 30})
	s.enforceLimits()
	u = get(uid)
	if u.PlanID != free || u.Status != "active" || u.Used() != 0 || u.ExpireAt != nil {
		t.Fatalf("after expiry: plan %s status %s used %d exp %v", u.PlanID, u.Status, u.Used(), u.ExpireAt)
	}
	// Without a fallback the account just expires.
	c.do("PUT", "/plans/"+pro, map[string]any{"name": "专业版", "level": 2, "trafficLimit": 100 << 30, "days": 30})
	c.do("POST", "/users/"+uid+"/plan", map[string]any{"planId": pro})
	s.st.DB.Model(&store.User{}).Where("id = ?", uid).Update("expire_at", past)
	s.enforceLimits()
	if u = get(uid); u.PlanID != pro || u.Status != "expired" {
		t.Fatalf("no fallback: plan %s status %s", u.PlanID, u.Status)
	}

	// Bulk: put several users on a plan at once.
	s.st.DB.Create(&store.User{ID: "u2", Username: "u2", UUID: "u2", Status: "active", SubToken: "tok-u2-0123456789"})
	if code, out := c.do("POST", "/users/bulk", map[string]any{"action": "plan", "ids": []string{"u2"}, "planId": free}); code != 200 {
		t.Fatalf("bulk %d %v", code, out)
	}
	if u = get("u2"); u.PlanID != free || u.TrafficLimit != 10<<30 {
		t.Fatalf("bulk plan: %+v", u)
	}
	if code, _ := c.do("POST", "/users/bulk", map[string]any{"action": "add-group", "ids": []string{"u2"}, "groupId": pro}); code != 400 {
		t.Fatal("add-group accepted a plan")
	}

	// Export and import keep the plan (by name).
	code, imp := c.do("POST", "/users/import", map[string]any{"users": []map[string]any{{"username": "imported", "status": "active", "plan": "免费版"}}})
	if code != 200 {
		t.Fatalf("import %d %v", code, imp)
	}
	var im store.User
	s.st.DB.First(&im, "username = ?", "imported")
	if im.PlanID != free || len(reach(im.ID)) != 1 {
		t.Fatalf("imported plan %q reach %v", im.PlanID, reach(im.ID))
	}

	// Deleting a plan leaves its users without a plan but with their limits.
	if code, _ := c.do("DELETE", "/plans/"+free, nil); code != 204 {
		t.Fatalf("delete plan %d", code)
	}
	if u = get("u2"); u.PlanID != "" || u.TrafficLimit != 10<<30 {
		t.Fatalf("after plan delete: %+v", u)
	}
	if got := reach("u2"); len(got) != 0 {
		t.Fatalf("deleted plan still reaches %v", got)
	}
}
