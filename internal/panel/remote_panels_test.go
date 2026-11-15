package panel

import (
	"errors"
	"net"
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
