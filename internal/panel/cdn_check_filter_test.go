package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// Write exactly the pre-upgrade detailed result format. No compact failure is
// required for already displayed failures to stop appearing in subscriptions.
func legacyCDNFailure(t *testing.T, s *Server, epID, address string) {
	t.Helper()
	var ep store.Endpoint
	var row store.Inbound
	var p store.CDNProfile
	s.st.DB.First(&ep, "id = ?", epID)
	s.st.DB.First(&row, "id = ?", ep.InboundID)
	s.st.DB.First(&p, "id = ?", ep.CDNProfileID)
	m := s.toModelInbound(&row)
	target := s.cdnTargetFor(&ep, &m, &p)
	target.Server = address
	result := cdnAddressResult{EndpointID: ep.ID, Target: target, CheckedAt: time.Now().Add(-48 * time.Hour), Viewpoint: "panel", cdnCheck: cdnCheck{Message: "fixture failed"}}
	raw, _ := json.Marshal(result)
	checks := p.AddressChecks.V
	if checks == nil {
		checks = map[string]json.RawMessage{}
	}
	checks[cdnCheckKey(ep.ID, target, m.Transport.Type, cdnHealthRevision(row, &p))] = raw
	if err := s.st.DB.Model(&p).Update("address_checks", store.NewJSON(checks)).Error; err != nil {
		t.Fatal(err)
	}
}
func TestCDNFailedChecksAffectSubscriptionsAndPreview(t *testing.T) {
	s, _, admin := cdnRegionFixture(t)
	s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "regional").Updates(map[string]any{"ip_top_n": 1, "pinned_hosts": store.NewJSON([]string{"fixed.example.com"}), "preferred_ips": store.NewJSON([]string{"104.16.1.1", "104.16.2.1", "fixed.example.com"})})
	legacyCDNFailure(t, s, "re", "104.16.1.1")
	want := []string{"104.16.2.1", "fixed.example.com"}
	if got := regionServers(t, s, "ra", "", ""); !slices.Equal(got, want) {
		t.Fatal("failure consumed Top N slot", got)
	}
	code, out := admin.do("GET", "/cdn-profiles/regional/address-tools?endpoint=re", nil)
	if code != 200 || fmt.Sprint(out["selected"]) != "[104.16.2.1 fixed.example.com]" {
		t.Fatal("preview disagreed with subscription", code, out)
	}
	row := out["items"].([]any)[0].(map[string]any)
	if row["reason"] != "check_failed" || row["selected"] != false || row["failedAt"] == nil {
		t.Fatal("failure still shown as selected", row)
	}
	// A new port/path/SNI does not inherit an unrelated connection verdict.
	for _, change := range []map[string]any{{"port": 2053}, {"path": "/changed"}, {"sni": "other.example.com"}} {
		s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Updates(change)
		if got := regionServers(t, s, "ra", "", ""); !slices.Contains(got, "104.16.1.1") {
			t.Fatal("unrelated context blocked", change, got)
		}
		s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Updates(map[string]any{"port": 0, "path": "", "sni": ""})
	}
	// Other entries are independent, even when they use the same profile/port.
	s.st.DB.Create(&store.Endpoint{ID: "re-other", InboundID: "ri", Kind: "cdn", CDNProfileID: "regional"})
	if got := regionServers(t, s, "ra", "", ""); !slices.Contains(got, "104.16.1.1") {
		t.Fatal("failure crossed entry boundary", got)
	}
	s.st.DB.Delete(&store.Endpoint{}, "id = ?", "re-other")
	// Personal lists and fixed addresses cannot override a matching failed check.
	s.st.DB.Model(&store.User{}).Where("id = ?", "ra").Update("cdn_preferred", store.NewJSON(map[string][]string{"regional": {"104.16.1.1", "104.16.3.1"}}))
	if got := regionServers(t, s, "ra", "", ""); !slices.Equal(got, []string{"104.16.3.1"}) {
		t.Fatal("personal list bypassed failure", got)
	}
	s.st.DB.Model(&store.User{}).Where("id = ?", "ra").Update("cdn_preferred", store.NewJSON(map[string][]string{}))
	legacyCDNFailure(t, s, "re", "104.16.2.1")
	legacyCDNFailure(t, s, "re", "fixed.example.com")
	// Automatic all-bad fail-open must not revive manually diagnosed failures.
	s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "regional").Updates(map[string]any{"node_check": true, "bad_ips": store.NewJSON([]string{"104.16.1.1", "104.16.2.1", "fixed.example.com"})})
	if got := regionServers(t, s, "ra", "", ""); len(got) != 0 {
		t.Fatal("all-failed entry reappeared through fallback", got)
	}
	// Explicit address and origin-only modes also obey their exact scoped result.
	s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Update("address", "104.16.1.1")
	if got := regionServers(t, s, "ra", "", ""); len(got) != 0 {
		t.Fatal("explicit entry address bypassed failure", got)
	}
	s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Update("address", "")
	s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "regional").Update("expand_per_ip", false)
	legacyCDNFailure(t, s, "re", "cdn.example.com")
	if got := regionServers(t, s, "ra", "", ""); len(got) != 0 {
		t.Fatal("failed origin-only target emitted", got)
	}
}

