package panel

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/mailer/mailtest"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// memberSetup builds a panel with mail working and (optionally) the
// member-login switch on.
func memberSetup(t *testing.T, loginOn bool) (*Server, *apiClient, *mailtest.Server) {
	t.Helper()
	s, _, admin := newAdminClient(t)
	m := useMail(t, s)
	s.mailCodeLimit = &sharedLimiter{s: s.shared, name: "mailcode-test", perMinute: 600000}
	if loginOn {
		s.st.SetSetting("panel_member_login", "true")
	}
	return s, admin, m
}

func memberUser(t *testing.T, s *Server, id, email string) store.User {
	t.Helper()
	u := store.User{ID: id, Username: id, Email: email, SubToken: id + "-sub-token",
		UUID: id + "-uuid", Password: "pw", Status: "active"}
	if err := s.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

func memberNode(t *testing.T, s *Server, id, ownerID, review string) {
	t.Helper()
	n := store.Node{ID: id, Name: "un-" + id, OwnerID: ownerID, ReviewState: review, DisplayName: id, Status: "offline"}
	if ownerID == "" {
		n.Name = id
		n.ReviewState = ""
	}
	if err := s.st.DB.Create(&n).Error; err != nil {
		t.Fatal(err)
	}
}

// memberMailLogin runs the full panel e-mail flow for addr (n-th mail to
// the address) and returns a client holding the member token.
func memberMailLogin(t *testing.T, base string, m *mailtest.Server, addr string, mailNo int) *apiClient {
	t.Helper()
	anon := &apiClient{t: t, base: base}
	if code, out := anon.do("POST", "/auth/mail/code", map[string]any{"email": addr}); code != 200 {
		t.Fatalf("mail code %d %v", code, out)
	}
	code, out := anon.do("POST", "/auth/mail/verify", map[string]any{"email": addr, "code": lastCode(t, m, addr, mailNo)})
	if code != 200 {
		t.Fatalf("mail verify %d %v", code, out)
	}
	return &apiClient{t: t, base: base, tok: out["token"].(string)}
}

func TestMemberLoginGates(t *testing.T) {
	s, admin, m := memberSetup(t, false)
	u := memberUser(t, s, "gate-user", "gate@example.com")
	anon := &apiClient{t: t, base: admin.base}

	// Switch off: both endpoints refuse, /info hides the method.
	if code, out := anon.do("POST", "/auth/mail/code", map[string]any{"email": u.Email}); code != 403 {
		t.Fatalf("code with switch off: %d %v", code, out)
	}
	if code, out := anon.do("POST", "/auth/mail/verify", map[string]any{"email": u.Email, "code": "123456"}); code != 403 {
		t.Fatalf("verify with switch off: %d %v", code, out)
	}
	if code, out := anon.do("GET", "/info", nil); code != 200 || out["memberMailLogin"] != false {
		t.Fatalf("info with switch off: %d %v", code, out)
	}

	// Switch on but user nodes off: still refused.
	s.st.SetSetting("panel_member_login", "true")
	s.st.SetSetting("user_nodes_mode", "off")
	if code, out := anon.do("POST", "/auth/mail/code", map[string]any{"email": u.Email}); code != 403 {
		t.Fatalf("code with mode off: %d %v", code, out)
	}
	if code, out := anon.do("GET", "/info", nil); code != 200 || out["memberMailLogin"] != false {
		t.Fatalf("info with mode off: %d %v", code, out)
	}

	// Fully on: the flow works and /info advertises it.
	s.st.SetSetting("user_nodes_mode", "approval")
	if code, out := anon.do("GET", "/info", nil); code != 200 || out["memberMailLogin"] != true {
		t.Fatalf("info when on: %d %v", code, out)
	}
	mc := memberMailLogin(t, admin.base, m, u.Email, 1)
	code, out := mc.do("GET", "/auth/me", nil)
	if code != 200 || out["role"] != "member" || out["username"] != u.Email {
		t.Fatalf("me: %d %v", code, out)
	}
	// The sign-in is audited under the member actor.
	var logs int64
	s.st.DB.Model(&store.AuditLog{}).Where("admin = ? AND action = ?", "member:"+u.ID, "member.login").Count(&logs)
	if logs != 1 {
		t.Fatalf("member.login audit rows: %d", logs)
	}
}

func TestMemberLoginRegistration(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	anon := &apiClient{t: t, base: admin.base}

	// Sign-up closed: an unknown address gets the same "ok" and no mail,
	// and no code verifies.
	if code, out := anon.do("POST", "/auth/mail/code", map[string]any{"email": "ghost@example.com"}); code != 200 || out["ok"] != true {
		t.Fatalf("unknown code request: %d %v", code, out)
	}
	if n := len(m.To("ghost@example.com")); n != 0 {
		t.Fatalf("mailed an unknown address (%d)", n)
	}
	if code, _ := anon.do("POST", "/auth/mail/verify", map[string]any{"email": "ghost@example.com", "code": "123456"}); code != 400 {
		t.Fatalf("unknown verify: %d", code)
	}

	// Sign-up open: the same entry registers, then signs in.
	s.st.SetSetting("portal_email_register", "true")
	mc := memberMailLogin(t, admin.base, m, "newbie@example.com", 1)
	var u store.User
	if err := s.st.DB.First(&u, "email = ?", "newbie@example.com").Error; err != nil {
		t.Fatalf("registered user missing: %v", err)
	}
	if !u.SelfService {
		t.Fatalf("registered user not self-service: %+v", u)
	}
	if code, out := mc.do("GET", "/auth/me", nil); code != 200 || out["role"] != "member" {
		t.Fatalf("new member me: %d %v", code, out)
	}
	// The panel code does not double as a portal code and vice versa: a
	// fresh panel code verifies only against the panel purpose.
	if code, out := anon.do("POST", "/auth/mail/verify", map[string]any{"email": "newbie@example.com", "code": "000000"}); code != 400 || out["error"] == nil {
		t.Fatalf("wrong code: %d %v", code, out)
	}
}

func TestMemberScopeEnforcement(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	alice := memberUser(t, s, "alice", "alice@example.com")
	bob := memberUser(t, s, "bob", "bob@example.com")
	memberNode(t, s, "a-ok", alice.ID, "approved")
	memberNode(t, s, "a-pend", alice.ID, "pending")
	memberNode(t, s, "a-rej", alice.ID, "rejected")
	memberNode(t, s, "b-ok", bob.ID, "approved")
	memberNode(t, s, "hq", "", "")
	// A core on Bob's node: its id must not reach Alice either.
	if err := s.st.DB.Create(&store.Core{ID: "b-core", NodeID: "b-ok"}).Error; err != nil {
		t.Fatal(err)
	}
	mc := memberMailLogin(t, admin.base, m, alice.Email, 1)

	// The list shows all of Alice's nodes (any review state) and nothing
	// else (design D4 list exception).
	code, body := staffRaw(t, mc, "/nodes")
	if code != 200 {
		t.Fatalf("list %d %s", code, body)
	}
	for _, want := range []string{`"id":"a-ok"`, `"id":"a-pend"`, `"id":"a-rej"`, `"reviewState":"pending"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("list missing %s: %s", want, body)
		}
	}
	for _, unwanted := range []string{`"id":"b-ok"`, `"id":"hq"`} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("list leaked %s: %s", unwanted, body)
		}
	}

	// Reads and operations: own approved only, everything else 403.
	for _, x := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/nodes/a-ok", 200},
		{"GET", "/nodes/a-ok/metrics", 200},
		{"GET", "/nodes/a-ok/revisions", 200},
		{"GET", "/nodes/a-ok/cores", 200},
		{"GET", "/nodes/a-ok/net-daily", 200},
		{"GET", "/nodes/b-ok", 403},
		{"GET", "/nodes/b-ok/metrics", 403},
		{"GET", "/nodes/hq", 403},
		{"GET", "/nodes/a-pend", 403},
		{"GET", "/nodes/a-pend/metrics", 403},
		{"GET", "/nodes/a-rej/metrics", 403},
		{"POST", "/nodes/a-pend/sync", 403},
		{"POST", "/nodes/b-ok/sync", 403},
		{"POST", "/nodes/b-ok/rollback", 403},
		{"GET", "/cores/b-core/logs", 403},
		{"POST", "/cores/b-core/restart", 403},
		{"GET", "/capabilities?node=b-ok", 403},
		{"GET", "/capabilities?node=a-ok", 200},
		{"GET", "/stats/traffic", 403}, // a node must be named
		{"GET", "/stats/traffic?node=b-ok", 403},
		{"GET", "/stats/traffic?node=a-ok", 200},
		{"GET", "/stats/traffic?user=" + bob.ID, 403},
		{"GET", "/stats/nodes", 200},
	} {
		if code, out := mc.do(x.method, x.path, nil); code != x.want {
			t.Fatalf("%s %s: %d (want %d) %v", x.method, x.path, code, x.want, out)
		}
	}

	// Sync on the own approved node works and is audited as the member.
	if code, out := mc.do("POST", "/nodes/a-ok/sync", nil); code != 200 {
		t.Fatalf("own sync: %d %v", code, out)
	}
	var logs int64
	s.st.DB.Model(&store.AuditLog{}).Where("admin = ? AND action = ?", "member:"+alice.ID, "node.sync").Count(&logs)
	if logs != 1 {
		t.Fatalf("node.sync audit rows for member actor: %d", logs)
	}

	// Approval takes effect on the next request, suspension takes it
	// away again — no re-login either way.
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "a-pend").Update("review_state", "approved").Error; err != nil {
		t.Fatal(err)
	}
	if code, out := mc.do("GET", "/nodes/a-pend/metrics", nil); code != 200 {
		t.Fatalf("approved node not reachable: %d %v", code, out)
	}
	if err := s.st.DB.Model(&store.Node{}).Where("id = ?", "a-pend").Update("review_state", "pending").Error; err != nil {
		t.Fatal(err)
	}
	if code, _ := mc.do("GET", "/nodes/a-pend/metrics", nil); code != 403 {
		t.Fatalf("suspended node still reachable: %d", code)
	}
}

func TestMemberForbiddenSurfaces(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	u := memberUser(t, s, "mallory", "mallory@example.com")
	memberNode(t, s, "m-ok", u.ID, "approved")
	mc := memberMailLogin(t, admin.base, m, u.Email, 1)

	// Whitelist-out routes are 403 across the whole admin surface.
	for _, x := range []struct{ method, path string }{
		{"GET", "/users"}, {"POST", "/users"}, {"GET", "/settings"}, {"PUT", "/settings"},
		{"GET", "/groups"}, {"GET", "/plans"}, {"GET", "/license"}, {"GET", "/admins"},
		{"GET", "/audit-logs"}, {"GET", "/tokens"}, {"GET", "/backup"}, {"GET", "/security"},
		{"POST", "/nodes"}, {"DELETE", "/nodes/m-ok"}, {"PUT", "/nodes/m-ok"},
		{"GET", "/inbounds"}, {"POST", "/inbounds"}, {"GET", "/templates"},
		{"GET", "/stats/overview"}, {"GET", "/openapi.json"},
	} {
		if code, out := mc.do(x.method, x.path, nil); code != 403 {
			t.Fatalf("%s %s: %d (want 403) %v", x.method, x.path, code, out)
		}
	}
	// Sudo never opens for a member: the endpoint itself refuses, and a
	// sudo-gated self route stays shut.
	if code, out := mc.do("POST", "/auth/sudo", map[string]any{"password": "whatever"}); code != 403 {
		t.Fatalf("member sudo: %d %v", code, out)
	}
	if code, out := mc.do("POST", "/auth/passkeys/register/begin", nil); code != 403 || out["sudoRequired"] != true {
		t.Fatalf("member sudo-gated route: %d %v", code, out)
	}
	// The member's own session list works and shows only their sessions.
	code, body := staffRaw(t, mc, "/auth/sessions")
	if code != 200 || !strings.Contains(body, memberSessionVia) {
		t.Fatalf("member sessions: %d %s", code, body)
	}
	if strings.Contains(body, `"username":"root"`) {
		t.Fatalf("member sees admin sessions: %s", body)
	}
}

func TestMemberSessionLifecycle(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	u := memberUser(t, s, "carol", "carol@example.com")
	mc := memberMailLogin(t, admin.base, m, u.Email, 1)
	if code, _ := mc.do("GET", "/auth/me", nil); code != 200 {
		t.Fatalf("fresh session: %d", code)
	}

	// Disabling the account ends the session on the next request.
	if code, out := admin.do("PUT", "/users/"+u.ID, map[string]any{"username": u.Username, "status": "disabled"}); code != 200 {
		t.Fatalf("disable: %d %v", code, out)
	}
	if code, _ := mc.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("disabled member session alive: %d", code)
	}
	var live int64
	s.st.DB.Model(&store.AdminSession{}).Where("admin_id = ? AND via = ? AND revoked = ?", u.ID, memberSessionVia, false).Count(&live)
	if live != 0 {
		t.Fatalf("member sessions not revoked on disable: %d live", live)
	}

	// A fresh sign-in works again once re-enabled; bumping the portal
	// generation (e-mail rebind / portal session reset) ends it.
	if code, out := admin.do("PUT", "/users/"+u.ID, map[string]any{"username": u.Username, "status": "active"}); code != 200 {
		t.Fatalf("enable: %d %v", code, out)
	}
	mc = memberMailLogin(t, admin.base, m, u.Email, 2)
	if code, _ := mc.do("GET", "/auth/me", nil); code != 200 {
		t.Fatalf("re-enabled session: %d", code)
	}
	if err := s.st.DB.Model(&store.PortalSessionEpoch{}).Where("user_id = ?", u.ID).Update("version", gorm.Expr("version + 1")).Error; err != nil {
		t.Fatal(err)
	}
	if code, _ := mc.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("session survived epoch bump: %d", code)
	}

	// Turning the switch off kills live sessions too.
	mc = memberMailLogin(t, admin.base, m, u.Email, 3)
	s.st.SetSetting("panel_member_login", "false")
	if code, _ := mc.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("session survived switch off: %d", code)
	}
	s.st.SetSetting("panel_member_login", "true")

	// Deleting the account revokes its sessions outright.
	if code, _ := admin.do("DELETE", "/users/"+u.ID, nil); code != 204 {
		t.Fatalf("delete user: %d", code)
	}
	if code, _ := mc.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("deleted member session alive: %d", code)
	}
	s.st.DB.Model(&store.AdminSession{}).Where("admin_id = ? AND via = ? AND revoked = ?", u.ID, memberSessionVia, false).Count(&live)
	if live != 0 {
		t.Fatalf("member sessions not revoked on delete: %d live", live)
	}
}

func TestMemberPortalTokenSeparation(t *testing.T) {
	s, admin, m := memberSetup(t, true)
	s.st.SetSetting("portal_enabled", "true")
	u := memberUser(t, s, "dora", "dora@example.com")

	// A portal token is not a panel token.
	ptok, err := s.issuePortalMailToken(&u)
	if err != nil {
		t.Fatal(err)
	}
	pc := &apiClient{t: t, base: admin.base, tok: ptok}
	if code, _ := pc.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("portal token reached the panel: %d", code)
	}

	// A member token is not a portal token either.
	mc := memberMailLogin(t, admin.base, m, u.Email, 1)
	portal := newShopClient(t, strings.TrimSuffix(admin.base, s.cfg.APIBase()))
	portal.setCookie(portalCookie, mc.tok)
	if _, page := portal.req("GET", portalPath, nil, false); strings.Contains(page, u.SubToken) {
		t.Fatal("member token signed into the portal")
	}
}
