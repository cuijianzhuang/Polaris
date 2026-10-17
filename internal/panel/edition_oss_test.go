//go:build !pro

package panel

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func adminMenusPro(*testing.T, *fakeTG, *gorm.DB) {}

// TestCommunityEdition: a build without the polaris-pro sources offers no
// paid feature, whatever the license or trial says, and says so.
func TestCommunityEdition(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.SetSetting("license_trial_until", "2099-01-01T00:00:00Z")
	s.reloadLicense()
	st := s.license()
	if st.Plan != "community" || len(st.Features) != 0 || st.TrialUntil != nil {
		t.Fatalf("state %+v", st)
	}
	if _, v := admin.do("GET", "/license", nil); v["edition"] != "community" {
		t.Fatalf("view %v", v)
	}
	// Speed limits cannot be set (they would have no effect) …
	if code, out := admin.do("POST", "/users", map[string]any{"username": "carol", "speedUpMbps": 10}); code != 402 {
		t.Fatalf("speed on community build: %d %v", code, out)
	}
	// … and none reach a node even if the database has some.
	if ls := s.userLimits(map[string]store.User{"u": {ID: "u", SpeedUpMbps: 5}}); ls != nil {
		t.Fatalf("limits %v", ls)
	}
	s.st.SetSetting("backup_webdav_url", "https://dav.example.com/")
	if res := s.pushBackup("/nonexistent", false); !strings.Contains(res["license"], "专业版") {
		t.Fatalf("push %v", res)
	}

	// Telegram: the admin views explain the edition, the user menu works.
	owner := tgIdentity{admin: &store.TGAdmin{ID: "o", Name: "boss", Role: "owner"}}
	for _, v := range []string{"status", "users", "top", "update"} {
		text, rows := s.tg.view(v, "", owner)
		if !strings.Contains(text, "专业版") || !strings.Contains(buttonData(rows), "v:home") {
			t.Fatalf("view %s: %s", v, text)
		}
	}
	if _, rows := s.tg.view("home", "", owner); !strings.Contains(buttonData(rows), "v:pro") || !strings.Contains(buttonData(rows), "m:login") {
		t.Fatalf("owner home: %s", buttonData(rows))
	}
}
