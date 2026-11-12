package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/secret"
)

// TestNewIndexesUsed seeds a realistic volume of rows and asserts that the
// hot queries added in optimize/batch2 actually use an index instead of a
// full table scan. SQLite is cost-based, so the seed volume matters: with
// only a handful of rows it would (correctly) prefer a scan.
//
// The queries below mirror the production predicates:
//   - users expiring / banned / reset-policy scans (api_users.go, metrics_prom.go, traffic.go)
//   - traffic_hourlies grouped by hour / node (api_stats.go)
//   - node_traffic_dailies grouped by day (costs.go)
//   - cdn_profiles scheduled address checks (cdn_schedule.go)
//   - endpoints by CDN profile + kind (cdn_address_tools.go)
//   - sub_profiles by panel user (account_binding.go)
func TestNewIndexesUsed(t *testing.T) {
	box, _ := secret.New("k")
	s, err := Open(filepath.Join(t.TempDir(), "idx.db"), box, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Hour)
	users := make([]User, 0, 2000)
	for i := 0; i < 2000; i++ {
		u := User{ID: fmt.Sprintf("u%04d", i), Username: fmt.Sprintf("user%04d", i), UUID: fmt.Sprintf("uuid-%04d", i), SubToken: fmt.Sprintf("sub-%04d", i), Status: "active", ResetPolicy: "none"}
		if i%10 == 0 {
			u.ResetPolicy = "monthly"
		}
		if i%5 == 0 {
			exp := now.Add(24 * time.Hour)
			u.ExpireAt = &exp
		}
		if i%20 == 0 {
			ban := now.Add(-time.Hour)
			u.BannedUntil = &ban
		}
		users = append(users, u)
	}
	if err := s.DB.CreateInBatches(users, 500).Error; err != nil {
		t.Fatal(err)
	}

	hourly := make([]TrafficHourly, 0, 5000)
	for i := 0; i < 5000; i++ {
		hourly = append(hourly, TrafficHourly{
			UserID: fmt.Sprintf("u%04d", i%2000), NodeID: fmt.Sprintf("n%d", i%10),
			Hour: now.Add(-time.Duration(i%2160) * time.Hour), Up: 1, Down: 1,
		})
	}
	if err := s.DB.CreateInBatches(hourly, 500).Error; err != nil {
		t.Fatal(err)
	}

	daily := make([]NodeTrafficDaily, 0, 300)
	for d := 0; d < 30; d++ {
		for n := 0; n < 10; n++ {
			daily = append(daily, NodeTrafficDaily{NodeID: fmt.Sprintf("n%d", n), Day: now.AddDate(0, 0, -d).Format("2006-01-02"), Rx: 1, Tx: 1})
		}
	}
	if err := s.DB.CreateInBatches(daily, 100).Error; err != nil {
		t.Fatal(err)
	}

	profiles := make([]CDNProfile, 0, 300)
	for i := 0; i < 300; i++ {
		p := CDNProfile{ID: fmt.Sprintf("p%03d", i), Name: fmt.Sprintf("p%03d", i), AddressCheckInterval: 60}
		next := now.Add(24 * time.Hour)
		if i%30 == 0 {
			next = now.Add(-time.Hour) // due
		}
		p.AddressCheckNextAt = &next
		profiles = append(profiles, p)
	}
	if err := s.DB.CreateInBatches(profiles, 100).Error; err != nil {
		t.Fatal(err)
	}
	eps := []Endpoint{
		{ID: "e1", InboundID: "i1", Kind: "cdn", CDNProfileID: "p000"},
		{ID: "e2", InboundID: "i2", Kind: "direct", CDNProfileID: "p000"},
	}
	if err := s.DB.Create(&eps).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&SubProfile{ID: "sp1", Name: "sp1", Token: "tok1", PanelUserID: "u0001"}).Error; err != nil {
		t.Fatal(err)
	}

	// Index existence: GORM names a tag index idx_<table>_<column>, a named
	// composite index keeps its given name.
	wantIndexes := map[string][]string{
		"users":                {"idx_users_status_expire", "idx_users_reset_policy", "idx_users_banned_until"},
		"traffic_hourlies":     {"idx_traffic_hourlies_hour"},
		"node_traffic_dailies": {"idx_node_traffic_dailies_day"},
		"cdn_profiles":         {"idx_cdn_profiles_address_check_next_at"},
		"endpoints":            {"idx_endpoints_cdn_profile_kind"},
		"sub_profiles":         {"idx_sub_profiles_panel_user_id"},
		// Already present before batch2; the audit claimed these were missing.
		"traffic_dailies":     {"idx_traffic_dailies_day"},
		"tag_traffic_dailies": {"idx_tag_traffic_dailies_day"},
	}
	for table, names := range wantIndexes {
		for _, name := range names {
			var n int64
			if err := s.DB.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name=? AND name=?", table, name).Scan(&n).Error; err != nil || n != 1 {
				t.Errorf("index %s on %s: count=%d, err=%v", name, table, n, err)
			}
		}
	}

	// EXPLAIN QUERY PLAN must show an index search (not a SCAN, even a SCAN
	// of a PK index) for each hot query, except cdn_profiles: its OR
	// predicate (IS NULL OR <= ?) makes SQLite scan the new index in order
	// instead of searching it, which still avoids the table scan and the
	// ORDER BY sort the baseline needed. That case only asserts the index
	// is the scan source.
	cases := []struct {
		name           string
		sql            string
		args           []any
		allowIndexScan bool
	}{
		{"users expiring", "SELECT id FROM users WHERE status = ? AND expire_at IS NOT NULL AND expire_at > ? AND expire_at < ?", []any{"active", now.Format(time.RFC3339), now.Add(48 * time.Hour).Format(time.RFC3339)}, false},
		{"users banned", "SELECT id FROM users WHERE banned_until IS NOT NULL AND banned_until < ?", []any{now.Format(time.RFC3339)}, false},
		{"users reset policy", "SELECT id FROM users WHERE reset_policy IN ('daily','weekly','monthly','cycle')", nil, false},
		{"traffic_hourlies by hour", "SELECT hour, SUM(up) FROM traffic_hourlies WHERE hour >= ? GROUP BY hour", []any{now.AddDate(0, 0, -30).Format(time.RFC3339)}, false},
		{"node_traffic_dailies by day", "SELECT node_id, SUM(rx) FROM node_traffic_dailies WHERE day >= ? AND day <= ? GROUP BY node_id", []any{now.AddDate(0, 0, -29).Format("2006-01-02"), now.Format("2006-01-02")}, false},
		{"cdn_profiles due", "SELECT id FROM cdn_profiles WHERE address_check_interval > 0 AND (address_check_next_at IS NULL OR address_check_next_at <= ?) ORDER BY address_check_next_at ASC, id ASC", []any{now.Format(time.RFC3339)}, true},
		{"endpoints by profile+kind", "SELECT id FROM endpoints WHERE cdn_profile_id = ? AND kind = ?", []any{"p000", "cdn"}, false},
		{"sub_profiles by panel user", "SELECT id FROM sub_profiles WHERE panel_user_id = ?", []any{"u0001"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rows []struct {
				ID, Parent, NotUsed, Detail string
			}
			// EXPLAIN QUERY PLAN returns (id, parent, notused, detail).
			if err := s.DB.Raw("EXPLAIN QUERY PLAN "+tc.sql, tc.args...).Scan(&rows).Error; err != nil {
				t.Fatal(err)
			}
			plan := ""
			for _, r := range rows {
				plan += r.Detail + "; "
			}
			// A SEARCH (not a SCAN, even a SCAN of a PK index) proves the
			// planner can seek into the index for this query's predicate.
			if tc.allowIndexScan {
				if !strings.Contains(plan, "USING INDEX idx_cdn_profiles_address_check_next_at") {
					t.Errorf("query did not use the cdn_profiles index: %s", plan)
				} else {
					t.Logf("plan: %s", plan)
				}
			} else if !strings.Contains(plan, "SEARCH") {
				t.Errorf("query did not use an index search: %s", plan)
			} else {
				t.Logf("plan: %s", plan)
			}
		})
	}
}
