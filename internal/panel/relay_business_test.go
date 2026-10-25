package panel

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func fakeBusinessRunner(t *testing.T, s *Server, run func(model.RelayBusinessRequest) model.RelayBusinessResult) func() {
	t.Helper()
	conn := s.hub.register("land")
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case m := <-conn.out:
				cmd := m.GetCommand()
				if cmd == nil || cmd.Kind != model.RelayBusinessFeature {
					continue
				}
				var req model.RelayBusinessRequest
				if json.Unmarshal([]byte(cmd.Args["request"]), &req) != nil {
					continue
				}
				result := run(req)
				b, _ := json.Marshal(result)
				s.hub.resolve("land", &polarisv1.CommandResult{Id: cmd.Id, Ok: true, Output: string(b)})
			}
		}
	}()
	return func() { close(stop); s.hub.unregister(conn) }
}

func TestRelayBusinessRequiresProofAndCleanup(t *testing.T) {
	for _, variant := range []string{"passed", "missing-tcp", "missing-udp", "cleanup-unconfirmed", "missing-cleanup-check", "contradictory", "old-agent", "not-applied"} {
		t.Run(variant, func(t *testing.T) {
			s, c := relayFixture(t)
			id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
			if variant != "not-applied" {
				markRelayApplied(t, s, id)
			}
			if variant != "old-agent" {
				s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Update("agent_features", store.NewJSON([]string{model.RelayBusinessFeature}))
			}
			if code, out := c.do("PUT", "/inbounds/in/relays/"+id+"/metadata", map[string]any{"shareAddress": "share.example.com", "sharePort": 8443}); code != 200 {
				t.Fatal(code, out)
			}
			called := make(chan model.RelayBusinessRequest, 1)
			defer fakeBusinessRunner(t, s, func(req model.RelayBusinessRequest) model.RelayBusinessResult {
				called <- req
				r := model.RelayBusinessResult{State: "passed", BusinessVerified: true, CleanupVerified: variant != "cleanup-unconfirmed"}
				if variant != "missing-tcp" {
					r.Checks = append(r.Checks, model.BusinessCheck{Kind: "proxy_tcp_business", State: "passed"})
				}
				if variant != "missing-udp" {
					r.Checks = append(r.Checks, model.BusinessCheck{Kind: "proxy_udp_business", State: "passed"})
				}
				if variant != "missing-cleanup-check" {
					r.Checks = append(r.Checks, model.BusinessCheck{Kind: "credential_cleanup", State: "passed"})
				}
				if variant == "contradictory" {
					r.Checks = append(r.Checks, model.BusinessCheck{Kind: "proxy_tcp_business", State: "failed"})
				}
				return r
			})()
			code, out := c.do("POST", "/inbounds/in/relays/"+id+"/diagnose/business", nil)
			if code != 200 || out["businessVerified"] != (variant == "passed") {
				t.Fatal(code, out)
			}
			if variant == "old-agent" || variant == "not-applied" {
				select {
				case <-called:
					t.Fatal("unsupported or undeployed core was contacted")
				default:
				}
				return
			}
			req := <-called
			if req.EntryAddress != "share.example.com" || req.EntryPort != 8443 || req.InboundTag != "target" || len(req.ID) != 32 {
				t.Fatal("wrong published path", req)
			}
		})
	}
}

func TestDiagnosticTrafficRejectedEvenIfAllowed(t *testing.T) {
	s := newTestServer(t)
	id := model.DiagnosticUserPrefix + "test"
	b := &polarisv1.TrafficBatch{BatchId: "diagnostic-injection", Users: []*polarisv1.UserTraffic{{UserId: id, Up: 123, Down: 456}}}
	if _, _, err := s.ingestTraffic("node", []*polarisv1.TrafficBatch{b}, map[string]bool{id: true}); err != nil {
		t.Fatal(err)
	}
	var count int64
	s.st.DB.Model(&store.TrafficInbox{}).Count(&count)
	if count != 0 {
		t.Fatal("diagnostic counter entered billing inbox")
	}
}

func TestRelayBusinessCanceledRequestSendsCancel(t *testing.T) {
	s, c := relayFixture(t)
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}}))
	markRelayApplied(t, s, id)
	s.st.DB.Model(&store.Node{}).Where("id = ?", "land").Update("agent_features", store.NewJSON([]string{model.RelayBusinessFeature}))
	conn := s.hub.register("land")
	defer s.hub.unregister(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gotCancel := make(chan bool, 1)
	go func() {
		var runID string
		for m := range conn.out {
			cmd := m.GetCommand()
			if cmd == nil {
				continue
			}
			if cmd.Kind == model.RelayBusinessFeature {
				var req model.RelayBusinessRequest
				json.Unmarshal([]byte(cmd.Args["request"]), &req)
				runID = req.ID
				cancel()
			} else if cmd.Kind == model.RelayBusinessFeature+"/cancel" {
				gotCancel <- cmd.Args["id"] == runID
				s.hub.resolve("land", &polarisv1.CommandResult{Id: cmd.Id, Ok: true})
				return
			}
		}
	}()
	req := httptest.NewRequest("POST", s.cfg.APIBase()+"/inbounds/in/relays/"+id+"/diagnose/business", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+c.tok)
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)
	select {
	case ok := <-gotCancel:
		if !ok {
			t.Fatal("cancel targeted another job")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel command not sent")
	}
}

func TestRelayBusinessUnsupportedProtocolDoesNotSuggestAgentUpgrade(t *testing.T) {
	s, c := relayFixture(t)
	if err := s.st.DB.Model(&store.Inbound{}).Where("id = ?", "in").Updates(map[string]any{"core": model.CoreSingbox, "protocol": store.NewJSON(model.ProtocolSpec{Type: model.ProtoAnyTLS})}).Error; err != nil {
		t.Fatal(err)
	}
	id := createRelayTest(t, c, previewRelayTest(t, c, map[string]any{"mode": "l4", "relays": []map[string]any{{"nodeId": "relay"}}}))
	markRelayApplied(t, s, id)
	st, out := c.do("POST", "/inbounds/in/relays/"+id+"/diagnose/business", nil)
	if st != 200 || out["state"] != "unsupported" || out["businessVerified"] != false {
		t.Fatal(st, out)
	}
	message, _ := out["message"].(string)
	if !strings.Contains(message, "不影响") || strings.Contains(message, "升级") {
		t.Fatal(message)
	}
}
