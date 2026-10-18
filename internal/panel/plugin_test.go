package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// fakePlugin is a plugin service: it serves a manifest and records calls.
type fakePlugin struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []string
	bodies   map[string][]byte
	headers  map[string]http.Header
	installs int
	noHook   bool // refuse the install handshake
	healthy  bool
}

func newFakePlugin(t *testing.T, manifest string) *fakePlugin {
	f := &fakePlugin{bodies: map[string][]byte{}, headers: map[string]http.Header{}, healthy: true}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.bodies[r.URL.Path], f.headers[r.URL.Path] = b, r.Header.Clone()
		f.mu.Unlock()
		switch r.URL.Path {
		case "/polaris-plugin.json":
			io.WriteString(w, manifest)
		case "/polaris/install":
			if f.noHook {
				http.NotFound(w, r)
				return
			}
			f.mu.Lock()
			f.installs++
			f.mu.Unlock()
			w.WriteHeader(204)
		case "/polaris/health":
			if !f.healthy {
				http.Error(w, "down", 503)
				return
			}
			w.WriteHeader(204)
		default:
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePlugin) called(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasSuffix(c, " "+path) {
			return true
		}
	}
	return false
}

func manifestFor(id string) string {
	return `{"id":"` + id + `","name":"插件 ` + id + `","version":"1.2.0","scopes":["users:read","users:renew"],
 "events":["user.expired"],
 "pages":[{"id":"orders","title":"订单","path":"/admin/orders"}],
 "portal":[{"title":"购买","path":"/shop"}],
 "settings":[{"key":"merchant","label":"商户号","type":"string","required":true},{"key":"key","label":"密钥","type":"secret"}]}`
}

