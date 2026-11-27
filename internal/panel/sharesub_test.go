package panel

import (
	"encoding/base64"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// shareFixture builds one node with a VLESS and an SS2022 inbound, both
// granted to user u1 through a group.
func shareFixture(t *testing.T) (*Server, string) {
	t.Helper()
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	for _, v := range []any{
		&store.Node{ID: "sn", Name: "share-node", Address: "203.0.113.10"},
		&store.Inbound{ID: "sv", NodeID: "sn", Tag: "vless", Port: 443, Core: model.CoreXray, Enabled: true,
			Protocol:  store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}),
			Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS, Path: "/ws"}),
			Security:  store.NewJSON(model.SecuritySpec{Type: model.SecTLS})},
		&store.Inbound{ID: "ss", NodeID: "sn", Tag: "ss", Port: 8388, Core: model.CoreSingbox, Enabled: true,
			Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoShadowsocks, Method: "2022-blake3-aes-128-gcm", ServerKey: "AAAAAAAAAAAAAAAAAAAAAA=="})},
		&store.Group{ID: "sg", Name: "share-group"},
		&store.GroupInbound{GroupID: "sg", InboundID: "sv"},
		&store.GroupInbound{GroupID: "sg", InboundID: "ss"},
		&store.User{ID: "u1", Username: "owner", Status: "active", TelegramID: 701,
			UUID: "11111111-2222-3333-4444-555555555555", Password: "main-password-1234", SSSeed: "main-seed-0123456789",
			SubToken: "main-sub-token-0123456789abcdef"},
		&store.User{ID: "u2", Username: "other", Status: "active", TelegramID: 702,
			UUID: "66666666-7777-8888-9999-000000000000", Password: "other-password-123", SSSeed: "other-seed-012345678",
			SubToken: "other-sub-token-0123456789abcdef"},
		&store.UserGroup{UserID: "u1", GroupID: "sg"},
	} {
		if err := s.st.DB.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s, srv.URL
}

func sharePortalClient(t *testing.T, s *Server, base, userID string) *portalClient {
	t.Helper()
	var u store.User
	if err := s.st.DB.First(&u, "id = ?", userID).Error; err != nil {
		t.Fatal(err)
	}
	token, err := s.issuePortalToken(&u)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	parsed, _ := url.Parse(base + portalPath)
	jar.SetCookies(parsed, []*http.Cookie{{Name: portalCookie, Value: token, Path: portalPath}})
	return &portalClient{t: t, base: base, hc: &http.Client{Jar: jar}}
}

func mkShare(t *testing.T, s *Server, sh *store.ShareSubscription) {
	t.Helper()
	if sh.ID == "" {
		sh.ID = store.NewID()
	}
	if sh.Token == "" {
		sh.Token = "share-token-" + sh.ID + "-0123456789abcdef"
	}
	if err := s.st.DB.Create(sh).Error; err != nil {
		t.Fatal(err)
	}
}

func getSub(t *testing.T, base, token string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + "/sub/" + token + "?format=links")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	body := b.String()
	// The links format ships as one base64 blob; decode it so assertions
	// can look for credentials and placeholder text.
	if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body)); err == nil && strings.Contains(string(dec), "://") {
		body = string(dec)
	}
	// Node names ride in the link fragments, percent-encoded.
	if dec, err := url.PathUnescape(body); err == nil {
		body = dec
	}
	return resp.StatusCode, body
}

