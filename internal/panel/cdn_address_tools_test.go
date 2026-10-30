package panel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestCDNAddressToolsPreviewAndCheck(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "localhost" || r.URL.Path != "/ws" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Sec-WebSocket-Accept", cdnWebSocketAccept(r.Header.Get("Sec-WebSocket-Key")))
		w.WriteHeader(101)
	}))
	defer edge.Close()
	_, port, _ := net.SplitHostPort(edge.Listener.Addr().String())
	pn, _ := strconv.Atoi(port)
	p := store.CDNProfile{ID: "tools", Name: "tools", Scheme: "http", Provider: "other", OriginDomain: "origin.example.com", Ports: store.NewJSON([]int{pn}), PreferredIPs: store.NewJSON([]string{"127.0.0.1", "104.16.1.1", "keep.example.com"}), PinnedHosts: store.NewJSON([]string{"keep.example.com"}), ExpandPerIP: true, IPTopN: 1}
	for _, v := range []any{&p, &store.Node{ID: "tools-node", Name: "tools-node"}, &store.Inbound{ID: "tools-in", NodeID: "tools-node", Tag: "ws", Enabled: true, Port: pn, Core: model.CoreXray, Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS, Path: "/ws"}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecNone})}, &store.Endpoint{ID: "tools-ep", InboundID: "tools-in", Kind: "cdn", CDNProfileID: p.ID, Host: "localhost", Port: pn}} {
		if err := s.st.DB.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	get := func() map[string]any {
		t.Helper()
		code, out := admin.do("GET", "/cdn-profiles/tools/address-tools", nil)
		if code != 200 {
			t.Fatalf("tools %d %v", code, out)
		}
		return out
	}
	out := get()
	if got := fmt.Sprint(out["selected"]); got != "[127.0.0.1 keep.example.com]" {
		t.Fatal("preview/top-N", got)
	}
	code, res := admin.do("POST", "/cdn-profiles/tools/check-address", map[string]any{"endpointId": "tools-ep", "address": "127.0.0.1"})
	if code != 200 || res["ok"] != true || res["businessVerified"] != false || res["viewpoint"] != "panel" {
		t.Fatalf("check %d %v", code, res)
	}
	if len(res["stages"].([]any)) != 6 {
		t.Fatal("missing structured phases", res)
	}
	rows := get()["items"].([]any)
	if rows[0].(map[string]any)["check"] == nil {
		t.Fatal("check not persisted")
	}
	// A different path cannot reuse an old green check.
	s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "tools-ep").Update("path", "/changed")
	rows = get()["items"].([]any)
	if rows[0].(map[string]any)["check"] != nil {
		t.Fatal("changed target reused a check")
	}
	for _, body := range []map[string]any{{"endpointId": "tools-ep", "address": "unrelated.example.com"}, {"endpointId": "missing", "address": "127.0.0.1"}} {
		if code, _ := admin.do("POST", "/cdn-profiles/tools/check-address", body); code < 400 {
			t.Fatal("unchecked target admitted", body)
		}
	}
	// A blacklist cannot be bypassed by pins, all-bad fallback, or source refresh.
	code, res = admin.do("PUT", "/cdn-profiles/tools/address-policy", map[string]any{"blockedHosts": []string{"127.0.0.1", "KEEP.EXAMPLE.COM."}, "stalePolicy": "keep"})
	if code != 200 {
		t.Fatal(code, res)
	}
	if got := fmt.Sprint(get()["selected"]); got != "[104.16.1.1]" {
		t.Fatal("blocked addresses selected", got)
	}
	var current store.CDNProfile
	s.st.DB.First(&current, "id = ?", p.ID)
	current.NodeCheck = true
	current.BadIPs = store.NewJSON([]string{"104.16.1.1"})
	if got := usableCDNIPsAt(&current, []string{"127.0.0.1", "keep.example.com"}, pn); len(got) != 0 {
		t.Fatal("fail-open bypassed blacklist", got)
	}
	if code, _ := admin.do("PUT", "/cdn-profiles/tools/address-policy", map[string]any{"stalePolicy": "keep"}); code != 400 {
		t.Fatal("missing blacklist accidentally cleared policy", code)
	}
	_, token, err := s.issueAPIToken("report", "", []string{ScopeCDNIPs}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	probe := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: token}
	for _, path := range []string{"/cdn-profiles/tools/address-tools", "/cdn-profiles/tools/check-address", "/cdn-profiles/tools/address-policy"} {
		method := "GET"
		if strings.HasSuffix(path, "check-address") {
			method = "POST"
		}
		if strings.HasSuffix(path, "address-policy") {
			method = "PUT"
		}
		if code, _ := probe.do(method, path, map[string]any{}); code != 403 {
			t.Fatal("report token accessed diagnostics/policy", path, code)
		}
	}
}
func TestCDNStructuredPhases(t *testing.T) {
	stage := func(r cdnCheck, kind string) cdnStage {
		for _, s := range r.Stages {
			if s.Kind == kind {
				return s
			}
		}
		t.Fatal("missing phase", kind)
		return cdnStage{}
	}
	target := cdnTarget{Scheme: "https", Provider: "other", Server: "127.0.0.1", Port: 1, SNI: "127.0.0.1", Host: "localhost", Path: "/ws"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r := probeCDN(ctx, target, model.TransWS, "")
	if stage(r, "tcp").State != "failed" || stage(r, "tls").State != "not_tested" {
		t.Fatal("TCP refusal confused with TLS", r)
	}
	target.Server = "missing-cdn-address.invalid"
	r = probeCDN(ctx, target, model.TransWS, "")
	if stage(r, "address_dns").State != "failed" || stage(r, "tcp").State != "not_tested" {
		t.Fatal("DNS failure not isolated", r)
	}
	edge := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer edge.Close()
	_, port, _ := net.SplitHostPort(edge.Listener.Addr().String())
	target.Port, _ = strconv.Atoi(port)
	target.Server = "127.0.0.1"
	r = probeCDN(context.Background(), target, model.TransWS, "")
	if stage(r, "tcp").State != "passed" || stage(r, "tls").State != "failed" || stage(r, "http").State != "not_tested" {
		t.Fatal("untrusted TLS passed", r)
	}
}
func TestCDNStalePoliciesAndFreshReportRace(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.SetSetting("allow_private_sources", "true")
	var mu sync.Mutex
	calls := 0
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		fmt.Fprintln(w, "104.18.1.1")
	}))
	defer source.Close()
	for _, policy := range []string{"source", "keep"} {
		p := store.CDNProfile{ID: policy, Name: policy, Provider: "other", Scheme: "https", Ports: store.NewJSON([]int{443}), IPSourceURL: source.URL, StalePolicy: policy, PreferredIPs: store.NewJSON([]string{"104.16.1.1"}), PinnedHosts: store.NewJSON([]string{"keep.example.com"})}
		if err := s.st.DB.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
		s.st.DB.Create(&store.CDNIPReport{ProfileID: p.ID, Source: "old", Port: 443, Scheme: "https", At: time.Now().Add(-48 * time.Hour), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.1.1"}})})
	}
	s.refreshAllPreferredIPs()
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", "source")
	if !slices.Contains(p.PreferredIPs.V, "104.18.1.1") || !slices.Contains(p.PreferredIPs.V, "keep.example.com") {
		t.Fatal("fallback missing/pins lost", p.PreferredIPs.V)
	}
	p = store.CDNProfile{}
	s.st.DB.First(&p, "id = ?", "keep")
	if fmt.Sprint(p.PreferredIPs.V) != "[104.16.1.1]" {
		t.Fatal("keep policy overwritten", p.PreferredIPs.V)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatal("keep policy fetched fallback", n)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; fmt.Fprintln(w, "104.19.1.1") }))
	defer blocking.Close()
	defer once.Do(func() { close(release) })
	s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "source").Update("ip_source_url", blocking.URL)
	p = store.CDNProfile{}
	s.st.DB.First(&p, "id = ?", "source")
	done := make(chan error, 1)
	go func() { _, e := s.refreshPreferredIPs(context.Background(), &p, true); done <- e }()
	<-started
	code, out := admin.do("POST", "/cdn-profiles/source/preferred-ips", map[string]any{"source": "fresh", "port": 443, "scheme": "https", "ips": []map[string]any{{"ip": "104.17.1.1"}}})
	once.Do(func() { close(release) })
	if code != 200 {
		t.Fatal(code, out)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	p = store.CDNProfile{}
	s.st.DB.First(&p, "id = ?", "source")
	if !slices.Contains(p.PreferredIPs.V, "104.17.1.1") || slices.Contains(p.PreferredIPs.V, "104.19.1.1") {
		t.Fatal("old URL response overwrote fresh measurement", p.PreferredIPs.V)
	}
}

