package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/backupfmt"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func lineHop(ttl int, addr string, asn uint32) model.RouteTraceHop {
	h := model.RouteTraceHop{TTL: ttl, Addresses: []string{addr}}
	if asn != 0 {
		h.ASN = []model.RouteTraceIPASN{{IP: addr, ASN: asn, State: "matched", Organization: "fixture"}}
	}
	return h
}

func lineResult(target string, hops ...model.RouteTraceHop) model.RouteTraceResult {
	return model.RouteTraceResult{Family: "ipv4", Target: target, CheckedAt: time.Now().UTC(), State: "reached", Hops: hops}
}

func recordLineTrace(t *testing.T, s *Server, r model.RouteTraceResult) store.NodeRouteTrace {
	t.Helper()
	rows, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: r.Target})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.finishRouteTrace(rows, []model.RouteTraceResult{r}, "completed")
	if err != nil {
		t.Fatal(err)
	}
	return saved[0]
}

func lineOf(t *testing.T, s *Server, traceID int64) store.NodeRouteLine {
	t.Helper()
	var lr store.NodeRouteLine
	if err := s.st.DB.First(&lr, "trace_id = ?", traceID).Error; err != nil {
		t.Fatalf("no classification for trace %d: %v", traceID, err)
	}
	return lr
}

// The full design D5 rule table, plus conflict resolution and the
// evidence-must-not-leak-addresses rule.
func TestClassifyRouteTable(t *testing.T) {
	plain := "203.0.113.9"
	for _, tc := range []struct {
		name                      string
		hops                      []model.RouteTraceHop
		carrier, line, confidence string
	}{
		{"cn2 segment without 163 is suspect gia", []model.RouteTraceHop{lineHop(1, plain, 0), lineHop(8, "59.43.10.1", 0), lineHop(12, "1.1.1.1", 0)}, "ct", "cn2", "suspect"},
		{"cn2 with 163 segment is segment-confident", []model.RouteTraceHop{lineHop(7, "202.97.1.1", 0), lineHop(9, "59.43.1.1", 0)}, "ct", "cn2", "segment"},
		{"163 segment", []model.RouteTraceHop{lineHop(8, "202.97.1.1", 0)}, "ct", "163", "segment"},
		{"163 by asn only", []model.RouteTraceHop{lineHop(8, plain, 4134)}, "ct", "163", "asn"},
		{"9929 segment 210.51", []model.RouteTraceHop{lineHop(8, "210.51.1.1", 0)}, "cu", "9929", "segment"},
		{"9929 segment 218.105", []model.RouteTraceHop{lineHop(8, "218.105.1.1", 0)}, "cu", "9929", "segment"},
		{"9929 by asn", []model.RouteTraceHop{lineHop(8, plain, 9929)}, "cu", "9929", "asn"},
		{"4837 by asn", []model.RouteTraceHop{lineHop(8, plain, 4837)}, "cu", "4837", "asn"},
		{"9929 beats earlier 4837", []model.RouteTraceHop{lineHop(5, plain, 4837), lineHop(9, plain, 9929)}, "cu", "9929", "asn"},
		{"cmin2 by asn", []model.RouteTraceHop{lineHop(8, plain, 58807)}, "cm", "cmin2", "asn"},
		{"cmi by asn", []model.RouteTraceHop{lineHop(8, plain, 58453)}, "cm", "cmi", "asn"},
		{"cmnet by asn", []model.RouteTraceHop{lineHop(8, plain, 9808)}, "cm", "cmnet", "asn"},
		{"mobile closest to target wins", []model.RouteTraceHop{lineHop(5, plain, 58807), lineHop(11, plain, 9808)}, "cm", "cmnet", "asn"},
		{"no features is unknown", []model.RouteTraceHop{lineHop(1, plain, 0), lineHop(9, "1.1.1.1", 0)}, "", "unknown", "none"},
		{"unrelated asn ignored", []model.RouteTraceHop{lineHop(8, plain, 15169)}, "", "unknown", "none"},
		{"cross-carrier last segment wins cu", []model.RouteTraceHop{lineHop(6, "59.43.1.1", 0), lineHop(12, plain, 4837)}, "cu", "4837", "asn"},
		{"cross-carrier last segment wins ct", []model.RouteTraceHop{lineHop(6, plain, 4837), lineHop(12, "59.43.1.1", 0)}, "ct", "cn2", "suspect"},
		{"segment breaks ttl tie across carriers", []model.RouteTraceHop{lineHop(8, plain, 4837), lineHop(8, "202.97.1.1", 0)}, "ct", "163", "segment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carrier, line, confidence, hits := classifyRoute(tc.hops)
			if carrier != tc.carrier || line != tc.line || confidence != tc.confidence {
				t.Fatalf("got %s/%s/%s, want %s/%s/%s (hits %v)", carrier, line, confidence, tc.carrier, tc.line, tc.confidence, hits)
			}
		})
	}
	// Evidence records the feature, never the hop address itself.
	_, _, _, hits := classifyRoute([]model.RouteTraceHop{lineHop(8, "59.43.10.1", 0)})
	if len(hits) != 1 || hits[0].Match != "59.43.*" || hits[0].Rule != "cn2-segment" || hits[0].TTL != 8 {
		t.Fatalf("evidence hits: %+v", hits)
	}
	b, _ := json.Marshal(hits)
	if strings.Contains(string(b), "59.43.10.1") {
		t.Fatal("evidence leaked a hop address", string(b))
	}
}

