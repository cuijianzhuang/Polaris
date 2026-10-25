package panel

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestRouteToolAPIAndCompatibility(t *testing.T) {
	s, c := traceHistoryFixture(t)
	if code, out := c.do("GET", "/nodes/trace/route-tool", nil); code != 200 || out["state"] != "unsupported" || out["issue"] != "agent_upgrade_required" {
		t.Fatal(code, out)
	}
	if code, out := c.do("POST", "/nodes/trace/route-tool/install", nil); code != 409 {
		t.Fatal(code, out)
	}
	if code, out := c.do("GET", "/nodes/missing/route-tool", nil); code != 404 {
		t.Fatal(code, out)
	}
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "trace").Update("agent_features", store.NewJSON([]string{model.RouteTraceFeature, model.RouteTraceToolFeature})).Error; err != nil {
		t.Fatal(err)
	}
	if code, out := c.do("GET", "/nodes/trace/route-tool", nil); code != 409 {
		t.Fatal(code, out)
	}
	conn := s.hub.register("trace")
	defer s.hub.unregister(conn)
	stop := make(chan struct{})
	defer close(stop)
	var mu sync.Mutex
	status := model.RouteTraceToolStatus{State: "missing", Issue: "tool_missing", CanInstall: true, CheckedAt: time.Now()}
	invalid, fail := false, false
	go func() {
		for {
			select {
			case <-stop:
				return
			case msg := <-conn.out:
				cmd := msg.GetCommand()
				if cmd == nil {
					continue
				}
				if cmd.Kind != model.RouteTraceToolFeature || len(cmd.Args) != 1 {
					t.Error(cmd.Kind, cmd.Args)
				}
				mu.Lock()
				data := status
				broken, failed := invalid, fail
				mu.Unlock()
				if cmd.Args["operation"] == "install" {
					data.State = "checking"
					data.InstallState = "installing"
					data.CanInstall = false
				} else if cmd.Args["operation"] != "status" {
					t.Error(cmd.Args)
				}
				raw, _ := json.Marshal(data)
				if broken {
					raw = []byte(`{"state":"bogus"}`)
				}
				s.hub.resolve("trace", &polarisv1.CommandResult{Id: cmd.Id, Ok: !failed, Output: string(raw), Error: "untrusted agent failure"})
			}
		}
	}()
	if code, out := c.do("GET", "/nodes/trace/route-tool", nil); code != 200 || out["state"] != "missing" {
		t.Fatal(code, out)
	}
	var audits int64
	s.st.DB.Model(&store.AuditLog{}).Where("action = ?", "node.route-tool.install").Count(&audits)
	if audits != 0 {
		t.Fatal("GET changed audit")
	}
	if code, out := c.do("POST", "/nodes/trace/route-tool/install", map[string]any{"package": "curl;sh", "command": "ignored"}); code != 200 || out["installState"] != "installing" {
		t.Fatal(code, out)
	}
	s.st.DB.Model(&store.AuditLog{}).Where("action = ?", "node.route-tool.install").Count(&audits)
	if audits != 1 {
		t.Fatal(audits)
	}
	mu.Lock()
	status = model.RouteTraceToolStatus{State: "ready", Path: "/usr/bin/traceroute", InstallState: "succeeded", InstallOutput: "installed", CheckedAt: time.Now()}
	mu.Unlock()
	if code, out := c.do("GET", "/nodes/trace/route-tool", nil); code != 200 || out["state"] != "ready" || out["installOutput"] != "installed" {
		t.Fatal(code, out)
	}
	mu.Lock()
	invalid = true
	mu.Unlock()
	if code, out := c.do("GET", "/nodes/trace/route-tool", nil); code != 502 {
		t.Fatal(code, out)
	}
	mu.Lock()
	invalid = false
	fail = true
	mu.Unlock()
	if code, out := c.do("GET", "/nodes/trace/route-tool", nil); code != 502 || strings.Contains(out["error"].(string), "untrusted") {
		t.Fatal(code, out)
	}
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "trace").Update("revoked", true).Error; err != nil {
		t.Fatal(err)
	}
	if code, out := c.do("GET", "/nodes/trace/route-tool", nil); code != 409 {
		t.Fatal(code, out)
	}
}

