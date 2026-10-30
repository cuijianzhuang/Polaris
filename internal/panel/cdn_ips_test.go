package panel

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Speed-test points report with a cdn:ips:write token; the profile's list is
// their best IPs in turn, the reports show on the CDN page, and the token can
// do nothing else.
func TestCDNIPReports(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.DB.Create(&store.CDNProfile{ID: "cf", Name: "cf-main", Provider: "cloudflare", OriginDomain: "cdn.example.com",
		Ports: store.NewJSON([]int{443}), PreferredIPs: store.NewJSON([]string{"104.16.0.1"})})
	_, tok, err := s.issueAPIToken("测速", "", []string{ScopeCDNIPs}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	probe := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: tok}
	report := func(source string, ips ...string) (int, map[string]any) {
		var rs []map[string]any
		for i, ip := range ips {
			rs = append(rs, map[string]any{"ip": ip, "latencyMs": 200 - i*10, "loss": 0})
		}
		return probe.do("POST", "/cdn-profiles/cf/preferred-ips", map[string]any{"source": source, "ips": rs})
	}
	// Reported worst-first: the panel ranks by loss, then latency.
	if code, out := report("上海电信", "104.16.1.3", "104.16.1.2", "104.16.1.1"); code != 200 || fmt.Sprint(out["preferredIps"]) != "[104.16.1.1 104.16.1.2 104.16.1.3]" {
		t.Fatalf("first report: %d %v", code, out)
	}
	// A second point: the list takes each point's best in turn (sources by name);
	// its own best, 104.16.1.1, is already in, so its next one follows in round two.
	if code, out := report("北京联通", "104.17.0.2", "104.17.0.1", "104.16.1.1"); code != 200 {
		t.Fatalf("second report: %d %v", code, out)
	}
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", "cf")
	if got := strings.Join(p.PreferredIPs.V, " "); got != "104.16.1.1 104.16.1.2 104.17.0.1 104.16.1.3 104.17.0.2" {
		t.Fatalf("merged list = %s", got)
	}
	// Loss outranks latency.
	if code, _ := probe.do("POST", "/cdn-profiles/cf/preferred-ips", map[string]any{"source": "上海电信", "ips": []map[string]any{
		{"ip": "104.16.9.9", "latencyMs": 20, "loss": 0.5}, {"ip": "104.16.9.8", "latencyMs": 90, "loss": 0}}}); code != 200 {
		t.Fatal(code)
	}
	var r store.CDNIPReport
	s.st.DB.First(&r, "profile_id = ? AND source = ?", "cf", "上海电信")
	if r.Carrier != "ct" {
		t.Fatalf("carrier from the source name = %q", r.Carrier)
	}
	if len(r.IPs.V) != 2 || r.IPs.V[0].IP != "104.16.9.8" {
		t.Fatalf("ranking = %+v", r.IPs.V)
	}

	// A download test ranks ahead of latency (same loss); colo is upper-cased.
	if code, _ := probe.do("POST", "/cdn-profiles/cf/preferred-ips", map[string]any{"source": "上海电信", "ips": []map[string]any{
		{"ip": "104.16.9.7", "latencyMs": 30, "speed": 2.5, "colo": "sjc"}, {"ip": "104.16.9.6", "latencyMs": 80, "speed": 18.2, "colo": "HKG"}}}); code != 200 {
		t.Fatal(code)
	}
	s.st.DB.First(&r, "profile_id = ? AND source = ?", "cf", "上海电信")
	if len(r.IPs.V) != 2 || r.IPs.V[0].IP != "104.16.9.6" || r.IPs.V[1].Colo != "SJC" || r.IPs.V[0].Speed != 18.2 {
		t.Fatalf("speed ranking = %+v", r.IPs.V)
	}

	for name, body := range map[string]map[string]any{
		"bad speed":   {"source": "x1", "ips": []map[string]any{{"ip": "104.16.0.9", "speed": -1}}},
		"bad colo":    {"source": "x1", "ips": []map[string]any{{"ip": "104.16.0.9", "colo": "<b>"}}},
		"private":     {"source": "x1", "ips": []map[string]any{{"ip": "192.168.1.1"}}},
		"not an ip":   {"source": "x1", "ips": []map[string]any{{"ip": "cf.example"}}},
		"no source":   {"source": "", "ips": []map[string]any{{"ip": "104.16.0.9"}}},
		"empty":       {"source": "x1", "ips": []map[string]any{}},
		"bad loss":    {"source": "x1", "ips": []map[string]any{{"ip": "104.16.0.9", "loss": 2}}},
		"bad latency": {"source": "x1", "ips": []map[string]any{{"ip": "104.16.0.9", "latencyMs": -1}}},
	} {
		if code, _ := probe.do("POST", "/cdn-profiles/cf/preferred-ips", body); code != 400 {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}
	// The token reaches nothing else.
	for _, path := range []string{"/cdn-profiles", "/users", "/nodes"} {
		if code, _ := probe.do("GET", path, nil); code != 403 {
			t.Errorf("GET %s with a cdn:ips:write token: %d", path, code)
		}
	}
	if code, _ := probe.do("PUT", "/cdn-profiles/cf", map[string]any{"name": "x", "originDomain": "evil.example"}); code != 403 {
		t.Errorf("profile edit with a cdn:ips:write token: %d", code)
	}

	code, list := admin.doList("GET", "/cdn-profiles")
	if code != 200 || len(list) != 1 {
		t.Fatalf("list %d %v", code, list)
	}
	reports, _ := list[0].(map[string]any)["reports"].([]any)
	if len(reports) != 2 {
		t.Fatalf("reports on the CDN page = %v", reports)
	}
	if code, _ := admin.do("DELETE", "/cdn-profiles/cf/ip-reports/北京联通", nil); code != 204 {
		t.Fatalf("delete report: %d", code)
	}
	var n int64
	s.st.DB.Model(&store.CDNIPReport{}).Count(&n)
	if n != 1 {
		t.Fatalf("reports after delete = %d", n)
	}
}

// When every point goes quiet past StaleHours, admins hear about it once and
// the list stays; the next report clears it with a recovery notice. The URL
// refresh leaves a list that points maintain alone.
func TestCDNIPReportsStale(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	events := make(chan Event, 10)
	s.sinks = append(s.sinks, events)
	s.st.DB.Create(&store.CDNProfile{ID: "cf", Name: "cf-main", OriginDomain: "cdn.example.com", StaleHours: 6,
		IPSourceURL: "http://127.0.0.1:1/never", PreferredIPs: store.NewJSON([]string{"104.16.0.1"})})
	_, tok, _ := s.issueAPIToken("测速", "", []string{ScopeCDNIPs}, 0, "root")
	probe := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: tok}
	body := map[string]any{"source": "上海电信", "ips": []map[string]any{{"ip": "104.16.0.7", "latencyMs": 50}}}
	if code, _ := probe.do("POST", "/cdn-profiles/cf/preferred-ips", body); code != 200 {
		t.Fatal(code)
	}
	// Fresh report: the 6-hourly URL pull skips this profile.
	s.refreshAllPreferredIPs()
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", "cf")
	if p.LastError != "" || fmt.Sprint(p.PreferredIPs.V) != "[104.16.0.7]" {
		t.Fatalf("URL refresh touched a reported list: %q %v", p.LastError, p.PreferredIPs.V)
	}

	s.checkCDNReportsStale()
	if len(events) != 0 {
		t.Fatalf("fresh report alerted: %v", <-events)
	}
	s.st.DB.Model(&store.CDNIPReport{}).Where("profile_id = ?", "cf").Update("at", time.Now().Add(-7*time.Hour))
	s.checkCDNReportsStale()
	s.checkCDNReportsStale() // once only
	if len(events) != 1 {
		t.Fatalf("stale alerts = %d, want 1", len(events))
	}
	if e := <-events; e.Kind != "cdn_ips_stale" || !strings.Contains(e.Text, "上海电信") {
		t.Fatalf("stale event = %+v", e)
	}
	s.st.DB.First(&p, "id = ?", "cf")
	if fmt.Sprint(p.PreferredIPs.V) != "[104.16.0.7]" {
		t.Fatalf("stale list was dropped: %v", p.PreferredIPs.V)
	}
	if code, _ := probe.do("POST", "/cdn-profiles/cf/preferred-ips", body); code != 200 {
		t.Fatal(code)
	}
	if e := <-events; e.Kind != "cdn_ips_fresh" {
		t.Fatalf("recovery event = %+v", e)
	}
}

