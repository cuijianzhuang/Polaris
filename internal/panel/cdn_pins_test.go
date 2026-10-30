package panel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func TestCDNManualDomains(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	body := map[string]any{"name": "manual", "originDomain": "cdn.example.com", "provider": "cloudflare",
		"preferredIps": []string{"104.16.1.1", "104.16.1.2", "source.example.com"},
		"pinnedHosts":  []string{" Best.Example.com. ", "best.example.com", "second.example.com"}, "expandPerIp": true, "ipTopN": 1}
	code, out := admin.do("POST", "/cdn-profiles", body)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["id"].(string)
	get := func() store.CDNProfile {
		t.Helper()
		var p store.CDNProfile
		if err := s.st.DB.First(&p, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := get()
	if fmt.Sprint(p.PinnedHosts.V) != "[best.example.com second.example.com]" || len(p.PreferredIPs.V) != 5 {
		t.Fatalf("created: %+v", p)
	}
	// Manual domains don't consume the top-N budget, and the TLS/Host identity
	// remains the origin rather than the preferred connection address.
	expanded := s.expandEndpoint(sub.Node{Port: 443}, store.Endpoint{Kind: "cdn", CDNProfileID: id}, map[string]string{}, "{kind}", nil, nil, &model.Inbound{Transport: model.TransportSpec{Type: model.TransWS}, Port: 443}, "")
	if len(expanded) != 3 || expanded[0].Server != "104.16.1.1" || expanded[1].Server != "best.example.com" || expanded[2].Server != "second.example.com" || expanded[1].TLS.ServerName != "cdn.example.com" {
		t.Fatalf("expanded = %+v", expanded)
	}
	_, tok, err := s.issueAPIToken("report", "", []string{ScopeCDNIPs}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	probe := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: tok}
	path := "/cdn-profiles/" + id + "/pinned-hosts"
	if code, _ := probe.do("PUT", path, map[string]any{"pinnedHosts": []string{}}); code != 403 {
		t.Fatalf("probe can edit domains: %d", code)
	}
	// Promoting a domain already in the automatic source must work explicitly.
	code, out = admin.do("PUT", path, map[string]any{"pinnedHosts": []string{"source.example.com", "new.example.com"}})
	if code != 200 {
		t.Fatalf("edit: %d %v", code, out)
	}
	p = get()
	if fmt.Sprint(p.PreferredIPs.V) != "[104.16.1.1 104.16.1.2 source.example.com new.example.com]" || p.IPTopN != 1 || p.OriginDomain != "cdn.example.com" {
		t.Fatalf("edit changed other settings: %+v", p)
	}
	// A report changes automatic results while retaining exactly the new pins.
	code, out = probe.do("POST", "/cdn-profiles/"+id+"/preferred-ips", map[string]any{"source": "fixture", "ips": []map[string]any{{"ip": "104.17.1.1", "latencyMs": 10}}})
	if code != 200 {
		t.Fatalf("report: %d %v", code, out)
	}
	if got := fmt.Sprint(get().PreferredIPs.V); got != "[104.17.1.1 source.example.com new.example.com]" {
		t.Fatalf("after report: %s", got)
	}
	// Clearing is explicit. Missing/null bodies must never accidentally clear pins.
	for _, invalid := range []any{map[string]any{}, map[string]any{"pinnedHosts": nil}, map[string]any{"pinnedHosts": []string{"https://bad.example.com"}}} {
		if code, _ := admin.do("PUT", path, invalid); code != 400 {
			t.Fatalf("invalid %v: %d", invalid, code)
		}
	}
	if len(get().PinnedHosts.V) != 2 {
		t.Fatal("invalid request changed pins")
	}
	if code, out := admin.do("PUT", path, map[string]any{"pinnedHosts": []string{}}); code != 200 {
		t.Fatalf("clear: %d %v", code, out)
	}
	if p = get(); len(p.PinnedHosts.V) != 0 || fmt.Sprint(p.PreferredIPs.V) != "[104.17.1.1]" {
		t.Fatalf("clear = %+v", p)
	}
	if code, _ := admin.do("PUT", "/cdn-profiles/missing/pinned-hosts", map[string]any{"pinnedHosts": []string{}}); code != 404 {
		t.Fatalf("missing: %d", code)
	}
	// The full editor accepts the explicit field, while old mixed-list clients
	// retain their original inference behavior (also covered by legacy tests).
	body["pinnedHosts"] = []string{"saved.example.com"}
	body["preferredIps"] = []string{"104.18.1.1", "source.example.com"}
	if code, out := admin.do("PUT", "/cdn-profiles/"+id, body); code != 200 {
		t.Fatalf("full save: %d %v", code, out)
	}
	if p = get(); fmt.Sprint(p.PinnedHosts.V) != "[saved.example.com]" || fmt.Sprint(p.PreferredIPs.V) != "[104.18.1.1 source.example.com saved.example.com]" {
		t.Fatalf("full save pins: %+v", p)
	}
}

func TestCDNManualDomainsValidation(t *testing.T) {
	for _, host := range []string{"1.1.1.1", "::1", "https://best.example.com", "best.example.com:443", "best.example.com/path", "*.example.com", "localhost", "bad domain.com", strings.Repeat("x", 64) + ".example.com", strings.Repeat("abcd.", 60) + "longexample.com"} {
		if _, err := normalizeCDNPinnedHosts([]string{host}); err == nil {
			t.Errorf("accepted %q", host)
		}
	}
}

func TestCDNManualDomainsDuringURLRefresh(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.SetSetting("allow_private_sources", "true")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; fmt.Fprintln(w, "104.16.2.1") }))
	defer source.Close()
	defer once.Do(func() { close(release) })
	p := store.CDNProfile{ID: "url", Name: "url", OriginDomain: "cdn.example.com", IPSourceURL: source.URL,
		PreferredIPs: store.NewJSON([]string{"old.example.com"}), PinnedHosts: store.NewJSON([]string{"old.example.com"})}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.refreshPreferredIPs(context.Background(), &p); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not start")
	}
	code, out := admin.do("PUT", "/cdn-profiles/url/pinned-hosts", map[string]any{"pinnedHosts": []string{"new.example.com"}})
	// Always release the HTTP handler before reporting a failed assertion.
	once.Do(func() { close(release) })
	if code != 200 {
		t.Fatalf("concurrent edit: %d %v", code, out)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var current store.CDNProfile
	s.st.DB.First(&current, "id = ?", p.ID)
	if fmt.Sprint(current.PreferredIPs.V) != "[104.16.2.1 new.example.com]" {
		t.Fatalf("URL restored stale pins: %v", current.PreferredIPs.V)
	}
}
