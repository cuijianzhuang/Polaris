package panel

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func relayBatchFixture(t *testing.T) (*Server, *apiClient) {
	t.Helper()
	s, c := relayFixture(t)
	if e := s.st.DB.Create(&store.Node{ID: "relay2", Name: "second entry", Address: "relay2.example.com", Status: "online", DesiredRev: 2, AppliedRev: 2}).Error; e != nil {
		t.Fatal(e)
	}
	return s, c
}
func relayBatchRequestTest(mode string, tail bool) map[string]any {
	req := map[string]any{"name": "multi-entry", "mode": mode, "entries": []any{map[string]any{"nodeId": "relay"}, map[string]any{"nodeId": "relay2"}}}
	if mode == "tunnel" {
		req["transport"] = "ws"
	}
	if tail {
		req["relays"] = []any{map[string]any{"nodeId": "middle"}}
	}
	return req
}
func previewRelayBatchTest(t *testing.T, c *apiClient, req any) map[string]any {
	t.Helper()
	code, out := c.do("POST", "/inbounds/in/relays/batch/preview", req)
	if code != 200 {
		t.Fatal(code, out)
	}
	return out
}
func createRelayBatchTest(t *testing.T, c *apiClient, p map[string]any) map[string]any {
	t.Helper()
	code, out := c.do("POST", "/inbounds/in/relays/batch", map[string]any{"planToken": p["planToken"]})
	if code != 201 {
		t.Fatal(code, out)
	}
	return out
}
func TestRelayBatchLifecycle(t *testing.T) {
	for _, mode := range []string{"l4", "tunnel"} {
		t.Run(mode, func(t *testing.T) {
			s, c := relayBatchFixture(t)
			u := relayTestUser(t, s)
			if code, out := c.do("PUT", "/inbounds/in/publication", map[string]any{"relayOnly": true}); code != 200 {
				t.Fatal(code, out)
			}
			preview := previewRelayBatchTest(t, c, relayBatchRequestTest(mode, true))
			var n int64
			s.st.DB.Model(&store.Chain{}).Count(&n)
			if n != 0 {
				t.Fatal("preview wrote chains")
			}
			entries := preview["entries"].([]any)
			if len(entries) != 2 {
				t.Fatal(preview)
			}
			used := map[string]bool{}
			for _, v := range entries {
				p := v.(map[string]any)
				for _, raw := range p["ports"].([]any) {
					port := raw.(map[string]any)
					key := fmt.Sprint(port["nodeId"], "/", port["port"])
					if used[key] {
						t.Fatal("preview allocated a shared port twice", key)
					}
					used[key] = true
				}
			}
			created := createRelayBatchTest(t, c, preview)
			if created["batchId"] != preview["batchId"] {
				t.Fatal(created)
			}
			var chains []store.Chain
			s.st.DB.Order("created_at").Find(&chains)
			if len(chains) != 2 {
				t.Fatal(chains)
			}
			if chains[0].Hops.V[0].NodeID != "relay" || chains[1].Hops.V[0].NodeID != "relay2" {
				t.Fatal("selection order changed")
			}
			for _, ch := range chains {
				if ch.BatchID != created["batchId"] || ch.Hops.V[1].NodeID != "middle" || ch.Hops.V[2].InboundID != "in" {
					t.Fatal(ch)
				}
			}
			if mode == "tunnel" && (chains[0].TunnelAuth == chains[1].TunnelAuth || chains[0].TunnelAuth == "") {
				t.Fatal("paths share tunnel credentials")
			}
			subs, e := s.subNodes(&u)
			if e != nil || len(subs) != 2 {
				t.Fatal("parallel entries not published", len(subs), e)
			}
			for _, id := range []string{"relay", "relay2", "middle", "land"} {
				spec, e := s.BuildSpec(id)
				if e != nil {
					t.Fatal(e)
				}
				want := 1
				if id == "middle" {
					want = 2
				}
				if id == "land" {
					want = 0
				}
				if mode == "tunnel" && (id == "middle" || id == "land") {
					want += 2
				}
				if len(spec.Forwards) != want {
					t.Fatal(id, len(spec.Forwards), want)
				}
			}
			first := chains[0].ID
			if code, out := c.do("PUT", "/inbounds/in/relays/"+first+"/state", map[string]any{"enabled": false}); code != 200 {
				t.Fatal(code, out)
			}
			subs, e = s.subNodes(&u)
			if e != nil || len(subs) != 1 || subs[0].Server != "relay2.example.com" {
				t.Fatal("disabling one entry affected another", subs, e)
			}
			if code, out := c.do("DELETE", "/inbounds/in/relays/"+first, nil); code != 204 {
				t.Fatal(code, out)
			}
			for _, row := range inboundRelaysTest(t, c) {
				if row.ID == first {
					t.Fatal("deleted path survived")
				}
				if row.BatchID != created["batchId"] {
					t.Fatal("group identity lost")
				}
			}
		})
	}
}
func TestRelayBatchAtomicRollback(t *testing.T) {
	for _, kind := range []string{"port", "address", "expired", "write"} {
		t.Run(kind, func(t *testing.T) {
			s, c := relayBatchFixture(t)
			preview := previewRelayBatchTest(t, c, relayBatchRequestTest("tunnel", true))
			raw, e := s.st.Box.Open(preview["planToken"].(string))
			if e != nil {
				t.Fatal(e)
			}
			var batch relayBatchPlan
			if e = json.Unmarshal([]byte(raw), &batch); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "port":
				h := batch.Plans[1].Chain.Hops.V[0]
				s.st.DB.Create(&store.ForwardRule{ID: "occupied", NodeID: h.NodeID, ListenPort: h.Port})
			case "address":
				s.st.DB.Model(&store.Node{}).Where("id = ?", "relay2").Update("address", "changed.example.com")
			case "expired":
				batch.Expires = time.Now().Add(-time.Second)
				for i := range batch.Plans {
					batch.Plans[i].Expires = batch.Expires
				}
				b, _ := json.Marshal(batch)
				preview["planToken"] = s.st.Box.Seal(string(b))
			case "write":
				s.st.DB.Create(&store.Chain{ID: "unrelated", Name: batch.Plans[1].Chain.Name})
			}
			code, out := c.do("POST", "/inbounds/in/relays/batch", map[string]any{"planToken": preview["planToken"]})
			if code < 400 {
				t.Fatal("invalid batch created", code, out)
			}
			for _, table := range []any{&store.Endpoint{}, &store.IdempotencyKey{}, &store.AuditLog{}} {
				var n int64
				q := s.st.DB.Model(table)
				if _, ok := table.(*store.AuditLog); ok {
					q = q.Where("action = ?", "relay.create")
				}
				q.Count(&n)
				if n != 0 {
					t.Fatalf("partial batch commit in %T: %d", table, n)
				}
			}
			var n int64
			s.st.DB.Model(&store.Chain{}).Where("managed = ?", true).Count(&n)
			if n != 0 {
				t.Fatal("first path persisted despite second failure")
			}
		})
	}
}
func TestRelayBatchValidation(t *testing.T) {
	s, c := relayBatchFixture(t)
	cases := []map[string]any{
		{"entries": []any{}},
		{"entries": []any{map[string]any{"nodeId": "relay"}, map[string]any{"nodeId": "relay"}}},
		{"entries": []any{map[string]any{"nodeId": "land"}}},
		{"entries": []any{map[string]any{"nodeId": "relay"}}, "relays": []any{map[string]any{"nodeId": "relay"}}},
		{"entries": []any{map[string]any{"nodeId": "relay"}}, "mode": "tunnel", "transport": "quic"},
	}
	for _, req := range cases {
		if code, out := c.do("POST", "/inbounds/in/relays/batch/preview", req); code != 400 {
			t.Fatal(code, out)
		}
	}
	req := relayBatchRequestTest("tunnel", true)
	req["relays"] = []any{map[string]any{"nodeId": "middle", "port": 25001}}
	if code, out := c.do("POST", "/inbounds/in/relays/batch/preview", req); code != 400 {
		t.Fatal("explicit common port reused", code, out)
	}
	preview := previewRelayBatchTest(t, c, relayBatchRequestTest("l4", false))
	if code, _ := c.do("POST", "/inbounds/in/relays", map[string]any{"planToken": preview["planToken"]}); code != 400 {
		t.Fatal("batch token used as single")
	}
	single := previewRelayTest(t, c, map[string]any{"relays": []any{map[string]any{"nodeId": "relay"}}})
	if code, _ := c.do("POST", "/inbounds/in/relays/batch", map[string]any{"planToken": single["planToken"]}); code != 400 {
		t.Fatal("single token used as batch")
	}
	var n int64
	s.st.DB.Model(&store.Chain{}).Count(&n)
	if n != 0 {
		t.Fatal("invalid requests wrote chains")
	}
}
func TestRelayBatchConcurrentReplay(t *testing.T) {
	s, c := relayBatchFixture(t)
	cfg := *s.cfg
	cfg.DataDir = t.TempDir()
	second, e := New(&cfg, s.log, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { db, _ := second.st.DB.DB(); db.Close() }()
	server := httptest.NewServer(second.Router())
	defer server.Close()
	preview := previewRelayBatchTest(t, c, relayBatchRequestTest("tunnel", true))
	body, _ := json.Marshal(map[string]any{"planToken": preview["planToken"]})
	bases := []string{c.base, server.URL + s.cfg.APIBase()}
	results := make([]commandReply, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = callUserCommand(bases[i], c.tok, "/inbounds/in/relays/batch", string(body), fmt.Sprintf("batch-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()
	for _, r := range results {
		if r.err != nil || r.status != 201 {
			t.Fatal(r)
		}
		if !strings.Contains(string(r.body), preview["batchId"].(string)) {
			t.Fatal("replay returned wrong batch")
		}
	}
	if !results[0].replay && !results[1].replay {
		t.Fatal("neither request replayed")
	}
	var n int64
	s.st.DB.Model(&store.Chain{}).Count(&n)
	if n != 2 {
		t.Fatal("duplicate batch paths", n)
	}
	replay := callUserCommand(c.base, c.tok, "/inbounds/in/relays/batch", string(body), "batch-third-key")
	if replay.status != 201 || !replay.replay {
		t.Fatal(replay)
	}
}

func TestRelayBatchProtectedSources(t *testing.T) {
	s, c := relayBatchFixture(t)
	for id, ip := range map[string]string{"relay": "198.51.100.1", "relay2": "198.51.100.2", "land": "198.51.100.3"} {
		s.st.DB.Model(&store.Node{}).Where("id = ?", id).Updates(map[string]any{"public_ipv4": ip, "agent_features": store.NewJSON([]string{model.RelayAccessFeature})})
	}
	if code, out := c.do("PUT", "/inbounds/in/relay-access", map[string]any{"enabled": true, "proxyProtocol": 2}); code != 200 {
		t.Fatal(code, out)
	}
	out := createRelayBatchTest(t, c, previewRelayBatchTest(t, c, relayBatchRequestTest("l4", false)))
	spec, e := s.BuildSpec("land")
	if e != nil || len(spec.AccessRules) != 1 || len(spec.AccessRules[0].Sources) != 2 {
		t.Fatal("sources not unioned", spec, e)
	}
	for _, node := range []string{"relay", "relay2"} {
		spec, e := s.BuildSpec(node)
		if e != nil || len(spec.Forwards) != 1 || spec.Forwards[0].SendProxy != 2 {
			t.Fatal("entry lacks PROXY protocol pairing", spec, e)
		}
	}
	chains := out["chains"].([]any)
	first := chains[0].(map[string]any)["id"].(string)
	if code, out := c.do("DELETE", "/inbounds/in/relays/"+first, nil); code != 204 {
		t.Fatal(code, out)
	}
	spec, e = s.BuildSpec("land")
	if e != nil || len(spec.AccessRules) != 1 || len(spec.AccessRules[0].Sources) != 1 || spec.AccessRules[0].Sources[0] != "198.51.100.2/32" {
		t.Fatal("deleting one path lost remaining source guard", spec, e)
	}
}