// Online nodes connect to each preferred IP; one no node reaches is left out
// of subscriptions. If none is reachable, the nodes are suspect and nothing
// is dropped.
func TestCDNIPsNodeCheck(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", Status: "online"})
	s.st.DB.Create(&store.Node{ID: "n2", Name: "hk", Status: "online"})
	s.st.DB.Create(&store.CDNProfile{ID: "cf", Name: "cf-main", OriginDomain: "cdn.example.com", NodeCheck: true,
		Ports: store.NewJSON([]int{2053}), PreferredIPs: store.NewJSON([]string{"104.16.0.1", "104.16.0.2", "104.16.0.3"})})
	// .1 reachable from n1 only, .2 from n2 only, .3 from neither; the port is the profile's first.
	stop1 := fakeProber(t, s, "n1", map[string]string{"104.16.0.1:2053": "40"})
	stop2 := fakeProber(t, s, "n2", map[string]string{"104.16.0.2:2053": "60"})
	s.checkCDNIPsFromNodes()
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", "cf")
	if fmt.Sprint(p.BadIPs.V) != "[104.16.0.3]" || p.CheckedAt == nil {
		t.Fatalf("bad IPs = %v checked=%v", p.BadIPs.V, p.CheckedAt)
	}
	if got := usableCDNIPs(&p); fmt.Sprint(got) != "[104.16.0.1 104.16.0.2]" {
		t.Fatalf("usable = %v", got)
	}
	p.NodeCheck = false
	if got := usableCDNIPs(&p); len(got) != 3 {
		t.Fatalf("check off still filters: %v", got)
	}
	stop1()
	stop2()

	// Nothing reachable: keep every IP.
	stop1 = fakeProber(t, s, "n1", map[string]string{})
	defer stop1()
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n2").Update("status", "offline")
	s.checkCDNIPsFromNodes()
	s.st.DB.First(&p, "id = ?", "cf")
	if len(p.BadIPs.V) != 0 || len(usableCDNIPs(&p)) != 3 {
		t.Fatalf("all-unreachable check dropped IPs: %v", p.BadIPs.V)
	}
}