// TestPluginInstallLifecycle: install gives the plugin a token and a webhook,
// the handshake carries the credentials, settings are sealed, pages and
// portal links get verifiable identity tokens, and uninstall cleans up.
func TestPluginInstallLifecycle(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	s.cfg.PublicURL = "https://panel.example.com"
	f := newFakePlugin(t, manifestFor("shop"))

	if code, out := admin.do("POST", "/plugins/preview", map[string]any{"baseUrl": f.srv.URL}); code != 200 || out["installed"] != false {
		t.Fatalf("preview %d %v", code, out)
	}
	code, out := admin.do("POST", "/plugins", map[string]any{"baseUrl": f.srv.URL + "/", "installKey": "k1"})
	if code != 201 || out["handshake"] != "ok" {
		t.Fatalf("install %d %v", code, out)
	}
	if out["credentials"] != nil {
		t.Fatal("credentials must not be shown when the handshake worked")
	}
	var hs map[string]any
	json.Unmarshal(f.bodies["/polaris/install"], &hs)
	tok, _ := hs["token"].(string)
	if !strings.HasPrefix(tok, "plt_") || !strings.HasPrefix(hs["webhook_secret"].(string), "whsec_") || !strings.HasPrefix(hs["plugin_secret"].(string), "plsec_") {
		t.Fatalf("handshake body %v", hs)
	}
	if hs["api_base"] != "https://panel.example.com/api/v1" || f.headers["/polaris/install"].Get("X-Polaris-Install-Key") != "k1" {
		t.Fatalf("api_base %v key %q", hs["api_base"], f.headers["/polaris/install"].Get("X-Polaris-Install-Key"))
	}
	ts, v1, _ := strings.Cut(strings.TrimPrefix(f.headers["/polaris/install"].Get("X-Polaris-Signature"), "t="), ",v1=")
	if v1 != signWebhook(hs["plugin_secret"].(string), ts, f.bodies["/polaris/install"]) {
		t.Fatal("install request signature does not verify with the plugin secret")
	}

	// The token works within its scopes and only while the plugin is enabled.
	pc := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: tok}
	if code, _ := pc.do("GET", "/users", nil); code != 200 {
		t.Fatalf("plugin token /users %d", code)
	}
	if code, _ := pc.do("GET", "/nodes", nil); code != 403 {
		t.Fatalf("plugin token /nodes %d, want 403", code)
	}
	var hooks []store.Webhook
	s.st.DB.Find(&hooks)
	if len(hooks) != 1 || hooks[0].PluginID != "shop" || len(hooks[0].Events.V) != 1 || hooks[0].URL != f.srv.URL+"/polaris/events" {
		t.Fatalf("webhook %+v", hooks)
	}
	if code, _ := admin.do("DELETE", "/webhooks/"+hooks[0].ID, nil); code != 400 {
		t.Fatalf("deleting a plugin webhook by hand: %d, want 400", code)
	}
	if code, out := admin.do("POST", "/plugins", map[string]any{"baseUrl": f.srv.URL}); code != 409 {
		t.Fatalf("second install %d %v", code, out)
	}

	// Settings: validated, secrets sealed, pushed to the plugin.
	if code, out := admin.do("PUT", "/plugins/shop/settings", map[string]any{"values": map[string]string{"key": "s3cret"}}); code != 400 {
		t.Fatalf("missing required setting %d %v", code, out)
	}
	code, out = admin.do("PUT", "/plugins/shop/settings", map[string]any{"values": map[string]string{"merchant": "1001", "key": "s3cret"}})
	if code != 200 || out["pushed"] != true {
		t.Fatalf("settings %d %v", code, out)
	}
	var p store.Plugin
	s.st.DB.First(&p, "id = ?", "shop")
	if strings.Contains(p.Settings, "s3cret") {
		t.Fatal("secret stored in the clear")
	}
	view := out["plugin"].(map[string]any)
	if view["hasSecret"].(map[string]any)["key"] != true || view["values"].(map[string]any)["key"] != nil {
		t.Fatalf("secret leaked in the view: %v", view)
	}
	if code, cfg := pc.do("GET", "/plugin/config", nil); code != 200 || cfg["settings"].(map[string]any)["key"] != "s3cret" {
		t.Fatalf("plugin reads its config %d %v", code, cfg)
	}
	if code, _ := admin.do("GET", "/plugin/config", nil); code != 403 {
		t.Fatalf("an admin session reading plugin config: %d, want 403", code)
	}
	// Keeping the secret when the field is left empty.
	admin.do("PUT", "/plugins/shop/settings", map[string]any{"values": map[string]string{"merchant": "1002"}})
	if _, cfg := pc.do("GET", "/plugin/config", nil); cfg["settings"].(map[string]any)["key"] != "s3cret" || cfg["settings"].(map[string]any)["merchant"] != "1002" {
		t.Fatalf("secret not kept: %v", cfg)
	}

	// An embedded page gets an identity token the plugin can verify.
	code, out = admin.do("GET", "/plugins/shop/pages/orders", nil)
	if code != 200 {
		t.Fatalf("page url %d %v", code, out)
	}
	u := out["url"].(string)
	raw := u[strings.Index(u, "polaris_token=")+len("polaris_token="):]
	raw = strings.SplitN(raw, "&", 2)[0]
	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(raw, jwt.MapClaims{}, func(*jwt.Token) (any, error) { return []byte("wrong-secret"), nil }); err == nil {
		t.Fatal("token verified with the wrong secret")
	}
	if _, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return []byte(hs["plugin_secret"].(string)), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience("shop")); err != nil || claims["scope"] != "admin" || claims["sub"] != "root" {
		t.Fatalf("page token %v %v", err, claims)
	}
	if !strings.HasPrefix(u, f.srv.URL+"/admin/orders?") {
		t.Fatalf("page url %s", u)
	}

	// Portal links carry a user token.
	user := store.User{ID: "u1", Username: "alice", UUID: "11111111-1111-1111-1111-111111111111", SubToken: "tok-alice-1234567890"}
	links := s.pluginPortalLinks(&user)
	if len(links) != 1 || links[0].Title != "购买" || !strings.HasPrefix(links[0].URL, f.srv.URL+"/shop?polaris_token=") {
		t.Fatalf("portal links %+v", links)
	}

	// Disabled: the token stops working, then uninstalling removes everything.
	if code, _ := admin.do("PUT", "/plugins/shop", map[string]any{"enabled": false}); code != 200 {
		t.Fatal("disable")
	}
	if code, _ := pc.do("GET", "/users", nil); code != 401 {
		t.Fatalf("disabled plugin token %d, want 401", code)
	}
	if len(s.pluginPortalLinks(&user)) != 0 {
		t.Fatal("a disabled plugin still adds portal links")
	}
	if code, _ := admin.do("DELETE", "/plugins/shop", nil); code != 204 || !f.called("/polaris/uninstall") {
		t.Fatal("uninstall")
	}
	var n int64
	s.st.DB.Model(&store.APIToken{}).Where("plugin_id = ?", "shop").Count(&n)
	var wn int64
	s.st.DB.Model(&store.Webhook{}).Count(&wn)
	if n != 0 || wn != 0 {
		t.Fatalf("leftovers: %d tokens, %d webhooks", n, wn)
	}
}

