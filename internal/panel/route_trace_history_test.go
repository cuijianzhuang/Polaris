package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func traceHistoryFixture(t *testing.T) (*Server, *apiClient) {
	t.Helper()
	s, _, admin := newAdminClient(t)
	for _, id := range []string{"trace", "other"} {
		if err := s.st.DB.Create(&store.Node{ID: id, Name: id}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s, admin
}

func traceResult(target string, path ...string) model.RouteTraceResult {
	r := model.RouteTraceResult{Family: "ipv4", Target: target, CheckedAt: time.Now(), State: "reached", Raw: "private-network-evidence", Hops: []model.RouteTraceHop{}}
	for i, p := range path {
		r.Hops = append(r.Hops, model.RouteTraceHop{TTL: i + 1, Addresses: []string{p}})
	}
	return r
}

func recordTrace(t *testing.T, s *Server, r model.RouteTraceResult) store.NodeRouteTrace {
	t.Helper()
	req := model.RouteTraceRequest{IPv4: r.Target}
	if r.Family == "ipv6" {
		req = model.RouteTraceRequest{IPv6: r.Target}
	}
	rows, err := s.beginRouteTrace("trace", req)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.finishRouteTrace(rows, []model.RouteTraceResult{r}, "completed")
	if err != nil {
		t.Fatal(err)
	}
	return saved[0]
}

func TestRouteTraceHistoryComparison(t *testing.T) {
	s, admin := traceHistoryFixture(t)
	r := traceResult("1.1.1.1", "10.0.0.1", "1.1.1.1")
	first := recordTrace(t, s, r)
	if first.Comparison.V.State != "baseline" {
		t.Fatal(first)
	}
	second := recordTrace(t, s, r)
	if second.Comparison.V.State != "same" || second.Comparison.V.PreviousID != first.ID {
		t.Fatal(second)
	}
	r.Hops[0].Addresses = []string{"10.0.0.2"}
	third := recordTrace(t, s, r)
	if third.Comparison.V.State != "changed" || fmt.Sprint(third.Comparison.V.ChangedTTLs) != "[1]" {
		t.Fatal(third)
	}
	// Incomplete observations break the comparison chain; do not reach across
	// an outage to an older successful sample and invent a continuous change.
	r.State = "timeout"
	incomplete := recordTrace(t, s, r)
	if incomplete.Comparison.V.State != "not_comparable" {
		t.Fatal(incomplete)
	}
	r.State = "reached"
	next := recordTrace(t, s, r)
	if next.Comparison.V.Reason != "previous_incomplete" || next.Comparison.V.PreviousID != incomplete.ID {
		t.Fatal(next)
	}
	if st, out := admin.do("GET", "/nodes/trace/route-traces?limit=2", nil); st != 200 {
		t.Fatal(st, out)
	} else {
		if len(out["items"].([]any)) != 2 || out["nextBefore"].(float64) != float64(incomplete.ID) {
			t.Fatal(out)
		}
		b, _ := json.Marshal(out)
		if strings.Contains(string(b), "private-network-evidence") {
			t.Fatal("raw evidence in list")
		}
	}
	st, out := admin.do("GET", fmt.Sprintf("/nodes/trace/route-traces?before=%d&limit=2", incomplete.ID), nil)
	if st != 200 || out["items"].([]any)[0].(map[string]any)["id"].(float64) != float64(third.ID) {
		t.Fatal(st, out)
	}
	st, out = admin.do("GET", fmt.Sprintf("/nodes/trace/route-traces/%d", first.ID), nil)
	if st != 200 || out["result"].(map[string]any)["raw"] != "private-network-evidence" {
		t.Fatal(st, out)
	}
	if st, out := admin.do("GET", fmt.Sprintf("/nodes/other/route-traces/%d", first.ID), nil); st != 404 {
		t.Fatal(st, out)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=x", "before=-1", "before=x", "family=ipv7", "target=not-an-ip"} {
		if st, out := admin.do("GET", "/nodes/trace/route-traces?"+query, nil); st != 400 {
			t.Fatal(query, st, out)
		}
	}
	if st, out := admin.do("GET", "/nodes/missing/route-traces", nil); st != 404 {
		t.Fatal(st, out)
	}
}

func TestRouteTraceComparisonEvidenceRules(t *testing.T) {
	r := traceResult("1.1.1.1", "10.0.0.1", "1.1.1.1")
	finished := time.Now().Add(-30 * time.Second)
	prev := store.NodeRouteTrace{ID: 1, NodeID: "n", Target: r.Target, Family: r.Family, Profile: routeTraceProfile, State: "completed", FinishedAt: &finished, StartedAt: time.Now().Add(-time.Minute), Result: store.NewJSON(r)}
	current := prev
	current.ID = 2
	current.StartedAt = time.Now()
	for _, tc := range []struct {
		name   string
		edit   func(*store.NodeRouteTrace)
		reason string
	}{
		{"gap", func(r *store.NodeRouteTrace) {
			r.Result.V.Hops = []model.RouteTraceHop{{TTL: 1, Addresses: []string{}}, {TTL: 2, Addresses: []string{"1.1.1.1"}}}
		}, "incomplete_result"},
		{"ttl gap", func(r *store.NodeRouteTrace) {
			r.Result.V.Hops = []model.RouteTraceHop{{TTL: 2, Addresses: []string{"1.1.1.1"}}}
		}, "incomplete_result"},
		{"wrong destination", func(r *store.NodeRouteTrace) {
			r.Result.V.Hops = []model.RouteTraceHop{{TTL: 1, Addresses: []string{"8.8.8.8"}}}
		}, "incomplete_result"},
		{"profile", func(r *store.NodeRouteTrace) { r.Profile = "new" }, "profile_changed"},
		{"stale", func(r *store.NodeRouteTrace) { r.StartedAt = r.StartedAt.Add(25 * time.Hour) }, "sample_window"},
		{"overlap", func(r *store.NodeRouteTrace) { r.StartedAt = finished.Add(-time.Second) }, "overlapping_samples"},
		{"clock skew", func(r *store.NodeRouteTrace) { r.StartedAt = prev.StartedAt.Add(-time.Hour) }, "sample_window"},
		{"scope", func(r *store.NodeRouteTrace) { r.NodeID = "different" }, "scope_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := current
			tc.edit(&row)
			c := compareRouteTrace(row, prev)
			if c.State != "not_comparable" || c.Reason != tc.reason {
				t.Fatal(c)
			}
		})
	}
	// Canonical IPv6 spelling, Agent clock and RTT text do not imply a change.
	v6 := traceResult("2606:4700:4700::1111", "2606:4700:4700:0:0:0:0:1111")
	v6.Family = "ipv6"
	prev.Family = "ipv6"
	prev.Target = v6.Target
	prev.Result = store.NewJSON(v6)
	current = prev
	current.ID = 2
	current.StartedAt = time.Now()
	current.Result.V.Raw = "different RTTs"
	current.Result.V.CheckedAt = time.Now().Add(time.Hour)
	if c := compareRouteTrace(current, prev); c.State != "same" {
		t.Fatal(c)
	}
}

func TestRouteTraceHistoryLateResultsAndDeletion(t *testing.T) {
	s, admin := traceHistoryFixture(t)
	req := model.RouteTraceRequest{IPv4: "1.1.1.1"}
	early, err := s.beginRouteTrace("trace", req)
	if err != nil {
		t.Fatal(err)
	}
	late, err := s.beginRouteTrace("trace", req)
	if err != nil {
		t.Fatal(err)
	}
	r := traceResult(req.IPv4, "1.1.1.1")
	saved, err := s.finishRouteTrace(late, []model.RouteTraceResult{r}, "completed")
	if err != nil {
		t.Fatal(err)
	}
	if saved[0].Comparison.V.Reason != "previous_incomplete" {
		t.Fatal(saved)
	}
	if _, err := s.finishRouteTrace(early, []model.RouteTraceResult{r}, "completed"); err != nil {
		t.Fatal(err)
	}
	st, out := admin.do("GET", "/nodes/trace/route-traces", nil)
	if st != 200 || out["items"].([]any)[0].(map[string]any)["id"].(float64) != float64(late[0].ID) {
		t.Fatal(st, out)
	}
	// A repeated or late completion cannot rewrite a terminal attempt.
	if _, err := s.finishRouteTrace(late, nil, "failed"); err != nil {
		t.Fatal(err)
	}
	var row store.NodeRouteTrace
	s.st.DB.First(&row, late[0].ID)
	if row.State != "completed" {
		t.Fatal(row)
	}
	pending, err := s.beginRouteTrace("trace", req)
	if err != nil {
		t.Fatal(err)
	}
	if st, out := admin.do("DELETE", "/nodes/trace", nil); st != 204 {
		t.Fatal(st, out)
	}
	if _, err := s.finishRouteTrace(pending, []model.RouteTraceResult{r}, "completed"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal(err)
	}
	var count int64
	s.st.DB.Model(&store.NodeRouteTrace{}).Where("node_id = ?", "trace").Count(&count)
	if count != 0 {
		t.Fatal("deleted node history resurrected", count)
	}
}

func TestRouteTraceHistoryRetentionAndRestore(t *testing.T) {
	s, admin := traceHistoryFixture(t)
	now := time.Now()
	var rows []store.NodeRouteTrace
	for i := 0; i < 105; i++ {
		rows = append(rows, store.NodeRouteTrace{NodeID: "trace", Family: "ipv4", Target: "1.1.1.1", State: "completed", StartedAt: now, Profile: routeTraceProfile})
	}
	rows = append(rows, store.NodeRouteTrace{NodeID: "trace", Family: "ipv4", Target: "1.1.1.1", State: "running", StartedAt: now.Add(-time.Hour)})
	if err := s.st.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	last := recordTrace(t, s, traceResult("1.1.1.1", "1.1.1.1"))
	var count int64
	s.st.DB.Model(&store.NodeRouteTrace{}).Where("node_id = ?", "trace").Count(&count)
	if count != 100 {
		t.Fatal("unbounded history", count)
	}
	var interrupted store.NodeRouteTrace
	if err := s.st.DB.First(&interrupted, rows[len(rows)-1].ID).Error; err != nil || interrupted.State != "interrupted" {
		t.Fatal(interrupted, err)
	}
	// Expired details are hidden even before the cleanup job runs.
	s.st.DB.Model(&store.NodeRouteTrace{}).Where("id = ?", last.ID).Update("started_at", now.Add(-8*24*time.Hour))
	if st, out := admin.do("GET", fmt.Sprintf("/nodes/trace/route-traces/%d", last.ID), nil); st != 404 {
		t.Fatal(st, out)
	}
	files, _, err := exportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["node_route_traces"]; ok {
		t.Fatal("route evidence exported")
	}
	if err := s.st.DB.Transaction(func(tx *gorm.DB) error { return importAll(tx, files, func(v string) string { return v }) }); err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.NodeRouteTrace{}).Count(&count)
	if count != 0 {
		t.Fatal("history survived restore", count)
	}
}

