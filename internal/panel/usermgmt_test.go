package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestWeeklyBoundary(t *testing.T) {
	wed := time.Date(2027, 1, 6, 15, 0, 0, 0, time.UTC) // a Wednesday
	if got := weeklyBoundary(wed, 1).Format("2006-01-02"); got != "2027-01-04" {
		t.Fatalf("monday boundary = %s", got)
	}
	if got := weeklyBoundary(wed, 3).Format("2006-01-02"); got != "2027-01-06" {
		t.Fatalf("same-day boundary = %s", got)
	}
	if got := weeklyBoundary(wed, 7).Format("2006-01-02"); got != "2027-01-03" {
		t.Fatalf("sunday boundary = %s", got)
	}
}

func TestAutoRenew(t *testing.T) {
	s := newTestServer(t)
	past := time.Now().Add(-48 * time.Hour)
	u := store.User{ID: store.NewID(), Username: "r", UUID: store.NewID(), Password: "p", SubToken: store.NewID(), Status: "active",
		ExpireAt: &past, UsedUp: 100, UsedDown: 200, RenewDays: 30, RenewMax: 1, TrafficLimit: 1000, OnlineIPs: store.NewJSON([]string{})}
	s.st.DB.Create(&u)
	s.enforceLimits()
	var got store.User
	s.st.DB.First(&got, "id = ?", u.ID)
	if got.Status != "active" || got.RenewCount != 1 || got.Used() != 0 || got.ExpireAt == nil || got.ExpireAt.Before(time.Now().Add(27*24*time.Hour)) {
		t.Fatalf("not renewed: status=%s count=%d used=%d exp=%v", got.Status, got.RenewCount, got.Used(), got.ExpireAt)
	}
	// The cap is reached: next expiry stays expired.
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("expire_at", past)
	s.enforceLimits()
	s.st.DB.First(&got, "id = ?", u.ID)
	if got.Status != "expired" || got.RenewCount != 1 {
		t.Fatalf("renew cap ignored: status=%s count=%d", got.Status, got.RenewCount)
	}
}

func TestTrustedIPsExemptFromLimit(t *testing.T) {
	s := newTestServer(t)
	s.st.SetSetting("ip_limit_trusted", "10.0.0.0/8\n203.0.113.7")
	s.st.SetSetting("ip_ban_minutes", "10")
	u := store.User{ID: store.NewID(), Username: "t", UUID: store.NewID(), Password: "p", SubToken: store.NewID(), Status: "active", IPLimit: 1, OnlineIPs: store.NewJSON([]string{})}
	s.st.DB.Create(&u)
	n := store.Node{ID: store.NewID(), Name: "n"}
	s.st.DB.Create(&n)
	s.nodeUserSets.mu.Lock()
	s.nodeUserSets.m = map[string]nodeUsersEntry{n.ID: {set: map[string]bool{u.ID: true}, own: map[string]bool{u.ID: true}}}
	s.nodeUserSets.mu.Unlock()
	s.handleOnlineIPs(n.ID, &polarisv1.OnlineIPs{Users: []*polarisv1.UserIPs{{UserId: u.ID, Ips: []string{"10.1.2.3", "203.0.113.7", "198.51.100.1"}}}})
	var got store.User
	s.st.DB.First(&got, "id = ?", u.ID)
	if got.BannedUntil != nil {
		t.Fatal("trusted addresses counted against the IP limit")
	}
}

func TestDeviceLimit(t *testing.T) {
	s := newTestServer(t)
	u := store.User{ID: store.NewID(), Username: "d", UUID: store.NewID(), Password: "p", SubToken: store.NewID(), Status: "active", DeviceLimit: 1}
	s.st.DB.Create(&u)
	req := func(hwid string) *http.Request {
		r := httptest.NewRequest("GET", "/sub/x", nil)
		if hwid != "" {
			r.Header.Set("X-Hwid", hwid)
		}
		r.Header.Set("User-Agent", "Happ/3.0")
		return r
	}
	if !s.checkDevice(&u, req("phone")) || !s.checkDevice(&u, req("phone")) {
		t.Fatal("first device refused")
	}
	if s.checkDevice(&u, req("laptop")) {
		t.Fatal("second device allowed past the limit")
	}
	if !s.checkDevice(&u, req("")) {
		t.Fatal("requests without a HWID must be served")
	}
	var got store.User
	s.st.DB.First(&got, "id = ?", u.ID)
	if got.LastSubAt == nil || got.LastSubUA != "Happ/3.0" {
		t.Fatalf("last fetch not recorded: %+v", got)
	}
}

func TestTagTrafficSettleAndReset(t *testing.T) {
	s, _, c := newAdminClient(t)
	n := store.Node{ID: "n1", Name: "tokyo", TrafficRate: 2}
	s.st.DB.Create(&n)
	s.traffic.Add("n1", []model.TrafficDelta{{UserID: "inbound>>>vless", Up: 10, Down: 20}, {UserID: "outbound>>>direct", Up: 5, Down: 7}})
	if err := s.settleTraffic(); err != nil {
		t.Fatal(err)
	}
	s.traffic.Add("n1", []model.TrafficDelta{{UserID: "inbound>>>vless", Up: 1, Down: 2}})
	s.settleTraffic()
	var rows []store.TagTraffic
	s.st.DB.Order("kind").Find(&rows)
	if len(rows) != 2 || rows[0].Kind != "inbound" || rows[0].Up != 11 || rows[0].Down != 22 || rows[1].Tag != "direct" {
		t.Fatalf("tag traffic = %+v (raw bytes expected, no multiplier)", rows)
	}
	var hourly int64
	s.st.DB.Model(&store.TrafficHourly{}).Count(&hourly)
	if hourly != 0 {
		t.Fatal("tag counters leaked into per-user traffic")
	}
	if code, _ := c.do("POST", "/stats/tags/reset", map[string]any{"kind": "outbound"}); code != 204 {
		t.Fatalf("reset %d", code)
	}
	s.st.DB.Find(&rows)
	if len(rows) != 1 || rows[0].Kind != "inbound" {
		t.Fatalf("after reset: %+v", rows)
	}
}
