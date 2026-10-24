package panel

import (
	"encoding/json"
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