func TestRouteTraceHistoryFailedAgentAndIPv6(t *testing.T) {
	s, admin := traceHistoryFixture(t)
	s.st.DB.Model(&store.Node{}).Where("id = ?", "trace").Update("agent_features", store.NewJSON([]string{model.RouteTraceFeature}))
	conn := s.hub.register("trace")
	defer s.hub.unregister(conn)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case m := <-conn.out:
				cmd := m.GetCommand()
				if cmd == nil {
					continue
				}
				answer := &polarisv1.CommandResult{Id: cmd.Id, Ok: i != 0, Output: "invalid json", Error: "do not persist arbitrary agent secrets"}
				if i > 1 {
					var req model.RouteTraceRequest
					json.Unmarshal([]byte(cmd.Args["request"]), &req)
					if req.IPv6 != "2606:4700:4700::1111" {
						t.Error("target not canonical", req)
					}
					r := traceResult(req.IPv6, req.IPv6)
					r.Family = "ipv6"
					b, _ := json.Marshal([]model.RouteTraceResult{r})
					answer.Output = string(b)
				}
				s.hub.resolve("trace", answer)
			}
		}
	}()
	for i := 0; i < 2; i++ {
		if st, out := admin.do("POST", "/nodes/trace/route-trace", map[string]any{"ipv4": "1.1.1.1"}); st != 502 {
			t.Fatal(st, out)
		}
	}
	var failed []store.NodeRouteTrace
	s.st.DB.Where("node_id = ?", "trace").Find(&failed)
	if len(failed) != 2 || failed[0].State != "failed" || failed[1].State != "failed" {
		t.Fatal(failed)
	}
	b, _ := json.Marshal(failed)
	if strings.Contains(string(b), "agent secrets") {
		t.Fatal("arbitrary agent error persisted")
	}
	if st, out := admin.do("POST", "/nodes/trace/route-trace", map[string]any{"ipv6": "2606:4700:4700:0:0:0:0:1111"}); st != 200 || len(out["history"].([]any)) != 1 {
		t.Fatal(st, out)
	}
	if st, out := admin.do("GET", "/nodes/trace/route-traces?family=ipv6&target=2606:4700:4700:0:0:0:0:1111", nil); st != 200 || len(out["items"].([]any)) != 1 {
		t.Fatal(st, out)
	}
}

