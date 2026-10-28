package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/backupfmt"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func routeScheduleFixture(t *testing.T) (*Server, *apiClient) {
	t.Helper()
	s, c := traceHistoryFixture(t)
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "trace").Update("agent_features", store.NewJSON([]string{model.RouteTraceFeature})).Error; err != nil {
		t.Fatal(err)
	}
	return s, c
}

func enableRouteSchedule(t *testing.T, s *Server, c *apiClient) store.NodeRouteScheduleState {
	t.Helper()
	if st, out := c.do("PUT", "/nodes/trace/route-schedule", map[string]any{"enabled": true, "targets": map[string]any{"ipv4": "1.1.1.1"}, "intervalMinutes": 5}); st != 200 {
		t.Fatal(st, out)
	}
	var state store.NodeRouteScheduleState
	if err := s.st.DB.First(&state, "node_id = ?", "trace").Error; err != nil {
		t.Fatal(err)
	}
	return state
}

func fakeScheduledTracer(t *testing.T, s *Server, count *atomic.Int32) func() {
	t.Helper()
	conn := s.hub.register("trace")
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case m := <-conn.out:
				cmd := m.GetCommand()
				if cmd == nil || cmd.Kind != model.RouteTraceFeature {
					continue
				}
				count.Add(1)
				var req model.RouteTraceRequest
				json.Unmarshal([]byte(cmd.Args["request"]), &req)
				results := []model.RouteTraceResult{}
				if req.IPv4 != "" {
					results = append(results, traceResult(req.IPv4, req.IPv4))
				}
				if req.IPv6 != "" {
					r := traceResult(req.IPv6, req.IPv6)
					r.Family = "ipv6"
					results = append(results, r)
				}
				b, _ := json.Marshal(results)
				s.hub.resolve("trace", &polarisv1.CommandResult{Id: cmd.Id, Ok: true, Output: string(b)})
			}
		}
	}()
	return func() { close(stop); s.hub.unregister(conn) }
}

func TestRouteScheduleAPI(t *testing.T) {
	s, c := routeScheduleFixture(t)
	if st, out := c.do("GET", "/nodes/trace/route-schedule", nil); st != 200 || out["policy"].(map[string]any)["enabled"] != false || out["runtime"] != nil {
		t.Fatal(st, out)
	}
	for _, r := range []map[string]any{
		{"enabled": true},
		{"enabled": true, "targets": map[string]any{"ipv4": "127.0.0.1"}},
		{"enabled": true, "targets": map[string]any{"ipv4": "1.1.1.1"}, "intervalMinutes": 1},
		{"intervalMinutes": 1441}, {"expectedRevision": -1},
	} {
		if st, out := c.do("PUT", "/nodes/trace/route-schedule", r); st != 400 {
			t.Fatal(st, out)
		}
	}
	if st, out := c.do("PUT", "/nodes/other/route-schedule", map[string]any{"enabled": true, "targets": map[string]any{"ipv4": "1.1.1.1"}}); st != 409 {
		t.Fatal(st, out)
	}
	state := enableRouteSchedule(t, s, c)
	if state.NextAt.Before(time.Now().Add(4 * time.Minute)) {
		t.Fatal("schedule ran immediately", state)
	}
	next := enableRouteSchedule(t, s, c)
	if !next.NextAt.Equal(state.NextAt) || next.Revision != state.Revision {
		t.Fatal("no-op rescheduled job", state, next)
	}
	if st, out := c.do("PUT", "/nodes/trace/route-schedule", map[string]any{"enabled": false, "expectedRevision": 0}); st != 409 {
		t.Fatal(st, out)
	}
	if st, out := c.do("PUT", "/nodes/trace/route-schedule", map[string]any{"enabled": false, "expectedRevision": state.Revision}); st != 200 || out["runtime"] != nil {
		t.Fatal(st, out)
	}
	var count int64
	s.st.DB.Model(&store.NodeRouteTrace{}).Count(&count)
	if count != 0 {
		t.Fatal("configuration triggered traffic")
	}
	if st, out := c.do("GET", "/nodes/missing/route-schedule", nil); st != 404 {
		t.Fatal(st, out)
	}
}

