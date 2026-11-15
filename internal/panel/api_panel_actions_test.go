package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestPanelActionScope(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	base := srv.URL + s.cfg.APIBase()

	_, actions, err := s.issueAPIToken("hub", "", []string{ScopePanelActions}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	_, bare, err := s.issueAPIToken("read-only", "", []string{ScopePanelRead}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}

	// The actions token passes the scope gate and reaches the handler (the
	// unknown action is then rejected with 400, not 403).
	with := &apiClient{t: t, base: base, tok: actions}
	code, _ := with.do("POST", "/panel/actions", map[string]any{"action": "no.such"})
	if code != 400 {
		t.Fatalf("panel:actions token should reach handler, got %d", code)
	}

	noScope := &apiClient{t: t, base: base, tok: bare}
	if code, _ := noScope.do("POST", "/panel/actions", map[string]any{"action": "node.sync"}); code != 403 {
		t.Fatalf("token without panel:actions should be 403, got %d", code)
	}
}

func TestPanelActionValidation(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	base := srv.URL + s.cfg.APIBase()
	_, secret, err := s.issueAPIToken("hub", "", []string{ScopePanelActions}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	c := &apiClient{t: t, base: base, tok: secret}

	if code, _ := c.do("POST", "/panel/actions", map[string]any{"action": "node.sync"}); code != 400 {
		t.Fatalf("node.sync without nodeId should be 400, got %d", code)
	}
	if code, _ := c.do("POST", "/panel/actions", map[string]any{"action": "user.renew"}); code != 400 {
		t.Fatalf("user.renew without userId should be 400, got %d", code)
	}
	if code, _ := c.do("POST", "/panel/actions", map[string]any{"action": "bogus", "nodeId": "n"}); code != 400 {
		t.Fatalf("unknown action should be 400, got %d", code)
	}
}

func TestPanelActionResetTraffic(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	base := srv.URL + s.cfg.APIBase()
	_, secret, err := s.issueAPIToken("hub", "", []string{ScopePanelActions}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	c := &apiClient{t: t, base: base, tok: secret}

	if err := s.st.DB.Create(&store.User{
		ID: "u1", Username: "alice", UUID: "11111111-1111-1111-1111-111111111111",
		Password: "p", SubToken: "t1", Status: "active", UsedUp: 100, UsedDown: 50,
	}).Error; err != nil {
		t.Fatal(err)
	}

	code, out := c.do("POST", "/panel/actions", map[string]any{"action": "user.reset-traffic", "userId": "u1"})
	if code != 200 && code != 204 {
		t.Fatalf("reset-traffic %d %v", code, out)
	}

	var u store.User
	if err := s.st.DB.First(&u, "id = ?", "u1").Error; err != nil {
		t.Fatal(err)
	}
	if u.UsedUp != 0 || u.UsedDown != 0 {
		t.Fatalf("traffic not reset: up=%d down=%d", u.UsedUp, u.UsedDown)
	}
}

func TestRemotePanelActionFeature(t *testing.T) {
	s, _, c := newAdminClient(t)
	// Pin a community license: the default license state differs between the
	// community and pro builds (pro keeps the granted features), so without
	// this the hub would forward the action instead of answering 402.
	s.lic.Store(&licenseState{st: license.State{Plan: "community", Features: []string{}}})
	p := store.RemotePanel{ID: "rp1", Name: "m", BaseURL: "https://m.example.com", Token: s.st.Box.Seal("t")}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	// No license: the hub must not forward write actions.
	code, _ := c.do("POST", "/panels/rp1/actions", map[string]any{"action": "node.sync", "nodeId": "n1"})
	if code != 402 {
		t.Fatalf("write action without multi_panel should be 402, got %d", code)
	}
}

func TestRemotePanelActionProxy(t *testing.T) {
	s, srv, c := newAdminClient(t)

	var gotAction, gotNodeID string
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/panel/actions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotAction, _ = body["action"].(string)
		gotNodeID, _ = body["nodeId"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rev": 7, "changed": true}`))
	}))
	defer member.Close()

	s.lic.Store(&licenseState{st: license.State{Plan: "pro", Features: []string{license.FeatureMultiPanel}}})
	// The fake member listens on loopback, which the SSRF dialer blocks by
	// design; use a plain transport in the test.
	s.remotePanelTransport = http.DefaultTransport

	p := store.RemotePanel{ID: "rp1", Name: "m", BaseURL: member.URL, Token: s.st.Box.Seal("tok")}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	code, out := c.do("POST", "/panels/rp1/actions", map[string]any{"action": "node.sync", "nodeId": "n1"})
	if code != 200 {
		t.Fatalf("proxy %d %v", code, out)
	}
	if gotAction != "node.sync" || gotNodeID != "n1" {
		t.Fatalf("member received action=%q node=%q", gotAction, gotNodeID)
	}

	// The hub audits the forwarded action.
	var al store.AuditLog
	if err := s.st.DB.Where("action = ?", "panel.action").Order("created_at desc").First(&al).Error; err != nil {
		t.Fatalf("hub audit missing: %v", err)
	}
	_ = srv
}