func TestRouteTraceHistoryConcurrentAttempts(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	const count = 6
	var wg sync.WaitGroup
	ids := make(chan int64, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: "1.1.1.1"})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- rows[0].ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatal("duplicate attempt", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatal(seen)
	}
}

func TestRouteTraceHistoryAuthorization(t *testing.T) {
	s, admin := traceHistoryFixture(t)
	row := recordTrace(t, s, traceResult("1.1.1.1", "1.1.1.1"))
	st, out := admin.do("POST", "/tokens", map[string]any{"name": "node-reader", "scopes": []string{ScopeNodesRead}})
	if st != 201 {
		t.Fatal(st, out)
	}
	token := &apiClient{t: t, base: admin.base, tok: out["secret"].(string)}
	anon := &apiClient{t: t, base: admin.base}
	for _, path := range []string{"/nodes/trace/route-traces", fmt.Sprintf("/nodes/trace/route-traces/%d", row.ID)} {
		if st, out := token.do("GET", path, nil); st != 403 {
			t.Fatal(st, out)
		}
		if st, out := anon.do("GET", path, nil); st != 401 {
			t.Fatal(st, out)
		}
	}
}

func TestRouteTraceHistoryDualStackBeginRollback(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	name := "route-history-test-failure"
	if err := s.st.DB.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*store.NodeRouteTrace); ok && row.Family == "ipv6" {
			tx.AddError(errors.New("injected failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer s.st.DB.Callback().Create().Remove(name)
	if _, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: "1.1.1.1", IPv6: "2606:4700:4700::1111"}); err == nil {
		t.Fatal("failure not propagated")
	}
	var count int64
	if err := s.st.DB.Model(&store.NodeRouteTrace{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("partial attempt survived rollback", count, err)
	}
}

func TestRouteTraceHistoryExpiredAttemptCannotBecomeSuccessful(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	rows, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Model(&store.NodeRouteTrace{}).Where("id = ?", rows[0].ID).Update("started_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	saved, err := s.finishRouteTrace(rows, []model.RouteTraceResult{traceResult("1.1.1.1", "1.1.1.1")}, "completed")
	if err != nil || saved[0].State != "interrupted" || saved[0].Comparison.V.State != "not_comparable" {
		t.Fatal(saved, err)
	}
}
