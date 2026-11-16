package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func seedRelayNodes(t *testing.T, s *Server, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := s.st.DB.Create(&store.Node{ID: id, Name: id, DefaultCore: model.CoreXray, RelayFor: store.NewJSON([]string{})}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func assertNodeRelayFor(t *testing.T, c *apiClient, id string, want []string) {
	t.Helper()
	code, out := c.do("GET", "/nodes/"+id, nil)
	if code != 200 {
		t.Fatalf("read node: %d %v", code, out)
	}
	raw, ok := out["relayFor"].([]any)
	if !ok {
		t.Fatalf("relayFor must be an array: %v", out["relayFor"])
	}
	got := make([]string, 0, len(raw))
	for _, target := range raw {
		got = append(got, target.(string))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("relayFor = %v, want %v", got, want)
	}
}

func TestNodeRelayForSaveReadAndPartialUpdates(t *testing.T) {
	s, _, c := newAdminClient(t)
	seedRelayNodes(t, s, "target-a", "target-z")
	code, out := c.do("POST", "/nodes", map[string]any{
		"name": "external-relay", "relayFor": []string{" target-z ", "target-a", "target-z"},
		"cores": []map[string]any{{"type": "xray", "version": "1.8.0"}},
	})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	created := out["node"].(map[string]any)
	id := created["id"].(string)
	if !reflect.DeepEqual(created["relayFor"], []any{"target-a", "target-z"}) {
		t.Fatalf("create response not normalized: %v", created["relayFor"])
	}
	assertNodeRelayFor(t, c, id, []string{"target-a", "target-z"})

	// Older clients and unrelated edits must preserve this optional field.
	for _, body := range []map[string]any{
		{"name": "external-relay-renamed"},
		{"name": "external-relay-renamed", "relayFor": nil},
	} {
		if code, out := c.do("PUT", "/nodes/"+id, body); code != 200 {
			t.Fatalf("partial update: %d %v", code, out)
		}
		assertNodeRelayFor(t, c, id, []string{"target-a", "target-z"})
	}
	code, out = c.do("PUT", "/nodes/"+id, map[string]any{"name": "external-relay-renamed", "relayFor": []string{" target-z ", "target-z"}})
	if code != 200 || !reflect.DeepEqual(out["relayFor"], []any{"target-z"}) {
		t.Fatalf("update response: %d %v", code, out)
	}
	assertNodeRelayFor(t, c, id, []string{"target-z"})
	if code, out := c.do("PUT", "/nodes/"+id, map[string]any{"name": "external-relay-renamed", "relayFor": []string{}}); code != 200 {
		t.Fatalf("clear: %d %v", code, out)
	}
	assertNodeRelayFor(t, c, id, []string{})

	// Recording external targets must not materialize panel-managed forwarding.
	for _, m := range []any{&store.Chain{}, &store.ForwardRule{}, &store.Outbound{}, &store.RoutingRule{}} {
		var count int64
		if err := s.st.DB.Model(m).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("metadata created forwarding in %T: count=%d err=%v", m, count, err)
		}
	}
}

func TestNodeRelayForValidation(t *testing.T) {
	s, _, c := newAdminClient(t)
	seedRelayNodes(t, s, "source", "target")
	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = "target"
	}
	for _, tc := range []struct {
		name    string
		targets any
		message string
	}{
		{"self", []string{" source "}, "itself"},
		{"missing", []string{"missing"}, "does not exist"},
		{"blank", []string{"  "}, "empty"},
		{"too many", tooMany, "64"},
		{"wrong JSON type", "target", ""},
		{"wrong element type", []any{42}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := c.do("PUT", "/nodes/source", map[string]any{"name": "should-not-save", "relayFor": tc.targets})
			if code != 400 || (tc.message != "" && !strings.Contains(fmt.Sprint(out["error"]), tc.message)) {
				t.Fatalf("invalid update: %d %v", code, out)
			}
			var source store.Node
			if err := s.st.DB.First(&source, "id = ?", "source").Error; err != nil {
				t.Fatal(err)
			}
			if source.Name != "source" || len(source.RelayFor.V) != 0 {
				t.Fatalf("invalid update partially saved: %+v", source)
			}
		})
	}
	for _, targets := range [][]string{{"missing"}, {" "}, tooMany} {
		code, out := c.do("POST", "/nodes", map[string]any{
			"name": "invalid-created-node", "relayFor": targets,
			"cores": []map[string]any{{"type": "xray", "version": "1.8.0"}},
		})
		if code != 400 {
			t.Fatalf("invalid create: %d %v", code, out)
		}
	}
	var created int64
	if err := s.st.DB.Model(&store.Node{}).Where("name = ?", "invalid-created-node").Count(&created).Error; err != nil || created != 0 {
		t.Fatalf("invalid create persisted: count=%d err=%v", created, err)
	}
}

