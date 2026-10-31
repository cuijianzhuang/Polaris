package panel

import (
	"encoding/base64"
	"encoding/json"
	backupfmt "github.com/cuijianzhuang/polaris/internal/backupfmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func cdnRegionFixture(t *testing.T) (*Server, string, *apiClient) {
	t.Helper()
	s, srv, admin := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	for _, v := range []any{
		&store.Node{ID: "rn", Name: "region-node"},
		&store.Inbound{ID: "ri", NodeID: "rn", Tag: "ws", Port: 443, Core: model.CoreXray, Enabled: true, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS, Path: "/ws"}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecTLS})},
		&store.CDNProfile{ID: "regional", Name: "regional", Provider: "cloudflare", Scheme: "https", OriginDomain: "cdn.example.com", Ports: store.NewJSON([]int{443}), RegionSplit: true, CarrierSplit: true, ExpandPerIP: true, PreferredIPs: store.NewJSON([]string{"104.16.1.1", "104.16.2.1", "104.16.3.1"})},
		&store.CDNProfile{ID: "foreign", Name: "foreign", Scheme: "https"},
		&store.Endpoint{ID: "re", InboundID: "ri", Kind: "cdn", CDNProfileID: "regional"},
		&store.Group{ID: "rg", Name: "region-group"}, &store.GroupInbound{GroupID: "rg", InboundID: "ri"},
		&store.User{ID: "ra", Username: "alice", Status: "active", UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", TelegramID: 701, SubToken: "alice-region-subscription-token", CDNCarrier: "ct", CDNRegion: "CN-31"},
		&store.User{ID: "rb", Username: "bob", Status: "active", UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a02", TelegramID: 702, SubToken: "bob-region-subscription-token", CDNCarrier: "ct", CDNRegion: "CN-44"},
		&store.UserGroup{UserID: "ra", GroupID: "rg"}, &store.UserGroup{UserID: "rb", GroupID: "rg"},
	} {
		if err := s.st.DB.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []store.CDNIPReport{
		{ProfileID: "regional", Source: "上海电信", Carrier: "ct", Region: "CN-31", Scheme: "https", Port: 443, At: time.Now(), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.1.1"}})},
		{ProfileID: "regional", Source: "广东电信", Carrier: "ct", Region: "CN-44", Scheme: "https", Port: 443, At: time.Now(), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.2.1"}})},
		{ProfileID: "regional", Source: "上海移动", Carrier: "cm", Region: "CN-31", Scheme: "https", Port: 443, At: time.Now(), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.3.1"}})},
	} {
		if err := s.st.DB.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s, srv.URL, admin
}
func regionPortalClient(t *testing.T, s *Server, base, id string) *portalClient {
	t.Helper()
	var u store.User
	s.st.DB.First(&u, "id = ?", id)
	token, err := s.issuePortalToken(&u)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	parsed, _ := url.Parse(base + portalPath)
	jar.SetCookies(parsed, []*http.Cookie{{Name: portalCookie, Value: token, Path: portalPath}})
	return &portalClient{t: t, base: base, hc: &http.Client{Jar: jar}}
}
func regionServers(t *testing.T, s *Server, id, carrier, region string) []string {
	t.Helper()
	var u store.User
	s.st.DB.First(&u, "id = ?", id)
	nodes, err := s.subNodesFor(&u, carrier, region)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, n := range nodes {
		out = append(out, n.Server)
	}
	return out
}
func TestCDNProvinceCarrierAndPersonalLists(t *testing.T) {
	s, base, admin := cdnRegionFixture(t)
	for _, tc := range []struct{ region, carrier, ip string }{{"CN-31", "ct", "104.16.1.1"}, {"CN-44", "ct", "104.16.2.1"}, {"CN-31", "cm", "104.16.3.1"}} {
		if got := regionServers(t, s, "ra", tc.carrier, tc.region); !slices.Equal(got, []string{tc.ip}) {
			t.Fatal(tc, got)
		}
	}
	got := regionServers(t, s, "ra", "ct", "CN-33")
	if len(got) != 2 || !slices.Contains(got, "104.16.1.1") || !slices.Contains(got, "104.16.2.1") {
		t.Fatal("carrier fallback", got)
	}
	if got := regionServers(t, s, "ra", "", "CN-31"); len(got) != 2 || !slices.Contains(got, "104.16.3.1") {
		t.Fatal("region-only", got)
	}
	a := regionPortalClient(t, s, base, "ra")
	body := map[string]any{"profileId": "regional", "addresses": []string{"104.20.9.9", "BEST.EXAMPLE.COM."}}
	if st, _ := a.req("POST", "/u/cdn-preferences", body, false); st != 403 {
		t.Fatal("missing CSRF", st)
	}
	if st, out := a.req("POST", "/u/cdn-preferences", body, true); st != 200 {
		t.Fatal(st, out)
	}
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.20.9.9", "best.example.com"}) {
		t.Fatal("private list ignored", got)
	}
	if got := regionServers(t, s, "rb", "ct", "CN-44"); !slices.Equal(got, []string{"104.16.2.1"}) {
		t.Fatal("private list leaked to other user", got)
	}
	for _, bad := range []map[string]any{
		{"profileId": "foreign", "addresses": []string{"104.20.9.9"}},
		{"profileId": "regional", "addresses": []string{"127.0.0.1"}},
		{"profileId": "regional", "addresses": []string{"https://bad.example.com"}},
		{"profileId": "regional"},
		{"profileId": "regional", "addresses": make([]string, 33)},
	} {
		if st, _ := a.req("POST", "/u/cdn-preferences", bad, true); st < 400 {
			t.Fatal("invalid/foreign list admitted", bad)
		}
	}
	if _, page := a.req("GET", "/u", nil, false); !strings.Contains(page, "维护个人优选 IP／域名") || !strings.Contains(page, "104.20.9.9") || strings.Contains(page, "foreign ·") {
		t.Fatal("personal UI missing or unauthorized profile shown")
	}
	s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "regional").Update("blocked_hosts", store.NewJSON([]string{"104.20.9.9"}))
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"best.example.com"}) {
		t.Fatal("deny rule lost precedence", got)
	}
	if st, _ := a.req("POST", "/u/cdn-preferences", map[string]any{"profileId": "regional", "addresses": []string{}}, true); st != 200 {
		t.Fatal(st)
	}
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.16.1.1"}) {
		t.Fatal("clear did not restore shared pool", got)
	}
	if st, _ := a.req("POST", "/u/region", map[string]any{"region": "广东省"}, true); st != 200 {
		t.Fatal(st)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "ra")
	if u.CDNRegion != "CN-44" {
		t.Fatal(u.CDNRegion)
	}
	r, _ := http.NewRequest(http.MethodGet, base+"/sub/"+u.SubToken+"?carrier=ct&region=CN-31", nil)
	if s.subRegion(r, &u) != "CN-31" {
		t.Fatal("explicit region did not override saved choice")
	}
	// An old probe paired with structured metadata inherits it when the payload omits region.
	code, pair := admin.do("POST", "/cdn-profiles/regional/probes", map[string]any{"source": "new-point", "region": "上海市", "port": 443})
	if code != 201 || pair["region"] != "CN-31" {
		t.Fatal(code, pair)
	}
	probe := &apiClient{t: t, base: admin.base, tok: pair["token"].(string)}
	payload := map[string]any{"source": "new-point", "ips": []map[string]any{{"ip": "104.16.8.8"}}}
	if code, out := probe.do("POST", "/cdn-profiles/regional/preferred-ips", payload); code != 200 {
		t.Fatal(code, out)
	}
	var report store.CDNIPReport
	s.st.DB.First(&report, "profile_id = ? AND source = ?", "regional", "new-point")
	if report.Region != "CN-31" {
		t.Fatal("old probe lost bound region", report)
	}
	payload["region"] = "广东"
	if code, _ := probe.do("POST", "/cdn-profiles/regional/preferred-ips", payload); code != 403 {
		t.Fatal("bound region bypassed", code)
	}
}

