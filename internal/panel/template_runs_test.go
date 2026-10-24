package panel

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func templateFixture(t *testing.T) (*Server, *apiClient, string, map[string]any) {
	t.Helper()
	s, _, c := newAdminClient(t)
	for _, id := range []string{"n1", "n2"} {
		if err := s.st.DB.Create(&store.Node{ID: id, Name: id, Address: "203.0.113.1", DefaultCore: model.CoreXray}).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.st.DB.Create(&store.Core{ID: id, NodeID: id, Type: model.CoreXray, Version: "26.3.27"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	it := store.TemplateItem{Key: "r", Core: model.CoreAuto, Protocol: model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"}, Transport: model.TransportSpec{Type: model.TransTCP}, Security: model.SecuritySpec{Type: model.SecReality, RealityDest: "www.example.com:443", RealityServerNames: []string{"www.example.com"}}, Port: store.PortPolicy{Mode: "range", Min: 20000, Max: 30000}, Cert: "reality", TagPattern: "{node.name}-vless", Endpoints: []store.EndpointTemplate{{Kind: "direct"}}}
	code, out := c.do("POST", "/templates", templateReq{Name: "test-plan", Items: []store.TemplateItem{it}})
	if code != 201 {
		t.Fatalf("create template: %d %v", code, out)
	}
	return s, c, out["id"].(string), map[string]any{"nodeIds": []string{"n1", "n2"}, "autoPort": true, "reassign": true}
}

func previewFixture(t *testing.T, c *apiClient, id string, opt map[string]any) map[string]any {
	t.Helper()
	code, out := c.do("POST", "/templates/"+id+"/preview", opt)
	if code != 200 {
		t.Fatalf("preview: %d %v", code, out)
	}
	return out
}

func TestTemplateFrozenPlanAndStatus(t *testing.T) {
	s, c, id, opt := templateFixture(t)
	preview := previewFixture(t, c, id, opt)
	runID := preview["planId"].(string)
	run, frozen, err := s.loadTemplateRun(runID, id)
	if err != nil {
		t.Fatal(err)
	}
	key := frozen.Nodes[0].Models[0].Security.RealityPrivateKey
	if key == "" || strings.Contains(run.Snapshot, key) || strings.Contains(fmt.Sprint(preview), key) {
		t.Fatal("preview credentials were exposed or not generated")
	}
	if frozen.Nodes[0].Models[0].Security.RealityPrivateKey == frozen.Nodes[1].Models[0].Security.RealityPrivateKey {
		t.Fatal("nodes share a private key")
	}
	// Heartbeats do not invalidate a preview.
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Updates(map[string]any{"status": "offline", "applied_rev": 0, "net_rx": 123})
	opt["planId"] = runID
	for i := 0; i < 2; i++ { // the same confirmation is idempotent
		code, out := c.do("POST", "/templates/"+id+"/apply", opt)
		if code != 200 || out["state"] != "finished" {
			t.Fatalf("apply: %d %v", code, out)
		}
	}
	var count int64
	s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", id).Count(&count)
	if count != 2 {
		t.Fatalf("duplicate application: %d", count)
	}
	for _, node := range frozen.Nodes {
		var row store.Inbound
		if err := s.st.DB.First(&row, "id = ?", node.Models[0].ID).Error; err != nil {
			t.Fatal(err)
		}
		actual := s.toModelInbound(&row)
		want := node.Models[0]
		if actual.Port != want.Port || actual.Security.RealityPrivateKey != want.Security.RealityPrivateKey || actual.Security.RealityShortIDs[0] != want.Security.RealityShortIDs[0] {
			t.Fatal("confirmation regenerated the preview configuration")
		}
	}
	url := "/templates/" + id + "/runs/" + runID
	_, out := c.do("GET", url, nil)
	if out["plans"].([]any)[0].(map[string]any)["applyStatus"] != "offline" {
		t.Fatalf("offline status: %v", out)
	}
	var node store.Node
	s.st.DB.First(&node, "id = ?", "n1")
	s.handleApplyResult("n1", &polarisv1.ApplyResult{Rev: node.DesiredRev, Ok: true})
	node = store.Node{}
	s.st.DB.First(&node, "id = ?", "n2")
	s.handleApplyResult("n2", &polarisv1.ApplyResult{Rev: node.DesiredRev, Error: "fixture config failure"})
	_, out = c.do("GET", url, nil)
	rows := out["plans"].([]any)
	if rows[0].(map[string]any)["applyStatus"] != "applied" || rows[1].(map[string]any)["applyStatus"] != "error" {
		t.Fatalf("agent status: %v", out)
	}
	var before store.Node
	s.st.DB.First(&before, "id = ?", "n1")
	code, out := c.do("POST", url+"/retry", nil)
	if code != 200 {
		t.Fatalf("retry: %d %v", code, out)
	}
	var after store.Node
	s.st.DB.First(&after, "id = ?", "n1")
	if after.DesiredRev != before.DesiredRev {
		t.Fatal("retried a successful node")
	}
	after = store.Node{}
	s.st.DB.First(&after, "id = ?", "n2")
	if after.DesiredRev <= node.DesiredRev {
		t.Fatal("failed node did not get a new revision")
	}
	s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", id).Count(&count)
	if count != 2 {
		t.Fatal("retry recreated inbounds")
	}
}

func TestTemplatePlanRejectsStaleInputs(t *testing.T) {
	for _, change := range []string{"expired", "template", "node", "options", "ports"} {
		t.Run(change, func(t *testing.T) {
			s, c, id, opt := templateFixture(t)
			preview := previewFixture(t, c, id, opt)
			opt["planId"] = preview["planId"]
			switch change {
			case "expired":
				s.st.DB.Model(&store.TemplateRun{}).Where("id = ?", opt["planId"]).Update("expires_at", time.Now().Add(-time.Minute))
			case "template":
				s.st.DB.Model(&store.InboundTemplate{}).Where("id = ?", id).Update("description", "changed")
			case "node":
				s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Update("address", "203.0.113.2")
			case "options":
				opt["autoPort"] = false
			case "ports":
				s.st.DB.Create(&store.Inbound{ID: "other", NodeID: "n1", Tag: "other", Port: 12345})
			}
			if code, out := c.do("POST", "/templates/"+id+"/apply", opt); code != 409 {
				t.Fatalf("stale %s plan accepted: %d %v", change, code, out)
			}
			var n int64
			s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", id).Count(&n)
			if n != 0 {
				t.Fatal("stale plan partially wrote configuration")
			}
		})
	}
}

func TestTemplatePlanConcurrentConfirmation(t *testing.T) {
	s, c, id, opt := templateFixture(t)
	opt["planId"] = previewFixture(t, c, id, opt)["planId"]
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := c.do("POST", "/templates/"+id+"/apply", opt)
			if code != 200 && code != 409 {
				t.Errorf("confirmation status: %d", code)
			}
		}()
	}
	wg.Wait()
	var n int64
	s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", id).Count(&n)
	if n != 2 {
		t.Fatalf("concurrent confirmations produced %d inbounds", n)
	}
}

func TestTemplateValidation(t *testing.T) {
	s, _, c := newAdminClient(t)
	base := builtinTemplates()[0].Items.V[0]
	for name, mutate := range map[string]func(*store.TemplateItem){
		"port":     func(it *store.TemplateItem) { it.Port.Fixed = 65536 },
		"range":    func(it *store.TemplateItem) { it.Port = store.PortPolicy{Mode: "range", Min: 40000, Max: 20000} },
		"variable": func(it *store.TemplateItem) { it.TagPattern = "{node.typo}" },
		"combo":    func(it *store.TemplateItem) { it.Transport.Type = model.TransWS },
		"flow":     func(it *store.TemplateItem) { it.Protocol.Flow = "xtls-rprx-vision-udp443" },
		"cert":     func(it *store.TemplateItem) { it.Cert = "selfsigned" },
		"cdn":      func(it *store.TemplateItem) { it.Endpoints = []store.EndpointTemplate{{Kind: "cdn"}} },
	} {
		t.Run(name, func(t *testing.T) {
			b, _ := json.Marshal(base)
			var item store.TemplateItem
			json.Unmarshal(b, &item)
			mutate(&item)
			code, out := c.do("POST", "/templates", templateReq{Name: name, Items: []store.TemplateItem{item}})
			if code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "第 1 个入站") {
				t.Fatalf("invalid template: %d %v", code, out)
			}
		})
	}
	if code, _ := c.do("POST", "/templates", templateReq{Name: "duplicate", Items: []store.TemplateItem{base, base}}); code != 400 {
		t.Fatal("duplicate item keys accepted")
	}
	var builtin store.InboundTemplate
	s.st.DB.Where("builtin = ?", true).First(&builtin)
	if code, _ := c.do("DELETE", "/templates/"+builtin.ID, nil); code != 400 {
		t.Fatal("builtin deleted")
	}
}

func TestBuiltinTemplateCopiesValidate(t *testing.T) {
	s := newTestServer(t)
	for _, tpl := range builtinTemplates() {
		req := templateReq{Name: tpl.Name, Description: tpl.Description, Items: tpl.Items.V}
		if err := s.validateTemplate(&req); err != nil {
			t.Errorf("%s: %v", tpl.Name, err)
		}
	}
}

func TestTemplateRunAcrossInstances(t *testing.T) {
	s, c, id, opt := templateFixture(t)
	opt["planId"] = previewFixture(t, c, id, opt)["planId"]
	cfg := *s.cfg
	other, err := New(&cfg, s.log, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db, _ := other.st.DB.DB(); db.Close() })
	server := httptest.NewServer(other.Router())
	t.Cleanup(server.Close)
	second := &apiClient{t: t, base: server.URL + cfg.APIBase(), tok: c.tok}
	if code, out := second.do("GET", "/templates/"+id+"/runs/"+opt["planId"].(string), nil); code != 200 {
		t.Fatalf("read plan from another instance: %d %v", code, out)
	}
	var wg sync.WaitGroup
	for _, client := range []*apiClient{c, second} {
		wg.Add(1)
		go func(a *apiClient) {
			defer wg.Done()
			code, out := a.do("POST", "/templates/"+id+"/apply", opt)
			if code != 200 && code != 409 {
				t.Errorf("cross-instance confirmation: %d %v", code, out)
			}
		}(client)
	}
	wg.Wait()
	var n int64
	if err := s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", id).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		var run store.TemplateRun
		if err := s.st.DB.First(&run, "id = ?", opt["planId"]).Error; err != nil {
			t.Fatal(err)
		}
		t.Fatalf("cross-instance confirmation produced %d inbounds: state=%s results=%s", n, run.State, run.Results)
	}
}