// A speed-test point reporting to one Cloudflare profile updates every
// Cloudflare profile; each keeps its own typed-in domains, and a profile
// of another provider is left alone.
func TestCDNIPReportsShared(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.CDNProfile{ID: "hk", Name: "hk-cdn", Provider: "cloudflare", OriginDomain: "hk-cdn.example.com",
		Ports: store.NewJSON([]int{443}), PreferredIPs: store.NewJSON([]string{})})
	db.Create(&store.CDNProfile{ID: "sg", Name: "sg-cdn", Provider: "cloudflare", OriginDomain: "sg-cdn.example.com",
		Ports: store.NewJSON([]int{443}), PreferredIPs: store.NewJSON([]string{"visa.cn"}), PinnedHosts: store.NewJSON([]string{"visa.cn"})})
	db.Create(&store.CDNProfile{ID: "ot", Name: "other", Provider: "other", OriginDomain: "cdn.other.net",
		Ports: store.NewJSON([]int{443}), PreferredIPs: store.NewJSON([]string{"1.1.1.1"})})
	_, tok, err := s.issueAPIToken("测速", "", []string{ScopeCDNIPs}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	probe := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: tok}
	if code, out := probe.do("POST", "/cdn-profiles/hk/preferred-ips", map[string]any{"source": "上海电信",
		"ips": []map[string]any{{"ip": "104.16.1.1", "latencyMs": 50, "loss": 0}, {"ip": "104.16.1.2", "latencyMs": 60, "loss": 0}}}); code != 200 {
		t.Fatalf("report %d %v", code, out)
	}
	get := func(id string) store.CDNProfile {
		var p store.CDNProfile
		db.First(&p, "id = ?", id)
		return p
	}
	if got := strings.Join(get("hk").PreferredIPs.V, " "); got != "104.16.1.1 104.16.1.2" {
		t.Fatalf("hk = %s", got)
	}
	if got := strings.Join(get("sg").PreferredIPs.V, " "); got != "104.16.1.1 104.16.1.2 visa.cn" {
		t.Fatalf("sg = %s", got)
	}
	if got := strings.Join(get("ot").PreferredIPs.V, " "); got != "1.1.1.1" {
		t.Fatalf("other provider changed: %s", got)
	}
	// The carrier list and the URL-refresh guard see the shared point too.
	sg := get("sg")
	sg.CarrierSplit = true
	if got := strings.Join(s.carrierCDNIPs(&sg, "ct"), " "); got != "104.16.1.1 104.16.1.2 visa.cn" {
		t.Fatalf("sg carrier list = %s", got)
	}
	if !s.cdnFreshReport(&sg, time.Now()) {
		t.Fatal("shared report not fresh for sg")
	}
	// The list says where sg's addresses come from.
	code, list := admin.doList("GET", "/cdn-profiles")
	if code != 200 {
		t.Fatal(code)
	}
	for _, x := range list {
		v := x.(map[string]any)
		if v["id"] == "sg" && fmt.Sprint(v["shared"]) != "[上海电信]" {
			t.Fatalf("sg shared = %v", v["shared"])
		}
		if v["id"] == "hk" && fmt.Sprint(v["shared"]) != "[]" {
			t.Fatalf("hk shared = %v", v["shared"])
		}
	}
}