func TestCDNRegionLegacyGeoAndCallback(t *testing.T) {
	s, base, admin := cdnRegionFixture(t)
	for input, want := range map[string]string{"上海市": "CN-31", "Guangdong": "CN-44", "广西壮族自治区": "CN-45", "香港": "HK", "gb": "GB", "": ""} {
		if got, e := normalizeCDNRegion(input); e != nil || got != want {
			t.Fatal(input, got, e)
		}
	}
	if _, e := normalizeCDNRegion("../../anything"); e == nil {
		t.Fatal("invalid region accepted")
	}
	s.st.DB.Model(&store.CDNIPReport{}).Where("source = ?", "上海电信").Update("region", "")
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.16.1.1"}) {
		t.Fatal("legacy source inference failed", got)
	}
	installGeo(t, s, admin)
	if got := s.regionOfIP("81.2.69.160"); got != "GB" {
		t.Fatal("offline country fallback", got)
	}
	if s.regionOfIP("127.0.0.1") != "" {
		t.Fatal("private IP assigned region")
	}
	callback := s.callbacks.issue(withCDNAudience("/sub/alice-region-subscription-token", "ct", "CN-31"))
	resp, err := http.Get(base + "/sub/cb/" + callback)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	encoded, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "104.16.1.1") || strings.Contains(string(raw), "104.16.2.1") {
		t.Fatal("converter lost regional selection", string(raw))
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "ra")
	r, _ := http.NewRequest("GET", base+"/sub/x?region=all&carrier=all", nil)
	if s.subRegion(r, &u) != "" || s.subCarrier(r, &u) != "" {
		t.Fatal("callback auto-disable ignored")
	}
	b, _ := json.Marshal(u)
	if strings.Contains(string(b), "cdnPreferred") {
		t.Fatal("private addresses exposed by generic user JSON")
	}
}

