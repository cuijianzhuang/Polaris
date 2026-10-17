package panel

import (
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// A user's traffic routed HK -> US is counted on HK for the user and on US
// for the chain's service account. Totals and rankings count it once; the
// US node's own view still shows the hop.
func TestServiceTrafficCountedOnce(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.User{ID: "u", Username: "alice", UUID: "u", Status: "active", SubToken: "u"})
	s.st.DB.Create(&store.User{ID: "svc", Username: "svc-chain", UUID: "svc", Status: "active", SubToken: "svc", IsService: true})
	hour := time.Now().Truncate(time.Hour).UTC()
	s.st.DB.Create(&store.TrafficHourly{UserID: "u", NodeID: "hk", Hour: hour, Up: 100, Down: 900})
	s.st.DB.Create(&store.TrafficHourly{UserID: "svc", NodeID: "us", Hour: hour, Up: 100, Down: 900})

	_, ov := c.do("GET", "/stats/overview", nil)
	today := ov["today"].(map[string]any)
	if today["Up"].(float64)+today["Down"].(float64) != 1000 {
		t.Errorf("today = %v, want 1000 (once)", today)
	}
	tops := ov["topUsers"].([]any)
	if len(tops) != 1 || tops[0].(map[string]any)["username"] != "alice" {
		t.Errorf("top users %v", tops)
	}
	_, tr := c.do("GET", "/stats/traffic", nil)
	if n := len(tr["top"].([]any)); n != 1 {
		t.Errorf("traffic ranking has %d users, want 1", n)
	}
	_, us := c.do("GET", "/stats/traffic?node=us", nil)
	series := us["series"].([]any)
	if len(series) != 1 || series[0].(map[string]any)["down"].(float64) != 900 {
		t.Errorf("US node view lost the hop: %v", us)
	}
}