// A different replica can ingest a heartbeat between the fingerprint read and
// the inbound insert. SQLite must reserve its writer before taking that snapshot.
func TestTemplateApplyWithConcurrentHeartbeat(t *testing.T) {
	s, c, id, opt := templateFixture(t)
	if s.st.DB.Dialector.Name() != "sqlite" {
		t.Skip("SQLite read-to-write snapshot regression")
	}
	opt["planId"] = previewFixture(t, c, id, opt)["planId"]
	other, err := store.Open(s.cfg.DB.DSN, s.st.Box, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db, _ := other.DB.DB(); db.Close() })
	if err := other.DB.Exec("PRAGMA busy_timeout = 50").Error; err != nil {
		t.Fatal(err)
	}
	injected := false
	var heartbeatErr error
	const hook = "fixture:heartbeat-during-template"
	if err := s.st.DB.Callback().Create().Before("gorm:create").Register(hook, func(db *gorm.DB) {
		if _, ok := db.Statement.ConnPool.(*sql.Tx); !ok || db.Statement.Table != "inbounds" || injected {
			return
		}
		injected = true
		// Change a real value: a no-op UPDATE need not append a WAL frame.
		heartbeatErr = other.DB.Exec("UPDATE nodes SET net_rx = net_rx + 1 WHERE id = ?", "n1").Error
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Create().Remove(hook) })
	code, out := c.do("POST", "/templates/"+id+"/apply", opt)
	if !injected {
		t.Fatal("heartbeat interleaving was not exercised")
	}
	if code != 200 {
		t.Fatalf("apply: %d %v", code, out)
	}
	var n int64
	if err := s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", id).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		var run store.TemplateRun
		s.st.DB.First(&run, "id = ?", opt["planId"])
		t.Fatalf("heartbeat interrupted template: %d inbounds, results=%s (heartbeat error: %v)", n, run.Results, heartbeatErr)
	}
	if heartbeatErr != nil {
		var busy interface {
			error
			Code() int
		}
		if !errors.As(heartbeatErr, &busy) || busy.Code()&0xff != 5 {
			t.Fatalf("unexpected heartbeat failure: %v", heartbeatErr)
		}
		if err := other.DB.Exec("UPDATE nodes SET net_rx = net_rx + 1 WHERE id = ?", "n1").Error; err != nil {
			t.Fatalf("heartbeat could not resume after template commit: %v", err)
		}
	}
	var node store.Node
	if err := s.st.DB.First(&node, "id = ?", "n1").Error; err != nil {
		t.Fatal(err)
	}
	if node.NetRx != 1 {
		t.Fatalf("heartbeat data was lost: net_rx=%d", node.NetRx)
	}
}