func TestClassifyRouteTraceStates(t *testing.T) {
	row := store.NodeRouteTrace{Family: "ipv4", State: "completed", Result: store.NewJSON(lineResult("1.1.1.1", lineHop(8, "59.43.1.1", 0)))}
	if _, line, conf, ev := classifyRouteTrace(row); line != "cn2" || conf != "suspect" || ev == "" {
		t.Fatalf("reached: %s %s %q", line, conf, ev)
	}
	for _, edit := range []func(*store.NodeRouteTrace){
		func(r *store.NodeRouteTrace) { r.Result.V.State = "partial" },
		func(r *store.NodeRouteTrace) { r.Result.V.State = "timeout" },
		func(r *store.NodeRouteTrace) { r.State = "failed" },
		func(r *store.NodeRouteTrace) { r.Family = "ipv6" },
		func(r *store.NodeRouteTrace) { r.Result.V.Hops = nil },
	} {
		r2 := row
		edit(&r2)
		if _, line, conf, ev := classifyRouteTrace(r2); line != "unmeasured" || conf != "none" || ev != "" {
			t.Fatalf("want unmeasured, got %s %s %q", line, conf, ev)
		}
	}
	// Evidence stays within its column budget even for feature-dense paths.
	var hops []model.RouteTraceHop
	for i := 1; i <= 20; i++ {
		hops = append(hops, lineHop(i, "59.43.1.1", 4134))
	}
	row.Result = store.NewJSON(lineResult("1.1.1.1", hops...))
	_, _, _, ev := classifyRouteTrace(row)
	if len(ev) > 1024 {
		t.Fatalf("evidence too long: %d", len(ev))
	}
}

func TestRouteLineDowngradeEventAndStorage(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	ch := routeEventSink(s)
	cn2 := lineResult("1.1.1.1", lineHop(8, "59.43.1.1", 0), lineHop(12, "1.1.1.1", 0))
	first := recordLineTrace(t, s, cn2)
	if lr := lineOf(t, s, first.ID); lr.Line != "cn2" || lr.Confidence != "suspect" || lr.Carrier != "ct" || lr.ClassifierVersion != routeClassifierVersion {
		t.Fatalf("first classification: %+v", lr)
	}
	if len(ch) != 0 {
		t.Fatal("first classification notified")
	}
	net163 := lineResult("1.1.1.1", lineHop(8, "202.97.1.1", 0), lineHop(12, "1.1.1.1", 0))
	second := recordLineTrace(t, s, net163)
	e := takeRouteEvent(t, ch)
	if e.Kind != "route_line_changed" || eventType(e) != "node.route_line_changed" || e.Data["from_line"] != "cn2" || e.Data["to_line"] != "163" || e.Data["change"] != "downgrade" || e.Data["trace_id"] != second.ID || e.Data["previous_trace_id"] != first.ID {
		t.Fatalf("downgrade event: %+v", e)
	}
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "59.43") || strings.Contains(string(b), "202.97") {
		t.Fatal("hop evidence in notification", string(b))
	}
	recordLineTrace(t, s, net163) // same line: quiet
	recordLineTrace(t, s, cn2)    // upgrade: quiet
	if len(ch) != 0 {
		t.Fatal("same-line or upgrade notified", len(ch))
	}
	// Duplicate completion of the downgrade attempt writes no second row
	// and fires no second event.
	rows, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.finishRouteTrace(rows, []model.RouteTraceResult{net163}, "completed"); err != nil {
		t.Fatal(err)
	}
	takeRouteEvent(t, ch) // the downgrade itself
	var count int64
	s.st.DB.Model(&store.NodeRouteLine{}).Where("trace_id = ?", rows[0].ID).Count(&count)
	if _, err := s.finishRouteTrace(rows, []model.RouteTraceResult{net163}, "completed"); err != nil {
		t.Fatal(err)
	}
	s.st.DB.Model(&store.NodeRouteLine{}).Where("trace_id = ?", rows[0].ID).Count(&count)
	if count != 1 || len(ch) != 0 {
		t.Fatalf("duplicate completion: rows=%d events=%d", count, len(ch))
	}
}