func TestCDNRegionMetadataAndPersonalBackup(t *testing.T) {
	s, base, _ := cdnRegionFixture(t)
	page := regionPortalClient(t, s, base, "ra")
	if code, body := page.req("POST", "/u/cdn-preferences", map[string]any{"profileId": "regional", "addresses": []string{"104.20.9.9", "keep.example.com"}}, true); code != 200 {
		t.Fatal(code, body)
	}
	// Explicit entry addresses and a disabled expansion policy remain authoritative.
	s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Update("address", "104.20.8.8")
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.20.8.8"}) {
		t.Fatal("personal list bypassed entry override", got)
	}
	s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Update("address", "")
	s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "regional").Update("expand_per_ip", false)
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"cdn.example.com"}) {
		t.Fatal("personal list bypassed expansion policy", got)
	}
	files, _, err := backupfmt.ExportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dst, err := store.Open(filepath.Join(dir, "restored.db"), s.st.Box, false)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := dst.DB.DB()
	defer db.Close()
	if err = restoreArchive(dst, dir, &backupfmt.Archive{Tables: files, Manifest: backupfmt.Manifest{Format: 2}}, s.st.Box); err != nil {
		t.Fatal(err)
	}
	var restored store.User
	if err = dst.DB.First(&restored, "id = ?", "ra").Error; err != nil {
		t.Fatal(err)
	}
	if restored.CDNRegion != "CN-31" || !slices.Equal(restored.CDNPreferred.V["regional"], []string{"104.20.9.9", "keep.example.com"}) {
		t.Fatal("backup lost private choices", restored.CDNRegion, restored.CDNPreferred.V)
	}
	var p store.CDNProfile
	dst.DB.First(&p, "id = ?", "regional")
	if !p.RegionSplit {
		t.Fatal("backup lost allocation mode")
	}
	var report store.CDNIPReport
	dst.DB.First(&report, "profile_id = ? AND source = ?", "regional", "广东电信")
	if report.Region != "CN-44" {
		t.Fatal("backup lost probe region")
	}
}

func TestCDNProvinceFilterPrecedesSourceDeduplication(t *testing.T) {
	s, _, _ := cdnRegionFixture(t)
	for _, v := range []any{
		&store.CDNProfile{ID: "duplicate", Name: "duplicate", Provider: "cloudflare", Scheme: "https"},
		&store.CDNIPReport{ProfileID: "duplicate", Source: "上海电信", Carrier: "ct", Region: "CN-44", Scheme: "https", Port: 443, At: time.Now().Add(time.Hour), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.99.1"}})},
	} {
		if err := s.st.DB.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.16.1.1"}) {
		t.Fatal("another province shadowed the source", got)
	}
}

func TestCDNRegionPortExpiryAndDenyRules(t *testing.T) {
	s, _, _ := cdnRegionFixture(t)
	for _, r := range []store.CDNIPReport{
		{ProfileID: "regional", Source: "上海电信-8443", Carrier: "ct", Region: "CN-31", Scheme: "https", Port: 8443, At: time.Now(), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.4.1"}})},
		{ProfileID: "regional", Source: "上海电信-http", Carrier: "ct", Region: "CN-31", Scheme: "http", Port: 80, At: time.Now(), IPs: store.NewJSON([]store.CDNIPResult{{IP: "104.16.5.1"}})},
	} {
		if err := s.st.DB.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Update("port", 8443)
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.16.4.1"}) {
		t.Fatal("measurement port/scheme mixed", got)
	}
	s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Update("port", 0)
	s.st.DB.Model(&store.CDNIPReport{}).Where("source = ?", "上海电信").Update("at", time.Now().Add(-48*time.Hour))
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.16.2.1"}) {
		t.Fatal("expired regional list did not fall back", got)
	}
	s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "regional").Update("stale_policy", "keep")
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.16.1.1"}) {
		t.Fatal("keep did not retain regional list", got)
	}
	s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "regional").Update("blocked_hosts", store.NewJSON([]string{"104.16.1.1"}))
	if got := regionServers(t, s, "ra", "ct", "CN-31"); slices.Contains(got, "104.16.1.1") {
		t.Fatal("regional fallback bypassed deny rule", got)
	}
}
