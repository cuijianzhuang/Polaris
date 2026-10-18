package panel

import (
	"errors"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestUserTrafficProvenanceSettlement(t *testing.T) {
	s, _, c := newAdminClient(t)
	db := s.st.DB
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Create(&store.User{ID: "u", Username: "alice", UUID: "u", SubToken: "u", Status: "active"}).Error)
	must(db.Create(&store.Node{ID: "n", Name: "HK", TrafficRate: 2}).Error)
	must(db.Create(&store.UserNodeQuota{UserID: "u", NodeID: "n", Extra: 50}).Error)
	settle := func(up, down int64) {
		t.Helper()
		must(s.traffic.Add("n", []model.TrafficDelta{{UserID: "u", Up: up, Down: down}}))
		must(s.settleTraffic())
	}
	settle(10, 20) // raw 30, weighted 60, pack 50, account 10
	must(db.Model(&store.Node{}).Where("id = ?", "n").Update("traffic_rate", 3).Error)
	settle(5, 5) // raw 10, weighted 30, account 30
	var row store.TrafficHourly
	must(db.First(&row, "user_id = ?", "u").Error)
	if !row.RawRecorded || row.RawUp != 15 || row.RawDown != 25 || row.Up != 35 || row.Down != 55 || row.RecordedUp != 35 || row.RecordedDown != 55 || row.AccountUp+row.AccountDown != 40 || row.RateMin != 2 || row.RateMax != 3 {
		t.Fatalf("provenance %+v", row)
	}
	var u store.User
	must(db.First(&u, "id = ?", "u").Error)
	if u.UsedUp != row.AccountUp || u.UsedDown != row.AccountDown {
		t.Fatalf("account mismatch: user=%+v row=%+v", u, row)
	}
	// Later configuration changes must never rewrite the historical factor.
	must(db.Model(&store.Node{}).Where("id = ?", "n").Update("traffic_rate", 9).Error)
	code, out := c.do("GET", "/users/u/traffic-sources", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	item := out["items"].([]any)[0].(map[string]any)
	if item["coverage"] != "complete" || item["rateMin"] != float64(2) || item["rateMax"] != float64(3) {
		t.Fatalf("view %v", out)
	}
	sum := out["summary"].(map[string]any)
	if sum["rawUp"] != float64(15) || sum["accountUp"].(float64)+sum["accountDown"].(float64) != 40 || sum["incompleteRows"] != float64(0) {
		t.Fatalf("summary %v", sum)
	}
}

func TestUserTrafficSourcesLegacyAndFilters(t *testing.T) {
	s, _, c := newAdminClient(t)
	db := s.st.DB
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"u", "other", "empty"} {
		must(db.Create(&store.User{ID: id, Username: id, UUID: id, SubToken: id, Status: "active"}).Error)
	}
	must(db.Create(&store.User{ID: "svc", Username: "service", UUID: "svc", SubToken: "svc", IsService: true}).Error)
	now := time.Now().UTC().Truncate(time.Hour)
	for _, row := range []store.TrafficHourly{
		{UserID: "u", NodeID: "deleted", Hour: now.Add(-time.Hour), Up: 100, Down: 200},
		{UserID: "u", NodeID: "mixed", Hour: now, Up: 50, Down: 100},
		{UserID: "u", NodeID: "old", Hour: now.Add(-48 * time.Hour), Down: 999},
		{UserID: "other", NodeID: "mixed", Hour: now, Down: 888},
	} {
		must(db.Create(&row).Error)
	}
	must(db.Create(&store.Node{ID: "mixed", Name: "US", TrafficRate: 2}).Error)
	must(s.traffic.Add("mixed", []model.TrafficDelta{{UserID: "u", Up: 10, Down: 20}}))
	must(s.settleTraffic())
	code, out := c.do("GET", "/users/u/traffic-sources?limit=1", nil)
	if code != 200 || out["total"] != float64(2) {
		t.Fatalf("%d %v", code, out)
	}
	item := out["items"].([]any)[0].(map[string]any)
	if item["coverage"] != "partial" || item["rawUp"] != float64(10) || item["rateMin"] != float64(2) || item["up"] != float64(70) {
		t.Fatalf("partial %v", item)
	}
	sum := out["summary"].(map[string]any)
	if sum["up"] != float64(170) || sum["down"] != float64(340) || sum["incompleteRows"] != float64(2) || sum["recordedRows"] != float64(1) {
		t.Fatalf("sum %v", sum)
	}
	_, out = c.do("GET", "/users/u/traffic-sources?limit=1&offset=1", nil)
	item = out["items"].([]any)[0].(map[string]any)
	if item["nodeId"] != "deleted" || item["coverage"] != "legacy" || item["rawUp"] != nil || item["rateMin"] != nil || item["accountDown"] != nil {
		t.Fatalf("legacy %v", item)
	}
	_, out = c.do("GET", "/users/u/traffic-sources?node=mixed", nil)
	if out["total"] != float64(1) || len(out["nodes"].([]any)) != 2 || out["summary"].(map[string]any)["up"] != float64(70) {
		t.Fatalf("filter %v", out)
	}
	_, out = c.do("GET", "/users/empty/traffic-sources", nil)
	if len(out["items"].([]any)) != 0 || len(out["nodes"].([]any)) != 0 || out["summary"].(map[string]any)["up"] != float64(0) {
		t.Fatalf("empty %v", out)
	}
	for _, q := range []string{"hours=0", "hours=2161", "hours=x", "limit=201", "limit=-1", "offset=-1"} {
		if code, _ := c.do("GET", "/users/u/traffic-sources?"+q, nil); code != 400 {
			t.Fatalf("%s: %d", q, code)
		}
	}
	for _, id := range []string{"missing", "svc"} {
		if code, _ := c.do("GET", "/users/"+id+"/traffic-sources", nil); code != 404 {
			t.Fatalf("%s: %d", id, code)
		}
	}
}

