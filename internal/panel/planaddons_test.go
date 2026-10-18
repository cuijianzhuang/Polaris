package panel

import (
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestPackLifecycle: traffic pack bytes (ExtraTraffic) last for the current
// period — a renewal keeps them, a new plan period, an automatic traffic
// reset or an admin's new quota drops them — and the community build
// refuses to apply packs or save plan node rates.
func TestPackLifecycle(t *testing.T) {
	s, _, c := newAdminClient(t)
	_, out := c.do("POST", "/plans", map[string]any{"name": "专业版", "level": 1, "trafficLimit": 100 << 30, "days": 30})
	pro := out["id"].(string)
	s.st.DB.Create(&store.User{ID: "u1", Username: "alice", UUID: "u1", Status: "active", SubToken: "tok-u1-0123456789"})
	if _, err := s.planAction("t", "u1", pro, true); err != nil {
		t.Fatal(err)
	}
	get := func() store.User {
		var u store.User
		s.st.DB.First(&u, "id = ?", "u1")
		return u
	}
	// As if a 20 GB pack had been applied.
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Updates(map[string]any{"traffic_limit": 120 << 30, "extra_traffic": 20 << 30, "used_down": 1 << 30})
	if _, err := s.planAction("t", "u1", pro, true); err != nil { // renewal: same period continues
		t.Fatal(err)
	}
	if u := get(); u.TrafficLimit != 120<<30 || u.ExtraTraffic != 20<<30 {
		t.Fatalf("renewal dropped the pack: %d / %d", u.TrafficLimit>>30, u.ExtraTraffic>>30)
	}
	// The monthly reset ends the period.
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Updates(map[string]any{"reset_policy": "daily", "last_reset_at": time.Now().AddDate(0, 0, -2)})
	s.resetTrafficCycles()
	if u := get(); u.TrafficLimit != 100<<30 || u.ExtraTraffic != 0 || u.Used() != 0 {
		t.Fatalf("reset kept the pack: %d / %d", u.TrafficLimit>>30, u.ExtraTraffic>>30)
	}
	// A switch (new period) and an admin's quota drop it too.
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Updates(map[string]any{"traffic_limit": 120 << 30, "extra_traffic": 20 << 30})
	if _, err := s.planAction("t", "u1", pro, false); err != nil {
		t.Fatal(err)
	}
	if u := get(); u.TrafficLimit != 100<<30 || u.ExtraTraffic != 0 {
		t.Fatalf("switch kept the pack: %d / %d", u.TrafficLimit>>30, u.ExtraTraffic>>30)
	}
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Update("extra_traffic", 5<<30)
	c.do("PUT", "/users/u1", map[string]any{"username": "alice", "trafficLimit": 50 << 30})
	if u := get(); u.TrafficLimit != 50<<30 || u.ExtraTraffic != 0 {
		t.Fatalf("admin quota: %d / %d", u.TrafficLimit>>30, u.ExtraTraffic>>30)
	}
	if packLimit(10, 20) != 10 || packLimit(30, 20) != 10 || packLimit(0, 5) != 0 {
		t.Fatal("packLimit")
	}
	if proBuild {
		return // the professional behaviour is tested in polaris-pro
	}
	if code, _ := c.do("POST", "/traffic-packs", map[string]any{"name": "50G", "traffic": 50 << 30}); code != 402 {
		t.Fatalf("community pack create: %d", code)
	}
	s.st.DB.Create(&store.TrafficPack{ID: "pk", Name: "50G", Traffic: 50 << 30})
	if code, out := c.do("POST", "/users/u1/pack", map[string]any{"packId": "pk"}); code != 400 {
		t.Fatalf("community pack apply: %d %v", code, out)
	}
	if code, _ := c.do("PUT", "/plans/"+pro, map[string]any{"name": "专业版", "level": 1, "nodeRates": map[string]float64{"n1": 0.5}}); code != 402 {
		t.Fatalf("community node rates: %d", code)
	}
	if s.planRates() != nil {
		t.Fatal("community plan rates")
	}
	if code, _ := c.do("POST", "/redeem-codes", map[string]any{"packId": "pk", "count": 1}); code != 402 {
		t.Fatalf("community pack codes: %d", code)
	}
}