func TestTemplateFrozenNodeRejectsChangedInputs(t *testing.T) {
	for _, field := range []string{"address", "port"} {
		t.Run(field, func(t *testing.T) {
			s, c, id, opt := templateFixture(t)
			runID := previewFixture(t, c, id, opt)["planId"].(string)
			_, frozen, err := s.loadTemplateRun(runID, id)
			if err != nil {
				t.Fatal(err)
			}
			nodeID := frozen.Nodes[0].Plan.NodeID
			switch field {
			case "address":
				err = s.st.DB.Model(&store.Node{}).Where("id = ?", nodeID).Update("address", "203.0.113.2").Error
			case "port":
				err = s.st.DB.Create(&store.Inbound{ID: "other", NodeID: nodeID, Tag: "other", Port: 12345}).Error
			}
			if err != nil {
				t.Fatal(err)
			}
			// Exercise the transaction guard itself, after the request-level
			// preflight could already have passed on a different replica.
			result := s.applyFrozenNode(frozen, &frozen.Nodes[0])
			if result.Written || !strings.Contains(result.Error, "节点配置已变化") {
				t.Fatalf("stale node was applied: %+v", result)
			}
			var count int64
			if err := s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", id).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("stale node created %d inbounds", count)
			}
		})
	}
}

func TestTemplateRetryAfterDatabaseFailure(t *testing.T) {
	s, c, id, opt := templateFixture(t)
	opt["bindLabel"] = "future-nodes"
	opt["planId"] = previewFixture(t, c, id, opt)["planId"]
	runID := opt["planId"].(string)
	if err := s.st.DB.Callback().Create().Before("gorm:create").Register("fixture:templatefailure", func(db *gorm.DB) {
		if row, ok := db.Statement.Dest.(*store.Inbound); ok && row.NodeID == "n2" {
			db.AddError(errors.New("fixture database failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.DB.Callback().Create().Remove("fixture:templatefailure") })
	if code, out := c.do("POST", "/templates/"+id+"/apply", opt); code != 200 {
		t.Fatalf("partial apply: %d %v", code, out)
	}
	var n int64
	s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", id).Count(&n)
	if n != 1 {
		t.Fatalf("failed node was not rolled back: %d", n)
	}
	s.st.DB.Callback().Create().Remove("fixture:templatefailure")
	if code, out := c.do("POST", "/templates/"+id+"/runs/"+runID+"/retry", nil); code != 200 {
		t.Fatalf("retry: %d %v", code, out)
	}
	s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", id).Count(&n)
	if n != 2 {
		t.Fatalf("failed node did not recover: %d", n)
	}
}

func TestTemplateCertificatePolicyCannotChangeAfterPreview(t *testing.T) {
	s, c, id, opt := templateFixture(t)
	opt["nodeIds"] = []string{"n1"}
	var tpl store.InboundTemplate
	s.st.DB.First(&tpl, "id = ?", id)
	tpl.Items.V[0].Security = model.SecuritySpec{Type: model.SecTLS, ServerName: "cert.example.com"}
	tpl.Items.V[0].Cert = "acme"
	s.st.DB.Model(&tpl).Update("items", tpl.Items)
	opt["planId"] = previewFixture(t, c, id, opt)["planId"]
	// Another operation changes the certificate selected by the policy.
	s.st.DB.Create(&store.Certificate{ID: "changed", Domain: "cert.example.com", Provider: "acme", Cert: "fixture", Key: s.st.Box.Seal("fixture")})
	code, out := c.do("POST", "/templates/"+id+"/apply", opt)
	if code != 200 || out["plans"].([]any)[0].(map[string]any)["applyStatus"] != "error" {
		t.Fatalf("certificate policy drift accepted: %d %v", code, out)
	}
	var n int64
	s.st.DB.Model(&store.Inbound{}).Where("template_id = ?", id).Count(&n)
	if n != 0 {
		t.Fatal("changed certificate policy was applied")
	}
}
