package panel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func TestCDNProbeVerdicts(t *testing.T) {
	var status atomic.Int32
	var cf atomic.Bool
	var upgrade atomic.Bool
	var invalid atomic.Bool
	cf.Store(true)
	upgrade.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cf.Load() {
			w.Header().Set("Server", "cloudflare")
		}
		if upgrade.Load() {
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Sec-WebSocket-Accept", cdnWebSocketAccept(r.Header.Get("Sec-WebSocket-Key")))
		}
		if status.Load() == 301 {
			w.Header().Set("Location", "https://cdn.example.com/ws")
		}
		if invalid.Load() {
			w.Header().Set("Sec-WebSocket-Accept", "invalid")
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	pn, _ := strconv.Atoi(port)
	target := cdnTarget{Scheme: "http", Server: host, Port: pn, SNI: host, Host: "cdn.example.com", Path: "/ws"}
	for _, tr := range []model.Transport{model.TransWS, model.TransHTTPUpgrade, model.TransGRPC, model.TransXHTTP} {
		for _, code := range []int{301, 401, 403, 429, 500, 522} {
			status.Store(int32(code))
			r := probeCDN(context.Background(), target, tr, "")
			if r.OK || r.Partial {
				t.Fatalf("%s HTTP %d counted as pass: %+v", tr, code, r)
			}
			if code == 403 && !strings.Contains(r.Message, "Cloudflare") {
				t.Fatalf("WAF not diagnosed: %+v", r)
			}
		}
	}
	status.Store(200)
	for _, tr := range []model.Transport{model.TransGRPC, model.TransXHTTP} {
		r := probeCDN(context.Background(), target, tr, "")
		if r.OK || !r.Partial {
			t.Fatalf("unverified %s considered available: %+v", tr, r)
		}
	}
	status.Store(101)
	r := probeCDN(context.Background(), target, model.TransWS, "")
	if !r.OK || r.Partial || !strings.Contains(r.Message, "尚未验证代理认证") {
		t.Fatalf("transport scope unclear: %+v", r)
	}
	invalid.Store(true)
	if bad := probeCDN(context.Background(), target, model.TransWS, ""); bad.OK {
		t.Fatalf("invalid WebSocket accept passed: %+v", bad)
	}
	invalid.Store(false)
	cf.Store(false)
	if r = probeCDN(context.Background(), target, model.TransWS, ""); r.OK || !r.Partial {
		t.Fatalf("origin response mistaken for CDN: %+v", r)
	}
	cf.Store(true)
	upgrade.Store(false)
	if r = probeCDN(context.Background(), target, model.TransWS, ""); r.OK {
		t.Fatalf("invalid Upgrade accepted: %+v", r)
	}
}

func TestCDNPortMeasurementsAndChecks(t *testing.T) {
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	for _, p := range []store.CDNProfile{
		{ID: "cf443", Name: "443", Provider: "cloudflare", OriginDomain: "cdn.example.com", Scheme: "https", Ports: store.NewJSON([]int{443}), ExpandPerIP: true, NodeCheck: true},
		{ID: "cf8443", Name: "8443", Provider: "cloudflare", OriginDomain: "cdn2.example.com", Scheme: "https", Ports: store.NewJSON([]int{8443})},
		{ID: "cf2053", Name: "2053", Provider: "cloudflare", OriginDomain: "cdn3.example.com", Scheme: "https", Ports: store.NewJSON([]int{2053})},
	} {
		if err := db.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	report := func(source string, port int, ips ...string) {
		t.Helper()
		rs := []map[string]any{}
		for _, ip := range ips {
			rs = append(rs, map[string]any{"ip": ip})
		}
		if code, out := admin.do("POST", "/cdn-profiles/cf443/preferred-ips", map[string]any{"source": source, "scheme": "https", "port": port, "ips": rs}); code != 200 {
			t.Fatalf("report %d: %d %v", port, code, out)
		}
	}
	report("test443", 443, "104.16.1.1", "104.16.1.2")
	report("test8443", 8443, "104.16.2.1", "104.16.2.2")
	get := func(id string) store.CDNProfile {
		t.Helper()
		var p store.CDNProfile
		if err := db.First(&p, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := get("cf443").PreferredIPs.V; fmt.Sprint(got) != "[104.16.1.1 104.16.1.2]" {
		t.Fatalf("443 received 8443 data: %v", got)
	}
	if got := get("cf8443").PreferredIPs.V; fmt.Sprint(got) != "[104.16.2.1 104.16.2.2]" {
		t.Fatalf("8443 results missing: %v", got)
	}
	if got := get("cf2053").PreferredIPs.V; len(got) != 0 {
		t.Fatalf("unmeasured 2053 inherited results: %v", got)
	}
	db.Create(&store.Node{ID: "n1", Name: "n1", Status: "online"})
	db.Create(&store.Inbound{ID: "i1", NodeID: "n1", Tag: "ws8443", Port: 8443, Enabled: true, Core: model.CoreXray, Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecTLS})})
	db.Create(&store.Endpoint{ID: "e1", InboundID: "i1", Kind: "cdn", CDNProfileID: "cf443"})
	stop := fakeProber(t, s, "n1", map[string]string{"104.16.1.1:443": "10", "104.16.2.2:8443": "10"})
	s.checkCDNIPsFromNodes()
	p := get("cf443")
	if !slices.Equal(p.PortChecks.V["443"].BadIPs, []string{"104.16.1.2"}) || !slices.Equal(p.PortChecks.V["8443"].BadIPs, []string{"104.16.2.1"}) {
		t.Fatalf("port checks mixed: %+v", p.PortChecks.V)
	}
	m := model.Inbound{ID: "i1", NodeID: "n1", Port: 8443, Transport: model.TransportSpec{Type: model.TransWS}, Security: model.SecuritySpec{Type: model.SecTLS}}
	nodes := s.expandEndpoint(sub.Node{}, store.Endpoint{Kind: "cdn", CDNProfileID: p.ID}, map[string]string{}, "{kind}", nil, nil, &m, "")
	if len(nodes) != 1 || nodes[0].Server != "104.16.2.2" || nodes[0].Port != 8443 {
		t.Fatalf("8443 subscription did not use matching checks: %+v", nodes)
	}
	if got := usableCDNIPsAt(&p, p.PreferredIPs.V, 2053); len(got) != 2 {
		t.Fatalf("443 verdict leaked into unmeasured port: %v", got)
	}
	stop()
	stop = fakeProber(t, s, "n1", map[string]string{})
	defer stop()
	s.checkCDNIPsFromNodes()
	p = get("cf443")
	if !p.PortChecks.V["8443"].AllFailed {
		t.Fatal("all-unreachable port shown as healthy")
	}
	// Retiring an 8443 report must not remove the same address from an
	// unrelated port's manually retained list.
	db.Model(&store.CDNProfile{}).Where("id = ?", "cf2053").Update("preferred_ips", store.NewJSON([]string{"104.16.2.1"}))
	if code, out := admin.do("DELETE", "/cdn-profiles/cf443/ip-reports/test8443", nil); code != 204 {
		t.Fatalf("retire: %d %v", code, out)
	}
	if got := get("cf2053").PreferredIPs.V; !slices.Equal(got, []string{"104.16.2.1"}) {
		t.Fatalf("retired another port's address: %v", got)
	}
	if got := get("cf8443").PreferredIPs.V; len(got) != 0 {
		t.Fatalf("retired port kept stale report addresses: %v", got)
	}
}

func TestCDNPairedMeasurementPort(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.st.DB.Create(&store.CDNProfile{ID: "cf", Name: "cf", Provider: "cloudflare", Scheme: "https", Ports: store.NewJSON([]int{8443})})
	code, out := admin.do("POST", "/cdn-profiles/cf/probes", map[string]any{"source": "test", "port": 8443})
	if code != 201 {
		t.Fatalf("pair: %d %v", code, out)
	}
	probe := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: out["token"].(string)}
	payload := map[string]any{"source": "test", "ips": []map[string]any{{"ip": "104.16.1.1"}}}
	if code, out := probe.do("POST", "/cdn-profiles/cf/preferred-ips", payload); code != 200 {
		t.Fatalf("legacy program on bound port: %d %v", code, out)
	}
	var report store.CDNIPReport
	s.st.DB.First(&report, "profile_id = ?", "cf")
	if report.Port != 8443 || report.Scheme != "https" {
		t.Fatalf("bound port not recorded: %+v", report)
	}
	payload["port"] = 443
	if code, _ := probe.do("POST", "/cdn-profiles/cf/preferred-ips", payload); code != 403 {
		t.Fatalf("bound token wrote another port: %d", code)
	}
}

func TestCDNRejectsImplicitOriginPortRewrite(t *testing.T) {
	p := &store.CDNProfile{Provider: "cloudflare", Ports: store.NewJSON([]int{443})}
	m := &model.Inbound{Port: 31000, Transport: model.TransportSpec{Type: model.TransWS}, Security: model.SecuritySpec{Type: model.SecTLS}}
	if got := cdnProblem(m, p, 0, false); !strings.Contains(got, "不能自动映射") {
		t.Fatalf("implicit rewrite accepted: %s", got)
	}
	if got := cdnProblem(m, p, 443, false); got != "" {
		t.Fatalf("explicit external rule rejected: %s", got)
	}
	if got := cdnProblem(m, p, 0, true); got != "" {
		t.Fatalf("managed Caddy rejected: %s", got)
	}
	p.TLSMode = "flexible"
	m.Port = 80
	m.Security.Type = model.SecNone
	if got := cdnProblem(m, p, 0, false); got != "" {
		t.Fatalf("native Flexible 443 -> 80 rejected: %s", got)
	}
	m.Port = 8443
	m.Security.Type = model.SecTLS
	if got := cdnProblem(m, p, 0, false); got != "" {
		t.Fatalf("Flexible non-443 Full fallback rejected: %s", got)
	}
}

func TestCDNWebSocketAcceptRFC(t *testing.T) {
	if got := cdnWebSocketAccept("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("RFC 6455 accept = %q", got)
	}
}
