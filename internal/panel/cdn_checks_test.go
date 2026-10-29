package panel

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestCDNProblem(t *testing.T) {
	cf := &store.CDNProfile{Provider: "cloudflare", Ports: store.NewJSON([]int{443})}
	flexible := &store.CDNProfile{Provider: "cloudflare", TLSMode: "flexible", Ports: store.NewJSON([]int{443})}
	in := func(tr model.Transport, sec model.Security, port int) *model.Inbound {
		return &model.Inbound{Port: port, Transport: model.TransportSpec{Type: tr}, Security: model.SecuritySpec{Type: sec}}
	}
	grpc := func(port int) *model.Inbound { return in(model.TransGRPC, model.SecTLS, port) }
	for i, c := range []struct {
		m       *model.Inbound
		p       *store.CDNProfile
		epPort  int
		fronted bool
		bad     bool
	}{
		{grpc(443), cf, 0, false, false},
		{grpc(2087), cf, 0, false, true},    // CF keeps the client on 2087
		{grpc(31000), cf, 0, false, false},  // not a CF port: clients use the profile's 443
		{grpc(2087), cf, 443, false, false}, // endpoint override
		{grpc(2087), cf, 0, true, false},    // Caddy on 443
		{grpc(2087), &store.CDNProfile{Provider: "other", Ports: store.NewJSON([]int{443})}, 0, false, false},
		{in(model.TransWS, model.SecTLS, 2087), cf, 0, false, false},
		{in(model.TransWS, model.SecTLS, 2087), cf, 8080, false, true},    // plain-HTTP port, clients use TLS
		{in(model.TransWS, model.SecNone, 2087), cf, 0, false, true},      // Full: CF talks HTTPS to a plain listener
		{in(model.TransWS, model.SecNone, 2087), cf, 0, true, true},       // Caddy needs the inbound's certificate
		{in(model.TransWS, model.SecTLS, 2087), flexible, 0, false, true}, // Flexible: CF talks HTTP to a TLS listener
		{in(model.TransWS, model.SecNone, 2087), flexible, 0, false, false},
		{in(model.TransWS, model.SecNone, 2087), &store.CDNProfile{Provider: "other"}, 0, false, false},
	} {
		if got := cdnProblem(c.m, c.p, c.epPort, c.fronted) != ""; got != c.bad {
			t.Errorf("case %d: problem=%v (%q)", i, got, cdnProblem(c.m, c.p, c.epPort, c.fronted))
		}
	}
	if !covers("*.example.com", "cdn.example.com") || covers("*.example.com", "a.b.example.com") || covers("n1.example.com", "cdn.example.com") {
		t.Fatal("covers")
	}
}

func cdnFixture(t *testing.T) (*Server, *apiClient) {
	t.Helper()
	s, _, admin := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "hk", Address: "203.0.113.5", DefaultCore: model.CoreXray})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	db.Create(&store.CDNProfile{ID: "cf", Name: "cf-main", Provider: "cloudflare", OriginDomain: "cdn.example.com", TLSMode: "full_strict",
		Ports: store.NewJSON([]int{443}), PreferredIPs: store.NewJSON([]string{})})
	return s, admin
}

