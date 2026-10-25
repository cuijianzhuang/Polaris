package panel

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func diagnoseRelayTest(t *testing.T, c *apiClient, id string) relayDiagnostic {
	t.Helper()
	code, out := c.do("POST", "/inbounds/in/relays/"+id+"/diagnose", nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	b, _ := json.Marshal(out)
	var report relayDiagnostic
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if report.Kind != "network" {
		t.Fatal("missing network diagnostic kind", report)
	}
	if report.BusinessVerified {
		t.Fatal("transport checks claimed proxy authentication")
	}
	return report
}

func diagnosticCheck(t *testing.T, report relayDiagnostic, kind string) relayDiagnosticCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Kind == kind {
			return check
		}
	}
	t.Fatalf("missing %s: %+v", kind, report)
	return relayDiagnosticCheck{}
}

func markRelayApplied(t *testing.T, s *Server, id string) store.Chain {
	t.Helper()
	var ch store.Chain
	if err := s.st.DB.First(&ch, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	for nodeID, rev := range ch.TargetRevs.V {
		if err := s.st.DB.Model(&store.Node{}).Where("id = ?", nodeID).Updates(map[string]any{"desired_rev": rev, "applied_rev": rev}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return ch
}

func TestRelayDiagnosticLayers(t *testing.T) {
	for _, variant := range []string{"tcp", "tls", "tls-fails", "udp", "tunnel-udp", "invalid-result"} {
		t.Run(variant, func(t *testing.T) {
			s, c := relayFixture(t)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
			server := httptest.NewUnstartedServer(handler)
			if variant == "tls" {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			entryPort := server.Listener.Addr().(*net.TCPAddr).Port
			s.st.DB.Model(&store.Node{}).Where("id = ?", "relay").Update("address", "127.0.0.1")
			if strings.HasPrefix(variant, "tls") {
				s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("security", store.NewJSON(model.SecuritySpec{Type: model.SecTLS, ServerName: "localhost"}))
			}
			if strings.Contains(variant, "udp") {
				s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("protocol", store.NewJSON(model.ProtocolSpec{Type: model.ProtoHysteria2}))
			}
			mode := "l4"
			if variant == "tunnel-udp" {
				mode = "tunnel"
			}
			id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"mode": mode, "relays": []any{map[string]any{"nodeId": "relay", "port": entryPort}}}))
			ch := markRelayApplied(t, s, id)
			port := 443
			if variant == "tunnel-udp" {
				port = ch.Hops.V[1].TunnelPort
			}
			output := "7"
			if variant == "invalid-result" {
				output = "not-a-latency"
			}
			defer fakeProber(t, s, "relay", map[string]string{net.JoinHostPort("land.example.com", strconv.Itoa(port)): output})()
			defer fakeProber(t, s, "land", map[string]string{"": "12"})()
			report := diagnoseRelayTest(t, c, id)
			want := "partial"
			if variant == "tls-fails" || variant == "invalid-result" {
				want = "failed"
			}
			if report.State != want {
				t.Fatalf("report %s: %+v", want, report)
			}
			kind, entryState := "entry_tcp", "passed"
			if strings.HasPrefix(variant, "tls") {
				kind = "entry_tls_handshake"
			}
			if variant == "tls-fails" {
				entryState = "failed"
			}
			if strings.Contains(variant, "udp") {
				entryState = "skipped"
			}
			if entry := diagnosticCheck(t, report, kind); entry.State != entryState {
				t.Fatalf("entry: %+v", entry)
			}
			hop := diagnosticCheck(t, report, "hop_tcp")
			if variant == "udp" {
				if hop.State != "skipped" {
					t.Fatalf("UDP was tested with TCP: %+v", hop)
				}
			} else if variant == "invalid-result" {
				if hop.State != "failed" {
					t.Fatalf("invalid agent response accepted: %+v", hop)
				}
			} else if hop.State != "passed" || hop.Target != net.JoinHostPort("land.example.com", strconv.Itoa(port)) {
				t.Fatalf("wrong hop target: %+v", hop)
			}
			if check := diagnosticCheck(t, report, "landing_egress_http"); check.State != "passed" {
				t.Fatalf("egress: %+v", check)
			}
			if check := diagnosticCheck(t, report, "proxy_business"); check.State != "not_tested" {
				t.Fatalf("business incorrectly verified: %+v", check)
			}
			var count int64
			s.st.DB.Model(&store.InboundHealth{}).Count(&count)
			if count != 0 {
				t.Fatal("diagnostic wrote production health")
			}
		})
	}
}

func TestRelayDiagnosticNotReady(t *testing.T) {
	s, c := relayFixture(t)
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	if report := diagnoseRelayTest(t, c, id); report.State != "not_ready" || report.Deployment.State != "pending" {
		t.Fatalf("pending: %+v", report)
	}
	markRelayApplied(t, s, id)
	s.st.DB.Model(&store.Node{}).Where("id = ?", "relay").Update("status", "offline")
	if report := diagnoseRelayTest(t, c, id); report.State != "not_ready" || report.Deployment.State != "offline" {
		t.Fatalf("offline: %+v", report)
	}
	s.st.DB.Model(&store.Chain{}).Where("id = ?", id).Update("disabled", true)
	if report := diagnoseRelayTest(t, c, id); report.Deployment.State != "disabled" || diagnosticCheck(t, report, "transport").State != "skipped" {
		t.Fatalf("disabled: %+v", report)
	}
	if code, _ := c.do("POST", "/inbounds/foreign/relays/"+id+"/diagnose", nil); code != 404 {
		t.Fatal("foreign inbound accepted", code)
	}
}

func TestUDPChainProbeDoesNotDisableLanding(t *testing.T) {
	s, c := relayFixture(t)
	s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Update("protocol", store.NewJSON(model.ProtocolSpec{Type: model.ProtoHysteria2}))
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	ch := markRelayApplied(t, s, id)
	defer fakeProber(t, s, "land", map[string]string{"": "10"})()
	probes := s.runChainProbe(&ch)
	if len(probes) != 2 || !probes[0].Skipped || !probes[1].OK {
		t.Fatalf("unsupported TCP check not skipped: %+v", probes)
	}
	s.st.DB.Create(&store.Outbound{ID: "ref", NodeID: "middle", Tag: "landing", Type: model.OutNodeRef, NodeRefInboundID: "in"})
	s.probeChains()
	if s.health.isDown("land") {
		t.Fatal("unsupported UDP check disabled a healthy landing")
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Update("status", "offline")
	s.probeChains()
	if !s.health.isDown("land") {
		t.Fatal("skipping UDP checks also skipped offline heartbeat")
	}
}
