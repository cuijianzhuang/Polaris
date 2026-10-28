package panel

import (
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

func businessOverviewFixture(t *testing.T) (*Server, *apiClient, store.Chain, store.Inbound, model.RelayBusinessRequest) {
	t.Helper()
	s, c := relayFixture(t)
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	ch := markRelayApplied(t, s, id)
	if err := s.st.DB.Model(&store.Node{}).Where("id IN ?", chainNodes(&ch)).Update("last_seen", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Update("agent_features", store.NewJSON([]string{model.RelayBusinessFeature})).Error; err != nil {
		t.Fatal(err)
	}
	var in store.Inbound
	s.st.DB.First(&in, "id = ?", "in")
	view := s.relayView(ch, in)
	var landing store.Node
	s.st.DB.First(&landing, "id = ?", in.NodeID)
	req := model.RelayBusinessRequest{ID: store.NewID() + store.NewID(), EntryAddress: view.Address, EntryPort: view.Port, InboundTag: in.Tag, ServerName: orDefault(in.Security.V.ServerName, landing.Domain), TCPURL: relayBusinessTCP, UDPAddress: relayBusinessUDP}
	return s, c, ch, in, req
}

func passingBusinessResult() model.RelayBusinessResult {
	return model.RelayBusinessResult{State: "passed", BusinessVerified: true, CleanupVerified: true, Checks: []model.BusinessCheck{
		{Kind: "proxy_tcp_business", State: "passed"}, {Kind: "proxy_udp_business", State: "passed"}, {Kind: "credential_cleanup", State: "passed"},
	}}
}

func TestOperationalOverviewRecordsStandardDiagnostic(t *testing.T) {
	s, c, ch, _, _ := businessOverviewFixture(t)
	defer fakeBusinessRunner(t, s, func(model.RelayBusinessRequest) model.RelayBusinessResult { return passingBusinessResult() })()
	if code, out := c.do("POST", "/inbounds/in/relays/"+ch.ID+"/diagnose/business", nil); code != 200 || out["businessVerified"] != true {
		t.Fatal(code, out)
	}
	out, err := s.loadOperationalOverview(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !out.Available || out.Relays.Total != 1 || out.Relays.Passed != 1 {
		t.Fatalf("diagnostic missing %+v", out.Relays)
	}
	// Match the same inbound-specific published endpoint as the diagnostic API.
	if err := s.st.DB.Create(&store.Endpoint{ID: "other-alias", RelayChainID: ch.ID, InboundID: "other", Kind: "relay", CreatedAt: time.Now().Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	out, err = s.loadOperationalOverview(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out.Relays.Passed != 1 {
		t.Fatal("an unrelated endpoint alias changed diagnostic scope")
	}
	var record store.RelayBusinessStatus
	s.st.DB.First(&record, "chain_id = ?", ch.ID)
	if record.State != "passed" || record.CheckedAt == nil || record.Fingerprint == "" {
		t.Fatalf("record %+v", record)
	}
	if code, api := c.do("GET", "/stats/overview", nil); code != 200 || api["operational"] == nil {
		t.Fatal(code, api)
	}
	// Sharing endpoint edits change the tested path without a core revision.
	if code, api := c.do("PUT", "/inbounds/in/relays/"+ch.ID+"/metadata", map[string]any{"shareAddress": "changed.example.com", "sharePort": 8443}); code != 200 {
		t.Fatal(code, api)
	}
	out, err = s.loadOperationalOverview(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out.Relays.Stale != 1 || out.Relays.Passed != 0 {
		t.Fatalf("old configuration still healthy %+v", out.Relays)
	}
}

func TestOperationalOverviewCustomTargetsDoNotOverwriteStandard(t *testing.T) {
	s, c, ch, in, req := businessOverviewFixture(t)
	if err := s.beginRelayBusinessStatus(ch, in, req); err != nil {
		t.Fatal(err)
	}
	failure := model.RelayBusinessResult{State: "failed", CleanupVerified: true, Checks: []model.BusinessCheck{{Kind: "proxy_udp_business", State: "failed", Message: "secret raw node error"}}}
	if err := s.finishRelayBusinessStatus(ch.ID, req.ID, "failed", failure); err != nil {
		t.Fatal(err)
	}
	defer fakeBusinessRunner(t, s, func(model.RelayBusinessRequest) model.RelayBusinessResult { return passingBusinessResult() })()
	code, out := c.do("POST", "/inbounds/in/relays/"+ch.ID+"/diagnose/business", map[string]any{"tcpUrl": "https://custom.example.com/test"})
	if code != 200 || out["businessVerified"] != true {
		t.Fatal(code, out)
	}
	var record store.RelayBusinessStatus
	s.st.DB.First(&record, "chain_id = ?", ch.ID)
	if record.RunID != req.ID || record.State != "failed" {
		t.Fatal("custom diagnostic replaced standard result")
	}
	summary, err := s.loadOperationalOverview(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Relays.Failed != 1 {
		t.Fatalf("failure not summarized %+v", summary.Relays)
	}
	if strings.Contains(strings.Join(record.FailedChecks.V, ","), "secret") {
		t.Fatal("raw result persisted")
	}
}

func TestOperationalOverviewLatestAttemptWinsAndDeletion(t *testing.T) {
	s, c, ch, in, req := businessOverviewFixture(t)
	if err := s.beginRelayBusinessStatus(ch, in, req); err != nil {
		t.Fatal(err)
	}
	newReq := req
	newReq.ID = store.NewID() + store.NewID()
	if err := s.beginRelayBusinessStatus(ch, in, newReq); err != nil {
		t.Fatal(err)
	}
	if err := s.finishRelayBusinessStatus(ch.ID, newReq.ID, "passed", passingBusinessResult()); err != nil {
		t.Fatal(err)
	}
	if err := s.finishRelayBusinessStatus(ch.ID, req.ID, "failed", model.RelayBusinessResult{}); err != nil {
		t.Fatal(err)
	}
	var record store.RelayBusinessStatus
	s.st.DB.First(&record, "chain_id = ?", ch.ID)
	if record.RunID != newReq.ID || record.State != "passed" {
		t.Fatal("late result replaced newer attempt")
	}
	if code, out := c.do("DELETE", "/inbounds/in/relays/"+ch.ID, nil); code != 204 {
		t.Fatal(code, out)
	}
	if err := s.finishRelayBusinessStatus(ch.ID, newReq.ID, "passed", passingBusinessResult()); err != nil {
		t.Fatal(err)
	}
	var count int64
	s.st.DB.Model(&store.RelayBusinessStatus{}).Where("chain_id = ?", ch.ID).Count(&count)
	if count != 0 {
		t.Fatal("deleted relay diagnostic resurrected")
	}
}

func TestOperationalOverviewDiagnosticAgeAndDeployment(t *testing.T) {
	s, _, ch, in, req := businessOverviewFixture(t)
	for _, tc := range []struct {
		name  string
		state string
		age   time.Duration
		want  string
	}{
		{"fresh", "passed", 0, "passed"}, {"failed", "failed", 0, "failed"}, {"expired", "passed", relayBusinessFreshFor, "stale"},
		{"running", "running", 0, "running"}, {"lost", "running", relayBusinessRunningFor, "interrupted"}, {"interrupted", "interrupted", 0, "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.beginRelayBusinessStatus(ch, in, req); err != nil {
				t.Fatal(err)
			}
			if tc.state != "running" {
				if err := s.finishRelayBusinessStatus(ch.ID, req.ID, tc.state, passingBusinessResult()); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			if tc.state == "running" {
				s.st.DB.Model(&store.RelayBusinessStatus{}).Where("chain_id = ?", ch.ID).Update("started_at", now.Add(-tc.age))
			} else {
				s.st.DB.Model(&store.RelayBusinessStatus{}).Where("chain_id = ?", ch.ID).Update("checked_at", now.Add(-tc.age))
			}
			summary, err := s.loadOperationalOverview(now)
			if err != nil {
				t.Fatal(err)
			}
			state := "passed"
			for _, issue := range summary.Issues {
				if issue.Kind == "relay_business" {
					state = issue.State
				}
			}
			if state != tc.want {
				t.Fatalf("got %s want %s, %+v", state, tc.want, summary.Relays)
			}
		})
	}
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "relay").Update("status", "offline").Error; err != nil {
		t.Fatal(err)
	}
	summary, err := s.loadOperationalOverview(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Relays.Unavailable != 1 {
		t.Fatal("offline deployment treated as current business failure")
	}
	if err := s.st.DB.Model(&store.Chain{}).Where("id = ?", ch.ID).Update("disabled", true).Error; err != nil {
		t.Fatal(err)
	}
	summary, err = s.loadOperationalOverview(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Relays.Inactive != 1 {
		t.Fatal("disabled relay treated as failing")
	}
}

func TestOperationalOverviewSamplingAndDelivery(t *testing.T) {
	s, _, admin := newAdminClient(t)
	now := time.Now()
	old := now.Add(-3 * time.Minute)
	for _, node := range []store.Node{
		{ID: "healthy", Name: "healthy", Status: "online", NetObservedAt: &now, NetBootID: "boot"},
		{ID: "stale", Name: "stale", Status: "online", NetObservedAt: &old, NetBootID: "boot"},
		{ID: "never", Name: "never", Status: "online"},
		{ID: "pending", Name: "pending", Status: "pending"},
		{ID: "offline", Name: "offline", Status: "offline", NetObservedAt: &old},
		{ID: "legacy", Name: "legacy", Status: "online", NetObservedAt: &now},
		{ID: "revoked", Name: "revoked", Status: "online", Revoked: true},
	} {
		if err := s.st.DB.Create(&node).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []store.PaymentReceipt{{Ref: "old", Status: "pending", CreatedAt: old}, {Ref: "new", Status: "pending", CreatedAt: now}, {Ref: "review", Status: "review", CreatedAt: now}, {Ref: "done", Status: "done", CreatedAt: old}} {
		s.st.DB.Create(&p)
	}
	s.st.DB.Create(&store.RefundJob{OrderID: "refund", Status: "confirming", CreatedAt: old})
	s.st.DB.Create(&store.ClusterOperation{ID: "operation", CreatedAt: old})
	s.st.DB.Create(&store.ClusterDelivery{OperationID: "operation", MemberID: "member", Status: "pending"})
	summary, err := s.loadOperationalOverview(now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Sampling.Total != 6 || summary.Sampling.Healthy != 2 || summary.Sampling.Stale != 1 || summary.Sampling.Never != 1 || summary.Sampling.Pending != 1 || summary.Sampling.Inactive != 1 || summary.Sampling.Legacy != 1 {
		t.Fatalf("sampling %+v", summary.Sampling)
	}
	if summary.Delivery.PaymentPending != 1 || summary.Delivery.PaymentReview != 1 || summary.Delivery.RefundPending != 1 || summary.Delivery.ClusterPending != 1 {
		t.Fatalf("delivery %+v", summary.Delivery)
	}
	if code, out := admin.do("GET", "/stats/operational", nil); code != 200 || out["available"] != true {
		t.Fatal(code, out)
	}
}

func TestOperationalOverviewTokenBoundaryAndFailure(t *testing.T) {
	s, _, admin := newAdminClient(t)
	code, out := admin.do("POST", "/tokens", map[string]any{"name": "traffic", "scopes": []string{ScopeTrafficRead}})
	if code != 201 {
		t.Fatal(code, out)
	}
	client := &apiClient{t: t, base: admin.base, tok: out["secret"].(string)}
	code, out = client.do("GET", "/stats/overview", nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	if _, exists := out["operational"]; exists {
		t.Fatal("traffic token received payment/diagnostic information")
	}
	if code, _ := client.do("GET", "/stats/operational", nil); code != 403 {
		t.Fatal("operational endpoint opened to traffic token")
	}
	const callback = "test:overview-read-failure"
	s.st.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "relay_business_statuses" {
			tx.AddError(errors.New("unavailable"))
		}
	})
	defer s.st.DB.Callback().Query().Remove(callback)
	code, out = admin.do("GET", "/stats/overview", nil)
	if code != 200 || out["operational"].(map[string]any)["available"] != false {
		t.Fatal("failed query reported a healthy zero overview", code, out)
	}
	if code, _ := admin.do("GET", "/stats/operational", nil); code != 500 {
		t.Fatal("dedicated API hid database failure")
	}
}

func TestOperationalOverviewBoundedIssuesAndRestore(t *testing.T) {
	s, _, ch, in, req := businessOverviewFixture(t)
	if err := s.beginRelayBusinessStatus(ch, in, req); err != nil {
		t.Fatal(err)
	}
	if err := s.finishRelayBusinessStatus(ch.ID, req.ID, "passed", passingBusinessResult()); err != nil {
		t.Fatal(err)
	}
	files, _, err := backupfmt.ExportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := files["relay_business_statuses"]; exists {
		t.Fatal("runtime diagnostic exported")
	}
	if err := s.st.DB.Transaction(func(tx *gorm.DB) error { return backupfmt.ImportAll(tx, files, func(s string) string { return s }) }); err != nil {
		t.Fatal(err)
	}
	var count int64
	s.st.DB.Model(&store.RelayBusinessStatus{}).Count(&count)
	if count != 0 {
		t.Fatal("restore retained unverified pre-restore success")
	}
	nodes := make([]store.Node, 210)
	for i := range nodes {
		id := fmt.Sprintf("sample-%03d", i)
		nodes[i] = store.Node{ID: id, Name: id, Status: "online"}
	}
	if err := s.st.DB.Create(&nodes).Error; err != nil {
		t.Fatal(err)
	}
	out, err := s.loadOperationalOverview(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !out.Truncated || len(out.Issues) != 200 || out.IssueCount <= 200 || out.Sampling.Never < 210 {
		t.Fatalf("truncation/counts %+v", out)
	}
	for i := 1; i < len(out.Issues); i++ {
		a, b := out.Issues[i-1], out.Issues[i]
		if a.Kind == b.Kind && a.ResourceID > b.ResourceID {
			t.Fatal("unstable issue ordering")
		}
	}
}

func TestOperationalOverviewDoesNotRunWithoutPersistence(t *testing.T) {
	s, c, ch, _, _ := businessOverviewFixture(t)
	called := make(chan bool, 1)
	defer fakeBusinessRunner(t, s, func(model.RelayBusinessRequest) model.RelayBusinessResult {
		called <- true
		return passingBusinessResult()
	})()
	const callback = "test:diagnostic-status-write-failure"
	if err := s.st.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "relay_business_statuses" {
			tx.AddError(errors.New("cannot persist"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer s.st.DB.Callback().Create().Remove(callback)
	if code, _ := c.do("POST", "/inbounds/in/relays/"+ch.ID+"/diagnose/business", nil); code != 500 {
		t.Fatal("untracked diagnostic started")
	}
	select {
	case <-called:
		t.Fatal("Agent contacted before status persisted")
	default:
	}
}