func TestRouteScheduleRunsOnceAndPauses(t *testing.T) {
	s, c := routeScheduleFixture(t)
	state := enableRouteSchedule(t, s, c)
	var calls atomic.Int32
	defer fakeScheduledTracer(t, s, &calls)()
	// Use real server time for trace timestamps; change only the cursor.
	due := time.Now().Add(-time.Second)
	s.st.DB.Model(&store.NodeRouteScheduleState{}).Where("node_id = ?", "trace").Update("next_at", due)
	now := time.Now().UTC()
	s.runDueRouteTraces(t.Context(), now)
	s.runDueRouteTraces(t.Context(), now)
	if calls.Load() != 1 {
		t.Fatal("repeated scheduled command", calls.Load())
	}
	var rows []store.NodeRouteTrace
	s.st.DB.Find(&rows)
	if len(rows) != 1 || rows[0].Source != "scheduled" || rows[0].ScheduleRevision != state.Revision || rows[0].State != "completed" {
		t.Fatal(rows)
	}
	s.st.DB.First(&state, "node_id = ?", "trace")
	if state.LastState != "completed" || state.LastStartedAt == nil || !state.NextAt.After(now) {
		t.Fatal(state)
	}
	s.st.DB.Model(&state).Update("next_at", due)
	s.st.SetSetting("probe_enabled", "false")
	s.runDueRouteTraces(t.Context(), time.Now())
	if calls.Load() != 1 {
		t.Fatal("global pause ignored")
	}
	s.st.SetSetting("probe_enabled", "true")
	s.runDueRouteTraces(t.Context(), time.Now())
	if calls.Load() != 2 {
		t.Fatal("global resume failed")
	}
}

func TestRouteScheduleSkipsAndRestarts(t *testing.T) {
	s, c := routeScheduleFixture(t)
	enableRouteSchedule(t, s, c)
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, want string
		setup      func()
	}{
		{"offline", "offline", func() {}},
		{"unsupported", "unsupported", func() {
			s.st.DB.Model(&store.Node{}).Where("id = ?", "trace").Update("agent_features", store.NewJSON([]string{}))
		}},
	} {
		tc.setup()
		s.st.DB.Model(&store.NodeRouteScheduleState{}).Where("node_id = ?", "trace").Update("next_at", now.Add(-time.Second))
		_, rows, err := s.claimScheduledRoute("trace", now)
		var state store.NodeRouteScheduleState
		s.st.DB.First(&state, "node_id = ?", "trace")
		if err != nil || len(rows) != 0 || state.LastState != tc.want || !state.NextAt.After(now) {
			t.Fatal(tc.name, rows, err, state)
		}
	}
	// Missing runtime state after restore initializes a future cursor, not a
	// catch-up probe for every historical interval.
	s.st.DB.Where("node_id = ?", "trace").Delete(&store.NodeRouteScheduleState{})
	_, rows, err := s.claimScheduledRoute("trace", now)
	var state store.NodeRouteScheduleState
	s.st.DB.First(&state, "node_id = ?", "trace")
	if err != nil || len(rows) != 0 || state.LastState != "waiting" || !state.NextAt.After(now) {
		t.Fatal(rows, err, state)
	}
}

func TestRouteScheduleClaimAcrossInstances(t *testing.T) {
	s, c := routeScheduleFixture(t)
	enableRouteSchedule(t, s, c)
	var calls atomic.Int32
	defer fakeScheduledTracer(t, s, &calls)()
	cfg := *s.cfg
	other, err := New(&cfg, s.log, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db, _ := other.st.DB.DB(); db.Close() }()
	// Both replicas see the connection here without issuing network commands;
	// claims themselves must still be serialized through shared database state.
	conn := other.hub.register("trace")
	defer other.hub.unregister(conn)
	s.st.DB.Model(&store.NodeRouteScheduleState{}).Where("node_id = ?", "trace").Update("next_at", time.Now().Add(-time.Second))
	var wg sync.WaitGroup
	claimed := make(chan []store.NodeRouteTrace, 2)
	now := time.Now().UTC()
	for _, server := range []*Server{s, other} {
		wg.Add(1)
		go func(server *Server) {
			defer wg.Done()
			_, rows, err := server.claimScheduledRoute("trace", now)
			if err != nil {
				t.Error(err)
			}
			claimed <- rows
		}(server)
	}
	wg.Wait()
	close(claimed)
	total := 0
	for rows := range claimed {
		total += len(rows)
	}
	if total != 1 || calls.Load() != 0 {
		t.Fatal("duplicate claim or dispatched inside transaction", total, calls.Load())
	}
}

