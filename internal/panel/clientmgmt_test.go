package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Bulk create (prefix + number + postfix, existing names skipped), delete
// depleted, sorting / summary, bulk adjust and manual traffic edits.
func TestUserBulkAndListing(t *testing.T) {
	s, _, c := newAdminClient(t)
	db := s.st.DB
	tpl := map[string]any{"username": "x", "trafficLimit": 1 << 30}
	code, out := c.do("POST", "/users/bulk", map[string]any{"action": "create", "prefix": "vip", "postfix": "@x", "start": 5, "count": 3, "template": tpl})
	if code != 200 || out["affected"].(float64) != 3 {
		t.Fatalf("create %d %v", code, out)
	}
	var n int64
	db.Model(&store.User{}).Where("username IN ?", []string{"vip005@x", "vip006@x", "vip007@x"}).Count(&n)
	if n != 3 {
		t.Fatalf("names: %d", n)
	}
	code, out = c.do("POST", "/users/bulk", map[string]any{"action": "create", "prefix": "vip", "postfix": "@x", "start": 5, "count": 4, "template": tpl})
	if code != 200 || out["affected"].(float64) != 1 || len(out["skipped"].([]any)) != 3 {
		t.Fatalf("re-run must skip existing names: %d %v", code, out)
	}

	var us []store.User
	db.Where("username LIKE ?", "vip%").Order("username").Find(&us)
	db.Model(&store.User{}).Where("id = ?", us[0].ID).Update("status", "limited")
	db.Model(&store.User{}).Where("id = ?", us[1].ID).Update("status", "expired")

	code, out = c.do("GET", "/users?filter=active&sort=username&order=desc", nil)
	sum := out["summary"].(map[string]any)
	if code != 200 || sum["limited"].(float64) != 1 || sum["expired"].(float64) != 1 || sum["active"].(float64) != 2 {
		t.Fatalf("summary %d %v", code, sum)
	}
	items := out["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["username"] != "vip008@x" {
		t.Fatalf("sort / filter: %v", items)
	}

	// +10 GB on limited quotas, IP limit for everyone selected.
	ids := []string{us[2].ID, us[3].ID}
	if code, out = c.do("POST", "/users/bulk", map[string]any{"action": "extend", "ids": ids, "addBytes": 10 << 30}); code != 200 {
		t.Fatalf("extend %d %v", code, out)
	}
	ip := 3
	if code, out = c.do("POST", "/users/bulk", map[string]any{"action": "set-limits", "ids": ids, "ipLimit": ip}); code != 200 {
		t.Fatalf("set-limits %d %v", code, out)
	}
	var got store.User
	db.First(&got, "id = ?", us[2].ID)
	if got.TrafficLimit != 11<<30 || got.IPLimit != 3 {
		t.Fatalf("adjust: limit=%d ip=%d", got.TrafficLimit, got.IPLimit)
	}
	if code, _ = c.do("PUT", "/users/"+us[2].ID+"/traffic", map[string]any{"usedUp": 12 << 30, "usedDown": 0}); code != 204 {
		t.Fatalf("set traffic %d", code)
	}
	db.First(&got, "id = ?", us[2].ID)
	if got.Status != "limited" {
		t.Fatalf("over quota after traffic edit, status=%s", got.Status)
	}

	if code, out = c.do("POST", "/users/bulk", map[string]any{"action": "delete-depleted"}); code != 200 || out["affected"].(float64) != 3 {
		t.Fatalf("delete depleted %d %v", code, out)
	}
	db.Model(&store.User{}).Where("username LIKE ?", "vip%").Count(&n)
	if n != 1 {
		t.Fatalf("left %d users", n)
	}
}

// Import accepts 3x-ui clients and keeps credentials; export round-trips.
func TestUserImportExport(t *testing.T) {
	s, srv, c := newAdminClient(t)
	uuid := "0f4d6a2e-3b1c-4d5e-8f90-123456789abc"
	code, out := c.do("POST", "/users/import", map[string]any{"users": []map[string]any{
		{"email": "alice", "id": uuid, "totalGB": 5 << 30, "expiryTime": -7 * 86400000, "limitIp": 2, "enable": true, "subId": "abcdefghijklmnop1234", "tgId": "42"},
		{"username": "bob", "trafficLimit": 0, "status": "disabled"},
		{"email": "alice"},
	}})
	if code != 200 || out["created"].(float64) != 2 || len(out["skipped"].([]any)) != 1 {
		t.Fatalf("import %d %v", code, out)
	}
	var a store.User
	s.st.DB.First(&a, "username = ?", "alice")
	if a.UUID != uuid || a.TrafficLimit != 5<<30 || a.ExpireAfterDays != 7 || a.IPLimit != 2 || a.SubToken != "abcdefghijklmnop1234" || a.TelegramID != 42 {
		t.Fatalf("alice = %+v", a)
	}
	req, _ := http.NewRequest("GET", srv.URL+s.cfg.APIBase()+"/users/export", nil)
	req.Header.Set("Authorization", "Bearer "+c.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var exp []userExport
	if err := json.Unmarshal(b, &exp); err != nil || len(exp) != 2 || exp[0].Password == "" {
		t.Fatalf("export: %v %s", err, b)
	}
}

// Clone copies an inbound on the same node with a new tag and port.
func TestCloneInbound(t *testing.T) {
	s, _, c := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.9"})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27", InstalledVersions: store.NewJSON([]string{})})
	code, out := c.do("POST", "/inbounds", map[string]any{"nodeId": "n1", "core": "xray", "tag": "r", "port": 2443,
		"protocol": map[string]any{"type": "vless", "flow": "xtls-rprx-vision"}, "transport": map[string]any{"type": "tcp"},
		"security": map[string]any{"type": "reality", "realityDest": "www.example.com:443"}})
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	id := out["inbound"].(map[string]any)["id"].(string)
	code, out = c.do("POST", "/inbounds/"+id+"/clone", map[string]any{})
	res := out["results"].([]any)[0].(map[string]any)
	if code != 200 || res["tag"] != "r-copy" || res["port"].(float64) != 2444 {
		t.Fatalf("clone %d %v", code, out)
	}
	var orig, cp store.Inbound
	db.First(&orig, "id = ?", id)
	db.First(&cp, "tag = ?", "r-copy")
	if s.toModelInbound(&cp).Security.RealityPrivateKey != s.toModelInbound(&orig).Security.RealityPrivateKey {
		t.Fatal("clone must keep the REALITY keys")
	}
	code, out = c.do("GET", "/inbounds/"+id+"/export", nil)
	if code != 200 || out["security"].(map[string]any)["realityPrivateKey"] == nil {
		t.Fatalf("export %d %v", code, out)
	}
}