func TestRouteToolPermissions(t *testing.T) {
	s, _, c := newAdminClient(t)
	anon := &apiClient{t: t, base: c.base}
	code, out := c.do("POST", "/tokens", map[string]any{"name": "tool-read", "scopes": []string{ScopeNodesRead, ScopeSettingsRead}})
	if code != 201 {
		t.Fatal(code, out)
	}
	api := &apiClient{t: t, base: c.base, tok: out["secret"].(string)}
	for _, v := range []struct{ method, path string }{{"GET", "/nodes/trace/route-tool"}, {"POST", "/nodes/trace/route-tool/install"}} {
		if code, out := anon.do(v.method, v.path, nil); code != 401 {
			t.Fatal(code, out)
		}
		if code, out := api.do(v.method, v.path, nil); code != 403 {
			t.Fatal(code, out)
		}
	}
	if err := s.st.DB.Model(&store.AdminSession{}).Where("1=1").Update("sudo_until", nil).Error; err != nil {
		t.Fatal(err)
	}
	if code, out := c.do("POST", "/nodes/trace/route-tool/install", nil); code != 403 || out["sudoRequired"] != true {
		t.Fatal(code, out)
	}
}

func TestRouteToolStatusBounds(t *testing.T) {
	base := model.RouteTraceToolStatus{State: "ready", CheckedAt: time.Now()}
	if !validRouteToolStatus(base) {
		t.Fatal("valid rejected")
	}
	for _, mutate := range []func(*model.RouteTraceToolStatus){
		func(s *model.RouteTraceToolStatus) { s.State = "success" },
		func(s *model.RouteTraceToolStatus) { s.CheckedAt = time.Time{} },
		func(s *model.RouteTraceToolStatus) { s.InstallState = "pending" },
		func(s *model.RouteTraceToolStatus) { s.InstallOutput = strings.Repeat("x", 16*1024+1) },
		func(s *model.RouteTraceToolStatus) { s.CheckOutput = strings.Repeat("x", 4097) },
	} {
		v := base
		mutate(&v)
		if validRouteToolStatus(v) {
			t.Fatal("invalid accepted")
		}
	}
}

func TestRouteTraceIssueInHistory(t *testing.T) {
	s, c := traceHistoryFixture(t)
	r := traceResult("1.1.1.1")
	r.State = "failed"
	r.Issue = "permission_denied"
	r.Raw = "socket: Operation not permitted"
	row := recordTrace(t, s, r)
	list := routeTraceView(row, false)["result"].(model.RouteTraceResult)
	if list.Issue != "permission_denied" || list.Raw != "" {
		t.Fatal(list)
	}
	code, out := c.do("GET", fmt.Sprintf("/nodes/trace/route-traces/%d", row.ID), nil)
	if code != 200 || out["result"].(map[string]any)["issue"] != "permission_denied" || out["result"].(map[string]any)["raw"] != r.Raw {
		t.Fatal(code, out)
	}
}

func TestRouteTraceLongAttemptNotPrematurelyInterrupted(t *testing.T) {
	s, _ := traceHistoryFixture(t)
	rows, err := s.beginRouteTrace("trace", model.RouteTraceRequest{IPv4: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	rows[0].StartedAt = time.Now().Add(-35 * time.Second)
	if err = s.st.DB.Model(&store.NodeRouteTrace{}).Where("id = ?", rows[0].ID).Update("started_at", rows[0].StartedAt).Error; err != nil {
		t.Fatal(err)
	}
	if got := routeTraceView(rows[0], false)["state"]; got != "running" {
		t.Fatal(got)
	}
	saved, err := s.finishRouteTrace(rows, []model.RouteTraceResult{traceResult("1.1.1.1", "1.1.1.1")}, "completed")
	if err != nil || saved[0].State != "completed" {
		t.Fatal(saved, err)
	}
}
