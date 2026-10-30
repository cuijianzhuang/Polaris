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

func TestCDNSchemePortsAndSubscriptions(t *testing.T) {
	s, _, admin := newAdminClient(t)
	body := map[string]any{"name": "http", "originDomain": "cdn.example.com", "scheme": "http"}
	code, out := admin.do("POST", "/cdn-profiles", body)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["id"].(string)
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", id)
	if p.Scheme != "http" || !slices.Equal(p.Ports.V, cloudflareHTTPPorts) {
		t.Fatalf("HTTP defaults: %+v", p)
	}
	for _, port := range cloudflareHTTPPorts {
		m := model.Inbound{Port: port, Transport: model.TransportSpec{Type: model.TransWS}, Security: model.SecuritySpec{Type: model.SecNone}}
		if problem := cdnProblem(&m, &p, 0, false); problem != "" {
			t.Errorf("port %d: %s", port, problem)
		}
		nodes := s.expandEndpoint(sub.Node{Port: port}, store.Endpoint{Kind: "cdn", CDNProfileID: id}, map[string]string{}, "{kind}", nil, nil, &m, "")
		if len(nodes) != 1 || nodes[0].Port != port || nodes[0].TLS.Type != model.SecNone || nodes[0].Transport.Host != "cdn.example.com" {
			t.Fatalf("HTTP subscription on %d: %+v", port, nodes)
		}
		target := s.cdnTargetFor(&store.Endpoint{}, &m, &p)
		if target.Scheme != "http" || target.Port != port {
			t.Fatalf("HTTP probe target: %+v", target)
		}
		for _, badPort := range cloudflareHTTPSPorts {
			if cdnProblem(&m, &p, badPort, false) == "" {
				t.Errorf("HTTP accepts HTTPS port %d", badPort)
			}
		}
	}
	for _, scheme := range []string{"http", "https", ""} {
		ports := cloudflareHTTPSPorts
		if scheme == "http" {
			ports = cloudflareHTTPPorts
		}
		req := cdnReq{Name: "x", OriginDomain: "cdn.example.com", Scheme: scheme, Ports: ports}
		if err := req.normalize(); err != nil {
			t.Fatal(err)
		}
		wrongPort := 80
		if scheme == "http" {
			wrongPort = 443
		}
		req.Ports = []int{wrongPort}
		if err := req.normalize(); err == nil {
			t.Fatalf("mixed scheme port accepted: %+v", req)
		}
	}
	// Old clients preserve a stored HTTP scheme when editing its other fields.
	if code, out := admin.do("PUT", "/cdn-profiles/"+id, map[string]any{"name": "renamed", "originDomain": "cdn.example.com", "ports": []int{8080}}); code != 200 || out["scheme"] != "http" {
		t.Fatalf("legacy edit: %d %v", code, out)
	}
	// Scheme is explicit, not inferred from Flexible (which still uses HTTPS clients).
	plain := &model.Inbound{Port: 8080, Transport: model.TransportSpec{Type: model.TransWS}}
	tls := *plain
	tls.Security.Type = model.SecTLS
	httpP := &store.CDNProfile{Scheme: "http", Provider: "cloudflare"}
	httpsP := &store.CDNProfile{Scheme: "https", Provider: "cloudflare", TLSMode: "full"}
	if cdnProblem(&tls, httpP, 8080, false) == "" || cdnProblem(plain, httpsP, 443, false) == "" {
		t.Fatal("origin TLS mismatch accepted")
	}
	plain.Transport.Type = model.TransGRPC
	if cdnProblem(plain, httpP, 80, false) == "" {
		t.Fatal("Cloudflare gRPC over HTTP accepted")
	}
	req := cdnReq{Name: "bad", OriginDomain: "cdn.example.com", Scheme: "http", CaddyFronting: true}
	if err := req.normalize(); err == nil {
		t.Fatal("HTTP Caddy accepted")
	}
	req.CaddyFronting = false
	req.Scheme = "ftp"
	if err := req.normalize(); err == nil {
		t.Fatal("unknown scheme accepted")
	}
}

