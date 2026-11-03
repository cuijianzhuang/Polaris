package caddy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestWebsiteRenderRejectsEscapesAndOverlaps(t *testing.T) {
	sec := coretest.TLS(t, "site.example.com")
	a := New(core.Options{WorkDir: t.TempDir()})
	site := model.WebsiteSpec{ID: "site", Domain: "site.example.com", Port: 443, Path: "/", Mode: "static", Files: map[string][]byte{"index.html": []byte("blog")}, CertPEM: sec.CertPEM, KeyPEM: sec.KeyPEM}
	render := func(w model.WebsiteSpec, fronts []model.FrontRule) error {
		_, err := a.Render(&model.NodeSpec{Websites: []model.WebsiteSpec{w}, Fronts: fronts})
		return err
	}
	if err := render(site, nil); err != nil {
		t.Fatal(err)
	}
	bad := site
	bad.Files = map[string][]byte{"index.html": []byte("x"), "../escape": []byte("x")}
	if render(bad, nil) == nil {
		t.Fatal("escaped assets accepted")
	}
	bad = site
	bad.Proxies = []model.WebsiteProxy{{Path: "/ws", Upstream: "http://127.0.0.1:8080"}}
	if render(bad, []model.FrontRule{{Domain: site.Domain, Port: 443, Path: "/ws", Upstream: "127.0.0.1:10000", CertPEM: sec.CertPEM, KeyPEM: sec.KeyPEM}}) == nil {
		t.Fatal("overlapping path accepted")
	}
	bad = site
	bad.Mode = "proxy"
	bad.Upstream = "http://192.168.1.2:3000"
	if render(bad, nil) == nil {
		t.Fatal("non-local upstream accepted")
	}
}
func TestLiveWebsiteSharedTLS(t *testing.T) {
	bin := os.Getenv("POLARIS_CADDY_BIN")
	if bin == "" {
		t.Skip("POLARIS_CADDY_BIN required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	sec := coretest.TLS(t, "site.example.com")
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "panel:"+r.URL.RequestURI()) }))
	defer panel.Close()
	blog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "blog:"+r.URL.Path) }))
	defer blog.Close()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		kind, data, err := ws.Read(ctx)
		if err == nil {
			ws.Write(ctx, kind, append([]byte("node:"), data...))
		}
	}))
	defer node.Close()
	port := coretest.FreePortRange(t, 1)
	admin := fmt.Sprintf("127.0.0.1:%d", coretest.FreePortRange(t, 1))
	a := New(core.Options{BinPath: bin, WorkDir: t.TempDir(), APIListen: admin})
	site := model.WebsiteSpec{ID: "site", Domain: "site.example.com", Port: port, Path: "/", Mode: "static", Files: map[string][]byte{"index.html": []byte("<h1>My blog</h1>"), "assets/site.css": []byte("body{color:red}")}, CertPEM: sec.CertPEM, KeyPEM: sec.KeyPEM, Proxies: []model.WebsiteProxy{{Path: "/manage", Upstream: panel.URL}, {Path: "/sub", Upstream: panel.URL}, {Path: "/u", Upstream: panel.URL}}}
	spec := &model.NodeSpec{Websites: []model.WebsiteSpec{site}, Fronts: []model.FrontRule{{Domain: site.Domain, Port: port, Path: "/private-ws", Upstream: strings.TrimPrefix(node.URL, "http://"), CertPEM: sec.CertPEM, KeyPEM: sec.KeyPEM, Kind: model.TransWS}}}
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Validate(cfg); err != nil {
		t.Fatalf("%v\n%s", err, cfg)
	}
	if err = core.AtomicWrite(a.configPath(), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(sec.CertPEM))
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: site.Domain}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", port))
	}}}
	url := "https://" + site.Domain
	get := func(path string) (int, string) {
		t.Helper()
		var resp *http.Response
		var e error
		for i := 0; i < 30; i++ {
			resp, e = client.Get(url + path)
			if e == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if e != nil {
			t.Fatal(e, strings.Join(a.Logs(20), "\n"))
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	for path, want := range map[string]string{"/": "<h1>My blog</h1>", "/assets/site.css": "body{color:red}", "/manage/ping": "panel:/manage/ping", "/sub/token?format=clash": "panel:/sub/token?format=clash", "/u": "panel:/u"} {
		if code, body := get(path); code != 200 || body != want {
			t.Fatalf("%s => %d %q", path, code, body)
		}
	}
	for _, path := range []string{"/private-wsx", "/manage-evil", "/missing"} {
		if code, _ := get(path); code != 404 {
			t.Fatal("prefix escaped", path, code)
		}
	}
	ws, _, err := websocket.Dial(ctx, "wss://"+site.Domain+"/private-ws", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if err = ws.Write(ctx, websocket.MessageText, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	_, data, err := ws.Read(ctx)
	ws.CloseNow()
	if err != nil || string(data) != "node:payload" {
		t.Fatal("node WS route failed", string(data), err)
	}
	pid := a.Health(ctx).PID
	spec.Websites[0].Mode = "proxy"
	spec.Websites[0].Upstream = blog.URL
	spec.Websites[0].Path = "/blog"
	cfg, err = a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	core.AtomicWrite(a.configPath(), cfg, 0o600)
	if err = a.Reload(ctx); err != nil || a.Health(ctx).PID != pid {
		t.Fatal("graceful reload failed", err)
	}
	if code, body := get("/blog/post"); code != 200 || body != "blog:/post" {
		t.Fatal("blog subpath", code, body)
	}
	if code, body := get("/manage/ping"); code != 200 || body != "panel:/manage/ping" {
		t.Fatal("panel route changed", code, body)
	}
	if code, _ := get("/"); code != 404 {
		t.Fatal("root should be outside blog path", code)
	}
}
