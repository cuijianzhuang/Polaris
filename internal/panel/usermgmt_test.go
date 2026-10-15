package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
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
	s.nodeUserSets.m = map[string]nodeUsersEntry{n.ID: {0, 0, map[string]bool{u.ID: true}}}
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
