package panel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// rawDo issues an admin API request and returns the status and raw body
// (list endpoints answer arrays, which apiClient.do cannot decode).
func rawDo(t *testing.T, c *apiClient, method, path string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestSubViewGroupCRUDAndDeleteProtection(t *testing.T) {
	s, _, admin := newAdminClient(t)
	payload := map[string]any{
		"name":     "香港组",
		"selector": map[string]any{"type": "filter", "regions": []string{"HK"}},
		"operators": []map[string]any{
			{"type": "rename", "template": "{group} {name}"},
		},
		"kinds": []string{"direct", "relay"},
	}
	code, body := rawDo(t, admin, "POST", "/sub-view-groups", payload)
	if code != 201 {
		t.Fatalf("create %d %s", code, body)
	}
	var vg store.SubViewGroup
	if err := json.Unmarshal(body, &vg); err != nil || vg.ID == "" || vg.Revision != 1 || !vg.Enabled {
		t.Fatalf("created group: %v %+v", err, vg)
	}
	// A broken selector regex is rejected at save time.
	bad := map[string]any{"name": "坏组", "selector": map[string]any{"type": "filter", "include": "["}}
	if code, body := rawDo(t, admin, "POST", "/sub-view-groups", bad); code != 400 {
		t.Fatalf("bad selector: %d %s", code, body)
	}
	// List contains it.
	code, body = rawDo(t, admin, "GET", "/sub-view-groups", nil)
	if code != 200 || !strings.Contains(string(body), "香港组") {
		t.Fatalf("list %d %s", code, body)
	}
	// Stale revision conflicts; the current one updates and bumps revision.
	payload["revision"] = 99
	if code, body := rawDo(t, admin, "PUT", "/sub-view-groups/"+vg.ID, payload); code != 409 {
		t.Fatalf("stale update: %d %s", code, body)
	}
	payload["revision"] = 1
	payload["remark"] = "改备注"
	code, body = rawDo(t, admin, "PUT", "/sub-view-groups/"+vg.ID, payload)
	if code != 200 {
		t.Fatalf("update %d %s", code, body)
	}
	if err := json.Unmarshal(body, &vg); err != nil || vg.Revision != 2 || vg.Remark != "改备注" {
		t.Fatalf("updated group: %v %+v", err, vg)
	}
	// Bind it to a plan: deletion is now refused with the reference named.
	code, body = rawDo(t, admin, "POST", "/plans", map[string]any{"name": "视图套餐", "viewGroups": []string{vg.ID}})
	if code != 200 {
		t.Fatalf("create plan %d %s", code, body)
	}
	var plan store.Group
	if err := json.Unmarshal(body, &plan); err != nil || len(plan.ViewGroups.V) != 1 {
		t.Fatalf("plan binding: %v %+v", err, plan)
	}
	code, body = rawDo(t, admin, "DELETE", "/sub-view-groups/"+vg.ID, nil)
	if code != 409 || !strings.Contains(string(body), "视图套餐") {
		t.Fatalf("delete while bound: %d %s", code, body)
	}
	// Binding a nonexistent view group is rejected.
	if code, body := rawDo(t, admin, "POST", "/plans", map[string]any{"name": "坏套餐", "viewGroups": []string{"nope"}}); code != 400 {
		t.Fatalf("bad binding: %d %s", code, body)
	}
	// Unbind (viewGroups = []), then deletion succeeds.
	var cur store.Group
	s.st.DB.First(&cur, "id = ?", plan.ID)
	code, body = rawDo(t, admin, "PUT", "/plans/"+plan.ID, map[string]any{"name": "视图套餐", "viewGroups": []string{}})
	if code != 200 {
		t.Fatalf("unbind %d %s", code, body)
	}
	if code, body := rawDo(t, admin, "DELETE", "/sub-view-groups/"+vg.ID, nil); code != 204 {
		t.Fatalf("delete after unbind: %d %s", code, body)
	}
}

func vgTestNodes() []sub.Node {
	return []sub.Node{
		{Name: "HK 01", Region: "HK", Protocol: model.ProtoVLESS, Server: "hk.example.com", Port: 443, UUID: "u", PanelKind: "direct", PanelTag: "hk-in"},
		{Name: "HK 02", Region: "HK", Protocol: model.ProtoVLESS, Server: "hk.example.com", Port: 8443, UUID: "u", PanelKind: "relay", PanelChainID: "c1", PanelChainName: "港线", PanelTag: "hk-in"},
		{Name: "JP 01", Region: "JP", Protocol: model.ProtoTrojan, Server: "jp.example.com", Port: 443, Password: "p", PanelKind: "direct", PanelTag: "jp-in"},
	}
}

func TestApplyPlanViewGroupsPipeline(t *testing.T) {
	s := newTestServer(t)
	db := s.st.DB
	pg := store.SubProcessingGroup{ID: "pg1", Name: "pg1", Revision: 1,
		Operators: store.NewJSON([]sub.Operator{{Type: "rename", Pattern: "^HK", Replace: "香港"}})}
	if err := db.Create(&pg).Error; err != nil {
		t.Fatal(err)
	}
	vgHK := store.SubViewGroup{ID: "vg-hk", Name: "香港组", Enabled: true, Revision: 1,
		Selector:         store.NewJSON(sub.Operator{Type: "filter", Regions: []string{"HK"}}),
		ProcessingGroups: store.NewJSON([]store.SubProcessingRef{{GroupID: "pg1"}}),
		Operators:        store.NewJSON([]sub.Operator{{Type: "rename", Template: "{group}·{kind} {name}"}})}
	vgOff := store.SubViewGroup{ID: "vg-off", Name: "停用组", Enabled: false, Revision: 1,
		Selector:  store.NewJSON(sub.Operator{Type: "filter", Regions: []string{"JP"}}),
		Operators: store.NewJSON([]sub.Operator{{Type: "rename", Template: "不应出现 {name}"}})}
	vgDangling := store.SubViewGroup{ID: "vg-dangling", Name: "悬空组", Enabled: true, Revision: 1,
		Selector:         store.NewJSON(sub.Operator{Type: "filter", Regions: []string{"JP"}}),
		ProcessingGroups: store.NewJSON([]store.SubProcessingRef{{GroupID: "missing"}}),
		Operators:        store.NewJSON([]sub.Operator{{Type: "rename", Template: "不应出现 {name}"}})}
	for _, vg := range []store.SubViewGroup{vgHK, vgOff, vgDangling} {
		if err := db.Create(&vg).Error; err != nil {
			t.Fatal(err)
		}
	}
	plan := store.Group{ID: "plan1", Name: "plan1", Plan: true,
		ViewGroups: store.NewJSON([]string{"vg-hk", "vg-off", "vg-dangling"})}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	u := &store.User{ID: "u1", PlanID: "plan1"}
	out, err := s.applyPlanViewGroups(u, vgTestNodes())
	if err != nil {
		t.Fatal(err)
	}
	// No node is lost: HK pair processed, JP node falls to the default group
	// (the disabled group and the dangling-ref group are both skipped).
	if len(out) != 3 {
		t.Fatalf("nodes lost: %v", out)
	}
	// Processing-group rename runs before the local rename (order pinned):
	// "HK 01" -> pg rename -> "香港 01" -> local template -> "香港组·直连 香港 01".
	if out[0].Name != "香港组·直连 香港 01" || out[1].Name != "香港组·中转 香港 02" {
		t.Fatalf("processed names: %q, %q", out[0].Name, out[1].Name)
	}
	if out[2].Name != "JP 01" {
		t.Fatalf("default-group node changed: %q", out[2].Name)
	}
	// A user without a plan gets the nodes back untouched, and so does a
	// user whose plan binds nothing.
	plain := vgTestNodes()
	got, err := s.applyPlanViewGroups(&store.User{ID: "u2"}, plain)
	if err != nil || fmt.Sprint(got) != fmt.Sprint(plain) {
		t.Fatalf("no-plan user changed: %v %v", err, got)
	}
}

// TestSubscriptionViewGroupsByteIdentical pins the compatibility red line:
// with no binding in play, the served subscription is byte-for-byte what the
// pre-view-group pipeline produced; a binding that matches nothing also
// leaves the bytes alone; a matching binding renames without losing nodes.
func TestSubscriptionViewGroupsByteIdentical(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	u := seedSubBench(t, s)
	fetch := func() string {
		resp, err := http.Get(srv.URL + "/sub/" + u.SubToken + "?format=links")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("subscription %d %s", resp.StatusCode, b)
		}
		return string(b)
	}
	baseline := fetch()
	if !strings.Contains(baseline, "ws0") && len(baseline) < 50 {
		t.Fatalf("baseline looks empty: %q", baseline)
	}
	// A view group exists but is bound nowhere: bytes unchanged.
	vg := store.SubViewGroup{ID: "vg-x", Name: "未绑定组", Enabled: true, Revision: 1,
		Selector:  store.NewJSON(sub.Operator{Type: "filter", Include: "tokyo"}),
		Operators: store.NewJSON([]sub.Operator{{Type: "rename", Template: "改名 {name}"}})}
	if err := s.st.DB.Create(&vg).Error; err != nil {
		t.Fatal(err)
	}
	if got := fetch(); got != baseline {
		t.Fatalf("unbound view group changed output:\n%s\n---\n%s", got, baseline)
	}
	// The user is on a plan that binds a group matching nothing: the
	// pipeline runs end to end and the bytes still do not move.
	vgNone := store.SubViewGroup{ID: "vg-none", Name: "空组", Enabled: true, Revision: 1,
		Selector:  store.NewJSON(sub.Operator{Type: "filter", Regions: []string{"XX"}}),
		Operators: store.NewJSON([]sub.Operator{})}
	if err := s.st.DB.Create(&vgNone).Error; err != nil {
		t.Fatal(err)
	}
	plan := store.Group{ID: "plan-b", Name: "plan-b", Plan: true, ViewGroups: store.NewJSON([]string{"vg-none"})}
	if err := s.st.DB.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("plan_id", "plan-b").Error; err != nil {
		t.Fatal(err)
	}
	if got := fetch(); got != baseline {
		t.Fatalf("non-matching binding changed output:\n%s\n---\n%s", got, baseline)
	}
	// Binding the matching group renames its nodes; the node count (one
	// link per line in the base64-decoded list is not assumed — count the
	// "://" occurrences instead) stays the same.
	if err := s.st.DB.Model(&store.Group{}).Where("id = ?", "plan-b").Update("view_groups", store.NewJSON([]string{"vg-x"})).Error; err != nil {
		t.Fatal(err)
	}
	got := fetch()
	if got == baseline {
		t.Fatal("matching binding had no effect")
	}
	// The links format is base64: decode both and compare link counts and
	// the rename marker.
	decode := func(b64 string) string {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			t.Fatalf("links body is not base64: %v", err)
		}
		return string(b)
	}
	decBase, decGot := decode(baseline), decode(got)
	if strings.Count(decGot, "://") != strings.Count(decBase, "://") {
		t.Fatalf("node count changed:\n%s\n---\n%s", decGot, decBase)
	}
	if !strings.Contains(decGot, "%E6%94%B9%E5%90%8D") && !strings.Contains(decGot, "改名") {
		t.Fatalf("rename did not reach the served links:\n%s", decGot)
	}
}