func TestRouteScheduleHistoryAndRestore(t *testing.T) {
	s, c := routeScheduleFixture(t)
	enableRouteSchedule(t, s, c)
	files, _, err := backupfmt.ExportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["node_route_schedules"]; !ok {
		t.Fatal("schedule configuration not backed up")
	}
	if _, ok := files["node_route_schedule_states"]; ok {
		t.Fatal("runtime cursor backed up")
	}
	if err := s.st.DB.Transaction(func(tx *gorm.DB) error { return backupfmt.ImportAll(tx, files, func(v string) string { return v }) }); err != nil {
		t.Fatal(err)
	}
	var policy store.NodeRouteSchedule
	if err := s.st.DB.First(&policy, "node_id = ?", "trace").Error; err != nil || !policy.Enabled {
		t.Fatal(policy, err)
	}
	var count int64
	s.st.DB.Model(&store.NodeRouteScheduleState{}).Count(&count)
	if count != 0 {
		t.Fatal("runtime cursor retained")
	}
	if st, out := c.do("DELETE", "/nodes/trace", nil); st != 204 {
		t.Fatal(st, out)
	}
	s.st.DB.Model(&store.NodeRouteSchedule{}).Count(&count)
	if count != 0 {
		t.Fatal("orphan schedule")
	}
}

func TestRouteScheduleChangedPolicySuppressesInFlightNotice(t *testing.T) {
	s, c := routeScheduleFixture(t)
	events := routeEventSink(s)
	recordTrace(t, s, traceResult("1.1.1.1", "10.0.0.1", "1.1.1.1"))
	enableRouteSchedule(t, s, c)
	var calls atomic.Int32
	defer fakeScheduledTracer(t, s, &calls)()
	s.st.DB.Model(&store.NodeRouteScheduleState{}).Where("node_id = ?", "trace").Update("next_at", time.Now().Add(-time.Second))
	_, rows, err := s.claimScheduledRoute("trace", time.Now())
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if st, out := c.do("PUT", "/nodes/trace/route-schedule", map[string]any{"enabled": false}); st != 200 {
		t.Fatal(st, out)
	}
	saved, err := s.finishRouteTrace(rows, []model.RouteTraceResult{traceResult("1.1.1.1", "10.0.0.2", "1.1.1.1")}, "completed")
	if err != nil || saved[0].Comparison.V.State != "changed" || len(events) != 0 {
		t.Fatal(saved, err, len(events))
	}
	st, out := c.do("GET", fmt.Sprintf("/nodes/trace/route-traces/%d", saved[0].ID), nil)
	if st != 200 || out["source"] != "scheduled" {
		t.Fatal(st, out)
	}
}

func TestRouteScheduleWorkerCancellation(t *testing.T) {
	s, c := routeScheduleFixture(t)
	enableRouteSchedule(t, s, c)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.runDueRouteTraces(ctx, time.Now().Add(time.Hour))
	var count int64
	s.st.DB.Model(&store.NodeRouteTrace{}).Count(&count)
	if count != 0 {
		t.Fatal("canceled job created attempts")
	}
}