func TestCDNKeepExpiredPortCarrierResults(t *testing.T) {
	s := newTestServer(t)
	p := store.CDNProfile{ID: "keep-port", Provider: "other", Scheme: "https", Ports: store.NewJSON([]int{443, 8443}), StalePolicy: "keep", CarrierSplit: true, PreferredIPs: store.NewJSON([]string{"104.16.1.1"}), PinnedHosts: store.NewJSON([]string{"keep.example.com"})}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	for _, r := range []store.CDNIPReport{
		{ProfileID: p.ID, Source: "expired-telecom", Scheme: "https", Port: 8443, Carrier: "ct", At: time.Now().Add(-48 * time.Hour), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.2.1"}})},
		{ProfileID: p.ID, Source: "expired-mobile", Scheme: "https", Port: 8443, Carrier: "cm", At: time.Now().Add(-48 * time.Hour), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.3.1"}})},
	} {
		if err := s.st.DB.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	if got := s.selectedCDNIPs(&p, 8443, "ct"); fmt.Sprint(got) != "[104.16.2.1 keep.example.com]" {
		t.Fatal("keep lost port/carrier list", got)
	}
	p.StalePolicy = "source"
	if got := s.selectedCDNIPs(&p, 8443, "ct"); fmt.Sprint(got) != "[104.16.1.1]" {
		t.Fatal("source policy did not use retained fallback", got)
	}
}

func TestCDNStaleFallbackFailureRetainsList(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.SetSetting("allow_private_sources", "true")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer source.Close()
	p := store.CDNProfile{ID: "failed-source", Name: "failed-source", Provider: "other", Scheme: "https", Ports: store.NewJSON([]int{443}), IPSourceURL: source.URL, PreferredIPs: store.NewJSON([]string{"104.16.1.1", "keep.example.com"}), PinnedHosts: store.NewJSON([]string{"keep.example.com"})}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.CDNIPReport{ProfileID: p.ID, Source: "old", Scheme: "https", Port: 443, At: time.Now().Add(-48 * time.Hour), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.1.1"}})}).Error; err != nil {
		t.Fatal(err)
	}
	s.checkCDNReportsStale()
	var current store.CDNProfile
	s.st.DB.First(&current, "id = ?", p.ID)
	if fmt.Sprint(current.PreferredIPs.V) != "[104.16.1.1 keep.example.com]" || current.LastError == "" || !current.StaleAlerted {
		t.Fatal("fallback failure lost list or error", current)
	}
	code, out := admin.do("POST", "/cdn-profiles/"+p.ID+"/preferred-ips", map[string]any{"source": "old", "scheme": "https", "port": 443, "ips": []map[string]any{{"ip": "104.17.1.1"}}})
	if code != 200 {
		t.Fatal(code, out)
	}
	current = store.CDNProfile{}
	s.st.DB.First(&current, "id = ?", p.ID)
	if current.StaleAlerted || current.LastError != "" || !slices.Contains(current.PreferredIPs.V, "104.17.1.1") || !slices.Contains(current.PreferredIPs.V, "keep.example.com") {
		t.Fatal("fresh report did not recover", current)
	}
}

func TestCDNFailedAutomaticSourceDoesNotRestoreRecoveredError(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.SetSetting("allow_private_sources", "true")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; w.WriteHeader(503) }))
	defer source.Close()
	defer once.Do(func() { close(release) })
	p := store.CDNProfile{ID: "error-race", Name: "error-race", Provider: "other", Scheme: "https", Ports: store.NewJSON([]int{443}), IPSourceURL: source.URL}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.refreshAllPreferredIPs(); close(done) }()
	<-started
	code, out := admin.do("POST", "/cdn-profiles/"+p.ID+"/preferred-ips", map[string]any{"source": "fresh", "scheme": "https", "port": 443, "ips": []map[string]any{{"ip": "104.17.1.1"}}})
	once.Do(func() { close(release) })
	<-done
	if code != 200 {
		t.Fatal(code, out)
	}
	var current store.CDNProfile
	s.st.DB.First(&current, "id = ?", p.ID)
	if current.LastError != "" || fmt.Sprint(current.PreferredIPs.V) != "[104.17.1.1]" {
		t.Fatal("old failure overwrote recovered state", current)
	}
}
