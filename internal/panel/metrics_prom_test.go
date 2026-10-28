package panel

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func scrape(t *testing.T, url, tok string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

// GET /metrics answers in the Prometheus text format with node, user, order,
// ticket, certificate, license and CDN series; a metrics:read token can read
// it and nothing else, and no series names a user.
func TestPrometheusMetrics(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	db := s.st.DB
	now := time.Now()
	db.Create(&store.Node{ID: "n1", Name: `tokyo "1"`, Status: "online", LastSeen: &now, NetRx: 1000, NetTx: 2000,
		System: store.NewJSON(store.SystemInfo{CPU: 12.5, Mem: 40, RxBps: 300, TxBps: 400, Load1: 0.5})})
	db.Create(&store.Node{ID: "n2", Name: "hk", Status: "offline", System: store.NewJSON(store.SystemInfo{CPU: 99})})
	db.Create(&store.Node{ID: "n3", Name: "gone", Status: "online", Revoked: true})
	soon := now.Add(48 * time.Hour)
	db.Create(&store.User{ID: "u1", Username: "alice-secret", UUID: "u1", SubToken: "u1-subscription-token", Status: "active", UsedUp: 10, UsedDown: 90, ExpireAt: &soon})
	db.Create(&store.User{ID: "u2", Username: "bob", UUID: "u2", SubToken: "u2-subscription-token", Status: "disabled", UsedUp: 5, UsedDown: 5})
	db.Create(&store.Order{ID: "o1", Status: "paid", PaidAt: &now})
	db.Create(&store.Order{ID: "o2", Status: "pending"})
	db.Create(&store.Ticket{No: 1, UserID: "u1", Status: "open"})
	exp := now.Add(30 * 24 * time.Hour)
	db.Create(&store.Certificate{ID: "c1", Domain: "cdn.example.com", Provider: "manual", ExpireAt: &exp})
	db.Create(&store.CDNProfile{ID: "cf", Name: "cf-main", OriginDomain: "cdn.example.com", NodeCheck: true,
		PreferredIPs: store.NewJSON([]string{"104.16.0.1", "104.16.0.2"}), BadIPs: store.NewJSON([]string{"104.16.0.2"})})
	db.Create(&store.CDNIPReport{ProfileID: "cf", Source: "上海电信", At: now.Add(-time.Hour)})

	url := srv.URL + s.cfg.APIBase() + "/metrics"
	code, ctype, body := scrape(t, url, admin.tok)
	if code != 200 || !strings.HasPrefix(ctype, "text/plain; version=0.0.4") {
		t.Fatalf("scrape %d %q", code, ctype)
	}
	for _, want := range []string{
		`polaris_nodes{status="online"} 1`,
		`polaris_nodes{status="offline"} 1`,
		`polaris_node_up{node="n1",name="tokyo \"1\""} 1`,
		`polaris_node_up{node="n2",name="hk"} 0`,
		`polaris_node_cpu_percent{node="n1",name="tokyo \"1\""} 12.5`,
		`polaris_node_network_transmit_bytes_per_second{node="n1",name="tokyo \"1\""} 400`,
		`polaris_node_cycle_transmit_bytes{node="n1",name="tokyo \"1\""} 2000`,
		`polaris_users{status="active"} 1`,
		`polaris_users{status="disabled"} 1`,
		`polaris_users_traffic_used_bytes{direction="down"} 95`,
		`polaris_users_expiring_7d 1`,
		`polaris_orders{status="paid"} 1`,
		`polaris_orders_paid_24h 1`,
		`polaris_tickets{status="open"} 1`,
		`polaris_certificate_expiry_seconds{domain="cdn.example.com"}`,
		`polaris_license_info{plan=`,
		`polaris_cdn_preferred_ips{profile="cf-main"} 2`,
		`polaris_cdn_unreachable_ips{profile="cf-main"} 1`,
		`polaris_cdn_report_age_seconds{profile="cf-main",source="上海电信"}`,
		"# TYPE polaris_node_up gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Offline nodes carry no stale resource readings; revoked nodes and users
	// never appear.
	for _, bad := range []string{`polaris_node_cpu_percent{node="n2"`, `node="n3"`, "alice-secret", "u1-subscription-token"} {
		if strings.Contains(body, bad) {
			t.Errorf("unexpected %s", bad)
		}
	}
	// Every line is a comment or "name{labels} value".
	line := regexp.MustCompile(`^(# (HELP|TYPE) .+|[a-z_][a-z0-9_]*(\{[^}]*\})? [-+0-9.eE]+|[a-z_][a-z0-9_]*(\{[^}]*\})? (NaN|[+-]Inf))$`)
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		if !line.MatchString(l) {
			t.Errorf("bad exposition line: %q", l)
		}
	}
	if strings.Count(body, "# TYPE polaris_node_up gauge") != 1 {
		t.Error("TYPE repeated")
	}

	// A metrics:read token scrapes; it opens nothing else; other tokens can't scrape.
	_, tok, err := s.issueAPIToken("prometheus", "", []string{ScopeMetricsRead}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	if code, _, body := scrape(t, url, tok); code != 200 || !strings.Contains(body, "polaris_nodes") {
		t.Fatalf("token scrape %d", code)
	}
	probe := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: tok}
	if code, _ := probe.do("GET", "/users", nil); code != 403 {
		t.Fatalf("metrics token read users: %d", code)
	}
	_, other, _ := s.issueAPIToken("ops", "", []string{ScopeTrafficRead}, 0, "root")
	if code, _, _ := scrape(t, url, other); code != 403 {
		t.Fatalf("traffic:read token scraped metrics: %d", code)
	}
	if code, _, _ := scrape(t, url, ""); code != 401 {
		t.Fatalf("anonymous scrape: %d", code)
	}
}