func TestSharePortalCRUDAndIsolation(t *testing.T) {
	s, base := shareFixture(t)
	pa := sharePortalClient(t, s, base, "u1")
	pb := sharePortalClient(t, s, base, "u2")

	// Create as the owner.
	code, body := pa.req("POST", "/u/shares", map[string]any{"name": "家庭共享甲", "trafficLimit": 0, "expireAt": ""}, true)
	if code != 200 {
		t.Fatalf("create: %d %s", code, body)
	}
	var sh store.ShareSubscription
	if err := s.st.DB.First(&sh, "user_id = ?", "u1").Error; err != nil {
		t.Fatal(err)
	}
	if sh.Name != "家庭共享甲" || !sh.Enabled || sh.CredUUID == "" || sh.CredPassword == "" || sh.CredSSSeed == "" {
		t.Fatalf("share row: %+v", sh)
	}
	if sh.CredUUID == "11111111-2222-3333-4444-555555555555" || sh.CredPassword == "main-password-1234" {
		t.Fatal("share reuses the main credentials")
	}

	// The owner's page lists it; the other user's page does not.
	if _, page := pa.req("GET", "/u/shares", nil, false); !strings.Contains(page, "家庭共享甲") {
		t.Fatal("owner page misses the share")
	}
	if _, page := pb.req("GET", "/u/shares", nil, false); strings.Contains(page, "家庭共享甲") {
		t.Fatal("other user sees the owner's share")
	}
	// The other user cannot touch it by id.
	for _, p := range []string{"/revoke", "/toggle", "/reset-credentials", "/update"} {
		if code, _ := pb.req("POST", "/u/shares/"+sh.ID+p, map[string]any{"enabled": false, "name": "x"}, true); code == 200 {
			t.Fatalf("other user %s succeeded", p)
		}
	}
	var still store.ShareSubscription
	if err := s.st.DB.First(&still, "id = ?", sh.ID).Error; err != nil || !still.Enabled || still.Name != "家庭共享甲" {
		t.Fatalf("share changed by another user: %+v %v", still, err)
	}

	// Per-user cap from the setting (D6).
	s.st.SetSetting(shareSubMaxSetting, "1")
	if code, body := pa.req("POST", "/u/shares", map[string]any{"name": "第二份"}, true); code == 200 {
		t.Fatalf("cap ignored: %d %s", code, body)
	}
	s.st.SetSetting(shareSubMaxSetting, "5")
	if code, body := pa.req("POST", "/u/shares", map[string]any{"name": "第二份"}, true); code != 200 {
		t.Fatalf("second create: %d %s", code, body)
	}

	// Reset credentials: new token + new core credentials, old token dies.
	oldToken := sh.Token
	code, body = pa.req("POST", "/u/shares/"+sh.ID+"/reset-credentials", nil, true)
	if code != 200 {
		t.Fatalf("reset: %d %s", code, body)
	}
	var after store.ShareSubscription
	s.st.DB.First(&after, "id = ?", sh.ID)
	if after.Token == oldToken || after.CredUUID == sh.CredUUID || after.CredSSSeed == sh.CredSSSeed {
		t.Fatal("reset did not rotate token and credentials")
	}
	if code, _ := getSub(t, base, oldToken); code != 404 {
		t.Fatalf("old token after reset: %d", code)
	}
	if code, _ := getSub(t, base, after.Token); code != 200 {
		t.Fatalf("new token after reset: %d", code)
	}

	// Revoke: row gone, token 404, main subscription untouched.
	var second store.ShareSubscription
	s.st.DB.Where("id <> ?", sh.ID).First(&second)
	if code, body := pa.req("POST", "/u/shares/"+second.ID+"/revoke", nil, true); code != 200 {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if code, _ := getSub(t, base, second.Token); code != 404 {
		t.Fatalf("revoked token: %d", code)
	}
	var left int64
	s.st.DB.Model(&store.ShareSubscription{}).Where("id = ?", second.ID).Count(&left)
	if left != 0 {
		t.Fatal("revoked share row still exists")
	}
	if code, body := getSub(t, base, "main-sub-token-0123456789abcdef"); code != 200 || !strings.Contains(body, "11111111-2222-3333-4444-555555555555") {
		t.Fatalf("main subscription after revoke: %d", code)
	}
}

func TestShareRenderingAndNodeIdentities(t *testing.T) {
	s, base := shareFixture(t)
	var u store.User
	s.st.DB.First(&u, "id = ?", "u1")
	sh := &store.ShareSubscription{UserID: "u1", Name: "家庭共享甲", Enabled: true,
		CredUUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", CredPassword: "share-password-123", CredSSSeed: "share-seed-0123456789"}
	mkShare(t, s, sh)

	mainBody1 := ""
	if code, body := getSub(t, base, u.SubToken); code != 200 {
		t.Fatalf("main sub: %d", code)
	} else {
		mainBody1 = body
	}
	// The share renders with its own credentials, including a different
	// SS2022 password (derived from its own seed).
	code, body := getSub(t, base, sh.Token)
	if code != 200 {
		t.Fatalf("share sub: %d %s", code, body)
	}
	if !strings.Contains(body, sh.CredUUID) {
		t.Fatal("share sub does not carry the share UUID")
	}
	if strings.Contains(body, u.UUID) {
		t.Fatal("share sub leaks the main UUID")
	}
	mainNodes, err := s.subNodesFor(&u, "")
	if err != nil {
		t.Fatal(err)
	}
	shadow := shadowUser(&u, sh)
	shareNodes, err := s.subNodesFor(&shadow, "")
	if err != nil {
		t.Fatal(err)
	}
	mainSS, shareSS := nodePassword(mainNodes, model.ProtoShadowsocks), nodePassword(shareNodes, model.ProtoShadowsocks)
	if mainSS == "" || shareSS == "" || mainSS == shareSS {
		t.Fatalf("SS2022 passwords not independent: main %q share %q", mainSS, shareSS)
	}
	if !strings.HasPrefix(shareSS, "AAAAAAAAAAAAAAAAAAAAAA==:") {
		t.Fatalf("share SS password shape: %q", shareSS)
	}
	// Main output is byte-identical with the share in place.
	if code, body := getSub(t, base, u.SubToken); code != 200 || body != mainBody1 {
		t.Fatalf("main subscription changed by a share: %d", code)
	}

	// Node side: the share appears as a composite identity next to the
	// main user; disabling or expiring the share removes only it.
	users, err := s.inboundUsers([]string{"sv", "ss"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]store.User{}
	for _, list := range users {
		for _, x := range list {
			ids[x.ID] = x
		}
	}
	comp, ok := ids[shareIdentity("u1", sh.ID)]
	if !ok {
		t.Fatalf("composite identity missing: %v", ids)
	}
	if comp.UUID != sh.CredUUID || comp.Password != sh.CredPassword || comp.SSSeed != sh.CredSSSeed {
		t.Fatalf("composite credentials: %+v", comp)
	}
	if _, ok := ids["u1"]; !ok {
		t.Fatal("main user missing from inbound users")
	}
	s.st.DB.Model(&store.ShareSubscription{}).Where("id = ?", sh.ID).Update("enabled", false)
	users, _ = s.inboundUsers([]string{"sv"})
	for _, x := range users["sv"] {
		if x.ID == shareIdentity("u1", sh.ID) {
			t.Fatal("disabled share still pushed to nodes")
		}
	}
	past := time.Now().Add(-time.Hour)
	s.st.DB.Model(&store.ShareSubscription{}).Where("id = ?", sh.ID).Updates(map[string]any{"enabled": true, "expire_at": past})
	users, _ = s.inboundUsers([]string{"sv"})
	for _, x := range users["sv"] {
		if x.ID == shareIdentity("u1", sh.ID) {
			t.Fatal("expired share still pushed to nodes")
		}
	}
}

func nodePassword(nodes []sub.Node, proto model.Protocol) string {
	for _, n := range nodes {
		if n.Protocol == proto {
			return n.Password
		}
	}
	return ""
}

func TestShareExpiryAndMainState(t *testing.T) {
	s, base := shareFixture(t)
	past := time.Now().Add(-time.Hour)
	expired := &store.ShareSubscription{UserID: "u1", Name: "已到期", Enabled: true, ExpireAt: &past,
		CredUUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", CredPassword: "share-password-123", CredSSSeed: "share-seed-0123456789"}
	mkShare(t, s, expired)
	live := &store.ShareSubscription{UserID: "u1", Name: "在用", Enabled: true,
		CredUUID: "bbbbbbbb-cccc-dddd-eeee-ffffffffffff", CredPassword: "share-password-456", CredSSSeed: "share-seed-9876543210"}
	mkShare(t, s, live)

	// Expired share: resolves but serves no nodes; the placeholder says why.
	code, body := getSub(t, base, expired.Token)
	if code != 200 || strings.Contains(body, expired.CredUUID) || !strings.Contains(body, "分享已到期") {
		t.Fatalf("expired share: %d %s", code, body)
	}
	// The live share and the main subscription are unaffected.
	if code, body := getSub(t, base, live.Token); code != 200 || !strings.Contains(body, live.CredUUID) {
		t.Fatalf("live share: %d", code)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "u1")
	if code, body := getSub(t, base, u.SubToken); code != 200 || !strings.Contains(body, u.UUID) {
		t.Fatalf("main sub: %d", code)
	}
	// Disabling the main account takes the live share down with it (D5).
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Update("status", "disabled")
	if code, body := getSub(t, base, live.Token); code != 200 || strings.Contains(body, live.CredUUID) {
		t.Fatalf("share of disabled main: %d", code)
	}
	// Main credential rotation does NOT touch shares (the point of D2).
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Updates(map[string]any{"status": "active", "sub_token": "rotated-main-token-0123456789", "uuid": "99999999-8888-7777-6666-555555555555"})
	if code, body := getSub(t, base, live.Token); code != 200 || !strings.Contains(body, live.CredUUID) {
		t.Fatalf("share after main rotation: %d", code)
	}
}

func TestShareSettleStripsSuffix(t *testing.T) {
	s, _ := shareFixture(t)
	s.traffic.Add("sn", []model.TrafficDelta{
		{UserID: shareIdentity("u1", "sh-x"), Up: 100, Down: 40},
		{UserID: "u1", Up: 10, Down: 5},
	})
	if err := s.settleTraffic(); err != nil {
		t.Fatal(err)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "u1")
	if u.UsedUp != 110 || u.UsedDown != 45 {
		t.Fatalf("share traffic not folded into the main account: up %d down %d", u.UsedUp, u.UsedDown)
	}
	// No phantom user is created for the composite identity.
	var n int64
	s.st.DB.Model(&store.User{}).Where("id LIKE ?", "%#%").Count(&n)
	if n != 0 {
		t.Fatal("composite identity became a user row")
	}
	if got := mainIdentity("u1#sh-x"); got != "u1" || mainIdentity("u1") != "u1" {
		t.Fatalf("mainIdentity: %q", got)
	}
}