// TestViewGroupQueryCount pins the performance red line: resolving and
// applying view groups costs a constant number of queries, independent of
// the node count.
func TestViewGroupQueryCount(t *testing.T) {
	s := newTestServer(t)
	db := s.st.DB
	pg := store.SubProcessingGroup{ID: "pg1", Name: "pg1", Revision: 1,
		Operators: store.NewJSON([]sub.Operator{{Type: "sort", SortBy: []string{"name"}}})}
	if err := db.Create(&pg).Error; err != nil {
		t.Fatal(err)
	}
	mkVG := func(id string, refs []store.SubProcessingRef) store.SubViewGroup {
		return store.SubViewGroup{ID: id, Name: id, Enabled: true, Revision: 1,
			Selector:         store.NewJSON(sub.Operator{Type: "filter", Regions: []string{"HK"}}),
			ProcessingGroups: store.NewJSON(refs),
			Operators:        store.NewJSON([]sub.Operator{})}
	}
	vg1 := mkVG("vg1", []store.SubProcessingRef{{GroupID: "pg1"}})
	vg2 := mkVG("vg2", nil)
	for _, vg := range []store.SubViewGroup{vg1, vg2} {
		if err := db.Create(&vg).Error; err != nil {
			t.Fatal(err)
		}
	}
	plan := store.Group{ID: "plan1", Name: "plan1", Plan: true, ViewGroups: store.NewJSON([]string{"vg1", "vg2"})}
	emptyPlan := store.Group{ID: "plan2", Name: "plan2", Plan: true}
	for _, p := range []store.Group{plan, emptyPlan} {
		if err := db.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	// 200 nodes: the query count must not grow with the node count.
	nodes := make([]sub.Node, 0, 200)
	for i := 0; i < 200; i++ {
		nodes = append(nodes, sub.Node{Name: fmt.Sprintf("HK %03d", i), Region: "HK",
			Protocol: model.ProtoVLESS, Server: fmt.Sprintf("h%d.example.com", i), Port: 443, UUID: "u", PanelKind: "direct"})
	}
	var n atomic.Int64
	old := s.st.DB.Logger
	s.st.DB.Logger = queryCounter{Interface: old, n: &n}
	t.Cleanup(func() { s.st.DB.Logger = old })

	out, err := s.applyPlanViewGroups(&store.User{ID: "u1", PlanID: "plan1"}, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 200 {
		t.Fatalf("nodes lost: %d", len(out))
	}
	bound := n.Load()
	t.Logf("bound render queries (200 nodes, 2 groups, 1 ref): %d", bound)
	if bound > 4 {
		t.Fatalf("too many queries for bound view groups: %d (want <= 4)", bound)
	}
	n.Store(0)
	if _, err := s.applyPlanViewGroups(&store.User{ID: "u2", PlanID: "plan2"}, nodes); err != nil {
		t.Fatal(err)
	}
	if got := n.Load(); got > 1 {
		t.Fatalf("unbound plan cost %d queries (want <= 1)", got)
	}
	n.Store(0)
	if _, err := s.applyPlanViewGroups(&store.User{ID: "u3"}, nodes); err != nil {
		t.Fatal(err)
	}
	if got := n.Load(); got != 0 {
		t.Fatalf("plan-less user cost %d queries (want 0)", got)
	}
}
