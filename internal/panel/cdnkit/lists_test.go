package cdnkit

import (
	"reflect"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestRebuildListsMergesFreshReports(t *testing.T) {
	now := time.Now().UTC()
	p1 := store.CDNProfile{ID: "p1", Provider: "cloudflare", Scheme: "https", StaleAlerted: true,
		PreferredIPs: store.NewJSON([]string{"5.5.5.5"}), PinnedHosts: store.NewJSON([]string{"9.9.9.9"})}
	p2 := store.CDNProfile{ID: "p2", Provider: "cloudflare", Scheme: "https",
		PreferredIPs: store.NewJSON([]string{"5.5.5.5", "6.6.6.6"})}
	st := &fakeReportStore{
		profiles: []store.CDNProfile{p1, p2},
		byProfile: map[string][]store.CDNIPReport{"p1": {
			testReport("p1", "广州电信", "ct", "", now, "1.1.1.1", "1.1.1.2"),
		}},
	}
	lists, recovered, err := RebuildLists(st, &p1, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.1.1.1", "1.1.1.2", "9.9.9.9"}; !reflect.DeepEqual(lists["p1"], want) {
		t.Fatalf("p1 list: %v, want %v", lists["p1"], want)
	}
	if _, ok := lists["p2"]; ok {
		t.Fatalf("p2 without reports must not get a list: %v", lists)
	}
	if len(recovered) != 1 || recovered[0].ID != "p1" {
		t.Fatalf("recovered: %+v", recovered)
	}
	if len(st.updates) != 1 || st.updates[0].id != "p1" {
		t.Fatalf("updates: %+v", st.updates)
	}
	upd := st.updates[0].updates
	if !reflect.DeepEqual(upd["preferred_ips"], store.NewJSON([]string{"1.1.1.1", "1.1.1.2", "9.9.9.9"})) {
		t.Fatalf("preferred_ips update: %v", upd["preferred_ips"])
	}
	if upd["last_refresh_at"] != now || upd["last_error"] != "" || upd["stale_alerted"] != false {
		t.Fatalf("update fields: %v", upd)
	}
}

func TestRebuildListsRemoved(t *testing.T) {
	now := time.Now().UTC()
	p2 := store.CDNProfile{ID: "p2", Provider: "cloudflare", Scheme: "https",
		PreferredIPs: store.NewJSON([]string{"5.5.5.5", "6.6.6.6"}), PinnedHosts: store.NewJSON([]string{"6.6.6.6"})}
	st := &fakeReportStore{profiles: []store.CDNProfile{p2}}
	lists, _, err := RebuildLists(st, &p2, now, []string{"5.5.5.5", "6.6.6.6"})
	if err != nil {
		t.Fatal(err)
	}
	// 6.6.6.6 is pinned: only the retired 5.5.5.5 drops out.
	if want := []string{"6.6.6.6"}; !reflect.DeepEqual(lists["p2"], want) {
		t.Fatalf("kept: %v, want %v", lists["p2"], want)
	}
	if len(st.updates) != 1 || !reflect.DeepEqual(st.updates[0].updates["preferred_ips"], store.NewJSON([]string{"6.6.6.6"})) {
		t.Fatalf("updates: %+v", st.updates)
	}
	// A removed list for another port leaves the default port alone.
	st = &fakeReportStore{profiles: []store.CDNProfile{p2}}
	lists, _, err = RebuildLists(st, &p2, now, []string{"5.5.5.5"}, 8443)
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 0 || len(st.updates) != 0 {
		t.Fatalf("other port: lists %v updates %v", lists, st.updates)
	}
	// Nothing fresh and nothing removed: no update at all.
	st = &fakeReportStore{profiles: []store.CDNProfile{p2}}
	lists, _, err = RebuildLists(st, &p2, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 0 || len(st.updates) != 0 {
		t.Fatalf("idle: lists %v updates %v", lists, st.updates)
	}
}

func TestRebuildListsErrors(t *testing.T) {
	now := time.Now().UTC()
	p1 := store.CDNProfile{ID: "p1", Provider: "cloudflare", Scheme: "https"}
	st := &fakeReportStore{listsErr: errTest}
	if _, _, err := RebuildLists(st, &p1, now, nil); err != errTest {
		t.Fatalf("profiles err: %v", err)
	}
	st = &fakeReportStore{profiles: []store.CDNProfile{p1}, reportsErr: errTest}
	if _, _, err := RebuildLists(st, &p1, now, nil); err != errTest {
		t.Fatalf("reports err: %v", err)
	}
}
