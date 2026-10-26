package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// rewritePlugin answers /polaris/transform with whatever reply builds.
type rewritePlugin struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reply func(req transformRequest) any
	slow  bool
	last  []byte
	calls int
}

func newRewritePlugin(t *testing.T, id string, reply func(transformRequest) any) *rewritePlugin {
	p := &rewritePlugin{reply: reply}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/polaris-plugin.json":
			io.WriteString(w, `{"id":"`+id+`","name":"改写 `+id+`","version":"1.0.0","subscription":{"transform":true}}`)
		case "/polaris/transform":
			b, _ := io.ReadAll(r.Body)
			p.mu.Lock()
			p.calls++
			p.last = b
			slow, reply := p.slow, p.reply
			p.mu.Unlock()
			if slow {
				time.Sleep(pluginTransformTimeout + 300*time.Millisecond)
			}
			var req transformRequest
			json.Unmarshal(b, &req)
			json.NewEncoder(w).Encode(reply(req))
		default:
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func TestPluginNodeRewrite(t *testing.T) {
	s, srv, c := newAdminClient(t)
	s.subLimit.perMinute = 1000
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.9"})
	s.st.DB.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	for i, tag := range []string{"aa", "bb", "cc"} {
		if code, out := c.do("POST", "/inbounds", map[string]any{"nodeId": "n1", "core": "xray", "tag": tag, "remark": tag, "port": 2443 + i,
			"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "tcp"}, "security": map[string]any{"type": "none"}}); code != 201 && code != 200 {
			t.Fatalf("inbound %d %v", code, out)
		}
	}
	_, alice := c.do("POST", "/users", map[string]any{"username": "alice"})
	ports := func() []string {
		t.Helper()
		resp, err := http.Get(srv.URL + "/sub/" + alice["subToken"].(string) + "?format=clash")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out []string
		for _, l := range strings.Split(string(b), "\n") { // panel nodes listen on 2443+; info nodes on 1, 2
			if p, ok := strings.CutPrefix(strings.TrimSpace(l), "port: "); ok && strings.HasPrefix(p, "244") {
				out = append(out, p)
			}
		}
		return out
	}
	base := ports()
	if len(base) != 3 {
		t.Fatalf("baseline %v", base)
	}

	// A keeps the first two nodes it is given, renamed; B reverses the order.
	a := newRewritePlugin(t, "ra", func(req transformRequest) any {
		var out []map[string]string
		for i, n := range req.Nodes {
			if i < 2 {
				out = append(out, map[string]string{"id": n.ID, "name": "A-" + n.Name})
			}
		}
		return map[string]any{"nodes": out}
	})
	b := newRewritePlugin(t, "rb", func(req transformRequest) any {
		var out []map[string]string
		for i := len(req.Nodes) - 1; i >= 0; i-- {
			out = append(out, map[string]string{"id": req.Nodes[i].ID})
		}
		return map[string]any{"nodes": out}
	})
	for _, p := range []*rewritePlugin{a, b} {
		if code, out := c.do("POST", "/plugins", map[string]any{"baseUrl": p.srv.URL}); code != 201 {
			t.Fatalf("install %d %v", code, out)
		}
	}
	_, list := c.do("GET", "/plugins", nil)
	if strings.Join(anyStrings(list["transformOrder"]), ",") != "ra,rb" {
		t.Fatalf("order %v", list["transformOrder"])
	}
	got := ports()
	if strings.Join(got, ",") != "2444,2443" {
		t.Fatalf("a then b: %v", got)
	}
	if resp, err := http.Get(srv.URL + "/sub/" + alice["subToken"].(string) + "?format=clash"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), "A-tokyo VLESS") || !strings.Contains(string(b), alice["uuid"].(string)) {
			t.Fatal("rename lost or credentials dropped from the rendered nodes")
		}
	}
	// No credentials leave the panel.
	if strings.Contains(string(a.last), alice["uuid"].(string)) || strings.Contains(string(a.last), "uuid") {
		t.Fatalf("transform request carries credentials: %s", a.last)
	}

	// Reordered by the admin: b first, then a.
	if code, out := c.do("PUT", "/plugin-transform-order", map[string]any{"ids": []string{"rb", "ra"}}); code != 200 {
		t.Fatalf("order %d %v", code, out)
	}
	if code, _ := c.do("PUT", "/plugin-transform-order", map[string]any{"ids": []string{"zzz"}}); code != 400 {
		t.Fatal("unknown plugin in order accepted")
	}
	s.renderCache = renderCache{}
	if got := ports(); strings.Join(got, ",") != "2445,2444" {
		t.Fatalf("b then a: %v", got)
	}

	// Bad answers are ignored: new node, repeated node, junk, timeout.
	c.do("PUT", "/plugins/rb", map[string]any{"enabled": false})
	for name, reply := range map[string]func(transformRequest) any{
		"unknown": func(transformRequest) any { return map[string]any{"nodes": []map[string]string{{"id": "99"}}} },
		"repeated": func(transformRequest) any {
			return map[string]any{"nodes": []map[string]string{{"id": "0"}, {"id": "0"}}}
		},
		"junk": func(transformRequest) any { return "nope" },
		"newline": func(transformRequest) any {
			return map[string]any{"nodes": []map[string]string{{"id": "0", "name": "x\ny"}}}
		},
	} {
		a.mu.Lock()
		a.reply = reply
		a.mu.Unlock()
		s.renderCache = renderCache{}
		if got := ports(); strings.Join(got, ",") != strings.Join(base, ",") {
			t.Fatalf("%s: %v, want untouched %v", name, got, base)
		}
	}
	a.mu.Lock()
	a.reply, a.slow = func(transformRequest) any { return map[string]any{"nodes": []any{}} }, true
	a.mu.Unlock()
	s.renderCache = renderCache{}
	if got := ports(); len(got) != 3 {
		t.Fatalf("timeout: %v", got)
	}
	// A legitimate empty answer filters everything.
	a.mu.Lock()
	a.slow = false
	a.mu.Unlock()
	s.renderCache = renderCache{}
	if got := ports(); len(got) != 0 {
		t.Fatalf("empty answer: %v", got)
	}
}
