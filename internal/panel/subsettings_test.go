package panel

import (
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestExpireAfterFirstUse(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "n1", TrafficRate: 1})
	s.st.DB.Create(&store.User{ID: "u1", Username: "bob", UUID: "u-1", SubToken: "bob-token-0123456789", Status: "active", ExpireAfterDays: 30})
	s.traffic.Add("n1", []model.TrafficDelta{{UserID: "u1", Up: 10, Down: 10}})
	if err := s.settleTraffic(); err != nil {
		t.Fatal(err)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "u1")
	if u.ExpireAt == nil || u.ExpireAfterDays != 0 || time.Until(*u.ExpireAt) < 29*24*time.Hour {
		t.Fatalf("expiry not started: %+v", u)
	}
}