func TestRouteLineUnknownStreakEvent(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	ch := routeEventSink(s)
	recordLineTrace(t, s, lineResult("1.1.1.1", lineHop(8, plain2, 58807))) // CMIN2 premium
	unknown := lineResult("1.1.1.1", lineHop(3, "203.0.113.9", 0), lineHop(9, "1.1.1.1", 0))
	recordLineTrace(t, s, unknown)
	recordLineTrace(t, s, unknown)
	if len(ch) != 0 {
		t.Fatal("two unknowns notified")
	}
	recordLineTrace(t, s, unknown)
	e := takeRouteEvent(t, ch)
	if e.Kind != "route_line_changed" || e.Data["change"] != "unidentifiable" || e.Data["from_line"] != "cmin2" || e.Data["to_line"] != "unknown" {
		t.Fatalf("streak event: %+v", e)
	}
	recordLineTrace(t, s, unknown) // fourth: streak already reported
	if len(ch) != 0 {
		t.Fatal("unknown streak re-notified")
	}
}

const plain2 = "203.0.113.9"

func TestRouteLineLateCompletionDoesNotNotify(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	ch := routeEventSink(s)
	recordLineTrace(t, s, lineResult("1.1.1.1", lineHop(8, "59.43.1.1", 0)))
	req := model.RouteTraceRequest{IPv4: "1.1.1.1"}
	old, err := s.beginRouteTrace("trace", req)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.beginRouteTrace("trace", req)
	if err != nil {
		t.Fatal(err)
	}
	net163 := lineResult("1.1.1.1", lineHop(8, "202.97.1.1", 0))
	if _, err := s.finishRouteTrace(newer, []model.RouteTraceResult{net163}, "completed"); err != nil {
		t.Fatal(err)
	}
	takeRouteEvent(t, ch) // the newer downgrade notifies
	if _, err := s.finishRouteTrace(old, []model.RouteTraceResult{net163}, "completed"); err != nil {
		t.Fatal(err)
	}
	if len(ch) != 0 {
		t.Fatal("late completion notified")
	}
	// The late row is still classified and stored.
	if lr := lineOf(t, s, old[0].ID); lr.Line != "163" {
		t.Fatalf("late classification: %+v", lr)
	}
}