func TestNodeRelayForAllowsOpposingExternalListeners(t *testing.T) {
	s, _, c := newAdminClient(t)
	seedRelayNodes(t, s, "a", "b", "c")
	// These may refer to distinct external listeners. Metadata does not carry
	// enough information to infer a forwarding cycle between whole nodes.
	for _, edge := range [][2]string{{"a", "b"}, {"b", "c"}, {"c", "a"}} {
		if code, out := c.do("PUT", "/nodes/"+edge[0], map[string]any{"name": edge[0], "relayFor": []string{edge[1]}}); code != 200 {
			t.Fatalf("save %v: %d %v", edge, code, out)
		}
		assertNodeRelayFor(t, c, edge[0], []string{edge[1]})
	}
}

func TestNodeRelayForDeleteReferenced(t *testing.T) {
	s, _, c := newAdminClient(t)
	seedRelayNodes(t, s, "relay", "target")
	if code, out := c.do("PUT", "/nodes/relay", map[string]any{"name": "relay", "relayFor": []string{"target"}}); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	if code, out := c.do("DELETE", "/nodes/target", nil); code != 409 || !strings.Contains(fmt.Sprint(out["error"]), "relayFor on: relay") {
		t.Fatalf("referenced target deletion: %d %v", code, out)
	}
	assertNodeRelayFor(t, c, "relay", []string{"target"})
	if code, out := c.do("GET", "/nodes/target", nil); code != 200 {
		t.Fatalf("conflict deleted target: %d %v", code, out)
	}
	// Deleting the source removes its outgoing metadata with the node.
	if code, out := c.do("DELETE", "/nodes/relay", nil); code != 204 {
		t.Fatalf("delete source: %d %v", code, out)
	}
	if code, out := c.do("DELETE", "/nodes/target", nil); code != 204 {
		t.Fatalf("delete unreferenced target: %d %v", code, out)
	}
}

func TestNodeRelayForConcurrentSaveAndDeleteAcrossPanels(t *testing.T) {
	s, _, c := newAdminClient(t)
	cfg := *s.cfg
	other, err := New(&cfg, s.log, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if db, err := other.st.DB.DB(); err == nil {
			db.Close()
		}
	})
	srv := httptest.NewServer(other.Router())
	t.Cleanup(srv.Close)
	// Separate Server instances have independent relayMu locks. The database
	// transaction gate must still serialize reference validation and deletion.
	for i := 0; i < 4; i++ {
		source, target := fmt.Sprintf("relay-%d", i), fmt.Sprintf("target-%d", i)
		seedRelayNodes(t, s, source, target)
		body, err := json.Marshal(map[string]any{"name": source, "relayFor": []string{target}})
		if err != nil {
			t.Fatal(err)
		}
		save, err := http.NewRequest("PUT", c.base+"/nodes/"+source, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		remove, err := http.NewRequest("DELETE", srv.URL+other.cfg.APIBase()+"/nodes/"+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		type result struct {
			method string
			code   int
			body   string
			err    error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		for _, req := range []*http.Request{save, remove} {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+c.tok)
			go func(req *http.Request) {
				<-start
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					results <- result{method: req.Method, err: err}
					return
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				results <- result{method: req.Method, code: resp.StatusCode, body: string(body), err: err}
			}(req)
		}
		close(start)
		codes := make(map[string]int, 2)
		for range 2 {
			r := <-results
			if r.err != nil {
				t.Fatal(r.err)
			}
			codes[r.method] = r.code
			if r.code >= 500 {
				t.Fatalf("concurrent %s: %d %s", r.method, r.code, r.body)
			}
		}
		switch {
		case codes["PUT"] == 200 && codes["DELETE"] == 409:
			assertNodeRelayFor(t, c, source, []string{target})
		case codes["PUT"] == 400 && codes["DELETE"] == 204:
			assertNodeRelayFor(t, c, source, []string{})
		default:
			t.Fatalf("save/delete must not leave dangling relay metadata: %v", codes)
		}
	}
}
