package panel

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestExpireTiers(t *testing.T) {
	for in, want := range map[string][]int{"": {7, 3, 1}, "7,3,1": {7, 3, 1}, "1,7": {7, 1}, "3": {3, 1}, "1": {1}, "0": nil, "x": nil, "14， 2": {14, 2}} {
		if got := expireTiers(in); !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
}

// Expiry reminders go out once per tier, start over after a renewal, and
// carry renewal buttons and mail links.
func TestUserExpiryRemindersByTier(t *testing.T) {
	s, _, _ := newAdminClient(t)
	events := make(chan Event, 50)
	s.sinks = append(s.sinks, events)
	now := time.Now()
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	u := store.User{ID: "u1", Username: "alice", UUID: "x", Password: "p", Status: "active", SubToken: "alice-token-000000000", TelegramID: 42, ExpireAt: at(5 * 24 * time.Hour)}
	s.st.DB.Create(&u)
	reminders := func() int {
		n := 0
		for {
			select {
			case e := <-events:
				if e.Kind == "expire_warn" && e.UserID == "u1" {
					n++
				}
			default:
				return n
			}
		}
	}
	set := func(d time.Duration) { // a renewal / admin edit: a new expiry
		s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Update("expire_at", at(d))
	}
	left := func(d time.Duration) { // time passes until d is left
		var cur store.User
		s.st.DB.First(&cur, "id = ?", "u1")
		shift := cur.ExpireAt.Sub(now) - d
		up := map[string]any{"expire_at": cur.ExpireAt.Add(-shift)}
		if cur.WarnedExpire != nil {
			up["warned_expire"] = cur.WarnedExpire.Add(-shift)
		}
		s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Updates(up)
	}

	s.remindUsers() // 5 days left: the 7-day tier
	s.remindUsers()
	if n := reminders(); n != 1 {
		t.Fatalf("7-day tier sent %d times", n)
	}
	left(2*24*time.Hour + time.Hour) // into the 3-day tier
	s.remindUsers()
	s.remindUsers()
	if n := reminders(); n != 1 {
		t.Fatalf("3-day tier sent %d times", n)
	}
	left(20 * time.Hour) // the last day
	s.remindUsers()
	if n := reminders(); n != 1 {
		t.Fatalf("1-day tier sent %d times", n)
	}
	s.remindUsers()
	if n := reminders(); n != 0 {
		t.Fatal("repeated within the last day")
	}
	// Renewed for a month: quiet, then the tiers start over.
	set(30 * 24 * time.Hour)
	s.remindUsers()
	if n := reminders(); n != 0 {
		t.Fatal("reminded right after renewal")
	}
	left(6 * 24 * time.Hour)
	s.remindUsers()
	if n := reminders(); n != 1 {
		t.Fatalf("new term's 7-day tier: %d", n)
	}
	// Off.
	s.st.SetSetting("remind_expire_days", "0")
	set(20 * time.Hour)
	s.remindUsers()
	if n := reminders(); n != 0 {
		t.Fatal("reminded while switched off")
	}

	// Renewal buttons and links follow the shop's state.
	if rows := s.userRenewButtons("expire_warn", &u); len(rows) != 0 {
		t.Fatalf("buttons without a shop: %v", rows)
	}
	s.st.SetSetting("shop_enabled", "true")
	s.st.SetSetting("portal_enabled", "true")
	s.lic.Store(&licenseState{st: license.State{Plan: "pro", Features: []string{license.FeaturePlanAddons}}})
	s.st.DB.Create(&store.TrafficPack{ID: "p1", Name: "100G", Traffic: 100 << 30, Sale: true})
	u.TrafficLimit = 10 << 30
	texts := func(kind string) string {
		var out []string
		for _, r := range s.userRenewButtons(kind, &u) {
			for _, b := range r {
				out = append(out, b.Text+"|"+b.Data+b.URL)
			}
		}
		return strings.Join(out, ",")
	}
	if got := texts("expire_warn"); !strings.Contains(got, "购买套餐|v:shop") || strings.Contains(got, "v:packs") || !strings.Contains(got, "/u") {
		t.Fatalf("expiry buttons %s", got)
	}
	u.PlanID = "plan1"
	if got := texts("user_limited"); !strings.Contains(got, "续费套餐|v:shop") || !strings.Contains(got, "加购流量包|v:packs") {
		t.Fatalf("traffic buttons %s", got)
	}
	if got := texts("node_offline"); got != "" {
		t.Fatalf("buttons on an unrelated notice: %s", got)
	}
	if l := s.userRenewLines("user_expired"); !strings.Contains(l, "Telegram") {
		t.Fatalf("mail line without web shop: %q", l)
	}
	s.st.SetSetting("shop_web_enabled", "true")
	if l := s.userRenewLines("traffic_warn"); !strings.Contains(l, shopWebPath) {
		t.Fatalf("mail line with web shop: %q", l)
	}
}
