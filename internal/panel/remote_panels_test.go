package panel

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestValidateRemotePanelURL(t *testing.T) {
	ok := []string{"https://panel.example.com", "http://10.0.0.5:8080", "https://panel.example.com:8443/sub"}
	for _, u := range ok {
		if err := validateRemotePanelURL(u); err != nil {
			t.Errorf("validateRemotePanelURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{"ftp://panel.example.com", "https://", "panel.example.com", ""}
	for _, u := range bad {
		if err := validateRemotePanelURL(u); err == nil {
			t.Errorf("validateRemotePanelURL(%q) = nil, want error", u)
		}
	}
}

func TestCheckSSRF(t *testing.T) {
	blocked := []string{"127.0.0.1", "::1", "169.254.169.254", "169.254.1.1", "fe80::1"}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if err := checkSSRF(ip); err == nil {
			t.Errorf("checkSSRF(%s) = nil, want blocked", s)
		}
	}
	allowed := []string{"1.1.1.1", "10.0.0.5", "192.168.1.10", "172.16.0.1"}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if err := checkSSRF(ip); err != nil {
			t.Errorf("checkSSRF(%s) = %v, want allowed", s, err)
		}
	}
}

func TestClassifyRemotePanelError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&net.OpError{Err: &timeoutError{}}, "timeout"},
		{errors.New("context deadline exceeded"), "timeout"},
		{errors.New("tls: bad certificate"), "tls"},
		{errors.New("x509: certificate signed by unknown authority"), "tls"},
		{errors.New("HTTP 401: unauthorized"), "unauthorized"},
		{errors.New("HTTP 403: forbidden"), "unauthorized"},
		{errors.New("HTTP 500: boom"), "http_5xx"},
		{errors.New("HTTP 503: unavailable"), "http_5xx"},
		{errors.New("blocked target host: loopback address"), "blocked_target"},
		{errors.New("connection refused"), "network"},
	}
	for _, c := range cases {
		if got := classifyRemotePanelError(c.err); got != c.want {
			t.Errorf("classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestRemotePanelCRUD(t *testing.T) {
	_, _, c := newAdminClient(t)

	code, out := c.do("POST", "/panels", map[string]any{
		"name": "member-1", "baseUrl": "https://panel1.example.com", "token": "tok-1",
	})
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	if out["id"] == "" || out["token"] != "tok-1" {
		t.Fatalf("create should echo id and token once: %v", out)
	}
	id := out["id"].(string)

	code, out = c.do("GET", "/panels", nil)
	if code != 200 {
		t.Fatalf("list %d %v", code, out)
	}
	arr := out["panels"].([]any)
	if len(arr) != 1 || arr[0].(map[string]any)["health"] != "never" {
		t.Fatalf("list should have one never-polled panel: %v", arr)
	}

	code, _ = c.do("PUT", "/panels/"+id, map[string]any{"name": "member-renamed"})
	if code != 200 {
		t.Fatalf("update %d", code)
	}

	code, _ = c.do("DELETE", "/panels/"+id, nil)
	if code != 200 {
		t.Fatalf("delete %d", code)
	}

	code, out = c.do("GET", "/panels", nil)
	if code != 200 || len(out["panels"].([]any)) != 0 {
		t.Fatalf("list after delete should be empty: %v", out)
	}
}

func TestRemotePanelCommunityLimit(t *testing.T) {
	s, _, c := newAdminClient(t)
	// No license is installed, so multi_panel is not granted.
	if s.license().Allow("multi_panel") {
		t.Skip("license grants multi_panel")
	}
	for i := 0; i < maxCommunityPanels; i++ {
		code, out := c.do("POST", "/panels", map[string]any{
			"name":    "member-" + string(rune('a'+i)),
			"baseUrl": "https://panel.example.com",
			"token":   "tok",
		})
		if code != 201 {
			t.Fatalf("panel %d: %d %v", i, code, out)
		}
	}
	code, _ := c.do("POST", "/panels", map[string]any{
		"name": "over-limit", "baseUrl": "https://panel.example.com", "token": "tok",
	})
	if code != 402 {
		t.Fatalf("4th panel should be 402, got %d", code)
	}
}

func TestAggregateRemotePanels(t *testing.T) {
	s, _, c := newAdminClient(t)
	now := time.Now()
	mk := func(name string, snap store.RemoteSnapshot, failures int) {
		p := store.RemotePanel{
			ID:       store.NewID(),
			Name:     name,
			BaseURL:  "https://" + name + ".example.com",
			Enabled:  true,
			Snapshot: store.JSON[store.RemoteSnapshot]{V: snap},
		}
		if failures == 0 {
			p.SnapshotAt = &now
			p.LastOKAt = &now
		} else {
			p.Failures = failures
		}
		if err := s.st.DB.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk("a", store.RemoteSnapshot{Users: 10, OnlineUsers: 2, Nodes: 3, NodesOnline: 2, TrafficUp: 100, TrafficDown: 200}, 0)
	mk("b", store.RemoteSnapshot{Users: 5, OnlineUsers: 1, Nodes: 2, NodesOnline: 1, TrafficUp: 50, TrafficDown: 60}, 0)
	mk("c", store.RemoteSnapshot{}, 3) // failed, must be skipped

	code, out := c.do("GET", "/panels/aggregate", nil)
	if code != 200 {
		t.Fatalf("aggregate %d %v", code, out)
	}
	if out["totalUsers"].(float64) != 15 || out["totalOnline"].(float64) != 3 {
		t.Fatalf("user sums wrong: %v", out)
	}
	if out["totalNodes"].(float64) != 5 || out["totalNodesOnline"].(float64) != 3 {
		t.Fatalf("node sums wrong: %v", out)
	}
	if out["totalTrafficUp"].(float64) != 150 || out["totalTrafficDown"].(float64) != 260 {
		t.Fatalf("traffic sums wrong: %v", out)
	}
	rows := out["panels"].([]any)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	failed := rows[2].(map[string]any)
	if failed["failed"] != true || failed["name"] != "c" {
		t.Fatalf("failed panel should be marked: %v", failed)
	}
}

func TestPanelSelfScope(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	base := srv.URL + s.cfg.APIBase()

	_, secret, err := s.issueAPIToken("hub", "", []string{ScopePanelRead}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	_, bare, err := s.issueAPIToken("no-scope", "", nil, 0, "root")
	if err != nil {
		t.Fatal(err)
	}

	withScope := &apiClient{t: t, base: base, tok: secret}
	code, out := withScope.do("GET", "/panel/self", nil)
	if code != 200 {
		t.Fatalf("panel:read token should read self: %d %v", code, out)
	}
	for _, k := range []string{"version", "users", "onlineUsers", "nodes", "nodesOnline", "trafficUp", "trafficDown"} {
		if _, ok := out[k]; !ok {
			t.Errorf("panel/self missing %q: %v", k, out)
		}
	}

	noScope := &apiClient{t: t, base: base, tok: bare}
	if code, _ := noScope.do("GET", "/panel/self", nil); code != 403 {
		t.Fatalf("token without panel:read should be 403, got %d", code)
	}
}

func TestNormalizeCAPin(t *testing.T) {
	const bare = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	cases := []struct{ in, want string }{
		{bare, bare},
		{"ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789", bare},
		{"SHA256:ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789", bare},
		{"sha256=abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", bare},
		{"ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef01" + "23456789"},
		{"  abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789  ", bare},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeCAPin(c.in); got != c.want {
			t.Errorf("normalizeCAPin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A poll that finishes after the operator edited or deleted the panel must
// only touch the poll-owned columns.
func TestPollStateWriteDoesNotClobberEdit(t *testing.T) {
	s := newTestServer(t)
	p := store.RemotePanel{ID: "rp1", Name: "before", BaseURL: "https://a.example.com",
		Token: s.st.Box.Seal("tok"), Enabled: true}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	// The operator edits while a slow fetch is in flight.
	if err := s.st.DB.Model(&store.RemotePanel{}).Where("id = ?", "rp1").
		Updates(map[string]any{"name": "after", "base_url": "https://b.example.com"}).Error; err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	s.saveRemotePanelState("rp1", map[string]any{
		"snapshot":    store.JSON[store.RemoteSnapshot]{V: store.RemoteSnapshot{Users: 7}},
		"snapshot_at": &now,
		"last_ok_at":  &now,
		"failures":    0,
		"next_try_at": nil,
	})

	var got store.RemotePanel
	if err := s.st.DB.First(&got, "id = ?", "rp1").Error; err != nil {
		t.Fatal(err)
	}
	if got.Name != "after" || got.BaseURL != "https://b.example.com" {
		t.Fatalf("poll write clobbered the edit: name=%q url=%q", got.Name, got.BaseURL)
	}
	if got.Snapshot.V.Users != 7 || got.Failures != 0 {
		t.Fatalf("snapshot not stored: %+v", got.Snapshot.V)
	}
}

// A poll write for a panel the operator deleted must not re-insert it.
func TestPollStateWriteDoesNotResurrectDeleted(t *testing.T) {
	s := newTestServer(t)
	p := store.RemotePanel{ID: "rp2", Name: "gone", BaseURL: "https://c.example.com",
		Token: s.st.Box.Seal("tok"), Enabled: true}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Delete(&store.RemotePanel{}, "id = ?", "rp2").Error; err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	s.saveRemotePanelState("rp2", map[string]any{"snapshot_at": &now, "failures": 0})

	var n int64
	s.st.DB.Model(&store.RemotePanel{}).Where("id = ?", "rp2").Count(&n)
	if n != 0 {
		t.Fatalf("poll write resurrected a deleted panel (rows=%d)", n)
	}
}

// A panel that failed its latest poll keeps contributing its last snapshot,
// flagged, so the dashboard totals do not shrink on a transient outage.
func TestAggregateKeepsLastSnapshotOnFailure(t *testing.T) {
	s, _, c := newAdminClient(t)
	now := time.Now()
	mk := func(id, name string, users int, failures int) {
		p := store.RemotePanel{ID: id, Name: name, BaseURL: "https://" + id + ".example.com", Enabled: true}
		if users > 0 {
			p.Snapshot = store.JSON[store.RemoteSnapshot]{V: store.RemoteSnapshot{Users: users, Nodes: 1, NodesOnline: 1}}
			p.SnapshotAt = &now
			p.LastOKAt = &now
		}
		p.Failures = failures
		if err := s.st.DB.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk("ok", "healthy", 10, 0)
	mk("down", "down-but-cached", 5, 3) // failed poll, still has a snapshot
	mk("never", "never-seen", 0, 0)     // no snapshot at all

	code, out := c.do("GET", "/panels/aggregate", nil)
	if code != 200 {
		t.Fatalf("aggregate %d %v", code, out)
	}
	if got := out["totalUsers"].(float64); got != 15 {
		t.Fatalf("totalUsers = %v, want 15 (failed panel keeps its snapshot)", got)
	}
	rows := out["panels"].([]any)
	var down map[string]any
	for _, r := range rows {
		m := r.(map[string]any)
		if m["name"] == "down-but-cached" {
			down = m
		}
	}
	if down == nil {
		t.Fatal("failed panel missing from rows")
	}
	if down["failed"] != true || down["users"].(float64) != 5 {
		t.Fatalf("failed panel should be flagged but keep its numbers: %v", down)
	}
}

// TestProbeRemotePanelUsesVersionedPath is a regression test for the probe
// returning 502 {"error":"schema"}: the client requested /api/panel/self,
// but the member serves the snapshot at /api/v1/panel/self (APIBase). The
// unversioned path fell through to the decoy/SPA and came back as HTML,
// which failed to decode. The mock only answers the versioned path.
func TestProbeRemotePanelUsesVersionedPath(t *testing.T) {
	s, _, c := newAdminClient(t)

	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/panel/self" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v0.1.262","capabilities":["v1"],"users":3,"onlineUsers":2,"nodes":1,"nodesOnline":1,"trafficUp":100,"trafficDown":200}`))
	}))
	defer member.Close()

	// The fake member listens on loopback, which the SSRF dialer blocks by
	// design; use a plain transport in the test.
	s.remotePanelTransport = http.DefaultTransport

	p := store.RemotePanel{ID: "rp1", Name: "m", BaseURL: member.URL, Token: s.st.Box.Seal("tok")}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	code, out := c.do("POST", "/panels/rp1/probe", nil)
	if code != 200 {
		t.Fatalf("probe %d %v", code, out)
	}
	if out["users"].(float64) != 3 || out["version"] != "v0.1.262" {
		t.Fatalf("probe returned wrong snapshot: %v", out)
	}
}
