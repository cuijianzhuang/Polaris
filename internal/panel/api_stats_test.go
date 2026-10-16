package panel

import (
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestStatsOverviewDashboard(t *testing.T) {
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	soon := time.Now().Add(48 * time.Hour)
	db.Create(&store.User{ID: "u1", Username: "heavy", UUID: "11111111-1111-1111-1111-111111111111", Password: "p", Status: "active", SubToken: "t1", TrafficLimit: 100, UsedDown: 90})
	db.Create(&store.User{ID: "u2", Username: "light", UUID: "22222222-2222-2222-2222-222222222222", Password: "p", Status: "active", SubToken: "t2", ExpireAt: &soon})
	db.Create(&store.User{ID: "u3", Username: "over", UUID: "33333333-3333-3333-3333-333333333333", Password: "p", Status: "limited", SubToken: "t3"})
	hour := time.Now().UTC().Truncate(time.Hour)
	db.Create(&store.TrafficHourly{UserID: "u1", NodeID: "n1", Hour: hour, Up: 10, Down: 500})
	db.Create(&store.TrafficHourly{UserID: "u2", NodeID: "n1", Hour: hour, Up: 5, Down: 50})
	db.Create(&store.TrafficHourly{UserID: "u2", NodeID: "n1", Hour: hour.AddDate(0, 0, -20), Up: 1, Down: 1})

	st, ov := admin.do("GET", "/stats/overview?days=30", nil)
	if st != 200 {
		t.Fatalf("overview %d %v", st, ov)
	}
	if n := len(ov["trend"].([]any)); n != 30 {
		t.Fatalf("trend days = %d", n)
	}
	last := ov["trend"].([]any)[29].(map[string]any)
	if last["down"].(float64) != 550 || last["date"] != time.Now().Format("2006-01-02") {
		t.Fatalf("today bucket: %v", last)
	}
	tops := ov["topUsers"].([]any)
	if len(tops) != 2 || tops[0].(map[string]any)["username"] != "heavy" {
		t.Fatalf("top users: %v", tops)
	}
	a := ov["attention"].(map[string]any)
	if a["near"].(float64) != 1 || a["expiring"].(float64) != 1 || a["limited"].(float64) != 1 {
		t.Fatalf("attention: %v", a)
	}
	if _, ov := admin.do("GET", "/stats/overview", nil); len(ov["trend"].([]any)) != 7 {
		t.Fatal("default trend should be 7 days")
	}
	if _, list := admin.do("GET", "/users?filter=near", nil); list["summary"].(map[string]any)["near"].(float64) != 1 {
		t.Fatalf("users near summary: %v", list["summary"])
	}
}