// TestPluginCommunityLimit: the community edition installs at most
// license.CommunityPlugins plugins; a license with no cap lifts it.
func TestPluginCommunityLimit(t *testing.T) {
	s, _, admin := newAdminClient(t)
	setLimits := func(l license.Limits) {
		s.lic.Store(&licenseState{st: license.State{Plan: "community", Limits: l}})
	}
	setLimits(license.Community)
	for i, id := range []string{"aa", "bb", "cc"} {
		f := newFakePlugin(t, manifestFor(id))
		if code, out := admin.do("POST", "/plugins", map[string]any{"baseUrl": f.srv.URL}); code != 201 {
			t.Fatalf("plugin %d: %d %v", i, code, out)
		}
	}
	f := newFakePlugin(t, manifestFor("dd"))
	code, out := admin.do("POST", "/plugins", map[string]any{"baseUrl": f.srv.URL})
	if code != 402 || out["limit"] != "plugins" {
		t.Fatalf("4th plugin: %d %v, want 402", code, out)
	}
	if f.called("/polaris/install") || s.st.DB.Where("plugin_id = ?", "dd").First(&store.APIToken{}).Error == nil {
		t.Fatal("a refused install must not leave anything behind")
	}
	if code, out := admin.do("GET", "/plugins", nil); code != 200 || out["limit"].(float64) != license.CommunityPlugins || out["used"].(float64) != 3 {
		t.Fatalf("list %d %v", code, out)
	}
	setLimits(license.Limits{}) // a license without a plugins cap: unlimited
	if code, out := admin.do("POST", "/plugins", map[string]any{"baseUrl": f.srv.URL}); code != 201 {
		t.Fatalf("with a license: %d %v", code, out)
	}
}

// TestPluginManualHandshake: a plugin that does not implement /polaris/install
// still installs; the credentials are returned once for pasting, and the
// status turns online when its health check passes.
func TestPluginManualHandshake(t *testing.T) {
	s, _, admin := newAdminClient(t)
	f := newFakePlugin(t, manifestFor("manual"))
	f.noHook = true
	code, out := admin.do("POST", "/plugins", map[string]any{"baseUrl": f.srv.URL})
	if code != 201 || out["handshake"] != "manual" {
		t.Fatalf("install %d %v", code, out)
	}
	creds, _ := out["credentials"].(map[string]any)
	if !strings.HasPrefix(creds["token"].(string), "plt_") || creds["pluginSecret"] == "" || creds["apiBase"] == "" {
		t.Fatalf("credentials %v", creds)
	}
	if out["plugin"].(map[string]any)["status"] != "manual" {
		t.Fatalf("status %v", out["plugin"])
	}
	f.healthy = false
	s.checkPlugins()
	var p store.Plugin
	s.st.DB.First(&p, "id = ?", "manual")
	if p.Status != "manual" || p.LastError == "" {
		t.Fatalf("never seen alive: %s %q", p.Status, p.LastError)
	}
	f.healthy = true
	s.checkPlugins()
	s.st.DB.First(&p, "id = ?", "manual")
	if p.Status != "online" || p.LastSeenAt == nil {
		t.Fatalf("after a good check: %s", p.Status)
	}
	f.healthy = false
	s.checkPlugins()
	s.st.DB.First(&p, "id = ?", "manual")
	if p.Status != "offline" {
		t.Fatalf("after a failed check: %s", p.Status)
	}
}

// TestPluginRefreshNeedsApproval: an upgrade that asks for more scopes is
// held until the admin approves it.
func TestPluginRefreshNeedsApproval(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	f := newFakePlugin(t, manifestFor("up"))
	_, out := admin.do("POST", "/plugins", map[string]any{"baseUrl": f.srv.URL})
	_ = out
	// The plugin is upgraded and now asks for node access.
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/polaris-plugin.json" {
			io.WriteString(w, strings.Replace(strings.Replace(manifestFor("up"), `"1.2.0"`, `"1.3.0"`, 1), `"users:renew"`, `"users:renew","nodes:read"`, 1))
			return
		}
		w.WriteHeader(204)
	})
	code, out := admin.do("POST", "/plugins/up/refresh", nil)
	if code != 409 || out["needApproval"] != true {
		t.Fatalf("refresh without approval %d %v", code, out)
	}
	var tk store.APIToken
	s.st.DB.First(&tk, "plugin_id = ?", "up")
	for _, sc := range tk.Scopes.V {
		if sc == "nodes:read" {
			t.Fatal("scope granted before approval")
		}
	}
	if code, out := admin.do("POST", "/plugins/up/refresh", map[string]any{"approve": true}); code != 200 || out["version"] != "1.3.0" {
		t.Fatalf("approved refresh %d %v", code, out)
	}
	s.st.DB.First(&tk, "plugin_id = ?", "up")
	got := strings.Join(tk.Scopes.V, ",")
	if !strings.Contains(got, "nodes:read") {
		t.Fatalf("scopes after approval: %s", got)
	}
	_ = srv
}

func TestPluginBase(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:9000/":        "http://127.0.0.1:9000",
		" https://shop.example.com/p/ ": "https://shop.example.com/p",
	} {
		if got, err := pluginBase(in); err != nil || got != want {
			t.Errorf("pluginBase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"ftp://x", "https://u:p@x.com", "https://x.com/?a=1", "https://x.com/#f", "x.com", ""} {
		if _, err := pluginBase(in); err == nil {
			t.Errorf("pluginBase(%q) accepted", in)
		}
	}
}
