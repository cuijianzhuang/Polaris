package panel

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestDisableUserIsolation(t *testing.T) {
	for _, mode := range []string{"edit", "bulk", "state"} {
		t.Run(mode, func(t *testing.T) {
			s, _, c := newAdminClient(t)
			db := s.st.DB
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(db.Create(&store.Node{ID: "n1", Name: "node", DefaultCore: model.CoreXray}).Error)
			must(db.Create(&store.Core{ID: "core", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"}).Error)
			must(db.Create(&store.Inbound{ID: "in", NodeID: "n1", Tag: "vless", Core: model.CoreXray, Port: 18080, Enabled: true, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS})}).Error)
			for _, name := range []string{"alice", "bob", "carol"} {
				code, out := c.do("POST", "/users", map[string]any{"username": name, "nodeIds": []string{"n1"}})
				if code != 201 {
					t.Fatal(code, out)
				}
			}
			var alice store.User
			must(db.First(&alice, "username = ?", "alice").Error)
			if mode == "bulk" {
				code, out := c.do("POST", "/users/bulk", map[string]any{"action": "disable", "ids": []string{alice.ID}})
				if code != 200 {
					t.Fatal(code, out)
				}
			} else if mode == "state" {
				code, out := c.do("PUT", "/users/"+alice.ID+"/state", map[string]any{"enabled": false})
				if code != 200 {
					t.Fatal(code, out)
				}
			} else {
				code, out := c.do("PUT", "/users/"+alice.ID, map[string]any{"username": "alice", "status": "disabled"})
				if code != 200 {
					t.Fatal(code, out)
				}
			}
			var users []store.User
			must(db.Order("username").Find(&users).Error)
			for _, u := range users {
				want := "active"
				if u.ID == alice.ID {
					want = "disabled"
				}
				if u.Status != want {
					t.Fatalf("%s status=%s want %s", u.Username, u.Status, want)
				}
			}
			spec, err := s.BuildSpec("n1")
			must(err)
			if len(spec.Inbounds) != 1 || len(spec.Inbounds[0].Users) != 2 {
				t.Fatalf("remaining users=%+v", spec.Inbounds)
			}
			for _, u := range spec.Inbounds[0].Users {
				if u.ID == alice.ID {
					t.Fatal("disabled user still authorized")
				}
			}
		})
	}
}

func TestUserStateOnlyChangesManualAvailability(t *testing.T) {
	s, _, admin := newAdminClient(t)
	_, view := admin.do("POST", "/users", map[string]any{"username": "alice", "trafficLimit": 1000, "nodeIds": []string{"n1"}, "remark": "keep this"})
	id := view["id"].(string)
	snapshot := func() store.User {
		t.Helper()
		var u store.User
		if err := s.st.DB.First(&u, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		return u
	}
	before := snapshot()
	for _, enabled := range []bool{false, true} {
		if code, out := admin.do("PUT", "/users/"+id+"/state", map[string]any{"enabled": enabled, "username": "must-not-overwrite", "trafficLimit": 0, "nodeIds": []string{}}); code != 200 {
			t.Fatal(code, out)
		}
		got := snapshot()
		want := "active"
		if !enabled {
			want = "disabled"
		}
		if got.Status != want {
			t.Fatal(got.Status, want)
		}
		got.Status = before.Status
		got.UpdatedAt = before.UpdatedAt
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(before)
		if !bytes.Equal(a, b) {
			t.Fatal("state mutation changed other user fields")
		}
		var access int64
		s.st.DB.Model(&store.UserAccess{}).Where("user_id = ?", id).Count(&access)
		if access != 1 {
			t.Fatal("state mutation rewrote grants")
		}
	}
	if code, _ := admin.do("PUT", "/users/"+id+"/state", map[string]any{}); code != 400 {
		t.Fatal("missing enabled accepted", code)
	}
	for _, tc := range []struct {
		status string
		expiry *time.Time
		used   int64
	}{{"limited", nil, 1000}, {"expired", func() *time.Time { v := time.Now().Add(-time.Hour); return &v }(), 0}} {
		s.st.DB.Model(&store.User{}).Where("id = ?", id).Updates(map[string]any{"status": "disabled", "expire_at": tc.expiry, "used_down": tc.used})
		if code, out := admin.do("PUT", "/users/"+id+"/state", map[string]any{"enabled": true}); code != 200 {
			t.Fatal(code, out)
		}
		if got := snapshot(); got.Status != tc.status {
			t.Fatal("enable bypassed entitlement limits", got.Status)
		}
	}
	_, token, err := s.issueAPIToken("reader", "", []string{ScopeUsersRead}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	reader := &apiClient{t: t, base: admin.base, tok: token}
	if code, _ := reader.do("PUT", "/users/"+id+"/state", map[string]any{"enabled": false}); code != 403 {
		t.Fatal("read token could change user state", code)
	}
	if err := s.st.DB.Create(&store.User{ID: "service", Username: "service", UUID: "service", SubToken: "service", Status: "active", IsService: true}).Error; err != nil {
		t.Fatal(err)
	}
	if code, _ := admin.do("PUT", "/users/service/state", map[string]any{"enabled": false}); code != 404 {
		t.Fatal("manual switch targeted service account", code)
	}
}