func grpcInboundReq(port int, extra map[string]any) map[string]any {
	r := map[string]any{"nodeId": "n1", "core": "xray", "tag": "g" + string(rune('0'+port%10)), "port": port,
		"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "grpc", "serviceName": "svc"},
		"security": map[string]any{"type": "tls", "serverName": "cdn.example.com"}}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

// A new inbound can be published through a CDN as it is created, and one
// clients could not reach through the CDN is refused with the reason.
func TestCreateInboundWithCDN(t *testing.T) {
	s, admin := cdnFixture(t)
	db := s.st.DB
	code, out := admin.do("POST", "/inbounds", grpcInboundReq(2087, map[string]any{"cdnProfileId": "cf", "cdnOnly": true}))
	if code != 400 || !strings.Contains(out["error"].(string), "443") {
		t.Fatalf("gRPC on 2087 through Cloudflare: %d %v", code, out)
	}
	var n int64
	db.Model(&store.Inbound{}).Count(&n)
	if n != 0 {
		t.Fatal("refused inbound was stored")
	}
	code, out = admin.do("POST", "/inbounds", grpcInboundReq(443, map[string]any{"cdnProfileId": "cf", "cdnOnly": true}))
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["inbound"].(map[string]any)["id"].(string)
	var eps []store.Endpoint
	db.Where("inbound_id = ?", id).Find(&eps)
	if len(eps) != 1 || eps[0].Kind != "cdn" || eps[0].CDNProfileID != "cf" {
		t.Fatalf("cdn-only endpoints %+v", eps)
	}
	// Without cdnOnly the direct entry stays.
	code, out = admin.do("POST", "/inbounds", grpcInboundReq(8443, map[string]any{"tag": "ws", "transport": map[string]any{"type": "ws", "path": "/x"}, "cdnProfileId": "cf"}))
	if code != 201 {
		t.Fatalf("ws create: %d %v", code, out)
	}
	eps = nil
	db.Where("inbound_id = ?", out["inbound"].(map[string]any)["id"]).Order("kind").Find(&eps)
	if len(eps) != 2 || eps[0].Kind != "cdn" || eps[1].Kind != "direct" {
		t.Fatalf("endpoints %+v", eps)
	}
	// Adding a cdn endpoint later goes through the same check.
	code, out = admin.do("POST", "/inbounds", grpcInboundReq(2083, map[string]any{"tag": "g2083"}))
	if code != 201 {
		t.Fatalf("plain create: %d %v", code, out)
	}
	gid := out["inbound"].(map[string]any)["id"].(string)
	if code, out := admin.do("POST", "/inbounds/"+gid+"/endpoints", map[string]any{"kind": "cdn", "cdnProfileId": "cf"}); code != 400 {
		t.Fatalf("cdn endpoint for gRPC on 2083: %d %v", code, out)
	}
	if code, out := admin.do("POST", "/inbounds/"+gid+"/endpoints", map[string]any{"kind": "cdn", "cdnProfileId": "cf", "port": 443}); code != 201 && code != 200 {
		t.Fatalf("cdn endpoint with port 443 override: %d %v", code, out)
	}
}

// Template preview says which CDN entries are added, skips a CDN-only item
// that would otherwise be published as direct, and warns about gRPC ports
// and Full (strict) certificates.
func TestTemplatePreviewCDN(t *testing.T) {
	s, admin := cdnFixture(t)
	ws := store.TemplateItem{Key: "ws", Core: model.CoreAuto, Protocol: model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport: model.TransportSpec{Type: model.TransWS, Path: "/{rand.path}"}, Security: model.SecuritySpec{Type: model.SecTLS, ServerName: "cdn.example.com"},
		Port: store.PortPolicy{Mode: "fixed", Fixed: 2053}, Cert: "selfsigned", TagPattern: "{node.name}-ws", Endpoints: []store.EndpointTemplate{{Kind: "cdn"}}}
	grpc := store.TemplateItem{Key: "grpc", Core: model.CoreAuto, Protocol: model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport: model.TransportSpec{Type: model.TransGRPC}, Security: model.SecuritySpec{Type: model.SecTLS, ServerName: "hk.example.com", ALPN: []string{"h2"}},
		Port: store.PortPolicy{Mode: "fixed", Fixed: 2087}, Cert: "selfsigned", TagPattern: "{node.name}-grpc", Endpoints: []store.EndpointTemplate{{Kind: "direct"}, {Kind: "cdn"}}}
	code, out := admin.do("POST", "/templates", templateReq{Name: "cdn", Items: []store.TemplateItem{ws, grpc}})
	if code != 201 {
		t.Fatalf("template: %d %v", code, out)
	}
	id := out["id"].(string)
	items := func(opt map[string]any) map[string]map[string]any {
		got := map[string]map[string]any{}
		plans := previewFixture(t, admin, id, opt)["plans"].([]any)
		for _, it := range plans[0].(map[string]any)["items"].([]any) {
			m := it.(map[string]any)
			got[m["itemKey"].(string)] = m
		}
		return got
	}
	// No profile chosen: the CDN-only item is skipped instead of going direct.
	got := items(map[string]any{"nodeIds": []string{"n1"}})
	if got["ws"]["result"] != "skip" || !strings.Contains(got["ws"]["reason"].(string), "未选配置档") {
		t.Fatalf("cdn-only without profile: %v", got["ws"])
	}
	if got["grpc"]["result"] != "new" || got["grpc"]["entries"] != "直连 + CDN（未选配置档，不添加）" {
		t.Fatalf("grpc without profile: %v", got["grpc"])
	}
	got = items(map[string]any{"nodeIds": []string{"n1"}, "cdnProfileId": "cf"})
	if got["ws"]["result"] != "new" || got["ws"]["entries"] != "CDN · cf-main" || got["ws"]["warnings"] != nil {
		t.Fatalf("ws with profile: %v", got["ws"])
	}
	g := got["grpc"]
	if g["entries"] != "直连 + CDN（不添加）" || !strings.Contains(g["warnings"].([]any)[0].(string), "443") {
		t.Fatalf("grpc on 2087: %v", g)
	}

	// Apply leaves out the endpoint the preview said it would not add.
	opt := map[string]any{"nodeIds": []string{"n1"}, "cdnProfileId": "cf"}
	opt["planId"] = previewFixture(t, admin, id, opt)["planId"]
	if code, out := admin.do("POST", "/templates/"+id+"/apply", opt); code != 200 {
		t.Fatalf("apply: %d %v", code, out)
	}
	var rows []store.Inbound
	s.st.DB.Where("node_id = ?", "n1").Find(&rows)
	kinds := map[string][]string{}
	for _, r := range rows {
		var eps []store.Endpoint
		s.st.DB.Where("inbound_id = ?", r.ID).Order("kind").Find(&eps)
		for _, e := range eps {
			kinds[r.Tag] = append(kinds[r.Tag], e.Kind)
		}
	}
	if strings.Join(kinds["hk-ws"], ",") != "cdn" || strings.Join(kinds["hk-grpc"], ",") != "direct" {
		t.Fatalf("endpoints after apply: %v", kinds)
	}
}

func TestFindFreePortForCDN(t *testing.T) {
	s, _ := cdnFixture(t)
	g := &model.Inbound{Transport: model.TransportSpec{Type: model.TransGRPC}}
	if p := s.findFreePort("n1", g, map[int]bool{}, 2087, true); p != 443 {
		t.Fatalf("gRPC through CDN: %d", p)
	}
	w := &model.Inbound{Transport: model.TransportSpec{Type: model.TransWS}}
	if p := s.findFreePort("n1", w, map[int]bool{2053: true}, 31000, true); p != 2083 {
		t.Fatalf("WS through CDN: %d", p)
	}
	if p := s.findFreePort("n1", w, map[int]bool{}, 31000, false); p < 20000 {
		t.Fatalf("direct: %d", p)
	}
}

// The CDN check names the hop that fails from Cloudflare's error code.
func TestProbeCDN(t *testing.T) {
	status := 526
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "cloudflare")
		if r.URL.Path != "/ws" {
			w.WriteHeader(404)
			return
		}
		if status == 101 && r.Header.Get("Upgrade") == "websocket" {
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Connection", "Upgrade")
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	cdnProbeRoots = x509.NewCertPool()
	cdnProbeRoots.AddCert(srv.Certificate())
	defer func() { cdnProbeRoots = nil }()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	pn, _ := strconv.Atoi(port)
	target := cdnTarget{Server: host, Port: pn, SNI: host, Host: "cdn.example.com", Path: "/ws"}
	ctx := context.Background()

	r := probeCDN(ctx, target, model.TransWS, "")
	if r.OK || r.Status != 526 || !strings.Contains(r.Message, "Full (strict)") {
		t.Fatalf("526: %+v", r)
	}
	status = 522
	if r = probeCDN(ctx, target, model.TransWS, ""); r.OK || !strings.Contains(r.Message, "安全组") {
		t.Fatalf("522: %+v", r)
	}
	status = 101
	if r = probeCDN(ctx, target, model.TransWS, ""); !r.OK {
		t.Fatalf("101: %+v", r)
	}
	bad := target
	bad.Path = "/nope"
	if r = probeCDN(ctx, bad, model.TransWS, ""); r.OK || !strings.Contains(r.Message, "路径") {
		t.Fatalf("wrong path: %+v", r)
	}
	closed := target
	closed.Port = 1
	if r = probeCDN(ctx, closed, model.TransWS, ""); r.OK || !strings.Contains(r.Message, "连不上") {
		t.Fatalf("closed port: %+v", r)
	}
}

// The endpoint list shows where CDN clients connect and what keeps them out.
func TestEndpointViews(t *testing.T) {
	s, admin := cdnFixture(t)
	code, out := admin.do("POST", "/inbounds", grpcInboundReq(2083, map[string]any{"tag": "ws", "transport": map[string]any{"type": "ws", "path": "/x"},
		"security": map[string]any{"type": "tls", "serverName": "hk.example.com"}, "cdnProfileId": "cf", "cdnOnly": true}))
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["inbound"].(map[string]any)["id"].(string)
	code, raw := admin.doList("GET", "/inbounds/"+id+"/endpoints")
	if code != 200 || len(raw) != 1 {
		t.Fatalf("list: %d %v", code, raw)
	}
	list := []map[string]any{raw[0].(map[string]any)}
	tg := list[0]["target"].(map[string]any)
	if tg["server"] != "cdn.example.com" || tg["port"] != float64(2083) || tg["path"] != "/x" {
		t.Fatalf("target %v", tg)
	}
	w := list[0]["warnings"].([]any)
	if len(w) != 1 || !strings.Contains(w[0].(string), "hk.example.com") {
		t.Fatalf("warnings %v", w)
	}
	_ = s
}

func TestCDNReqNormalize(t *testing.T) {
	r := cdnReq{Name: " cf ", OriginDomain: "https://CDN.Example.com/ws", Ports: []int{2053, 443, 2053}, PreferredIPs: []string{" 104.16.1.1 ", "", "visa.cn", "[2606:4700::1]"}}
	if err := r.normalize(); err != nil {
		t.Fatal(err)
	}
	if r.OriginDomain != "cdn.example.com" || r.TLSMode != "full_strict" || r.Provider != "cloudflare" || len(r.Ports) != 2 || len(r.PreferredIPs) != 3 {
		t.Fatalf("%+v", r)
	}
	for _, bad := range []cdnReq{
		{OriginDomain: "not a domain"},
		{OriginDomain: "cdn.example.com", Ports: []int{8080}},
		{OriginDomain: "cdn.example.com", Ports: []int{70000}, Provider: "other"},
		{OriginDomain: "cdn.example.com", PreferredIPs: []string{"1.2.3.4:443"}},
		{OriginDomain: "cdn.example.com", TLSMode: "strict"},
	} {
		if err := bad.normalize(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// When Cloudflare cannot reach the node, the panel tries the node's port
// itself to tell "nothing listens / firewall" from "DNS points elsewhere".
func TestDiagnoseOrigin(t *testing.T) {
	s := newTestServer(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens there now
	node := &store.Node{Name: "sg", Address: "127.0.0.1"}
	r := cdnCheck{Message: "错误 521"}
	s.diagnoseOrigin(context.Background(), &r, &store.CDNProfile{OriginDomain: "cdn.example.com"}, node, cdnTarget{SNI: "cdn.example.com", Port: port})
	if !strings.Contains(r.Message, "拒绝") || !strings.Contains(strings.Join(r.Steps, " "), "API Token") {
		t.Fatalf("closed port: %+v", r)
	}
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	_, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ = strconv.Atoi(ps)
	r = cdnCheck{Message: "错误 521"}
	s.diagnoseOrigin(context.Background(), &r, &store.CDNProfile{OriginDomain: "cdn.example.com"}, node, cdnTarget{SNI: "cdn.example.com", Port: port})
	// 127.0.0.1 is this machine: the direct check passed through no firewall.
	if !strings.Contains(r.Message, "同一台机器") {
		t.Fatalf("open port on the panel's machine: %+v", r)
	}
}

// Cloudflare's Origin CA answers expires_on in a non-RFC 3339 layout.
func TestOriginCertResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"errors":[],"result":{"certificate":"-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----","expires_on":"2041-10-24 16:58:00 +0000 UTC"}}`))
	}))
	defer srv.Close()
	cf := newCF("token")
	cf.base = srv.URL
	cert, key, err := cf.originCert(context.Background(), []string{"cdn.example.com"})
	if err != nil || !strings.Contains(cert, "BEGIN CERTIFICATE") || key == "" {
		t.Fatalf("origin cert: %v %q", err, cert)
	}
}

// Source lists may carry preferred domains; typed-in domains survive
// automatic updates and follow the top N in subscriptions.
func TestPreferredDomains(t *testing.T) {
	got := parseIPList(strings.NewReader("# best\n104.16.1.1:443,ok\nVisa.cn\nicook.hk:2053 # note\nIP地址,延迟\n1.2.3\n104.16.1.1\n*.bad.com\n"))
	if strings.Join(got, " ") != "104.16.1.1 visa.cn icook.hk" {
		t.Fatalf("parsed %v", got)
	}
	r := cdnReq{Name: "cf", OriginDomain: "cdn.example.com", PreferredIPs: []string{"104.16.1.1", "visa.cn"}}
	if err := r.normalize(); err != nil {
		t.Fatal(err)
	}
	var p store.CDNProfile
	r.apply(&p, func(s string) string { return s })
	if strings.Join(p.PinnedHosts.V, ",") != "visa.cn" {
		t.Fatalf("pinned %v", p.PinnedHosts.V)
	}
	if l := withPinned([]string{"172.64.1.1", "visa.cn"}, []string{"visa.cn", "icook.hk"}); strings.Join(l, ",") != "172.64.1.1,visa.cn,icook.hk" {
		t.Fatalf("withPinned %v", l)
	}
}

// Profiles without a token of their own use the panel-wide Cloudflare token.
func TestCFTokenFallback(t *testing.T) {
	s, admin := cdnFixture(t)
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", "cf")
	if s.cfToken(&p) != "" {
		t.Fatal("token without any configured")
	}
	s.st.SetSetting("acme_cf_token", s.st.Box.Seal("global"))
	if s.cfToken(&p) != "global" {
		t.Fatal("global token not used")
	}
	code, list := admin.doList("GET", "/cdn-profiles")
	if v := list[0].(map[string]any); code != 200 || v["hasToken"] != true || v["globalToken"] != true {
		t.Fatalf("list %d %v", code, list)
	}
	p.APIToken = s.st.Box.Seal("own")
	if s.cfToken(&p) != "own" {
		t.Fatal("own token not preferred")
	}
}

// Deleting a profile in use lists the inbounds; force removes their CDN
// entries and disables those left with no entry at all.
func TestDeleteCDNProfileInUse(t *testing.T) {
	s, admin := cdnFixture(t)
	db := s.st.DB
	mk := func(tag string, port int, only bool) string {
		code, out := admin.do("POST", "/inbounds", map[string]any{"nodeId": "n1", "core": "xray", "tag": tag, "port": port,
			"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "ws", "path": "/" + tag},
			"security": map[string]any{"type": "tls", "serverName": "cdn.example.com"}, "cdnProfileId": "cf", "cdnOnly": only})
		if code != 201 {
			t.Fatalf("create %s: %d %v", tag, code, out)
		}
		return out["inbound"].(map[string]any)["id"].(string)
	}
	only, both := mk("only", 2053, true), mk("both", 2083, false)
	code, out := admin.do("DELETE", "/cdn-profiles/cf", nil)
	if code != 409 || len(out["usage"].([]any)) != 2 {
		t.Fatalf("in use: %d %v", code, out)
	}
	if code, out = admin.do("DELETE", "/cdn-profiles/cf?force=1", nil); code != 200 || len(out["disabled"].([]any)) != 1 {
		t.Fatalf("force: %d %v", code, out)
	}
	var n int64
	db.Model(&store.CDNProfile{}).Where("id = ?", "cf").Count(&n)
	var a, b store.Inbound
	db.First(&a, "id = ?", only)
	db.First(&b, "id = ?", both)
	var eps []store.Endpoint
	db.Where("inbound_id = ?", both).Find(&eps)
	if n != 0 || a.Enabled || !b.Enabled || len(eps) != 1 || eps[0].Kind != "direct" {
		t.Fatalf("after: profile %d, only.enabled %v, both.enabled %v, eps %+v", n, a.Enabled, b.Enabled, eps)
	}
}