func TestRouteScheduleBudgetAndBusyManualProbe(t *testing.T) {
	s, c := routeScheduleFixture(t)
	enableRouteSchedule(t, s, c)
	var calls atomic.Int32
	defer fakeScheduledTracer(t, s, &calls)()
	due := time.Now().Add(-time.Second)
	s.st.DB.Model(&store.NodeRouteScheduleState{}).Where("node_id = ?", "trace").Update("next_at", due)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	s.runDueRouteTraces(ctx, time.Now())
	if calls.Load() != 0 {
		t.Fatal("short job budget dispatched a truncated probe")
	}
	manual, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	_, rows, err := s.claimScheduledRoute("trace", time.Now())
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	var state store.NodeRouteScheduleState
	s.st.DB.First(&state, "node_id = ?", "trace")
	if state.LastState != "busy" {
		t.Fatal(state)
	}
	s.finishRouteTrace(manual, nil, "interrupted")
}

func TestRouteScheduleClaimRollback(t *testing.T) {
	s, c := routeScheduleFixture(t)
	enableRouteSchedule(t, s, c)
	var calls atomic.Int32
	defer fakeScheduledTracer(t, s, &calls)()
	due := time.Now().Add(-time.Second).UTC()
	s.st.DB.Model(&store.NodeRouteScheduleState{}).Where("node_id = ?", "trace").Update("next_at", due)
	hook := "test:scheduled-attempt-create-failure"
	s.st.DB.Callback().Create().Before("gorm:create").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "node_route_traces" {
			tx.AddError(fmt.Errorf("injected creation failure"))
		}
	})
	_, _, err := s.claimScheduledRoute("trace", time.Now())
	s.st.DB.Callback().Create().Remove(hook)
	if err == nil {
		t.Fatal("creation failure ignored")
	}
	var state store.NodeRouteScheduleState
	s.st.DB.First(&state, "node_id = ?", "trace")
	// PostgreSQL timestamps have microsecond precision.
	if state.NextAt.Sub(due).Abs() > time.Microsecond || state.LastStartedAt != nil {
		t.Fatal("failed claim advanced cursor", state)
	}
	var count int64
	s.st.DB.Model(&store.NodeRouteTrace{}).Count(&count)
	if count != 0 {
		t.Fatal("partial history after failed claim")
	}
}

func TestRouteScheduleTokenAndAnonymousAuthorization(t *testing.T) {
	_, c := routeScheduleFixture(t)
	st, out := c.do("POST", "/tokens", map[string]any{"name": "node-read", "scopes": []string{ScopeNodesRead}})
	if st != 201 {
		t.Fatal(st, out)
	}
	token := &apiClient{t: t, base: c.base, tok: out["secret"].(string)}
	anon := &apiClient{t: t, base: c.base}
	for _, method := range []string{"GET", "PUT"} {
		if st, out := token.do(method, "/nodes/trace/route-schedule", nil); st != 403 {
			t.Fatal(st, out)
		}
		if st, out := anon.do(method, "/nodes/trace/route-schedule", nil); st != 401 {
			t.Fatal(st, out)
		}
	}
}

