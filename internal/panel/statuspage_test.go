package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func getRaw(t *testing.T, url string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, body
}

type svcSnapshotJSON struct {
	Title   string `json:"title"`
	Note    string `json:"note"`
	At      string `json:"at"`
	Stale   bool   `json:"stale"`
	Summary struct {
		Total  int    `json:"total"`
		Online int    `json:"online"`
		State  string `json:"state"`
	} `json:"summary"`
	Nodes []struct {
		Name    string `json:"name"`
		Region  string `json:"region"`
		Flag    string `json:"flag"`
		State   string `json:"state"`
		Latency []struct {
			Name    string   `json:"name"`
			Carrier string   `json:"carrier"`
			Ms      *float64 `json:"ms"`
			Loss    float64  `json:"loss"`
		} `json:"latency"`
	} `json:"nodes"`
}

func keysOf(m map[string]any) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func assertKeysSubset(t *testing.T, what string, m map[string]any, allowed ...string) {
	t.Helper()
	ok := keysOf(m)
	for k := range ok {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: unexpected key %q in service snapshot", what, k)
		}
	}
}

// The service snapshot is a whitelist projection: only the fields listed in
// the design may appear, and no internal value (node id, address, version,
// probe target address, user data) may leak through any field.
func TestStatusServiceWhitelistAndScope(t *testing.T) {
	s, srv, client := newAdminClient(t)
	if err := s.st.SetSetting("status_page", "true"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	target := store.ProbeTarget{ID: "t-secret", Name: "上海电信", Carrier: "ct", Addr: "secret-target.example:80", Enabled: true}
	if err := s.st.DB.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	nodes := []store.Node{
		{ID: "n-admin", Name: "internal-name-1", PublicName: "东京 01", Region: "东京", Flag: "JP", Status: "online",
			LastSeen: &now, Address: "203.0.113.99", PublicIPv4: "203.0.113.99", AgentVersion: "v9.9.9-test", DisplayName: "display-sentinel"},
		{ID: "n-plain", Name: "plain-internal", Region: "大阪", Flag: "JP", Status: "offline"},
		{ID: "n-hidden", Name: "hidden-node", Status: "online", LastSeen: &now, StatusHidden: true},
		{ID: "n-revoked", Name: "revoked-node", Status: "online", LastSeen: &now, Revoked: true},
		{ID: "n-user", Name: "un-u1", DisplayName: "用户节点名", Status: "online", LastSeen: &now, OwnerID: "u1", ReviewState: "approved"},
	}
	for i := range nodes {
		if err := s.st.DB.Create(&nodes[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	lat := store.NodeLatency{NodeID: "n-admin", TargetID: "t-secret", At: now.UTC().Truncate(latencySlot),
		Sent: 100, Lost: 10, SumMs: 4500, MinMs: 40, MaxMs: 60, LastMs: 42, LastAt: now}
	if err := s.st.DB.Create(&lat).Error; err != nil {
		t.Fatal(err)
	}

	code, header, body := getRaw(t, srv.URL+"/status/service")
	if code != http.StatusOK {
		t.Fatalf("service snapshot: %d %s", code, body)
	}
	if cc := header.Get("Cache-Control"); cc != "public, max-age=15" {
		t.Errorf("Cache-Control = %q, want public, max-age=15", cc)
	}
	var snap svcSnapshotJSON
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (admin nodes only): %s", len(snap.Nodes), body)
	}
	first := snap.Nodes[0]
	if first.Name != "东京 01" || first.State != "online" || first.Region != "东京" {
		t.Errorf("first node = %+v, want public alias, online, 东京", first)
	}
	if len(first.Latency) != 1 || first.Latency[0].Name != "上海电信" || first.Latency[0].Carrier != "ct" ||
		first.Latency[0].Ms == nil || *first.Latency[0].Ms != 42 || first.Latency[0].Loss != 10 {
		t.Errorf("latency = %+v, want 上海电信/ct/42ms/10%%", first.Latency)
	}
	second := snap.Nodes[1]
	if second.Name != "plain-internal" || second.State != "offline" {
		t.Errorf("second node = %+v, want internal name fallback, offline", second)
	}
	if snap.Summary.Total != 2 || snap.Summary.Online != 1 || snap.Summary.State != "major" {
		t.Errorf("summary = %+v, want total 2, online 1, major (1/2 bad > 1/3)", snap.Summary)
	}
	if snap.Stale {
		t.Error("fresh snapshot marked stale")
	}

	// Whitelist: exact key sets at every level.
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatal(err)
	}
	assertKeysSubset(t, "top", top, "title", "note", "at", "stale", "summary", "nodes")
	var rawNodes []map[string]any
	raw, _ := json.Marshal(top["nodes"])
	if err := json.Unmarshal(raw, &rawNodes); err != nil {
		t.Fatal(err)
	}
	for _, rn := range rawNodes {
		assertKeysSubset(t, "node", rn, "name", "region", "flag", "state", "latency")
		var rawLat []map[string]any
		lb, _ := json.Marshal(rn["latency"])
		if err := json.Unmarshal(lb, &rawLat); err != nil {
			t.Fatal(err)
		}
		for _, rl := range rawLat {
			assertKeysSubset(t, "latency", rl, "name", "carrier", "ms", "loss")
		}
	}
	// Forbidden values must not appear anywhere in the payload.
	for _, bad := range []string{
		"n-admin", "n-plain", "internal-name-1", "203.0.113.99", "v9.9.9-test",
		"secret-target.example", "display-sentinel", "hidden-node", "revoked-node",
		"un-u1", "用户节点名", "ownerId", "agentVersion", "publicIpv4", `"id":`, "cpu", "price",
	} {
		if strings.Contains(string(body), bad) {
			t.Errorf("service snapshot leaks %q: %s", bad, body)
		}
	}

	// The alias is editable through the node API and applies at once.
	if code, out := client.do("PUT", "/nodes/n-plain", map[string]any{"name": "plain-internal", "publicName": "公开名二号"}); code != http.StatusOK {
		t.Fatalf("update node publicName: %d %v", code, out)
	}
	_, _, body = getRaw(t, srv.URL+"/status/service")
	if !strings.Contains(string(body), "公开名二号") || strings.Contains(string(body), "plain-internal") {
		t.Errorf("alias update not reflected: %s", body)
	}
}

// Gate behaviour: page off -> everything decoys; service-only -> the
// monitor data decoys while the page and the service snapshot stay public.
func TestStatusServiceGateAndServiceOnly(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	if code, _, _ := getRaw(t, srv.URL+"/status/service"); code != http.StatusNotFound {
		t.Fatalf("page off: /status/service = %d, want decoy 404", code)
	}
	if err := s.st.SetSetting("status_page", "true"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := getRaw(t, srv.URL+"/status/service"); code != http.StatusOK {
		t.Fatalf("page on: /status/service = %d, want 200", code)
	}
	if code, _, _ := getRaw(t, srv.URL+"/status/data"); code != http.StatusOK {
		t.Fatalf("page on: /status/data = %d, want 200", code)
	}
	_, _, html := getRaw(t, srv.URL+"/status")
	for _, want := range []string{`id="view-service"`, `id="view-monitor"`, `id="tab-monitor"`, "SVC_ONLY=false"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("dual-view page missing %q", want)
		}
	}
	if err := s.st.SetSetting("status_service_only", "true"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := getRaw(t, srv.URL+"/status/data"); code != http.StatusNotFound {
		t.Fatalf("service-only: /status/data = %d, want decoy 404", code)
	}
	if code, _, _ := getRaw(t, srv.URL+"/status/service"); code != http.StatusOK {
		t.Fatalf("service-only: /status/service = %d, want 200", code)
	}
	_, _, html = getRaw(t, srv.URL+"/status")
	for _, banned := range []string{`id="view-monitor"`, `id="tab-monitor"`, "var DATA="} {
		if strings.Contains(string(html), banned) {
			t.Errorf("service-only page still carries monitor markup %q", banned)
		}
	}
	if !strings.Contains(string(html), "SVC_ONLY=true") {
		t.Error("service-only page missing SVC_ONLY=true")
	}
}

// Cache: repeated reads share one build; after expiry a failed rebuild
// answers with the previous snapshot marked stale instead of an error.
func TestStatusServiceCacheAndStale(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	if err := s.st.DB.Create(&store.Node{ID: "n1", Name: "one", Status: "online", LastSeen: &now}).Error; err != nil {
		t.Fatal(err)
	}
	body, status := s.serviceSnapshot()
	if status != http.StatusOK {
		t.Fatalf("first build: %d", status)
	}
	again, status := s.serviceSnapshot()
	if status != http.StatusOK || string(again) != string(body) {
		t.Fatal("second read should serve the cached build verbatim")
	}
	sn := s.svcCache.Load()
	if sn == nil {
		t.Fatal("cache not populated")
	}
	s.svcCache.Store(&svcSnap{at: time.Now().Add(-serviceCacheTTL - time.Second), body: sn.body, staleBody: sn.staleBody})
	sqlDB, err := s.st.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	stale, status := s.serviceSnapshot()
	if status != http.StatusOK {
		t.Fatalf("failed rebuild with a cached snapshot: %d, want stale 200", status)
	}
	var snap svcSnapshotJSON
	if err := json.Unmarshal(stale, &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.Stale {
		t.Error("fallback snapshot not marked stale")
	}
	if len(snap.Nodes) != 1 || snap.Nodes[0].Name != "one" {
		t.Errorf("fallback snapshot lost its nodes: %s", stale)
	}
}

// The endpoint is rate limited per IP (design: 120/min in production).
func TestStatusServiceRateLimit(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	if err := s.st.SetSetting("status_page", "true"); err != nil {
		t.Fatal(err)
	}
	// The shared limiter is a token bucket with a fixed burst of 10; a
	// near-zero refill makes the 11th rapid request the first refusal.
	s.statusLimit = &sharedLimiter{s: s.shared, name: "status", perMinute: 1}
	for i := 0; i < 10; i++ {
		if code, _, _ := getRaw(t, srv.URL+"/status/service"); code != http.StatusOK {
			t.Fatalf("request %d: %d, want 200", i+1, code)
		}
	}
	if code, _, _ := getRaw(t, srv.URL+"/status/service"); code != http.StatusTooManyRequests {
		t.Fatalf("request 11: %d, want 429", code)
	}
}