func TestCDNDuplicateReportsKeepBestResult(t *testing.T) {
	for _, tc := range []struct {
		name        string
		first, best store.CDNIPResult
	}{
		{"loss", store.CDNIPResult{IP: "104.16.1.1", Loss: 0.5, Speed: 99, LatencyMs: 1}, store.CDNIPResult{IP: "::ffff:104.16.1.1", Loss: 0, Speed: 1, LatencyMs: 500}},
		{"speed", store.CDNIPResult{IP: "104.16.1.1", Speed: 1, LatencyMs: 1}, store.CDNIPResult{IP: "104.16.1.1", Speed: 50, LatencyMs: 200, Colo: "hkg"}},
		{"latency", store.CDNIPResult{IP: "2606:4700::1111", LatencyMs: 500}, store.CDNIPResult{IP: "2606:4700:0::1111", LatencyMs: 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				ips := []store.CDNIPResult{tc.first, tc.best}
				if reverse {
					ips[0], ips[1] = ips[1], ips[0]
				}
				out, err := checkCDNReport(cdnIPReportReq{Source: "duplicate", IPs: ips})
				if err != nil || len(out) != 1 {
					t.Fatalf("%+v %v", out, err)
				}
				if out[0].LatencyMs != tc.best.LatencyMs || out[0].Speed != tc.best.Speed || out[0].Loss != tc.best.Loss {
					t.Fatalf("reverse=%v kept %+v instead of %+v", reverse, out[0], tc.best)
				}
				if tc.name == "speed" && out[0].Colo != "HKG" {
					t.Fatal("lost best result metadata", out)
				}
			}
		})
	}
	// Every duplicate is still validated; malformed metrics cannot hide behind one valid entry.
	if _, err := checkCDNReport(cdnIPReportReq{Source: "invalid", IPs: []store.CDNIPResult{{IP: "104.16.1.1"}, {IP: "104.16.1.1", Loss: 2}}}); err == nil {
		t.Fatal("accepted invalid duplicate")
	}
}
