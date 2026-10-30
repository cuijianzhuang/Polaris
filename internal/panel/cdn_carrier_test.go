package panel

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A profile with CarrierSplit hands each carrier the IPs its own speed-test
// points reported; unknown carriers, missing or stale reports, and profiles
// without the switch use the merged list.
func TestCDNIPsByCarrier(t *testing.T) {
	s := newTestServer(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "jp", Address: "203.0.113.5"})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	db.Create(&store.Inbound{ID: "ws", NodeID: "n1", Tag: "ws", Port: 2053, Core: model.CoreXray, Enabled: true,
		Protocol:  store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}),
		Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS, Path: "/ws"}),
		Security:  store.NewJSON(model.SecuritySpec{Type: model.SecTLS, ServerName: "cdn.example.com"})})
	merged := []string{"104.16.0.1", "104.16.0.9", "104.16.0.2"}
	db.Create(&store.CDNProfile{ID: "cf", Name: "cf", OriginDomain: "cdn.example.com", Ports: store.NewJSON([]int{2053}),
		ExpandPerIP: true, CarrierSplit: true, PreferredIPs: store.NewJSON(merged)})
	db.Create(&store.Endpoint{ID: "ep", InboundID: "ws", Kind: "cdn", CDNProfileID: "cf"})
	now := time.Now()
	report := func(source, carrier string, at time.Time, ips ...string) {
		var rs []store.CDNIPResult
		for _, ip := range ips {
			rs = append(rs, store.CDNIPResult{IP: ip, LatencyMs: 50})
		}
		db.Create(&store.CDNIPReport{Port: 2053, Scheme: "https", ProfileID: "cf", Source: source, Carrier: carrier, At: at, IPs: store.NewJSON(rs)})
	}
	report("上海电信", "ct", now, "104.16.0.1")
	report("广州移动", "cm", now, "104.16.0.9", "104.16.0.2")
	report("北京联通", "cu", now.Add(-48*time.Hour), "104.16.0.5") // stale
	db.Create(&store.User{ID: "u", Username: "u", UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", Password: "p", Status: "active", SubToken: "tok-tok-tok-tok-tok"})
	db.Create(&store.Group{ID: "g", Name: "g"})
	db.Create(&store.UserGroup{UserID: "u", GroupID: "g"})
	db.Create(&store.GroupInbound{GroupID: "g", InboundID: "ws"})
	var u store.User
	db.First(&u, "id = ?", "u")

	servers := func(carrier string) []string {
		t.Helper()
		nodes, err := s.subNodesFor(&u, carrier)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range nodes {
			out = append(out, n.Server)
		}
		return out
	}
	if got := servers("cm"); !slices.Equal(got, []string{"104.16.0.9", "104.16.0.2"}) {
		t.Fatalf("mobile = %v", got)
	}
	// An entry with its own address keeps it: one node, not one per IP.
	db.Model(&store.Endpoint{}).Where("id = ?", "ep").Update("address", "1.2.3.4")
	if got := servers("cm"); !slices.Equal(got, []string{"1.2.3.4"}) {
		t.Fatalf("address override = %v", got)
	}
	db.Model(&store.Endpoint{}).Where("id = ?", "ep").Update("address", "")
	if got := servers("ct"); !slices.Equal(got, []string{"104.16.0.1"}) {
		t.Fatalf("telecom = %v", got)
	}
	for _, c := range []string{"", "cu"} { // unknown; only a stale report
		if got := servers(c); !slices.Equal(got, merged) {
			t.Fatalf("carrier %q = %v, want the merged list", c, got)
		}
	}
	// The user's own choice applies when no carrier is given explicitly.
	u.CDNCarrier = "ct"
	if nodes, _ := s.subNodes(&u); len(nodes) != 1 || nodes[0].Server != "104.16.0.1" {
		t.Fatalf("user choice ignored: %+v", nodes)
	}
	// Node check still drops unreachable IPs from a carrier's list.
	db.Model(&store.CDNProfile{}).Where("id = ?", "cf").Updates(map[string]any{"node_check": true, "bad_ips": store.NewJSON([]string{"104.16.0.9"})})
	if got := servers("cm"); !slices.Equal(got, []string{"104.16.0.2"}) {
		t.Fatalf("mobile with node check = %v", got)
	}
	// Without the switch every carrier gets the merged list.
	db.Model(&store.CDNProfile{}).Where("id = ?", "cf").Updates(map[string]any{"carrier_split": false, "node_check": false})
	if got := servers("cm"); !slices.Equal(got, merged) {
		t.Fatalf("split off = %v", got)
	}
}

func TestSubCarrier(t *testing.T) {
	s := newTestServer(t)
	get := func(q string, u store.User) string {
		r := httptest.NewRequest("GET", "/sub/tok"+q, nil)
		return s.subCarrier(r, &u)
	}
	if c := get("?carrier=cu", store.User{CDNCarrier: "ct"}); c != "cu" {
		t.Fatalf("link override = %q", c)
	}
	if c := get("", store.User{CDNCarrier: "cm"}); c != "cm" {
		t.Fatalf("user choice = %q", c)
	}
	// No ASN database: automatic detection finds nothing.
	if c := get("?carrier=auto", store.User{CDNCarrier: "cm"}); c != "" {
		t.Fatalf("auto without ASN data = %q", c)
	}
	if c := get("?carrier=bogus", store.User{CDNCarrier: "cm"}); c != "" {
		t.Fatalf("bogus carrier = %q", c)
	}
	for name, want := range map[string]string{"上海电信": "ct", "Beijing Unicom": "cu", "广州移动 2": "cm", "香港": ""} {
		if got := carrierFromName(name); got != want {
			t.Errorf("carrierFromName(%q) = %q, want %q", name, got, want)
		}
	}
	if _, err := reportCarrier(cdnIPReportReq{Source: "x", Carrier: "att"}); err == nil {
		t.Fatal("unknown carrier accepted")
	}
	if c, _ := reportCarrier(cdnIPReportReq{Source: "上海电信", Carrier: "CM"}); c != "cm" {
		t.Fatalf("explicit carrier = %q", c)
	}
}

// The user center offers the carrier choice once a profile splits by
// carrier, and saves it.
func TestPortalCarrier(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	db := s.st.DB
	db.Create(&store.User{ID: "u1", Username: "alice", UUID: "u1-uuid", Status: "active", SubToken: "u1-subscription-token", TelegramID: 700})
	var u store.User
	db.First(&u, "id = ?", "u1")
	tok, err := s.issuePortalToken(&u)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse(srv.URL + portalPath)
	jar.SetCookies(base, []*http.Cookie{{Name: portalCookie, Value: tok, Path: portalPath}})
	page := &portalClient{t: t, base: srv.URL, hc: &http.Client{Jar: jar}}
	if _, body := page.req("GET", "/u", nil, false); !strings.Contains(body, `data-portal="reset"`) || strings.Contains(body, "<select data-portal-carrier") {
		t.Fatal("not signed in, or carrier choice shown with no profile splitting by carrier")
	}
	if err := db.Create(&store.CDNProfile{ID: "cf", Name: "cf", OriginDomain: "cdn.example.com", CarrierSplit: true}).Error; err != nil {
		t.Fatal(err)
	}
	if _, body := page.req("GET", "/u", nil, false); !strings.Contains(body, "<select data-portal-carrier") {
		t.Fatal("no carrier choice in the user center")
	}
	if st, _ := page.req("POST", "/u/carrier", map[string]string{"carrier": "cm"}, false); st != 403 {
		t.Fatalf("without the portal header: %d", st)
	}
	if st, _ := page.req("POST", "/u/carrier", map[string]string{"carrier": "att"}, true); st != 400 {
		t.Fatalf("unknown carrier: %d", st)
	}
	if st, body := page.req("POST", "/u/carrier", map[string]string{"carrier": "cm"}, true); st != 200 {
		t.Fatalf("save: %d %s", st, body)
	}
	db.First(&u, "id = ?", "u1")
	if u.CDNCarrier != "cm" {
		t.Fatalf("saved carrier = %q", u.CDNCarrier)
	}
	if _, body := page.req("GET", "/u", nil, false); !strings.Contains(body, `value="cm" selected`) {
		t.Fatal("saved choice not shown")
	}
}
