package panel

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func TestPortalExplicitRegistration(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	m := useMail(t, s)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("portal_email_register", "true")
	s.mailCodeLimit = &sharedLimiter{s: s.shared, name: "explicit-registration", perMinute: 600000}
	b := newShopClient(t, srv.URL)
	request := func(path, purpose, email, code string) (int, string) {
		return b.req("POST", "/u/email/"+path, map[string]any{"purpose": purpose, "email": email, "code": code}, true)
	}
	// Login cannot silently register an account, even when registration is open.
	if st, _ := request("code", "login", "unknown@example.com", ""); st != 200 {
		t.Fatal(st)
	}
	if len(m.To("unknown@example.com")) != 0 {
		t.Fatal("sign-in sent a registration code")
	}
	if st, _ := request("verify", "login", "unknown@example.com", "123456"); st != 400 {
		t.Fatal(st)
	}
	if st, _ := request("code", "register", "new@example.com", ""); st != 200 {
		t.Fatal(st)
	}
	code := lastCode(t, m, "new@example.com", 1)
	// Codes for registration cannot be replayed as login or legacy auto-register codes.
	for _, purpose := range []string{"login", ""} {
		if st, _ := request("verify", purpose, "new@example.com", code); st != 400 {
			t.Fatal("purpose accepted", purpose, st)
		}
	}
	// Closing registration is enforced at verification, even for an already-issued code.
	s.st.SetSetting("portal_email_register", "false")
	for _, action := range []string{"code", "verify"} {
		if st, body := request(action, "register", "new@example.com", code); st != 403 || !strings.Contains(body, "registration_closed") {
			t.Fatal(st, body)
		}
	}
	var count int64
	s.st.DB.Model(&store.User{}).Where("email = ?", "new@example.com").Count(&count)
	if count != 0 {
		t.Fatal("closed registration created account")
	}
	_, page := b.req("GET", "/u/register", nil, false)
	if !strings.Contains(page, "暂未开放注册") || strings.Contains(page, `data-auth-mode="register"`) {
		t.Fatal("closed registration page")
	}
	u := newUser(&userReq{Username: "existing"})
	u.Email = "existing@example.com"
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	if st, _ := request("code", "login", u.Email, ""); st != 200 {
		t.Fatal(st)
	}
	if st, body := request("verify", "login", u.Email, lastCode(t, m, u.Email, 1)); st != 200 {
		t.Fatal(st, body)
	}
	_, page = b.req("GET", "/u", nil, false)
	if !strings.Contains(page, `data-view="overview"`) || !strings.Contains(page, u.SubToken) {
		t.Fatal("existing account cannot sign in")
	}
	// Reopening permits explicit registration, consuming the code once.
	s.st.SetSetting("portal_email_register", "true")
	if st, body := request("verify", "register", "new@example.com", code); st != 200 || !strings.Contains(body, `"created":true`) {
		t.Fatal(st, body)
	}
	_, page = b.req("GET", "/u", nil, false)
	if !strings.Contains(page, "待开通") || strings.Contains(page, "· 不限流量") {
		t.Fatal("unprovisioned account presented as unlimited")
	}
	if st, _ := request("verify", "register", "new@example.com", code); st != 400 {
		t.Fatal("registration code reused")
	}
	if st, _ := request("code", "other", "new@example.com", ""); st != 400 {
		t.Fatal("unknown purpose accepted")
	}
}

