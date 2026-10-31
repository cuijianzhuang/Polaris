package panel

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestTruncatePreservesUTF8(t *testing.T) {
	for _, input := range []string{"", "plain ASCII", "中文诊断记录", "a中🙂z", strings.Repeat("检测记录🙂", 1000)} {
		for _, limit := range []int{-1, 0, 1, 2, 3, 4, 7, 8, 9, 250, 4000, len(input), len(input) + 1} {
			got := truncate(input, limit)
			if !utf8.ValidString(got) || !strings.HasPrefix(input, got) || (limit >= 0 && len(got) > limit) {
				t.Fatalf("limit %d produced an invalid or oversized prefix: %q", limit, got)
			}
			if len(got) < len(input) && limit >= 0 {
				_, size := utf8.DecodeRuneInString(input[len(got):])
				if len(got)+size <= limit {
					t.Fatalf("limit %d omitted a character that fits", limit)
				}
			}
		}
	}
}

// TestNewSettingsSaveable: the settings added for recent features are
// accepted by PUT /settings (a key missing from the whitelist is dropped).
func TestNewSettingsSaveable(t *testing.T) {
	s, _, c := newAdminClient(t)
	keys := map[string]string{
		"portal_enabled": "true", "sub_show_rate": "false", "traffic_spike_factor": "4", "traffic_spike_min_gb": "2",
		"login_allowlist": "10.0.0.0/8", "login_ban_failures": "8", "inbound_probe": "false", "audit_changes": "true",
		"tpl_traffic_warn": "a", "tpl_expire_warn": "b", "tpl_user_limited": "c", "tpl_user_expired": "d", "tpl_user_active": "e",
		"backup_webdav_url": "https://dav.example.com/x/", "backup_s3_bucket": "bk", "backup_s3_region": "auto",
	}
	body := map[string]any{}
	for k, v := range keys {
		body[k] = v
	}
	if code, out := c.do("PUT", "/settings", body); code != 200 {
		t.Fatalf("put %d %v", code, out)
	}
	for k, v := range keys {
		if got := s.st.GetSetting(k, ""); got != v {
			t.Errorf("%s not saved: %q", k, got)
		}
	}
}

func TestChangeRecords(t *testing.T) {
	s, _, c := newAdminClient(t)
	logs := func(action string) []store.AuditLog {
		var ls []store.AuditLog
		s.st.DB.Where("action = ?", action).Order("id").Find(&ls)
		return ls
	}
	// Off by default.
	c.do("POST", "/users", map[string]any{"username": "before-on"})
	if n := len(logs("change.create")); n != 0 {
		t.Fatalf("recorded while off: %d", n)
	}
	if code, _ := c.do("PUT", "/settings", map[string]string{"audit_changes": "true"}); code != 200 {
		t.Fatal("enable")
	}

	_, u := c.do("POST", "/users", map[string]any{"username": "alice", "trafficLimit": 1 << 30})
	id := u["id"].(string)
	creates := logs("change.create")
	var userCreate *store.AuditLog
	for i := range creates {
		if strings.HasPrefix(creates[i].Target, "users/"+id) {
			userCreate = &creates[i]
		}
	}
	if userCreate == nil || userCreate.Admin != "root" || !strings.Contains(userCreate.Target, "alice") {
		t.Fatalf("create record: %+v", creates)
	}
	if !strings.Contains(userCreate.Detail, `"TrafficLimit"`) || !strings.Contains(userCreate.Detail, "已隐藏") || strings.Contains(userCreate.Detail, u["uuid"].(string)) {
		t.Fatalf("create detail (credentials must be hidden): %s", userCreate.Detail)
	}

	u["trafficLimit"] = 2 << 30
	u["remark"] = "vip"
	if code, out := c.do("PUT", "/users/"+id, u); code != 200 {
		t.Fatalf("update %d %v", code, out)
	}
	ups := logs("change.update")
	found := false
	for _, l := range ups {
		if strings.HasPrefix(l.Target, "users/"+id) && strings.Contains(l.Detail, `"vip"]`) &&
			strings.Contains(l.Detail, `"TrafficLimit":[1073741824,2147483648]`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("update record with old → new: %+v", ups)
	}

	// Writes outside admin requests (traffic settlement) are not recorded.
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", TrafficRate: 1})
	n := len(logs("change.update"))
	s.traffic.Add("n1", []model.TrafficDelta{{UserID: id, Up: 1, Down: 1}})
	s.settleTraffic()
	if len(logs("change.update")) != n {
		t.Fatal("system write recorded")
	}

	if code, _ := c.do("DELETE", "/users/"+id, nil); code != 204 {
		t.Fatal("delete")
	}
	dels := logs("change.delete")
	ok := false
	for _, l := range dels {
		ok = ok || strings.HasPrefix(l.Target, "users/"+id)
	}
	if !ok {
		t.Fatalf("delete record: %+v", dels)
	}
	// Turning it off needs a re-verified session and stops recording.
	c.do("PUT", "/settings", map[string]string{"audit_changes": "false"})
	before := len(logs("change.create"))
	c.do("POST", "/users", map[string]any{"username": "after-off"})
	if len(logs("change.create")) != before {
		t.Fatal("recorded after turning off")
	}
}
