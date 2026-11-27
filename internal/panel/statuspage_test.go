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
		Uptime7d   *float64       `json:"uptime7d"`
		Uptime30d  *float64       `json:"uptime30d"`
		UptimeDays []*float64     `json:"uptimeDays"`
		Carriers   *svcCarriersIn `json:"carriers"`
	} `json:"nodes"`
}

type svcCarrierIn struct {
	Ms   *float64 `json:"ms"`
	Loss *float64 `json:"loss"`
	Line *string  `json:"line"`
}

type svcCarriersIn struct {
	CT svcCarrierIn `json:"ct"`
	CU svcCarrierIn `json:"cu"`
	CM svcCarrierIn `json:"cm"`
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
		assertKeysSubset(t, "node", rn, "name", "region", "flag", "state", "latency", "uptime7d", "uptime30d", "uptimeDays", "carriers")
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

// Availability (design D6): NodeMetric slot presence over rolling windows.
// Missing slots count as unavailable (a panel-downtime gap is
// indistinguishable from node downtime and is counted, per the footnote
// the page carries); too little history yields null, never a made-up
// number.
func TestStatusServiceUptime(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	slotNow := now.UTC().Truncate(metricSlot)
	today := time.Date(slotNow.Year(), slotNow.Month(), slotNow.Day(), 0, 0, 0, 0, time.UTC)
	old := slotNow.AddDate(0, 0, -40)

	mkNode := func(id string, created time.Time) {
		t.Helper()
		if err := s.st.DB.Create(&store.Node{ID: id, Name: id, Status: "offline", CreatedAt: created}).Error; err != nil {
			t.Fatal(err)
		}
	}
	seed := func(id string, slots []time.Time) {
		t.Helper()
		rows := make([]store.NodeMetric, len(slots))
		for i, at := range slots {
			rows[i] = store.NodeMetric{NodeID: id, At: at, CPU: 1, N: 1}
		}
		if err := s.st.DB.CreateInBatches(&rows, 1000).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Node A: full 30 days of heartbeats except a 1-hour gap three days
	// ago and the second half of the day ten days ago.
	mkNode("up-a", old)
	gapDay := today.AddDate(0, 0, -3)
	halfDay := today.AddDate(0, 0, -10)
	var slotsA []time.Time
	for at := slotNow.AddDate(0, 0, -30); at.Before(slotNow); at = at.Add(metricSlot) {
		if !at.Before(gapDay) && at.Before(gapDay.Add(time.Hour)) {
			continue
		}
		if !at.Before(halfDay.Add(12*time.Hour)) && at.Before(halfDay.AddDate(0, 0, 1)) {
			continue
		}
		slotsA = append(slotsA, at)
	}
	seed("up-a", slotsA)

	// Node B: created 12 hours ago, heartbeating ever since.
	createdB := slotNow.Add(-12 * time.Hour)
	mkNode("up-b", createdB)
	var slotsB []time.Time
	for at := createdB; at.Before(slotNow); at = at.Add(metricSlot) {
		slotsB = append(slotsB, at)
	}
	seed("up-b", slotsB)

	// Node C: old, but never a single heartbeat.
	mkNode("up-c", old)

	data, err := s.buildServiceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]serviceNode{}
	for _, n := range data.Nodes {
		byName[n.Name] = n
	}
	a := byName["up-a"]
	if a.Uptime7d == nil || *a.Uptime7d != 99.4 {
		t.Errorf("up-a uptime7d = %v, want 99.4 (2004/2016 slots)", a.Uptime7d)
	}
	if a.Uptime30d == nil || *a.Uptime30d != 98.2 {
		t.Errorf("up-a uptime30d = %v, want 98.2 (8484/8640 slots)", a.Uptime30d)
	}
	if len(a.UptimeDays) != 30 {
		t.Fatalf("up-a uptimeDays len = %d, want 30", len(a.UptimeDays))
	}
	if a.UptimeDays[26] == nil || *a.UptimeDays[26] != 95.8 {
		t.Errorf("up-a day -3 = %v, want 95.8 (276/288, the 1h gap counts as down)", a.UptimeDays[26])
	}
	if a.UptimeDays[19] == nil || *a.UptimeDays[19] != 50.0 {
		t.Errorf("up-a day -10 = %v, want 50.0 (144/288)", a.UptimeDays[19])
	}
	if a.UptimeDays[0] == nil || *a.UptimeDays[0] != 100.0 {
		t.Errorf("up-a day -29 = %v, want 100.0", a.UptimeDays[0])
	}

	b := byName["up-b"]
	if b.Uptime7d != nil || b.Uptime30d != nil {
		t.Errorf("up-b uptimes = %v / %v, want null/null (node younger than 24h)", b.Uptime7d, b.Uptime30d)
	}
	nonNil := 0
	for _, d := range b.UptimeDays {
		if d != nil {
			nonNil++
			if *d != 100.0 {
				t.Errorf("up-b day = %v, want 100.0 where a figure exists", *d)
			}
		}
	}
	if nonNil == 0 {
		t.Error("up-b: expected at least one day bar with a figure (12h of slots)")
	}

	c := byName["up-c"]
	if c.Uptime7d == nil || *c.Uptime7d != 0 || c.Uptime30d == nil || *c.Uptime30d != 0 {
		t.Errorf("up-c uptimes = %v / %v, want 0/0 (full history, no heartbeats)", c.Uptime7d, c.Uptime30d)
	}
	if c.UptimeDays[0] == nil || *c.UptimeDays[0] != 0 {
		t.Errorf("up-c day -29 = %v, want 0", c.UptimeDays[0])
	}
}

// Carriers passthrough (design D7, shape fixed by backhaul D10): the
// field is absent while probe_public_carriers is off, and appears with
// the shared projection when on — aggregates and labels only, never a
// target address.
func TestStatusServiceCarriersSwitch(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	if err := s.st.SetSetting("status_page", "true"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	target := store.ProbeTarget{ID: "t-ct", Name: "上海电信", Carrier: "ct", Addr: "secret-target.example:80", Enabled: true}
	if err := s.st.DB.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	for _, n := range []store.Node{
		{ID: "n-admin", Name: "admin-node", Status: "online", LastSeen: &now},
		{ID: "n-plain", Name: "plain-node", Status: "offline"},
	} {
		n := n
		if err := s.st.DB.Create(&n).Error; err != nil {
			t.Fatal(err)
		}
	}
	lat := store.NodeLatency{NodeID: "n-admin", TargetID: "t-ct", At: now.UTC().Truncate(latencySlot),
		Sent: 100, Lost: 10, SumMs: 4500, MinMs: 40, MaxMs: 60, LastMs: 42, LastAt: now}
	if err := s.st.DB.Create(&lat).Error; err != nil {
		t.Fatal(err)
	}

	_, _, body := getRaw(t, srv.URL+"/status/service")
	if strings.Contains(string(body), `"carriers"`) {
		t.Fatalf("carriers present while probe_public_carriers is off: %s", body)
	}

	if err := s.st.SetSetting("probe_public_carriers", "true"); err != nil {
		t.Fatal(err)
	}
	s.svcCache.Store(nil) // settings writes via API clear it; SetSetting does not
	_, _, body = getRaw(t, srv.URL+"/status/service")
	if strings.Contains(string(body), "secret-target.example") {
		t.Fatalf("carriers leak the probe target address: %s", body)
	}
	var snap svcSnapshotJSON
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(snap.Nodes))
	}
	car := snap.Nodes[0].Carriers
	if car == nil {
		t.Fatalf("carriers missing after switch on: %s", body)
	}
	if car.CT.Ms == nil || *car.CT.Ms != 42 || car.CT.Loss == nil || *car.CT.Loss != 10 || car.CT.Line != nil {
		t.Errorf("ct = %+v, want ms 42, loss 10, line null (classification not landed yet)", car.CT)
	}
	if car.CU.Ms != nil || car.CU.Loss != nil || car.CM.Ms != nil || car.CM.Loss != nil {
		t.Errorf("cu/cm = %+v / %+v, want all-null (fixed shape, no data)", car.CU, car.CM)
	}
	if snap.Nodes[1].Carriers != nil {
		t.Errorf("plain node carriers = %+v, want nil (no samples at all)", snap.Nodes[1].Carriers)
	}

	// Whitelist discipline extends to the new object: exact key sets.
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatal(err)
	}
	rawNodes, _ := json.Marshal(top["nodes"])
	var rawList []map[string]any
	if err := json.Unmarshal(rawNodes, &rawList); err != nil {
		t.Fatal(err)
	}
	rawCar, _ := json.Marshal(rawList[0]["carriers"])
	var carMap map[string]any
	if err := json.Unmarshal(rawCar, &carMap); err != nil {
		t.Fatal(err)
	}
	assertKeysSubset(t, "carriers", carMap, "ct", "cu", "cm")
	for k, v := range carMap {
		vm, _ := v.(map[string]any)
		assertKeysSubset(t, "carrier "+k, vm, "ms", "loss", "line")
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