func TestRouteScheduleOldResultCannotCompleteReusedHistoryID(t *testing.T) {
	s, _ := routeScheduleFixture(t)
	rows, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Delete(&store.NodeRouteTrace{}, rows[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	replacement := rows[0]
	replacement.RunID = store.NewID()
	if err := s.st.DB.Create(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.finishRouteTrace(rows, []model.RouteTraceResult{traceResult("1.1.1.1", "1.1.1.1")}, "completed"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal(err)
	}
	var saved store.NodeRouteTrace
	s.st.DB.First(&saved, replacement.ID)
	if saved.State != "running" || saved.RunID != replacement.RunID {
		t.Fatal("old run overwrote new attempt", saved)
	}
}

func TestRouteScheduleFanoutBound(t *testing.T) {
	s, _ := routeScheduleFixture(t)
	var active, peak, calls atomic.Int32
	stops := []func(){}
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("scheduled-%d", i)
		if err := s.st.DB.Create(&store.Node{ID: id, Name: id, AgentFeatures: store.NewJSON([]string{model.RouteTraceFeature})}).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.st.DB.Create(&store.NodeRouteSchedule{NodeID: id, Enabled: true, Targets: store.NewJSON(model.RouteTraceRequest{IPv4: "1.1.1.1"}), IntervalMinutes: 5, Revision: 1}).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.st.DB.Create(&store.NodeRouteScheduleState{NodeID: id, Revision: 1, NextAt: now.Add(-time.Second)}).Error; err != nil {
			t.Fatal(err)
		}
		conn := s.hub.register(id)
		stop := make(chan struct{})
		stops = append(stops, func() { close(stop); s.hub.unregister(conn) })
		go func() {
			for {
				select {
				case <-stop:
					return
				case msg := <-conn.out:
					cmd := msg.GetCommand()
					if cmd == nil {
						continue
					}
					calls.Add(1)
					n := active.Add(1)
					for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
					}
					time.Sleep(20 * time.Millisecond)
					active.Add(-1)
					raw, _ := json.Marshal([]model.RouteTraceResult{traceResult("1.1.1.1", "1.1.1.1")})
					s.hub.resolve(id, &polarisv1.CommandResult{Id: cmd.Id, Ok: true, Output: string(raw)})
				}
			}
		}()
	}
	s.runDueRouteTraces(t.Context(), now)
	if calls.Load() != 10 || peak.Load() > 4 {
		t.Fatal("unbounded fanout or missing nodes", calls.Load(), peak.Load())
	}
}

func TestRouteScheduleGlobalPauseSuppressesInFlightNotice(t *testing.T) {
	s, c := routeScheduleFixture(t)
	events := routeEventSink(s)
	recordTrace(t, s, traceResult("1.1.1.1", "10.0.0.1", "1.1.1.1"))
	enableRouteSchedule(t, s, c)
	var calls atomic.Int32
	defer fakeScheduledTracer(t, s, &calls)()
	s.st.DB.Model(&store.NodeRouteScheduleState{}).Where("node_id = ?", "trace").Update("next_at", time.Now().Add(-time.Second))
	_, rows, err := s.claimScheduledRoute("trace", time.Now())
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	s.st.SetSetting("probe_enabled", "false")
	saved, err := s.finishRouteTrace(rows, []model.RouteTraceResult{traceResult("1.1.1.1", "10.0.0.2", "1.1.1.1")}, "completed")
	if err != nil || saved[0].Comparison.V.State != "changed" || len(events) != 0 {
		t.Fatal(saved, err, len(events))
	}
}

func TestRouteScheduleCanceledBeforeDispatchDoesNotSend(t *testing.T) {
	s, _ := routeScheduleFixture(t)
	conn := s.hub.register("trace")
	defer s.hub.unregister(conn)
	rows, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := s.executeRouteTrace(ctx, "trace", model.RouteTraceRequest{IPv4: "1.1.1.1"}, rows); err == nil {
		t.Fatal("canceled request executed")
	}
	if len(conn.out) != 0 {
		t.Fatal("command sent after request was canceled")
	}
	var row store.NodeRouteTrace
	s.st.DB.First(&row, rows[0].ID)
	if row.State != "interrupted" {
		t.Fatal(row)
	}
}

func TestRouteScheduleDailyComparisonAllowsDispatchJitter(t *testing.T) {
	now := time.Now().UTC()
	finished := now.Add(-24 * time.Hour)
	result := traceResult("1.1.1.1", "1.1.1.1")
	previous := store.NodeRouteTrace{ID: 1, NodeID: "n", Family: "ipv4", Target: "1.1.1.1", Profile: routeTraceProfile, Source: "scheduled", State: "completed", StartedAt: now.Add(-24*time.Hour - 30*time.Second), FinishedAt: &finished, Result: store.NewJSON(result)}
	current := previous
	current.ID = 2
	current.StartedAt = now
	if comparison := compareRouteTrace(current, previous); comparison.State != "same" {
		t.Fatal("daily job lost comparability because of tick jitter", comparison)
	}
	current.StartedAt = now.Add(2 * time.Hour)
	if comparison := compareRouteTrace(current, previous); comparison.State != "not_comparable" || comparison.Reason != "sample_window" {
		t.Fatal("stale sample compared", comparison)
	}
}
