package panel

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// The flow that used to produce empty subscriptions: node → inbound → user
// through the API with no groups chosen.
func TestDefaultGroupAndEmptySubscription(t *testing.T) {
	s, srv, c := newAdminClient(t)
	db := s.st.DB
	var g store.Group
	if err := db.First(&g, "is_default = ?", true).Error; err != nil || g.Name != defaultGroupName {
		t.Fatalf("default group not seeded: %v %+v", err, g)
	}
	db.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.9"})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27", InstalledVersions: store.NewJSON([]string{})})
	code, out := c.do("POST", "/inbounds", map[string]any{"nodeId": "n1", "core": "xray", "tag": "vless", "port": 2443,
		"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "tcp"}, "security": map[string]any{"type": "none"}})
	if code != 201 && code != 200 {
		t.Fatalf("create inbound %d %v", code, out)
	}
	_, u := c.do("POST", "/users", map[string]any{"username": "alice"})
	_, bob := c.do("POST", "/users", map[string]any{"username": "bob", "nodeIds": []string{}, "groupIds": []string{}})

	fetch := func(token string) string {
		req, _ := http.NewRequest("GET", srv.URL+"/sub/"+token+"?format=clash", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if body := fetch(u["subToken"].(string)); !strings.Contains(body, "203.0.113.9") {
		t.Fatalf("alice should get the inbound through the default group:\n%s", body)
	}
	// Explicitly no access: the client sees why.
	if body := fetch(bob["subToken"].(string)); !strings.Contains(body, "未授权") {
		t.Fatalf("empty subscription should carry the reason:\n%s", body)
	}
	// Diagnosis.
	_, d := c.do("GET", "/users/"+bob["id"].(string)+"/sub-check", nil)
	if probs := d["problems"].([]any); len(probs) == 0 || !strings.Contains(probs[0].(string), "未授权") {
		t.Fatalf("diagnosis: %v", d)
	}
	_, d = c.do("GET", "/users/"+u["id"].(string)+"/sub-check", nil)
	ins := d["inbounds"].([]any)
	if len(ins) != 1 || ins[0].(map[string]any)["included"] != true || d["formats"].(map[string]any)["clash"].(float64) != 1 {
		t.Fatalf("diagnosis for alice: %v", d)
	}
	// Disabling the inbound shows up with its reason.
	db.Model(&store.Inbound{}).Where("tag = ?", "vless").Update("enabled", false)
	_, d = c.do("GET", "/users/"+u["id"].(string)+"/sub-check", nil)
	if r := d["inbounds"].([]any)[0].(map[string]any)["reason"]; r != "入站已停用" {
		t.Fatalf("disabled inbound reason: %v", r)
	}
}

// An install that never used groups gets its users and inbounds connected.
func TestDefaultGroupMigratesUngroupedInstall(t *testing.T) {
	s := newTestServer(t)
	db := s.st.DB
	db.Where("1 = 1").Delete(&store.Group{})
	db.Create(&store.Node{ID: "n1", Name: "n1"})
	db.Create(&store.Inbound{ID: "i1", NodeID: "n1", Tag: "a", Port: 1, Enabled: true})
	db.Create(&store.User{ID: "u1", Username: "u1", UUID: "1", SubToken: "t1-0000000000000000", Status: "active"})
	db.Create(&store.User{ID: "u2", Username: "u2", UUID: "2", SubToken: "t2-0000000000000000", Status: "active"})
	db.Create(&store.UserAccess{UserID: "u2", Kind: "node", TargetID: "n1"}) // already has access: left alone
	if err := s.ensureDefaultGroup(); err != nil {
		t.Fatal(err)
	}
	got, _ := s.inboundUsers([]string{"i1"})
	if ids := userIDs(got["i1"]); !ids["u1"] || !ids["u2"] {
		t.Fatalf("users on i1: %v", ids)
	}
	var n int64
	db.Model(&store.UserGroup{}).Where("user_id = ?", "u2").Count(&n)
	if n != 0 {
		t.Fatal("a user with existing access should not be added to the group")
	}
}

func TestUserFromSubLink(t *testing.T) {
	s := newTestServer(t)
	db := s.st.DB
	db.Create(&store.User{ID: "u1", Username: "alice", UUID: "1", Status: "active", SubToken: "Abc-def_0123456789xyz"})
	db.Create(&store.SubProfile{ID: "p1", Name: "p", Token: "profile-token-0000000", Enabled: true, PanelUserID: "u1"})
	db.Create(&store.SubProfile{ID: "p2", Name: "q", Token: "profile-token-1111111", Enabled: true})
	ok := []string{
		"Abc-def_0123456789xyz",
		"https://panel.example.com/sub/Abc-def_0123456789xyz",
		"订阅：https://panel.example.com/sub/Abc-def_0123456789xyz/ 谢谢",
		"https://panel.example.com/sub/Abc-def_0123456789xyz?format=clash#x",
		"https://panel.example.com/sub/p/profile-token-0000000",
	}
	for _, in := range ok {
		if u, msg := s.userFromSubLink(in); u == nil || u.ID != "u1" {
			t.Errorf("%q: %s", in, msg)
		}
	}
	for in, want := range map[string]string{
		"https://panel.example.com/sub/p/profile-token-1111111": "未关联面板用户",
		"https://panel.example.com/sub/doesnotexist-12345678":   "链接标识",
		"你好": "没有识别到订阅链接",
	} {
		if u, msg := s.userFromSubLink(in); u != nil || !strings.Contains(msg, want) {
			t.Errorf("%q: got %v %q", in, u, msg)
		}
	}
}
