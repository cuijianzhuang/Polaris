package cdnkit

import (
	"errors"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// fakeReportStore is a ReportStore over canned rows. ReportsForPort
// filters by carrier, as the SQL does; the region filter and the
// newest-per-source dedup are cdnkit's job and stay under test.
type fakeReportStore struct {
	byProfile  map[string][]store.CDNIPReport
	profiles   []store.CDNProfile
	byIDs      []store.CDNIPReport
	reportsErr error
	listsErr   error
	updateErr  error
	updates    []profileUpdate
}

type profileUpdate struct {
	id      string
	updates map[string]any
}

func (f *fakeReportStore) ReportsForPort(p *store.CDNProfile, port int, since time.Time, carrier string) ([]store.CDNIPReport, error) {
	if f.reportsErr != nil {
		return nil, f.reportsErr
	}
	var out []store.CDNIPReport
	for _, r := range f.byProfile[p.ID] {
		if carrier != "" && r.Carrier != carrier {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeReportStore) ProfilesSharingLists(origin *store.CDNProfile) ([]store.CDNProfile, error) {
	if f.listsErr != nil {
		return nil, f.listsErr
	}
	return f.profiles, nil
}

func (f *fakeReportStore) UpdateProfile(p *store.CDNProfile, updates map[string]any) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates = append(f.updates, profileUpdate{id: p.ID, updates: updates})
	return nil
}

func (f *fakeReportStore) ReportsByProfiles(ids []string) ([]store.CDNIPReport, error) {
	if f.reportsErr != nil {
		return nil, f.reportsErr
	}
	return f.byIDs, nil
}

func testReport(profile, source, carrier, region string, at time.Time, ips ...string) store.CDNIPReport {
	results := make([]store.CDNIPResult, len(ips))
	for i, ip := range ips {
		results[i] = store.CDNIPResult{IP: ip, LatencyMs: 10}
	}
	return store.CDNIPReport{ProfileID: profile, Source: source, Carrier: carrier, Region: region, At: at, IPs: store.NewJSON(results)}
}

func TestReportsForPortDedupsNewestPerSource(t *testing.T) {
	now := time.Now().UTC()
	p := &store.CDNProfile{ID: "p1", Scheme: "https"}
	st := &fakeReportStore{byProfile: map[string][]store.CDNIPReport{"p1": {
		testReport("p1", "上海电信", "ct", "", now, "1.1.1.1"),
		testReport("p1", "北京联通", "cu", "", now.Add(-time.Minute), "2.2.2.2"),
		testReport("p1", "上海电信", "ct", "", now.Add(-2*time.Minute), "3.3.3.3"),
	}}}
	rs, err := ReportsForPort(st, p, 443, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Source != "上海电信" || rs[0].IPs.V[0].IP != "1.1.1.1" || rs[1].Source != "北京联通" {
		t.Fatalf("dedup: %+v", rs)
	}
}

func TestReportsForPortRegionFilterBeforeDedup(t *testing.T) {
	now := time.Now().UTC()
	p := &store.CDNProfile{ID: "p1", Scheme: "https"}
	st := &fakeReportStore{byProfile: map[string][]store.CDNIPReport{"p1": {
		testReport("p1", "广州电信", "ct", "CN-44", now, "1.1.1.1"),
		// Same source, other region: filtered out, must not consume
		// the source's newest slot.
		testReport("p1", "广州电信", "ct", "CN-31", now.Add(-time.Minute), "9.9.9.9"),
		testReport("p1", "上海电信", "ct", "CN-31", now.Add(-2*time.Minute), "2.2.2.2"),
	}}}
	rs, err := ReportsForPort(st, p, 443, time.Time{}, "", "CN-44")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].IPs.V[0].IP != "1.1.1.1" {
		t.Fatalf("region filter: %+v", rs)
	}
}

func TestSelectionCutoff(t *testing.T) {
	if got := SelectionCutoff(&store.CDNProfile{StalePolicy: "keep"}); !got.IsZero() {
		t.Fatalf("keep: %v", got)
	}
	got := SelectionCutoff(&store.CDNProfile{StaleHours: 48})
	if d := time.Since(got); d < 47*time.Hour || d > 49*time.Hour {
		t.Fatalf("48h: cutoff %v ago", d)
	}
	got = SelectionCutoff(&store.CDNProfile{})
	if d := time.Since(got); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("default: cutoff %v ago", d)
	}
}

func TestAudienceReports(t *testing.T) {
	now := time.Now().UTC()
	p := &store.CDNProfile{ID: "p1", Scheme: "https", RegionSplit: true, CarrierSplit: true}
	st := &fakeReportStore{byProfile: map[string][]store.CDNIPReport{"p1": {
		testReport("p1", "广州电信", "ct", "CN-44", now, "1.1.1.1"),
		testReport("p1", "北京联通", "cu", "CN-11", now, "2.2.2.2"),
	}}}
	cases := []struct {
		carrier, region, mode string
		want                  string
	}{
		{"ct", "CN-44", "region_carrier", "1.1.1.1"},
		{"cu", "CN-44", "carrier", "2.2.2.2"}, // no cu report in GD: fall back to carrier
		{"", "CN-44", "region", "1.1.1.1"},
		{"ct", "", "carrier", "1.1.1.1"},
		{"cm", "CN-44", "merged", ""},
	}
	for _, tc := range cases {
		rs, mode, err := AudienceReports(st, p, 443, tc.carrier, tc.region)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.carrier, tc.region, err)
		}
		if mode != tc.mode {
			t.Fatalf("%s/%s: mode %q, want %q", tc.carrier, tc.region, mode, tc.mode)
		}
		if tc.want == "" {
			if len(rs) != 0 {
				t.Fatalf("%s/%s: want no reports, got %+v", tc.carrier, tc.region, rs)
			}
		} else if len(rs) != 1 || rs[0].IPs.V[0].IP != tc.want {
			t.Fatalf("%s/%s: reports %+v, want %s", tc.carrier, tc.region, rs, tc.want)
		}
	}
	plain := &store.CDNProfile{ID: "p1", Scheme: "https"}
	if _, mode, _ := AudienceReports(st, plain, 443, "ct", "CN-44"); mode != "merged" {
		t.Fatalf("no splits: mode %q", mode)
	}
}

