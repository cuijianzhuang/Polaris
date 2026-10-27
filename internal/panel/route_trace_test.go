package panel

import (
	"connectrpc.com/connect"
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestRouteTraceAPI(t *testing.T) {
	s, _, admin := newAdminClient(t)
	n := store.Node{ID: "trace", Name: "trace", AgentFeatures: store.NewJSON([]string{model.RouteTraceFeature})}
	if err := s.st.DB.Create(&n).Error; err != nil {
		t.Fatal(err)
	}
	req := map[string]any{"ipv4": "1.1.1.1"}
	if st, out := admin.do("POST", "/nodes/trace/route-trace", req); st != 409 {
		t.Fatal(st, out)
	}
	if st, out := admin.do("POST", "/nodes/missing/route-trace", req); st != 404 {
		t.Fatal(st, out)
	}
	if st, out := admin.do("POST", "/nodes/trace/route-trace", map[string]any{"ipv4": "127.0.0.1"}); st != 400 {
		t.Fatal(st, out)
	}
	conn := s.hub.register(n.ID)
	defer s.hub.unregister(conn)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case m := <-conn.out:
				cmd := m.GetCommand()
				if cmd == nil {
					continue
				}
				if cmd.Kind != model.RouteTraceFeature {
					t.Error(cmd.Kind)
				}
				b, _ := json.Marshal([]model.RouteTraceResult{{Family: "ipv4", Target: "1.1.1.1", CheckedAt: time.Now(), State: "partial", Raw: "1 *", Hops: []model.RouteTraceHop{}}})
				s.hub.resolve(n.ID, &polarisv1.CommandResult{Id: cmd.Id, Ok: true, Output: string(b)})
			}
		}
	}()
	if st, out := admin.do("POST", "/nodes/trace/route-trace", req); st != 200 || len(out["results"].([]any)) != 1 {
		t.Fatal(st, out)
	}
	s.st.DB.Model(&n).Update("agent_features", store.NewJSON([]string{}))
	if st, out := admin.do("POST", "/nodes/trace/route-trace", req); st != 409 {
		t.Fatal(st, out)
	}
}

func TestRouteTraceRejectsMismatchedResults(t *testing.T) {
	req := model.RouteTraceRequest{IPv4: "1.1.1.1"}
	r := model.RouteTraceResult{Family: "ipv4", Target: "1.1.1.1", CheckedAt: time.Now(), State: "partial"}
	if !validRouteTraceResults(req, []model.RouteTraceResult{r}) {
		t.Fatal("valid rejected")
	}
	r.Target = "8.8.8.8"
	if validRouteTraceResults(req, []model.RouteTraceResult{r}) {
		t.Fatal("wrong target accepted")
	}
	r.Target = "1.1.1.1"
	r.State = "success"
	if validRouteTraceResults(req, []model.RouteTraceResult{r}) {
		t.Fatal("invalid state accepted")
	}
}

// Exercise the actual authenticated GetSpec handshake rather than seeding the
// capability directly, so a missing service allowlist entry cannot hide behind
// the API fixtures that already assume a capable Agent.
func TestRouteTraceCapabilityNegotiation(t *testing.T) {
	s, c := traceHistoryFixture(t)
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "trace").Updates(map[string]any{"cert_serial": "1", "desired_rev": 7, "agent_features": store.NewJSON([]string{})}).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(model.NodeSpec{NodeID: "trace", Rev: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.ConfigRevision{NodeID: "trace", Rev: 7, Spec: s.st.Box.Seal(string(raw))}).Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), peerCertKey, &x509.Certificate{Subject: pkix.Name{CommonName: "trace"}, SerialNumber: big.NewInt(1)})
	service := &agentService{s: s}
	req := connect.NewRequest(&polarisv1.GetSpecRequest{Rev: 7})
	req.Header().Set("X-Polaris-Agent-Features", strings.Join([]string{model.RouteTraceFeature, model.RouteTraceToolFeature, model.RelayAccessFeature, model.RouteTraceFeature, model.AgentRollbackFeature, "unknown-test-feature"}, ","))
	if _, err := service.GetSpec(ctx, req); err != nil {
		t.Fatal(err)
	}
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", "trace").Error; err != nil {
		t.Fatal(err)
	}
	if len(node.AgentFeatures.V) != 4 || !contains(node.AgentFeatures.V, model.RouteTraceToolFeature) || !contains(node.AgentFeatures.V, model.RouteTraceFeature) || !contains(node.AgentFeatures.V, model.RelayAccessFeature) || !contains(node.AgentFeatures.V, model.AgentRollbackFeature) {
		t.Fatal(node.AgentFeatures.V)
	}
	if st, out := c.do("GET", "/nodes/trace/route-schedule", nil); st != 200 || out["agentSupported"] != true {
		t.Fatal("advertised feature unavailable to the frontend", st, out)
	}
	// Older/non-Linux Agents that do not advertise it must remain unsupported.
	req.Header().Set("X-Polaris-Agent-Features", model.RelayAccessFeature)
	if _, err := service.GetSpec(ctx, req); err != nil {
		t.Fatal(err)
	}
	if st, out := c.do("GET", "/nodes/trace/route-schedule", nil); st != 200 || out["agentSupported"] != false {
		t.Fatal(st, out)
	}
}
