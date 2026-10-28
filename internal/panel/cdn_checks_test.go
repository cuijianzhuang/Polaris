package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestCDNPortProblem(t *testing.T) {
	cf := &store.CDNProfile{Provider: "cloudflare", Ports: store.NewJSON([]int{443})}
	grpc := func(port int) *model.Inbound {
		return &model.Inbound{Port: port, Transport: model.TransportSpec{Type: model.TransGRPC}, Security: model.SecuritySpec{Type: model.SecTLS}}
	}
	for _, c := range []struct {
		m      *model.Inbound
		p      *store.CDNProfile
		epPort int
		bad    bool
	}{
		{grpc(443), cf, 0, false},
		{grpc(2087), cf, 0, true},    // CF keeps the client on 2087
		{grpc(31000), cf, 0, false},  // not a CF port: clients use the profile's 443
		{grpc(2087), cf, 443, false}, // endpoint override
		{grpc(2087), &store.CDNProfile{Provider: "cloudflare", CaddyFronting: true, Ports: store.NewJSON([]int{443})}, 0, false},
		{grpc(2087), &store.CDNProfile{Provider: "other", Ports: store.NewJSON([]int{443})}, 0, false},
		{&model.Inbound{Port: 2087, Transport: model.TransportSpec{Type: model.TransWS}}, cf, 0, false},
	} {
		if got := cdnPortProblem(c.m, c.p, c.epPort) != ""; got != c.bad {
			t.Errorf("port %d ep %d profile %+v: problem=%v", c.m.Port, c.epPort, c.p, got)
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
