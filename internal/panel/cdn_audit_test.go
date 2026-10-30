package panel

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A CDN sibling on a second node expects the Host its entry sends (its own
// hostname), and syncing moves an inbound whose Host was the shared domain
// along with its entry: the WS server answers 404 to any other Host.
func TestCDNInboundHostFollowsEntry(t *testing.T) {
	s, admin := cdnFixture(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n2", Name: "ORACLE-SG", Address: "203.0.113.6", DefaultCore: model.CoreXray})
	db.Create(&store.Core{ID: "c2", NodeID: "n2", Type: model.CoreXray, Version: "26.3.27"})
	ws := func(node, tag string, port int, host string) map[string]any {
		return map[string]any{"nodeId": node, "core": "xray", "tag": tag, "port": port, "protocol": map[string]any{"type": "vless"},
			"transport": map[string]any{"type": "ws", "path": "/" + tag, "host": host}, "security": map[string]any{"type": "tls", "serverName": "cdn.example.com"},
			"cdnProfileId": "cf", "cdnOnly": true}
	}
	if code, out := admin.do("POST", "/inbounds", ws("n1", "hk", 2083, "")); code != 201 {
		t.Fatalf("hk: %d %v", code, out)
	}
	// Sibling on n2.
	db.Create(&store.Inbound{ID: "re2", NodeID: "n2", Tag: "reality", Port: 443, Core: model.CoreXray, Enabled: true,
		Protocol:  store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"}),
		Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}),
		Security:  store.NewJSON(model.SecuritySpec{Type: model.SecReality, RealityDest: "www.microsoft.com:443"})})
	code, out := admin.do("POST", "/inbounds/re2/cdn-sibling", map[string]any{"cdnProfileId": "cf"})
	if code != 201 {
		t.Fatalf("sibling: %d %v", code, out)
	}
	var sib store.Inbound
	db.First(&sib, "id = ?", out["inbound"].(map[string]any)["id"])
	var ep store.Endpoint
	db.First(&ep, "inbound_id = ? AND kind = ?", sib.ID, "cdn")
	if ep.Host != "oracle-sg-0480a93d2e9b094b-cdn.example.com" || sib.Transport.V.Host != ep.Host || sib.Security.V.ServerName != ep.Host {
		t.Fatalf("sibling host %q / %q / %q", ep.Host, sib.Transport.V.Host, sib.Security.V.ServerName)
	}

	// An older entry made before hostnames were split: both on the shared domain.
	if code, out := admin.do("POST", "/inbounds", ws("n2", "sg-old", 2087, "cdn.example.com")); code != 201 {
		t.Fatalf("sg-old: %d %v", code, out)
	}
	var old store.Inbound
	db.First(&old, "tag = ?", "sg-old")
	db.Model(&store.Endpoint{}).Where("inbound_id = ?", old.ID).Update("host", "")
	// Before syncing the panel warns about the mismatch the split would cause.
	var p store.CDNProfile
	db.First(&p, "id = ?", "cf")
	notes := s.splitSharedCDNHosts(&p)
	db.First(&old, "id = ?", old.ID)
	var oldEp store.Endpoint
	db.First(&oldEp, "inbound_id = ?", old.ID)
	if oldEp.Host != "oracle-sg-0480a93d2e9b094b-cdn.example.com" || old.Transport.V.Host != oldEp.Host {
		t.Fatalf("split: notes %v, entry %q, inbound host %q", notes, oldEp.Host, old.Transport.V.Host)
	}
	// A mismatch the panel did not make is reported on the entry.
	tr := old.Transport.V
	tr.Host = "elsewhere.example.com"
	db.Model(&old).Update("transport", store.NewJSON(tr))
	db.First(&old, "id = ?", old.ID)
	if w := strings.Join(s.cdnEndpointWarnings(&oldEp, &old, &p), " "); !strings.Contains(w, "HTTP 404") {
		t.Fatalf("warnings %q", w)
	}
}