func TestCDNSchemeCreatesPlainSibling(t *testing.T) {
	s, admin := cdnFixture(t)
	s.st.DB.Create(&store.Inbound{ID: "source", NodeID: "n1", Tag: "source", Port: 10001, Core: model.CoreXray, Enabled: true,
		Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP})})
	code, out := admin.do("POST", "/cdn-profiles", map[string]any{"name": "HTTP", "scheme": "http", "originDomain": "http.example.com", "ports": []int{8080}})
	if code != 201 {
		t.Fatalf("profile: %d %v", code, out)
	}
	profile := out["id"].(string)
	code, out = admin.do("POST", "/inbounds/source/cdn-sibling", map[string]any{"cdnProfileId": profile})
	if code != 201 || out["port"] != float64(8080) {
		t.Fatalf("sibling: %d %v", code, out)
	}
	id := out["inbound"].(map[string]any)["id"].(string)
	var row store.Inbound
	s.st.DB.First(&row, "id = ?", id)
	if row.Security.V.Type != model.SecNone || row.Security.V.CertificateID != "" {
		t.Fatalf("HTTP sibling has TLS: %+v", row.Security.V)
	}
	// Changing a live HTTP origin to HTTPS / Full must not silently break it.
	code, out = admin.do("PUT", "/cdn-profiles/"+profile, map[string]any{"name": "HTTP", "scheme": "https", "originDomain": "http.example.com", "ports": []int{443}})
	if code != 400 || !strings.Contains(fmt.Sprint(out), "source-cdn") {
		t.Fatalf("unsafe switch: %d %v", code, out)
	}
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", profile)
	if p.Scheme != "http" {
		t.Fatal("rejected switch mutated scheme")
	}
	code, _ = admin.do("POST", "/cdn-profiles/"+profile+"/probes", map[string]any{"source": "fixture"})
	if code != 400 {
		t.Fatalf("HTTPS-only installer allowed on HTTP: %d", code)
	}
	port := s.findFreePort("n1", &model.Inbound{Transport: model.TransportSpec{Type: model.TransWS}}, map[int]bool{80: true}, 80, true, &p)
	if port != 8880 {
		t.Fatalf("HTTP template port: %d", port)
	}
}

func TestProbeCDNHTTP(t *testing.T) {
	var redirect atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || r.Host != "cdn.example.com" || r.URL.Path != "/ws" {
			t.Errorf("wrong HTTP handshake: %+v", r)
		}
		w.Header().Set("Server", "cloudflare")
		if redirect.Load() {
			w.Header().Set("Location", "https://cdn.example.com/ws")
			w.WriteHeader(301)
			return
		}
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(101)
	}))
	defer srv.Close()
	host, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(ps)
	target := cdnTarget{Scheme: "http", Server: host, Port: port, SNI: host, Host: "cdn.example.com", Path: "/ws"}
	result := probeCDN(context.Background(), target, model.TransWS, "")
	if !result.OK || result.Status != 101 || !strings.Contains(strings.Join(result.Steps, " "), "HTTP 连上") {
		t.Fatalf("HTTP handshake: %+v", result)
	}
	redirect.Store(true)
	result = probeCDN(context.Background(), target, model.TransWS, "")
	if result.OK || !strings.Contains(result.Message, "重定向") {
		t.Fatalf("HTTPS redirect: %+v", result)
	}
}

func TestCDNReportsDoNotMixSchemes(t *testing.T) {
	s, _, admin := newAdminClient(t)
	for _, p := range []store.CDNProfile{
		{ID: "https", Name: "HTTPS", Provider: "cloudflare", Scheme: "https", PreferredIPs: store.NewJSON([]string{"104.16.0.1"})},
		{ID: "http1", Name: "HTTP1", Provider: "cloudflare", Scheme: "http", PreferredIPs: store.NewJSON([]string{"104.16.0.2"})},
		{ID: "http2", Name: "HTTP2", Provider: "cloudflare", Scheme: "http", PreferredIPs: store.NewJSON([]string{"keep.example.com"}), PinnedHosts: store.NewJSON([]string{"keep.example.com"})},
	} {
		if err := s.st.DB.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	report := func(id, ip string) {
		t.Helper()
		if code, out := admin.do("POST", "/cdn-profiles/"+id+"/preferred-ips", map[string]any{"source": "fixture", "ips": []map[string]any{{"ip": ip}}}); code != 200 {
			t.Fatalf("report: %d %v", code, out)
		}
	}
	get := func(id string) store.CDNProfile {
		t.Helper()
		var p store.CDNProfile
		s.st.DB.First(&p, "id = ?", id)
		return p
	}
	report("http1", "104.17.0.1")
	if got := fmt.Sprint(get("https").PreferredIPs.V); got != "[104.16.0.1]" {
		t.Fatalf("HTTP overwrote HTTPS: %s", got)
	}
	if got := fmt.Sprint(get("http2").PreferredIPs.V); got != "[104.17.0.1 keep.example.com]" {
		t.Fatalf("same-scheme sharing: %s", got)
	}
	report("https", "104.18.0.1")
	if got := fmt.Sprint(get("http1").PreferredIPs.V); got != "[104.17.0.1]" {
		t.Fatalf("HTTPS overwrote HTTP: %s", got)
	}
	if code, out := admin.do("PUT", "/cdn-profiles/http1", map[string]any{"name": "HTTP1", "scheme": "https", "originDomain": "cdn.example.com", "ports": []int{443}}); code != 400 || !strings.Contains(fmt.Sprint(out), "测速点") {
		t.Fatalf("scheme reused old reports: %d %v", code, out)
	}
	code, list := admin.doList("GET", "/cdn-profiles")
	if code != 200 {
		t.Fatal(code)
	}
	for _, v := range list {
		row := v.(map[string]any)
		if row["id"] == "https" && fmt.Sprint(row["shared"]) != "[]" {
			t.Fatalf("HTTPS sees HTTP report: %v", row)
		}
	}
}
