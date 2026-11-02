package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
	"gopkg.in/yaml.v3"
)

const testTunnelUUID = "12345678-abcd-1234-abcd-123456789abc"

func tunnelFixture(t *testing.T) (*Server, *apiClient) {
	t.Helper()
	s, a := cdnFixture(t)
	if err := s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "cf").Updates(map[string]any{"origin_mode": "tunnel", "tunnel_id": testTunnelUUID}).Error; err != nil {
		t.Fatal(err)
	}
	return s, a
}
func tunnelInboundRequest() map[string]any {
	return map[string]any{"nodeId": "n1", "core": "xray", "tag": "tunnel-ws", "listen": "127.0.0.1", "port": 18080, "protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "ws", "path": "/ws"}, "security": map[string]any{"type": "none"}, "cdnProfileId": "cf", "cdnOnly": true}
}
func addTunnelInbound(t *testing.T, s *Server, a *apiClient) store.Inbound {
	t.Helper()
	if code, out := a.do("POST", "/inbounds", tunnelInboundRequest()); code != 201 {
		t.Fatal(code, out)
	}
	var in store.Inbound
	if err := s.st.DB.First(&in, "tag = ?", "tunnel-ws").Error; err != nil {
		t.Fatal(err)
	}
	return in
}

func TestCDNTunnelValidation(t *testing.T) {
	_, a := tunnelFixture(t)
	profile := map[string]any{"name": "tunnel2", "originDomain": "tunnel.example.com", "provider": "cloudflare", "scheme": "https", "originMode": "tunnel", "tunnelId": "22345678-abcd-1234-abcd-123456789abc", "ports": []int{443}}
	for _, change := range []map[string]any{{"tunnelId": "secret-token"}, {"scheme": "http"}, {"provider": "other"}, {"ports": []int{2053}}, {"caddyFronting": true}, {"originMode": "unknown"}, {"tunnelId": testTunnelUUID}} {
		body := map[string]any{}
		for k, v := range profile {
			body[k] = v
		}
		for k, v := range change {
			body[k] = v
		}
		if code, out := a.do("POST", "/cdn-profiles", body); code != 400 {
			t.Fatal("invalid tunnel accepted", change, code, out)
		}
	}
	if code, out := a.do("POST", "/cdn-profiles", profile); code != 201 {
		t.Fatal(code, out)
	}
	for _, change := range []map[string]any{{"listen": "0.0.0.0"}, {"cdnOnly": false}, {"security": map[string]any{"type": "tls", "serverName": "cdn.example.com"}}, {"transport": map[string]any{"type": "grpc", "serviceName": "grpc"}}} {
		body := tunnelInboundRequest()
		for k, v := range change {
			body[k] = v
		}
		if code, out := a.do("POST", "/inbounds", body); code != 400 {
			t.Fatal("invalid origin accepted", change, code, out)
		}
	}
	body := tunnelInboundRequest()
	body["extra"] = map[string]any{"listen": "0.0.0.0"}
	if code, out := a.do("POST", "/inbounds", body); code != 400 {
		t.Fatal("unsafe extra accepted", code, out)
	}
}

func TestCDNTunnelEntrySubscriptionAndConfig(t *testing.T) {
	s, a := tunnelFixture(t)
	in := addTunnelInbound(t, s, a)
	s.st.DB.Create(&store.InboundHealth{InboundID: in.ID, OK: false, Error: "old public probe failed"})
	if code, out := a.do("GET", "/inbounds/"+in.ID, nil); code != 200 || out["health"] != nil {
		t.Fatal("loopback origin shows stale public health", code, out)
	}
	code, out := a.do("GET", "/cdn-profiles/cf/tunnel-config", nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	var cfg struct {
		Tunnel      string           `yaml:"tunnel"`
		Credentials string           `yaml:"credentials-file"`
		Ingress     []cdnTunnelRoute `yaml:"ingress"`
	}
	if err := yaml.Unmarshal([]byte(out["config"].(string)), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Tunnel != testTunnelUUID || cfg.Credentials != "/etc/cloudflared/"+testTunnelUUID+".json" || len(cfg.Ingress) != 2 || cfg.Ingress[0].Service != "http://127.0.0.1:18080" || cfg.Ingress[0].Path != "^/ws$" || cfg.Ingress[1].Service != "http_status:404" {
		t.Fatal("bad cloudflared config", out)
	}
	if strings.Contains(out["config"].(string), "203.0.113.5") {
		t.Fatal("origin IP leaked into config")
	}
	if bin := os.Getenv("POLARIS_CLOUDFLARED_BIN"); bin != "" {
		path := filepath.Join(t.TempDir(), "config.yml")
		if err := os.WriteFile(path, []byte(out["config"].(string)), 0600); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command(bin, "tunnel", "--config", path, "ingress", "validate").CombinedOutput(); err != nil {
			t.Fatalf("cloudflared rejected generated config: %v %s", err, output)
		}
	}
	var ep store.Endpoint
	s.st.DB.First(&ep, "inbound_id = ?", in.ID)
	m := s.toModelInbound(&in)
	nodes := s.expandEndpoint(sub.Node{Port: m.Port, Transport: m.Transport, TLS: model.ClientTLS{Type: model.SecNone}}, ep, nil, "{kind}", nil, nil, &m, "")
	if len(nodes) != 1 || nodes[0].Port != 443 || nodes[0].Server != "cdn.example.com" || nodes[0].TLS.Type != model.SecTLS || nodes[0].TLS.ServerName != "cdn.example.com" {
		t.Fatalf("bad tunnel subscription: %+v", nodes)
	}
	spec, err := s.BuildSpec(in.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Fronts) != 0 || len(spec.Inbounds) != 1 || spec.Inbounds[0].Listen != "127.0.0.1" || strings.Join(spec.Inbounds[0].Transport.TrustedForwardedFor, ",") != "CF-Connecting-IP" {
		t.Fatalf("unsafe tunnel spec: %+v", spec)
	}
	for _, kind := range []string{"direct", "relay"} {
		if code, out := a.do("POST", "/inbounds/"+in.ID+"/endpoints", map[string]any{"kind": kind}); code != 400 {
			t.Fatal("public endpoint accepted", code, out)
		}
	}
	body := tunnelInboundRequest()
	body["listen"] = "::"
	if code, out := a.do("PUT", "/inbounds/"+in.ID, body); code != 400 {
		t.Fatal("public listener accepted", code, out)
	}
	// One tunnel must not load-balance between unrelated node configurations.
	s.st.DB.Create(&store.Node{ID: "n2", Name: "other", DefaultCore: model.CoreXray})
	s.st.DB.Create(&store.Core{ID: "c2", NodeID: "n2", Type: model.CoreXray, Version: "26.3.27"})
	body = tunnelInboundRequest()
	body["nodeId"] = "n2"
	body["tag"] = "other"
	if code, out := a.do("POST", "/inbounds", body); code != 400 {
		t.Fatal("shared tunnel across nodes accepted", code, out)
	}
	// Legacy clients preserve the new routing mode when saving other settings.
	body = map[string]any{"name": "legacy save", "originDomain": "cdn.example.com", "ports": []int{443}}
	if code, out := a.do("PUT", "/cdn-profiles/cf", body); code != 200 || out["originMode"] != "tunnel" || out["tunnelId"] != testTunnelUUID {
		t.Fatal("legacy save lost tunnel mode", code, out)
	}
	// Removing the final tunnel entry must not generate a direct subscription.
	if code, out := a.do("DELETE", "/inbounds/"+in.ID+"/endpoints/"+ep.ID, nil); code != 204 {
		t.Fatal(code, out)
	}
	s.st.DB.First(&in, "id = ?", in.ID)
	if in.Enabled {
		t.Fatal("last tunnel entry removal left direct fallback enabled")
	}
}

func TestCDNTunnelConfigRejectsRouteCollision(t *testing.T) {
	s, a := tunnelFixture(t)
	in := addTunnelInbound(t, s, a)
	other := in
	other.ID = "collision"
	other.Tag = "collision"
	other.Port++
	s.st.DB.Create(&other)
	s.st.DB.Create(&store.Endpoint{ID: "collision-ep", InboundID: other.ID, Kind: "cdn", CDNProfileID: "cf"})
	if code, out := a.do("GET", "/cdn-profiles/cf/tunnel-config", nil); code != 400 {
		t.Fatal("ambiguous origin accepted", code, out)
	}
	// A manually imported public listener may never gain forwarded-header trust.
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", in.ID).Update("listen", "::")
	if _, err := s.BuildSpec(in.NodeID); err == nil {
		t.Fatal("unsafe restored listener accepted")
	}
}

func TestCDNTunnelConcurrentNodeAttachment(t *testing.T) {
	s, a := tunnelFixture(t)
	s.st.DB.Create(&store.Node{ID: "n2", Name: "other", DefaultCore: model.CoreXray})
	s.st.DB.Create(&store.Core{ID: "c2", NodeID: "n2", Type: model.CoreXray, Version: "26.3.27"})
	start := make(chan struct{})
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, node := range []string{"n1", "n2"} {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			body := tunnelInboundRequest()
			body["nodeId"] = node
			body["tag"] = node
			<-start
			code, _ := a.do("POST", "/inbounds", body)
			codes <- code
		}(node)
	}
	close(start)
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[201] != 1 || counts[400] != 1 {
		t.Fatal("concurrent tunnel binding was not exclusive", counts)
	}
	var p store.CDNProfile
	s.st.DB.First(&p, "id = ?", "cf")
	if err := validateCDNTunnel(s.st.DB, &p); err != nil {
		t.Fatal(err)
	}
}

func TestCDNTunnelDNSUsesCNAMEAndRestoresOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name, ip string
		fail     bool
	}{{"success", "203.0.113.5", false}, {"unrelated", "198.51.100.1", false}, {"rollback", "203.0.113.5", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, a := tunnelFixture(t)
			addTunnelInbound(t, s, a)
			records := []map[string]any{{"id": "old", "type": "A", "content": tc.ip, "proxied": true}}
			writes := []string{}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/zones/z/dns_records") {
					t.Error("touched SSL or credentials", r.URL.Path)
					w.WriteHeader(500)
					return
				}
				result := any([]map[string]any{})
				switch r.Method {
				case "GET":
					found := []map[string]any{}
					for _, record := range records {
						if r.URL.Query().Get("type") == "" || r.URL.Query().Get("type") == record["type"] {
							found = append(found, record)
						}
					}
					result = found
				case "DELETE":
					writes = append(writes, "DELETE")
					records = nil
				case "POST", "PUT":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					writes = append(writes, fmt.Sprint(body["type"]))
					if body["type"] == "CNAME" && tc.fail {
						json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"Message": "fixture refused"}}})
						return
					}
					if body["type"] == "CNAME" && (body["content"] != testTunnelUUID+".cfargotunnel.com" || body["proxied"] != true) {
						t.Error("wrong tunnel CNAME", body)
					}
					body["id"] = "new"
					records = append(records, body)
					result = body
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
			}))
			defer api.Close()
			var p store.CDNProfile
			s.st.DB.First(&p, "id = ?", "cf")
			res, err := s.syncCDNTunnel(context.Background(), &cfClient{token: "fixture", base: api.URL, http: api.Client()}, "z", &p)
			if tc.name == "success" {
				if err != nil || len(res.Records) != 1 || strings.Join(writes, ",") != "DELETE,CNAME" {
					t.Fatal(res, err, writes)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
			if tc.name == "unrelated" && len(writes) != 0 {
				t.Fatal("modified unrelated record", writes)
			}
			if tc.fail && (len(records) != 1 || records[0]["type"] != "A" || records[0]["content"] != tc.ip) {
				t.Fatal("original record was not restored", records, writes)
			}
		})
	}
}