// Retiring a speed-test point takes its addresses out of every list it fed;
// the stale alert reads the shared pool; a save keeps URL domains unpinned.
func TestCDNReportsRetireAndStale(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.CDNProfile{ID: "hk", Name: "hk-cdn", Provider: "cloudflare", OriginDomain: "hk-cdn.example.com",
		Ports: store.NewJSON([]int{443}), PreferredIPs: store.NewJSON([]string{})})
	db.Create(&store.CDNProfile{ID: "sg", Name: "sg-cdn", Provider: "cloudflare", OriginDomain: "sg-cdn.example.com",
		Ports: store.NewJSON([]int{443}), PreferredIPs: store.NewJSON([]string{"visa.cn"}), PinnedHosts: store.NewJSON([]string{"visa.cn"}), StaleAlerted: true})
	_, tok, err := s.issueAPIToken("测速", "", []string{ScopeCDNIPs}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	probe := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: tok}
	report := func(source string, ips ...string) {
		var rs []map[string]any
		for _, ip := range ips {
			rs = append(rs, map[string]any{"ip": ip, "latencyMs": 50, "loss": 0})
		}
		if code, out := probe.do("POST", "/cdn-profiles/hk/preferred-ips", map[string]any{"source": source, "ips": rs}); code != 200 {
			t.Fatalf("report: %d %v", code, out)
		}
	}
	get := func(id string) store.CDNProfile {
		var p store.CDNProfile
		db.First(&p, "id = ?", id)
		return p
	}
	report("上海电信", "104.16.1.1")
	// sg was stale; the shared report clears it there too.
	if get("sg").StaleAlerted {
		t.Fatal("shared report did not clear sg's stale alert")
	}
	report("北京联通", "104.17.2.2")
	if got := fmt.Sprint(get("sg").PreferredIPs.V); got != "[104.16.1.1 104.17.2.2 visa.cn]" {
		t.Fatalf("sg before = %s", got)
	}
	// Retire 北京联通: rebuilt from 上海电信 everywhere.
	if code, _ := admin.do("DELETE", "/cdn-profiles/hk/ip-reports/北京联通", nil); code != 204 {
		t.Fatalf("delete %d", code)
	}
	if got := fmt.Sprint(get("sg").PreferredIPs.V); got != "[104.16.1.1 visa.cn]" {
		t.Fatalf("sg after retire = %s", got)
	}
	// Retire the last one: its address leaves the lists, typed-in domains stay.
	admin.do("DELETE", "/cdn-profiles/hk/ip-reports/上海电信", nil)
	if got := fmt.Sprint(get("sg").PreferredIPs.V); got != "[visa.cn]" {
		t.Fatalf("sg after last retire = %s", got)
	}
	if got := fmt.Sprint(get("hk").PreferredIPs.V); got != "[]" {
		t.Fatalf("hk after last retire = %s", got)
	}

	// Stale: sg has no point of its own but uses hk's; when it goes quiet sg is alerted.
	report("上海电信", "104.16.1.1")
	db.Model(&store.CDNIPReport{}).Where("1 = 1").Update("at", time.Now().Add(-48*time.Hour))
	s.checkCDNReportsStale()
	if !get("sg").StaleAlerted || !get("hk").StaleAlerted {
		t.Fatalf("stale: sg %v hk %v", get("sg").StaleAlerted, get("hk").StaleAlerted)
	}

	// A domain the list holds but that was not typed in stays unpinned on save.
	db.Model(&store.CDNProfile{}).Where("id = ?", "sg").Updates(map[string]any{"preferred_ips": store.NewJSON([]string{"104.16.1.1", "url.example.org", "visa.cn"})})
	code, out := admin.do("PUT", "/cdn-profiles/sg", map[string]any{"name": "sg-cdn", "provider": "cloudflare", "originDomain": "sg-cdn.example.com",
		"preferredIps": []string{"104.16.1.1", "url.example.org", "visa.cn", "new.example.net"}})
	if code != 200 {
		t.Fatalf("save %d %v", code, out)
	}
	if got := fmt.Sprint(get("sg").PinnedHosts.V); got != "[visa.cn new.example.net]" {
		t.Fatalf("pinned = %s", got)
	}
}
