package panel

import (
	"strings"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestMonthlyBoundaryClampsToMonthEnd(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		now         time.Time
		day, offset int
		want        string
	}{
		{time.Date(2027, 2, 15, 12, 0, 0, 0, loc), 31, 0, "2027-02-28"},
		{time.Date(2027, 2, 15, 12, 0, 0, 0, loc), 31, -1, "2027-01-31"},
		{time.Date(2027, 3, 10, 0, 0, 0, 0, loc), 30, -1, "2027-02-28"},
		{time.Date(2028, 2, 10, 0, 0, 0, 0, loc), 30, 0, "2028-02-29"},
		{time.Date(2027, 1, 5, 0, 0, 0, 0, loc), 15, -1, "2026-12-15"},
		{time.Date(2027, 1, 5, 0, 0, 0, 0, loc), 0, 0, "2027-01-01"},
	}
	for _, c := range cases {
		if got := monthlyBoundary(c.now, c.day, c.offset).Format("2006-01-02"); got != c.want {
			t.Errorf("monthlyBoundary(%s, %d, %d) = %s, want %s", c.now.Format("2006-01-02"), c.day, c.offset, got, c.want)
		}
	}
}

func TestMemBufferForgetAllowsRetry(t *testing.T) {
	b := newMemBuffer()
	if b.Seen("n/1") {
		t.Fatal("first sight reported as seen")
	}
	b.Forget("n/1")
	if b.Seen("n/1") {
		t.Fatal("a forgotten batch must be processed on retry")
	}
	if !b.Seen("n/1") {
		t.Fatal("duplicate not detected")
	}
}

// A user who left node A must not keep A's old IPs in the ip_limit count.
func TestOnlineIPsIgnoreStaleNodes(t *testing.T) {
	s := newTestServer(t)
	u := store.User{ID: store.NewID(), Username: "ipu", UUID: store.NewID(), Password: "p", SubToken: store.NewID(), Status: "active", IPLimit: 1}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	a, b := store.Node{ID: store.NewID(), Name: "a"}, store.Node{ID: store.NewID(), Name: "b"}
	s.st.DB.Create(&a)
	s.st.DB.Create(&b)
	s.nodeUserSets.mu.Lock()
	s.nodeUserSets.m = map[string]nodeUsersEntry{a.ID: {0, 0, map[string]bool{u.ID: true}}, b.ID: {0, 0, map[string]bool{u.ID: true}}}
	s.nodeUserSets.mu.Unlock()
	s.onlineMu.Lock()
	s.onlineIPs[u.ID] = map[string]map[string]time.Time{a.ID: {"1.1.1.1": time.Now().Add(-10 * time.Minute)}}
	s.onlineMu.Unlock()
	s.handleOnlineIPs(b.ID, &polarisv1.OnlineIPs{Users: []*polarisv1.UserIPs{{UserId: u.ID, Ips: []string{"2.2.2.2"}}}})
	var got store.User
	s.st.DB.First(&got, "id = ?", u.ID)
	if ips := got.OnlineIPs.V; len(ips) != 1 || ips[0] != "2.2.2.2" {
		t.Fatalf("online ips = %v, want only 2.2.2.2", ips)
	}
	if got.BannedUntil != nil {
		t.Fatalf("user banned for a stale IP: %v", got.BannedUntil)
	}
}

func TestMatchItemByKeyAndAmbiguity(t *testing.T) {
	reality := store.TemplateItem{Key: "r", Protocol: model.ProtocolSpec{Type: model.ProtoVLESS}, Security: model.SecuritySpec{Type: model.SecReality}}
	plain := store.TemplateItem{Key: "p", Protocol: model.ProtocolSpec{Type: model.ProtoVLESS}}
	items := map[string]store.TemplateItem{"r": reality, "p": plain}
	in := store.Inbound{TemplateItemKey: "p", Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}),
		Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP})}
	for i := 0; i < 20; i++ { // map order is random
		if it, ok := matchItem(items, &in); !ok || it.Key != "p" {
			t.Fatalf("matched %q, want p", it.Key)
		}
	}
	in.TemplateItemKey = ""
	if _, ok := matchItem(items, &in); ok {
		t.Fatal("two candidate items must not be guessed")
	}
}

func TestSSClientPasswordPerCore(t *testing.T) {
	u := model.User{ID: "u1", Password: "pw", SSSeed: "seed"}
	m := model.Inbound{Core: model.CoreMihomo, Protocol: model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "2022-blake3-aes-128-gcm", ServerKey: "SK"}}
	if got := ssClientPassword(&m, &u); got != "SK" {
		t.Errorf("mihomo (single-user listener) client password = %q, want the server key", got)
	}
	m.Core = model.CoreXray
	if got := ssClientPassword(&m, &u); !strings.HasPrefix(got, "SK:") {
		t.Errorf("xray 2022 client password = %q, want SK:<user key>", got)
	}
}

// Syncing a template right after applying it must find nothing to change.
func TestTemplateSyncIsQuietAfterApply(t *testing.T) {
	s, _, c := newAdminClient(t)
	n := store.Node{ID: store.NewID(), Name: "tk", Address: "1.2.3.4", Domain: "tk.example.com", DefaultCore: model.CoreXray}
	s.st.DB.Create(&n)
	s.st.DB.Create(&store.Core{ID: store.NewID(), NodeID: n.ID, Type: model.CoreXray, Version: "26.3.27", IsDefault: true})
	s.st.DB.Create(&store.Core{ID: store.NewID(), NodeID: n.ID, Type: model.CoreSingbox, Version: "1.13.5"})
	var tpl store.InboundTemplate
	if err := s.st.DB.First(&tpl, "name = ?", "全协议").Error; err != nil {
		t.Fatal(err)
	}
	code, out := c.do("POST", "/templates/"+tpl.ID+"/apply", map[string]any{"nodeIds": []string{n.ID}, "autoPort": true, "reassign": true})
	if code != 200 {
		t.Fatalf("apply %d %v", code, out)
	}
	var made int64
	s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", tpl.ID).Count(&made)
	if made == 0 {
		t.Fatalf("apply generated nothing: %v", out)
	}
	code, out = c.do("POST", "/templates/"+tpl.ID+"/sync", map[string]any{})
	if code != 200 {
		t.Fatalf("sync %d %v", code, out)
	}
	for _, ch := range out["changes"].([]any) {
		if f := ch.(map[string]any)["fields"]; f != nil && len(f.([]any)) > 0 {
			t.Errorf("sync reports changes right after apply: %v", ch)
		}
	}
}
