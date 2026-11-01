package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

type cdnScheduleFixture struct {
	s      *Server
	admin  *apiClient
	status atomic.Int64
	cf     atomic.Bool
	hits   atomic.Int64
}

func scheduleFixture(t *testing.T) *cdnScheduleFixture {
	t.Helper()
	s, _, admin := newAdminClient(t)
	f := &cdnScheduleFixture{s: s, admin: admin}
	f.status.Store(403)
	f.cf.Store(true)
	for i := 1; i <= 2; i++ {
		entry := i
		edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.hits.Add(1)
			if r.Host != "localhost" || r.URL.Path != fmt.Sprintf("/ws%d", entry) {
				w.WriteHeader(404)
				return
			}
			if f.cf.Load() {
				w.Header().Set("Server", "cloudflare")
			}
			code := int(f.status.Load())
			if entry == 2 {
				code = 101
			}
			if code == 101 {
				w.Header().Set("Connection", "Upgrade")
				w.Header().Set("Upgrade", "websocket")
				w.Header().Set("Sec-WebSocket-Accept", cdnWebSocketAccept(r.Header.Get("Sec-WebSocket-Key")))
			}
			w.WriteHeader(code)
		}))
		t.Cleanup(edge.Close)
		port, _ := strconv.Atoi(strings.Split(edge.URL, ":")[2])
		rows := []any{&store.Inbound{ID: fmt.Sprint("si", i), NodeID: "sn", Tag: fmt.Sprint("ws", i), Enabled: true, Port: port, Core: model.CoreXray, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS, Path: fmt.Sprintf("/ws%d", i)}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone})}, &store.Endpoint{ID: fmt.Sprint("se", i), InboundID: fmt.Sprint("si", i), Kind: "cdn", CDNProfileID: "schedule", Port: port}}
		for _, row := range rows {
			if err := s.st.DB.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, row := range []any{&store.Node{ID: "sn", Name: "scheduled node"}, &store.CDNProfile{ID: "schedule", Name: "schedule", Scheme: "http", OriginDomain: "localhost", ExpandPerIP: true, PreferredIPs: store.NewJSON([]string{"127.0.0.1", "localhost"}), PinnedHosts: store.NewJSON([]string{"blocked.example.com"}), BlockedHosts: store.NewJSON([]string{"blocked.example.com"})}} {
		if err := s.st.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func (f *cdnScheduleFixture) profile(t *testing.T) store.CDNProfile {
	t.Helper()
	var p store.CDNProfile
	if err := f.s.st.DB.First(&p, "id = ?", "schedule").Error; err != nil {
		t.Fatal(err)
	}
	return p
}
func (f *cdnScheduleFixture) due(t *testing.T) {
	t.Helper()
	if err := f.s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "schedule").Update("address_check_next_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
}
func (f *cdnScheduleFixture) configure(t *testing.T, interval int) {
	t.Helper()
	if code, out := f.admin.do("PUT", "/cdn-profiles/schedule/check-schedule", map[string]any{"intervalMinutes": interval}); code != 200 {
		t.Fatal(code, out)
	}
}

func TestCDNScheduledAddressChecksAndRecovery(t *testing.T) {
	f := scheduleFixture(t)
	for _, body := range []map[string]any{{}, {"intervalMinutes": 1}, {"intervalMinutes": 1441}} {
		if code, _ := f.admin.do("PUT", "/cdn-profiles/schedule/check-schedule", body); code != 400 {
			t.Fatal("invalid schedule", code)
		}
	}
	f.configure(t, 5)
	f.s.checkScheduledCDNAddresses()
	p := f.profile(t)
	if len(p.AddressChecks.V) != 4 || len(p.AddressFailures.V) != 2 || p.AddressCheckRuntime.V.Checked != 4 || p.AddressCheckRuntime.V.FinishedAt == nil || p.AddressCheckLeaseID != "" {
		t.Fatalf("wrong persisted sweep: %+v", p.AddressCheckRuntime.V)
	}
	for _, ep := range []string{"se1", "se2"} {
		code, out := f.admin.do("GET", "/cdn-profiles/schedule/address-tools?endpoint="+ep, nil)
		if code != 200 {
			t.Fatal(code, out)
		}
		selected := out["selected"].([]any)
		if (ep == "se1" && len(selected) != 0) || (ep == "se2" && len(selected) == 0) {
			t.Fatal("entry/port verdict leaked", ep, out)
		}
	}
	hits := f.hits.Load()
	f.s.checkScheduledCDNAddresses()
	if f.hits.Load() != hits {
		t.Fatal("future schedule ran again")
	}
	// Partial evidence cannot restore a failed address; an actual successful
	// transport recheck can. Manually blocked addresses are never requested.
	f.status.Store(101)
	f.cf.Store(false)
	f.due(t)
	f.s.checkScheduledCDNAddresses()
	p = f.profile(t)
	if len(p.AddressFailures.V) != 2 || p.AddressCheckRuntime.V.Partial != 4 {
		t.Fatal("partial result cleared a failure", p.AddressCheckRuntime.V)
	}
	f.cf.Store(true)
	f.due(t)
	f.s.checkScheduledCDNAddresses()
	p = f.profile(t)
	if len(p.AddressFailures.V) != 0 || p.AddressCheckRuntime.V.Failed != 0 {
		t.Fatal("successful recheck did not recover")
	}
	_, token, err := f.s.issueAPIToken("probe", "", []string{ScopeCDNIPs}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	probe := &apiClient{t: t, base: f.admin.base, tok: token}
	if code, _ := probe.do("PUT", "/cdn-profiles/schedule/check-schedule", map[string]any{"intervalMinutes": 5}); code != 403 {
		t.Fatal("report token changed schedule", code)
	}
}

func TestCDNScheduledAddressBatchResumesBeyondHistory(t *testing.T) {
	f := scheduleFixture(t)
	f.s.st.DB.Delete(&store.Endpoint{}, "id = ?", "se2")
	ips := []string{}
	for i := 1; i <= 70; i++ {
		ips = append(ips, fmt.Sprintf("127.0.0.%d", i))
	}
	f.s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "schedule").Update("preferred_ips", store.NewJSON(ips))
	f.configure(t, 5)
	for batch := 0; batch < 20; batch++ {
		f.due(t)
		f.s.checkScheduledCDNAddresses()
		p := f.profile(t)
		if p.AddressCheckRuntime.V.Checked > 70 {
			t.Fatal("sweep revisited already checked fingerprints")
		}
		if p.AddressCheckRuntime.V.FinishedAt != nil {
			if p.AddressCheckRuntime.V.Checked != 70 || len(p.AddressChecks.V) != 64 || len(p.AddressFailures.V) != 70 {
				t.Fatal("history eviction lost sweep coverage", p.AddressCheckRuntime.V, len(p.AddressChecks.V), len(p.AddressFailures.V))
			}
			return
		}
		if p.AddressCheckRuntime.V.Cursor == "" {
			t.Fatal("unfinished sweep lost its cursor")
		}
	}
	t.Fatal("sweep did not complete")
}

func TestCDNScheduledLeaseAndDisable(t *testing.T) {
	f := scheduleFixture(t)
	f.configure(t, 5)
	now := time.Now()
	p, ok, err := f.s.claimCDNAddressCheck(context.Background(), "schedule", now)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, ok, err := f.s.claimCDNAddressCheck(context.Background(), "schedule", now); err != nil || ok {
		t.Fatal("duplicate claim", ok, err)
	}
	newer, ok, err := f.s.claimCDNAddressCheck(context.Background(), "schedule", now.Add(3*time.Minute+time.Second))
	if err != nil || !ok || newer.AddressCheckLeaseID == p.AddressCheckLeaseID {
		t.Fatal("abandoned lease did not expire", err)
	}
	result := cdnAddressResult{EndpointID: "se1", Target: cdnTarget{Server: "127.0.0.1"}, CheckedAt: time.Now()}
	if err := f.s.saveCDNAddressResult(context.Background(), p.ID, "old", result, p.AddressCheckLeaseID); !errors.Is(err, errCDNScheduleChanged) {
		t.Fatal("old worker could write", err)
	}
	f.configure(t, 0)
	if err := f.s.saveCDNAddressResult(context.Background(), p.ID, "disabled", result, newer.AddressCheckLeaseID); !errors.Is(err, errCDNScheduleChanged) {
		t.Fatal("disabled sweep could write", err)
	}
	if p = f.profile(t); len(p.AddressChecks.V) != 0 || p.AddressCheckNextAt != nil {
		t.Fatal("disabled schedule retained work")
	}
}

func TestCDNScheduledCancellationDoesNotExcludeAddress(t *testing.T) {
	f := scheduleFixture(t)
	entered := make(chan struct{})
	var once sync.Once
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(entered) }); <-r.Context().Done() }))
	defer edge.Close()
	port, _ := strconv.Atoi(strings.Split(edge.URL, ":")[2])
	f.s.st.DB.Model(&store.Endpoint{}).Where("cdn_profile_id = ?", "schedule").Update("port", port)
	f.configure(t, 5)
	p, ok, err := f.s.claimCDNAddressCheck(context.Background(), "schedule", time.Now())
	if err != nil || !ok {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.s.runCDNAddressCheckBatch(ctx, p) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled probe remained blocked")
	}
	p = f.profile(t)
	if len(p.AddressFailures.V) != 0 || p.AddressCheckRuntime.V.Checked != 0 || p.AddressCheckLeaseID != "" {
		t.Fatal("cancellation was persisted as an address failure", p.AddressCheckRuntime.V)
	}
}