func TestUserTrafficSourcesTokenScope(t *testing.T) {
	s, srv, c := newAdminClient(t)
	if err := s.st.DB.Create(&store.User{ID: "u", Username: "alice", UUID: "private-uuid", Password: "private-password", SubToken: "secret"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"traffic:read", "users:read"} {
		code, out := c.do("POST", "/tokens", map[string]any{"name": scope, "scopes": []string{scope}})
		if code != 201 {
			t.Fatalf("token %d %v", code, out)
		}
		tc := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: out["secret"].(string)}
		code, out = tc.do("GET", "/users/u/traffic-sources", nil)
		want := 403
		if scope == "traffic:read" {
			want = 200
		}
		if code != want {
			t.Fatalf("%s %d %v", scope, code, out)
		}
		if out["uuid"] != nil || out["password"] != nil || out["subToken"] != nil {
			t.Fatalf("credentials exposed: %v", out)
		}
	}
}

// Provenance must roll back with billing, and retrying the restored buffer
// must not double either the raw readings or the account charge.
func TestUserTrafficProvenanceRollback(t *testing.T) {
	s, _, _ := newAdminClient(t)
	db := s.st.DB
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Create(&store.User{ID: "u", Username: "alice", UUID: "u", SubToken: "u", Status: "active"}).Error)
	must(db.Create(&store.Node{ID: "n", Name: "HK", TrafficRate: 2}).Error)
	must(s.traffic.Add("n", []model.TrafficDelta{{UserID: "u", Up: 10, Down: 20}}))
	const callback = "test:fail-daily-traffic"
	must(db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "traffic_dailies" {
			tx.AddError(errors.New("injected settlement failure"))
		}
	}))
	if err := s.settleTraffic(); err == nil {
		t.Fatal("expected rollback")
	}
	db.Callback().Create().Remove(callback)
	var count int64
	must(db.Model(&store.TrafficHourly{}).Count(&count).Error)
	if count != 0 {
		t.Fatal("hourly provenance survived rollback")
	}
	must(s.settleTraffic())
	var row store.TrafficHourly
	must(db.First(&row).Error)
	var u store.User
	must(db.First(&u, "id = ?", "u").Error)
	if row.RawUp != 10 || row.RawDown != 20 || row.Up != 20 || row.Down != 40 || row.AccountUp != 20 || row.AccountDown != 40 || u.UsedUp != 20 || u.UsedDown != 40 {
		t.Fatalf("retry: %+v user=%+v", row, u)
	}
}