func TestCDNFailedCheckPersistenceAndRecovery(t *testing.T) {
	s, _, admin := newAdminClient(t)
	var status atomic.Int64
	status.Store(403)
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status.Load() != 101 {
			w.WriteHeader(int(status.Load()))
			return
		}
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Sec-WebSocket-Accept", cdnWebSocketAccept(r.Header.Get("Sec-WebSocket-Key")))
		w.WriteHeader(101)
	}))
	defer edge.Close()
	port, _ := strconv.Atoi(strings.Split(edge.URL, ":")[2])
	for _, v := range []any{
		&store.Node{ID: "failure-node"},
		&store.Inbound{ID: "failure-in", NodeID: "failure-node", Tag: "ws", Enabled: true, Port: port, Core: model.CoreXray, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS, Path: "/ws"}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone})},
		&store.CDNProfile{ID: "failure", Name: "failure", Provider: "other", Scheme: "http", OriginDomain: "localhost", Ports: store.NewJSON([]int{port}), ExpandPerIP: true, PreferredIPs: store.NewJSON([]string{"127.0.0.1", "127.0.0.2"})},
		&store.Endpoint{ID: "failure-ep", InboundID: "failure-in", Kind: "cdn", CDNProfileID: "failure"},
	} {
		if err := s.st.DB.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	check := func(address string) map[string]any {
		t.Helper()
		code, out := admin.do("POST", "/cdn-profiles/failure/check-address", map[string]any{"endpointId": "failure-ep", "address": address})
		if code != 200 {
			t.Fatal(code, out)
		}
		return out
	}
	selected := func() string {
		t.Helper()
		code, out := admin.do("GET", "/cdn-profiles/failure/address-tools", nil)
		if code != 200 {
			t.Fatal(code, out)
		}
		return fmt.Sprint(out["selected"])
	}
	if r := check("127.0.0.1"); r["ok"] != false || r["status"] != float64(403) {
		t.Fatal(r)
	}
	if got := selected(); got != "[127.0.0.2]" {
		t.Fatal("real failed check not applied", got)
	}
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", "failure")
	// Make the real failure older than a full 64-entry history, then trigger an
	// actual save/prune through another address check.
	for key, raw := range p.AddressChecks.V {
		var r cdnAddressResult
		json.Unmarshal(raw, &r)
		r.CheckedAt = time.Now().Add(-time.Hour)
		b, _ := json.Marshal(r)
		p.AddressChecks.V[key] = b
	}
	for i := 0; i < 64; i++ {
		raw, _ := json.Marshal(cdnAddressResult{EndpointID: "another-entry", CheckedAt: time.Now().Add(-time.Duration(i+1) * time.Second), cdnCheck: cdnCheck{OK: true}})
		p.AddressChecks.V[fmt.Sprint("history-", i)] = raw
	}
	s.st.DB.Model(&p).Update("address_checks", p.AddressChecks)
	check("127.0.0.2")
	p = store.CDNProfile{}
	s.st.DB.First(&p, "id = ?", "failure")
	if len(p.AddressChecks.V) != 64 {
		t.Fatal("detail history unbounded", len(p.AddressChecks.V))
	}
	for _, raw := range p.AddressChecks.V {
		var r cdnAddressResult
		json.Unmarshal(raw, &r)
		if r.Target.Server == "127.0.0.1" {
			t.Fatal("test did not evict old details")
		}
	}
	if got := selected(); got != "[]" {
		t.Fatal("evicted failure revived address", got)
	}
	slot := cdnFailureSlot("failure-ep", "127.0.0.1")
	failure := p.AddressFailures.V[slot]
	// Partial evidence cannot clear an existing failure.
	r := cdnAddressResult{EndpointID: "failure-ep", Target: cdnTarget{Server: "127.0.0.1"}, CheckedAt: time.Now(), cdnCheck: cdnCheck{Partial: true}}
	recordCDNAddressVerdict(&p, failure.Fingerprint, r)
	if _, ok := p.AddressFailures.V[slot]; !ok {
		t.Fatal("partial result cleared failure")
	}
	// A successful real recheck restores only this address.
	status.Store(101)
	if r := check("127.0.0.1"); r["ok"] != true {
		t.Fatal(r)
	}
	if got := selected(); got != "[127.0.0.1]" {
		t.Fatal("successful recheck did not restore address", got)
	}
	p = store.CDNProfile{}
	s.st.DB.First(&p, "id = ?", "failure")
	if _, ok := p.AddressFailures.V[slot]; ok {
		t.Fatal("success did not clear compact failure")
	}

	// A fresh partial result remains eligible, while a partial retry of a
	// definite failure is insufficient to restore that same context.
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "failure-in").Update("transport", store.NewJSON(model.TransportSpec{Type: model.TransXHTTP, Path: "/ws"}))
	status.Store(200)
	if r := check("127.0.0.1"); r["partial"] != true {
		t.Fatal("expected real XHTTP partial", r)
	}
	if got := selected(); !strings.Contains(got, "127.0.0.1") {
		t.Fatal("fresh partial incorrectly excluded", got)
	}
	status.Store(500)
	check("127.0.0.1")
	status.Store(200)
	if r := check("127.0.0.1"); r["partial"] != true {
		t.Fatal(r)
	}
	if got := selected(); strings.Contains(got, "127.0.0.1") {
		t.Fatal("partial retry cleared known failure", got)
	}

	if code, out := admin.do("DELETE", "/cdn-profiles/failure/check-address", map[string]any{"endpointId": "failure-ep", "address": "127.0.0.1"}); code != 200 {
		t.Fatal(code, out)
	}
	if got := selected(); !strings.Contains(got, "127.0.0.1") {
		t.Fatal("explicit reset did not restore untested candidate", got)
	}
	_, token, err := s.issueAPIToken("report", "", []string{ScopeCDNIPs}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	probe := &apiClient{t: t, base: admin.base, tok: token}
	if code, _ := probe.do("DELETE", "/cdn-profiles/failure/check-address", map[string]any{"endpointId": "failure-ep", "address": "127.0.0.1"}); code != 403 {
		t.Fatal("report token could reset diagnostic state", code)
	}
	if code, _ := admin.do("DELETE", "/cdn-profiles/failure/check-address", map[string]any{"endpointId": "re", "address": "127.0.0.1"}); code != 404 {
		t.Fatal("foreign entry admitted", code)
	}
}