func TestPortalUserDataScope(t *testing.T) {
	s, base, _ := cdnRegionFixture(t)
	for _, v := range []any{
		&store.Node{ID: "secret-node", Name: "Secret server", Address: "private.example.com"},
		&store.Inbound{ID: "secret-in", NodeID: "secret-node", Tag: "secret", Port: 443, Enabled: true, Core: model.CoreXray, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS})},
		&store.Group{ID: "secret-group", Name: "Secret group"},
		&store.GroupInbound{GroupID: "secret-group", InboundID: "secret-in"},
		&store.UserGroup{UserID: "rb", GroupID: "secret-group"},
		&store.Order{ID: "own-order", UserID: "ra", PlanName: "My renewal", Amount: "10", Currency: "CNY", Status: "paid", WebKey: "own-secret-key"},
		&store.Order{ID: "other-order", UserID: "rb", PlanName: "Other renewal", Amount: "20", Currency: "CNY", Status: "paid", WebKey: "other-secret-key"},
		&store.Order{ID: "guest-order", PlanName: "Guest renewal", WebKey: "guest-secret-key"},
	} {
		if err := s.st.DB.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	b := regionPortalClient(t, s, base, "ra")
	// Auth pages must not render the dashboard at a second path: fragment
	// navigation from there could otherwise keep stale UI after signing out.
	canonical := &http.Client{Jar: b.hc.Jar, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/u/login", "/u/register"} {
		resp, err := canonical.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/u" {
			t.Fatal("noncanonical authenticated page", path, resp.StatusCode)
		}
	}
	st, page := b.req("GET", "/u", nil, false)
	if st != 200 || !strings.Contains(page, "own-order") || !strings.Contains(page, "My renewal") || !strings.Contains(page, `data-view="nodes"`) {
		t.Fatal("missing user data", st)
	}
	for _, secret := range []string{"secret-in", "private.example.com", "Other renewal", "other-order", "guest-order", "own-secret-key", "other-secret-key", "guest-secret-key", "bob-region-subscription-token"} {
		if strings.Contains(page, secret) {
			t.Fatal("exposed unrelated/private data", secret)
		}
	}
	if st, _ := b.req("GET", "/u/shop/order/other-order", nil, false); st != http.StatusNotFound {
		t.Fatal("other order accessible", st)
	}
	if err := s.st.DB.Model(&store.User{}).Where("id = ?", "ra").Update("status", "disabled").Error; err != nil {
		t.Fatal(err)
	}
	_, page = b.req("GET", "/u", nil, false)
	if strings.Contains(page, `class="p-node-row"`) || !strings.Contains(page, "已停用") {
		t.Fatal("disabled account exposes nodes")
	}
}

func TestPortalNodeEvidence(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	if err := s.st.DB.Create(&store.Node{ID: "n", Name: "test", Status: "online"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.InboundHealth{InboundID: "in", OK: true, CheckedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	n := sub.Node{Name: "Test", Server: "example.com", Port: 443, Protocol: model.ProtoVLESS, UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", PanelNodeID: "n", PanelInboundID: "in", TrafficRate: 2}
	check := func(state, reason string) {
		t.Helper()
		var info sub.Info
		if err := s.portalNodeDetails(&info, []sub.Node{n}); err != nil {
			t.Fatal(err)
		}
		if len(info.Links) != 1 || info.Links[0].State != state || !strings.Contains(info.Links[0].Reason, reason) || info.Links[0].Rate != "×2" {
			t.Fatalf("incorrect status %+v", info.Links)
		}
	}
	check("unknown", "尚未完成代理认证")
	s.st.DB.Model(&store.InboundHealth{}).Where("inbound_id = ?", "in").Updates(map[string]any{"ok": false, "error": "secret diagnostic internals"})
	check("unavailable", "近期连接检查失败")
	s.st.DB.Model(&store.InboundHealth{}).Where("inbound_id = ?", "in").Update("checked_at", now.Add(-time.Hour))
	check("unknown", "尚未执行端到端")
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n").Update("status", "offline")
	check("unavailable", "离线")
}

func TestPortalVerifiedRelayExpires(t *testing.T) {
	s, _, ch, in, req := businessOverviewFixture(t)
	if err := s.beginRelayBusinessStatus(ch, in, req); err != nil {
		t.Fatal(err)
	}
	if err := s.finishRelayBusinessStatus(ch.ID, req.ID, "passed", passingBusinessResult()); err != nil {
		t.Fatal(err)
	}
	var ep store.Endpoint
	if err := s.st.DB.Where("relay_chain_id = ? AND inbound_id = ?", ch.ID, in.ID).First(&ep).Error; err != nil {
		t.Fatal(err)
	}
	n := sub.Node{Name: "Relay", Server: "example.com", Port: 443, Protocol: model.ProtoVLESS, UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", PanelNodeID: ch.Hops.V[0].NodeID, PanelInboundID: in.ID, PanelChainID: ch.ID, PanelEndpointID: ep.ID, PanelKind: "relay"}
	var info sub.Info
	if err := s.portalNodeDetails(&info, []sub.Node{n}); err != nil {
		t.Fatal(err)
	}
	if len(info.Links) != 1 || info.Links[0].State != "verified" {
		t.Fatalf("missing business proof %+v", info.Links)
	}
	s.st.DB.Model(&store.RelayBusinessStatus{}).Where("chain_id = ?", ch.ID).Update("checked_at", time.Now().Add(-time.Hour))
	if err := s.portalNodeDetails(&info, []sub.Node{n}); err != nil {
		t.Fatal(err)
	}
	if info.Links[0].State != "unknown" {
		t.Fatal("expired result claimed success", info.Links)
	}
}