func TestReportsByProfiles(t *testing.T) {
	now := time.Now().UTC()
	st := &fakeReportStore{byIDs: []store.CDNIPReport{
		testReport("p1", "上海电信", "ct", "", now, "1.1.1.1"),
		testReport("p1", "深圳电信", "ct", "CN-44", now, "2.2.2.2"),
		testReport("p2", "x", "", "HK", now, "3.3.3.3"),
	}}
	out, err := ReportsByProfiles(st, []string{"p1", "p2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out["p1"]) != 2 || out["p1"][0].Region != "CN-31" || out["p1"][1].Region != "CN-44" {
		t.Fatalf("p1: %+v", out["p1"])
	}
	if len(out["p2"]) != 1 || out["p2"][0].Region != "HK" {
		t.Fatalf("p2: %+v", out["p2"])
	}
	out, err = ReportsByProfiles(st, nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("empty ids: %+v %v", out, err)
	}
}

func TestReportsForPortError(t *testing.T) {
	st := &fakeReportStore{reportsErr: errTest}
	if _, err := ReportsForPort(st, &store.CDNProfile{ID: "p1"}, 443, time.Time{}, ""); err != errTest {
		t.Fatalf("err: %v", err)
	}
	if _, _, err := AudienceReports(st, &store.CDNProfile{ID: "p1", RegionSplit: true, CarrierSplit: true}, 443, "ct", "CN-44"); err != errTest {
		t.Fatalf("audience err: %v", err)
	}
}

var errTest = errors.New("test error")
