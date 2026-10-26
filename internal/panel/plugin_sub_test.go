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

// renderPlugin serves a manifest with subscription formats and answers
// /polaris/render according to mode.
type renderPlugin struct {
	srv      *httptest.Server
	mu       sync.Mutex
	manifest string
	mode     string // ok / html / fail / slow / big
	renders  int
	last     renderRequest
	sig      string
	body     []byte
}

func newRenderPlugin(t *testing.T, id, formats string) *renderPlugin {
	p := &renderPlugin{mode: "ok", manifest: `{"id":"` + id + `","name":"格式 ` + id + `","version":"1.0.0","subscription":{"formats":[` + formats + `]}}`}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		switch r.URL.Path {
		case "/polaris-plugin.json":
			io.WriteString(w, p.manifest)
		case "/polaris/render":
			p.renders++
			p.body, _ = io.ReadAll(r.Body)
			p.sig = r.Header.Get("X-Polaris-Signature")
			json.Unmarshal(p.body, &p.last)
			switch p.mode {
			case "html":
				w.Header().Set("Content-Type", "text/html")
				io.WriteString(w, "<script>alert(1)</script>")
			case "fail":
				http.Error(w, "down", 500)
			case "slow":
				p.mu.Unlock()
				time.Sleep(pluginRenderTimeout + 500*time.Millisecond)
				p.mu.Lock()
				w.Header().Set("Content-Type", "text/plain")
			case "big":
				w.Header().Set("Content-Type", "text/plain")
				w.Write(make([]byte, pluginRenderMax+10))
			default:
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				for _, n := range p.last.Nodes {
					io.WriteString(w, "node "+n.Name+" "+n.Server+" "+n.UUID+"\n")
				}
			}
		default:
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *renderPlugin) set(mode string) { p.mu.Lock(); p.mode = mode; p.mu.Unlock() }
func (p *renderPlugin) count() int      { p.mu.Lock(); defer p.mu.Unlock(); return p.renders }

func TestPluginSubscriptionFormat(t *testing.T) {
	s, srv, c := newAdminClient(t)
	s.subLimit.perMinute = 1000 // this test fetches many times
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "203.0.113.9"})
	s.st.DB.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	if code, out := c.do("POST", "/inbounds", map[string]any{"nodeId": "n1", "core": "xray", "tag": "vl", "port": 2443,
		"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "tcp"}, "security": map[string]any{"type": "none"}}); code != 201 && code != 200 {
		t.Fatalf("inbound %d %v", code, out)
	}
	_, alice := c.do("POST", "/users", map[string]any{"username": "alice"})
	_, bob := c.do("POST", "/users", map[string]any{"username": "bob"})
	fetch := func(user map[string]any, format string) (int, string, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/sub/" + user["subToken"].(string) + "?format=" + format)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}

	// Built-in names and another plugin's names cannot be taken.
	bad := newRenderPlugin(t, "bad", `{"name":"clash","title":"x","contentType":"text/plain"}`)
	if code, out := c.do("POST", "/plugins", map[string]any{"baseUrl": bad.srv.URL}); code != 400 || !strings.Contains(out["error"].(string), "内置") {
		t.Fatalf("built-in name: %d %v", code, out)
	}
	html := newRenderPlugin(t, "html", `{"name":"page","title":"x","contentType":"text/html"}`)
	if code, _ := c.do("POST", "/plugins", map[string]any{"baseUrl": html.srv.URL}); code != 400 {
		t.Fatal("text/html format accepted")
	}
	p := newRenderPlugin(t, "fmt", `{"name":"myfmt","title":"我的格式","contentType":"text/plain"}`)
	if code, out := c.do("POST", "/plugins", map[string]any{"baseUrl": p.srv.URL}); code != 201 {
		t.Fatalf("install %d %v", code, out)
	}
	dup := newRenderPlugin(t, "dup", `{"name":"myfmt","title":"x","contentType":"text/plain"}`)
	if code, out := c.do("POST", "/plugins", map[string]any{"baseUrl": dup.srv.URL}); code != 400 || !strings.Contains(out["error"].(string), "已被插件") {
		t.Fatalf("duplicate format: %d %v", code, out)
	}
	if _, meta := c.do("GET", "/sub-meta", nil); !strings.Contains(strings.Join(anyStrings(meta["pluginFormats"]), ","), "myfmt") {
		t.Fatalf("meta %v", meta["pluginFormats"])
	}

	// The plugin renders, signed, with the user's nodes; responses are cached per user.
	code, ctype, body := fetch(alice, "myfmt")
	if code != 200 || ctype != "text/plain; charset=utf-8" || !strings.Contains(body, "203.0.113.9") {
		t.Fatalf("render %d %q %q", code, ctype, body)
	}
	var plSecret string
	var row store.Plugin
	s.st.DB.First(&row, "id = ?", "fmt")
	plSecret, _ = s.st.Box.Open(row.Secret)
	ts, v1, _ := strings.Cut(strings.TrimPrefix(p.sig, "t="), ",v1=")
	if v1 != signWebhook(plSecret, ts, p.body) || p.last.Format != "myfmt" || p.last.User.Username != "alice" || len(p.last.Nodes) == 0 {
		t.Fatalf("render request %+v", p.last)
	}
	fetch(alice, "myfmt")
	if p.count() != 1 {
		t.Fatalf("cache missed: %d renders", p.count())
	}
	if _, _, b := fetch(bob, "myfmt"); p.count() != 2 || p.last.User.Username != "bob" || b == body {
		t.Fatal("cache shared between users")
	}

	// Failures answer 503 and leave the built-in formats alone.
	for _, mode := range []string{"fail", "html", "big", "slow"} {
		p.set(mode)
		s.renderCache = renderCache{}
		if code, _, _ := fetch(alice, "myfmt"); code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d, want 503", mode, code)
		}
		if code, _, _ := fetch(alice, "clash"); code != 200 {
			t.Fatalf("clash broken while plugin is %s", mode)
		}
	}

	// Disabled: the format is gone.
	p.set("ok")
	if code, out := c.do("PUT", "/plugins/fmt", map[string]any{"enabled": false}); code != 200 {
		t.Fatalf("disable %d %v", code, out)
	}
	if code, _, _ := fetch(alice, "myfmt"); code != http.StatusBadRequest {
		t.Fatalf("disabled plugin format: %d", code)
	}

	// A new version asking for another format needs approval.
	c.do("PUT", "/plugins/fmt", map[string]any{"enabled": true})
	p.mu.Lock()
	p.manifest = strings.Replace(p.manifest, `]}}`, `,{"name":"other","title":"其他","contentType":"application/json"}]}}`, 1)
	p.mu.Unlock()
	if code, out := c.do("POST", "/plugins/fmt/refresh", map[string]any{}); code != 409 || !strings.Contains(strings.Join(anyStrings(out["addFormats"]), ","), "other") {
		t.Fatalf("refresh without approval: %d %v", code, out)
	}
	if code, _ := c.do("POST", "/plugins/fmt/refresh", map[string]any{"approve": true}); code != 200 {
		t.Fatal("approved refresh refused")
	}
}

func anyStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