func TestRouteLineRollbackStoresNothing(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	ch := routeEventSink(s)
	recordLineTrace(t, s, lineResult("1.1.1.1", lineHop(8, "59.43.1.1", 0)))
	rows, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	hook := "test:route-line-rollback"
	if err := s.st.DB.Callback().Delete().Before("gorm:delete").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "node_route_traces" {
			tx.AddError(errors.New("injected retention failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.finishRouteTrace(rows, []model.RouteTraceResult{lineResult("1.1.1.1", lineHop(8, "202.97.1.1", 0))}, "completed")
	s.st.DB.Callback().Delete().Remove(hook)
	if err == nil {
		t.Fatal("finish should have failed")
	}
	var count int64
	s.st.DB.Model(&store.NodeRouteLine{}).Where("trace_id = ?", rows[0].ID).Count(&count)
	if count != 0 || len(ch) != 0 {
		t.Fatalf("rolled-back classification persisted or notified: rows=%d events=%d", count, len(ch))
	}
}

func TestRouteLineRetentionAndBackupExclusion(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	now := time.Now().UTC()
	var traces []store.NodeRouteTrace
	for i := 0; i < 105; i++ {
		traces = append(traces, store.NodeRouteTrace{NodeID: "trace", Family: "ipv4", Target: "1.1.1.1", State: "completed", StartedAt: now, Profile: routeTraceProfile})
	}
	if err := s.st.DB.Create(&traces).Error; err != nil {
		t.Fatal(err)
	}
	var lines []store.NodeRouteLine
	for _, tr := range traces {
		lines = append(lines, store.NodeRouteLine{TraceID: tr.ID, NodeID: "trace", Family: "ipv4", Target: "1.1.1.1", Line: "163", Confidence: "segment", ClassifierVersion: 1, CreatedAt: now})
	}
	if err := s.st.DB.Create(&lines).Error; err != nil {
		t.Fatal(err)
	}
	recordTrace(t, s, traceResult("1.1.1.1", "1.1.1.1"))
	var traceCount, lineCount, orphans int64
	s.st.DB.Model(&store.NodeRouteTrace{}).Where("node_id = ?", "trace").Count(&traceCount)
	s.st.DB.Model(&store.NodeRouteLine{}).Where("node_id = ?", "trace").Count(&lineCount)
	s.st.DB.Model(&store.NodeRouteLine{}).Where("trace_id NOT IN (?)", s.st.DB.Model(&store.NodeRouteTrace{}).Select("id")).Count(&orphans)
	if traceCount != 100 || orphans != 0 || lineCount > traceCount {
		t.Fatalf("retention: traces=%d lines=%d orphans=%d", traceCount, lineCount, orphans)
	}
	files, _, err := backupfmt.ExportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["node_route_lines"]; ok {
		t.Fatal("route line classifications exported")
	}
}

func TestRouteLineHistoryFields(t *testing.T) {
	s, admin := traceHistoryFixture(t)
	row := recordLineTrace(t, s, lineResult("1.1.1.1", lineHop(8, "59.43.1.1", 0)))
	if st, out := admin.do("GET", "/nodes/trace/route-traces?limit=5", nil); st != 200 {
		t.Fatal(st, out)
	} else {
		item := out["items"].([]any)[0].(map[string]any)
		if item["line"] != "cn2" || item["confidence"] != "suspect" {
			t.Fatal("list line fields", item)
		}
		if _, ok := item["lineEvidence"]; ok {
			t.Fatal("evidence in list view", item)
		}
	}
	st, out := admin.do("GET", fmt.Sprintf("/nodes/trace/route-traces/%d", row.ID), nil)
	if st != 200 {
		t.Fatal(st, out)
	}
	if out["line"] != "cn2" || out["classifierVersion"].(float64) != 1 {
		t.Fatal("detail line fields", out)
	}
	ev, ok := out["lineEvidence"].([]any)
	if !ok || len(ev) != 1 || ev[0].(map[string]any)["rule"] != "cn2-segment" {
		t.Fatal("detail evidence", out)
	}
}

func TestRouteLineCarriersFill(t *testing.T) {
	s := newTestServer(t)
	node := store.Node{ID: "n1", Name: "n1", Status: "online"}
	if err := s.st.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.ProbeTarget{ID: "ct-a", Name: "上海电信", Carrier: "ct", Addr: "sh-ct-v4.example:80", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, lr := range []store.NodeRouteLine{
		{TraceID: 11, NodeID: "n1", Family: "ipv4", Target: "1.1.1.1", Carrier: "ct", Line: "cn2", Confidence: "suspect", ClassifierVersion: 1, CreatedAt: now.Add(-time.Hour)},
		{TraceID: 12, NodeID: "n1", Family: "ipv4", Target: "1.1.1.1", Carrier: "ct", Line: "163", Confidence: "segment", ClassifierVersion: 1, CreatedAt: now},
		{TraceID: 13, NodeID: "n1", Family: "ipv4", Target: "1.1.1.1", Carrier: "cu", Line: "unknown", Confidence: "none", ClassifierVersion: 1, CreatedAt: now},
		{TraceID: 14, NodeID: "n1", Family: "ipv6", Target: "2606:4700:4700::1111", Carrier: "ct", Line: "cn2", Confidence: "segment", ClassifierVersion: 1, CreatedAt: now},
	} {
		if err := s.st.DB.Create(&lr).Error; err != nil {
			t.Fatal(err)
		}
	}
	out := s.carrierSummary(&node, now)
	ct := carrierOf(t, out, "ct")
	if ct.Line == nil || *ct.Line != "163" || ct.Confidence == nil || *ct.Confidence != "segment" || ct.LineAt == nil {
		t.Fatalf("ct line fill: %+v", ct)
	}
	cu := carrierOf(t, out, "cu")
	if cu.Line == nil || *cu.Line != "unknown" || cu.LineAt == nil {
		t.Fatalf("cu line fill: %+v", cu)
	}
	cm := carrierOf(t, out, "cm")
	if cm.Line != nil || cm.LineAt != nil {
		t.Fatalf("cm should stay null: %+v", cm)
	}
}

func TestRouteLineChangedNotificationPolicy(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	now := time.Now()
	e := Event{Kind: "route_line_changed", NodeID: "trace", ResourceID: "target-a", Text: "downgrade", noticeRecipient: "admin"}
	for _, channel := range notificationChannels {
		if _, allow := s.operationalNotice(channel, e, now); allow {
			t.Fatal("route_line_changed enabled by default on", channel)
		}
	}
	p := defaultNotificationPolicy()
	p.Channels = map[string]map[string]bool{"email": {"route_line_changed": true}}
	saveNoticePolicy(t, s, p)
	if _, allow := s.operationalNotice("email", e, now); !allow {
		t.Fatal("opt-in did not enable route_line_changed")
	}
	if got := eventType(Event{Kind: "route_line_changed"}); got != "node.route_line_changed" {
		t.Fatal("webhook type", got)
	}
}
