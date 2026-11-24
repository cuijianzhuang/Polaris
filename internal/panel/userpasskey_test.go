package panel

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// softAuth is a software WebAuthn authenticator for the user passkey
// tests: an ECDSA P-256 key that speaks just enough of the protocol
// (attestation format "none", hand-encoded CBOR) for the real server
// ceremonies to verify real signatures.
type softAuth struct {
	priv    *ecdsa.PrivateKey
	credID  []byte
	counter uint32
}

func newSoftAuth(t *testing.T) *softAuth {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatal(err)
	}
	return &softAuth{priv: priv, credID: credID, counter: 1}
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func cborByteString(b []byte) []byte {
	var head []byte
	switch {
	case len(b) < 24:
		head = []byte{0x40 | byte(len(b))}
	case len(b) < 256:
		head = []byte{0x58, byte(len(b))}
	default:
		head = []byte{0x59, byte(len(b) >> 8), byte(len(b))}
	}
	return append(head, b...)
}

func cborTextString(s string) []byte {
	return append([]byte{0x60 | byte(len(s))}, s...)
}

func (a *softAuth) coseKey() []byte {
	pad := func(n *big.Int) []byte {
		b := n.Bytes()
		out := make([]byte, 32)
		copy(out[32-len(b):], b)
		return out
	}
	out := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21}
	out = append(out, cborByteString(pad(a.priv.PublicKey.X))...)
	out = append(out, 0x22)
	out = append(out, cborByteString(pad(a.priv.PublicKey.Y))...)
	return out
}

func rpHashOf(origin string) []byte {
	u, err := url.Parse(origin)
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256([]byte(u.Hostname()))
	return h[:]
}

func (a *softAuth) createPayload(origin, challenge string) map[string]any {
	authData := rpHashOf(origin)
	authData = append(authData, 0x45) // UP | UV | AT
	authData = binary.BigEndian.AppendUint32(authData, a.counter)
	authData = append(authData, make([]byte, 16)...) // AAGUID
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(a.credID)))
	authData = append(authData, a.credID...)
	authData = append(authData, a.coseKey()...)
	attObj := []byte{0xa3}
	attObj = append(attObj, cborTextString("fmt")...)
	attObj = append(attObj, cborTextString("none")...)
	attObj = append(attObj, cborTextString("attStmt")...)
	attObj = append(attObj, 0xa0)
	attObj = append(attObj, cborTextString("authData")...)
	attObj = append(attObj, cborByteString(authData)...)
	clientData := fmt.Sprintf(`{"type":"webauthn.create","challenge":%q,"origin":%q,"crossOrigin":false}`, challenge, origin)
	return map[string]any{
		"id": b64url(a.credID), "rawId": b64url(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64url([]byte(clientData)),
			"attestationObject": b64url(attObj),
		},
	}
}

func (a *softAuth) assertionPayload(t *testing.T, origin, challenge string, handle []byte, counter uint32) map[string]any {
	t.Helper()
	authData := rpHashOf(origin)
	authData = append(authData, 0x05) // UP | UV
	authData = binary.BigEndian.AppendUint32(authData, counter)
	clientData := fmt.Sprintf(`{"type":"webauthn.get","challenge":%q,"origin":%q,"crossOrigin":false}`, challenge, origin)
	cdHash := sha256.Sum256([]byte(clientData))
	signed := append(append([]byte{}, authData...), cdHash[:]...)
	digest := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, a.priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": b64url(a.credID), "rawId": b64url(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64url([]byte(clientData)),
			"authenticatorData": b64url(authData),
			"signature":         b64url(sig),
			"userHandle":        b64url(handle),
		},
	}
}

func challengeOf(t *testing.T, out map[string]any) string {
	t.Helper()
	opts, ok := out["options"].(map[string]any)
	if !ok {
		t.Fatalf("begin options: %v", out)
	}
	pk, ok := opts["publicKey"].(map[string]any)
	if !ok {
		t.Fatalf("begin publicKey: %v", opts)
	}
	ch, ok := pk["challenge"].(string)
	if !ok || ch == "" {
		t.Fatalf("begin challenge: %v", pk)
	}
	return ch
}

// userPasskeySetup: member admission on, panel RP configured, one
// password user already signed in to the panel as a member.
func userPasskeySetup(t *testing.T) (*Server, *apiClient, *apiClient, store.User) {
	t.Helper()
	s, admin, _ := memberSetup(t, true)
	s.cfg.PublicURL = "https://panel.example.com"
	u := pwUser(t, s, "pkuser", "pkuser", "pk@example.com", "password-123")
	code, out := memberPasswordLogin(t, admin.base, "pkuser", "password-123")
	if code != 200 {
		t.Fatalf("member password login: %d %v", code, out)
	}
	mc := &apiClient{t: t, base: admin.base, tok: out["token"].(string)}
	return s, admin, mc, u
}

func registerPanelPasskey(t *testing.T, mc *apiClient, auth *softAuth, name string) string {
	t.Helper()
	code, out := mc.do("POST", "/auth/passkeys/user/register/begin", map[string]any{"name": name, "currentPassword": "password-123"})
	if code != 200 {
		t.Fatalf("register begin: %d %v", code, out)
	}
	cred := auth.createPayload("https://panel.example.com", challengeOf(t, out))
	code, out = mc.do("POST", "/auth/passkeys/user/register/finish", map[string]any{"challengeId": out["challengeId"], "credential": cred})
	if code != 201 {
		t.Fatalf("register finish: %d %v", code, out)
	}
	pk, _ := out["passkey"].(map[string]any)
	id, _ := pk["id"].(string)
	if id == "" {
		t.Fatalf("register finish payload: %v", out)
	}
	return id
}

func panelPasskeyLogin(t *testing.T, base string, auth *softAuth, handle []byte, counter uint32) (int, map[string]any) {
	t.Helper()
	anon := &apiClient{t: t, base: base}
	code, out := anon.do("POST", "/auth/passkeys/user/login/begin", nil)
	if code != 200 {
		t.Fatalf("login begin: %d %v", code, out)
	}
	cred := auth.assertionPayload(t, "https://panel.example.com", challengeOf(t, out), handle, counter)
	return anon.do("POST", "/auth/passkeys/user/login/finish", map[string]any{"challengeId": out["challengeId"], "credential": cred})
}

func TestUserPasskeyPanelRegisterAndLogin(t *testing.T) {
	s, admin, mc, u := userPasskeySetup(t)
	auth := newSoftAuth(t)
	id := registerPanelPasskey(t, mc, auth, "我的手机")

	var row store.UserPasskey
	if err := s.st.DB.First(&row, "id = ?", id).Error; err != nil {
		t.Fatalf("key row: %v", err)
	}
	if row.UserID != u.ID || row.RPID != "panel.example.com" || row.CredentialHash == "" || row.Data == "" {
		t.Fatalf("key row state: %+v", row)
	}
	// The admin table stays empty: separate tables, separate worlds.
	var admins int64
	s.st.DB.Model(&store.Passkey{}).Count(&admins)
	if admins != 0 {
		t.Fatal("user key landed in the admin table")
	}
	// The binding view counts the key as a method.
	if code, out := mc.do("GET", "/auth/bindings", nil); code != 200 || out["passkey"] != true || out["passkeyCount"] != float64(1) {
		t.Fatalf("bindings view: %d %v", code, out)
	}

	// Discoverable login: no username, the credential finds the user.
	code, out := panelPasskeyLogin(t, admin.base, auth, []byte(u.ID), 2)
	if code != 200 || out["token"] == "" {
		t.Fatalf("passkey login: %d %v", code, out)
	}
	lc := &apiClient{t: t, base: admin.base, tok: out["token"].(string)}
	if code, me := lc.do("GET", "/auth/me", nil); code != 200 || me["role"] != "member" {
		t.Fatalf("me after passkey login: %d %v", code, me)
	}
	// Sign count advanced and last-used was stamped.
	if err := s.st.DB.First(&row, "id = ?", id).Error; err != nil || row.LastUsedAt == nil {
		t.Fatalf("key after login: %+v %v", row, err)
	}
	cred, err := s.readUserPasskey(row)
	if err != nil || cred.Authenticator.SignCount != 2 {
		t.Fatalf("sign count after login: %+v %v", cred, err)
	}

	// An unknown credential gets the one vague failure.
	stranger := newSoftAuth(t)
	if code, _ := panelPasskeyLogin(t, admin.base, stranger, []byte(u.ID), 2); code != 401 {
		t.Fatalf("unknown credential login: %d", code)
	}
	// A known credential with someone else's handle fails too.
	if code, _ := panelPasskeyLogin(t, admin.base, auth, []byte("someone-else"), 3); code != 401 {
		t.Fatalf("foreign handle login: %d", code)
	}
}

func TestUserPasskeyCloneWarningRejected(t *testing.T) {
	s, admin, mc, u := userPasskeySetup(t)
	auth := newSoftAuth(t)
	id := registerPanelPasskey(t, mc, auth, "克隆测试")
	if code, _ := panelPasskeyLogin(t, admin.base, auth, []byte(u.ID), 5); code != 200 {
		t.Fatalf("first login: %d", code)
	}
	// Replaying the same counter is a clone warning: rejected, and
	// the stored counter does not move.
	if code, _ := panelPasskeyLogin(t, admin.base, auth, []byte(u.ID), 5); code != 401 {
		t.Fatalf("replayed counter login: %d", code)
	}
	if code, _ := panelPasskeyLogin(t, admin.base, auth, []byte(u.ID), 3); code != 401 {
		t.Fatalf("regressed counter login: %d", code)
	}
	var row store.UserPasskey
	if err := s.st.DB.First(&row, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	cred, err := s.readUserPasskey(row)
	if err != nil || cred.Authenticator.SignCount != 5 {
		t.Fatalf("sign count after clone attempts: %+v %v", cred, err)
	}
	var audits int64
	s.st.DB.Model(&store.AuditLog{}).Where("action = ? AND admin = ?", "user.passkey_login_failed", "pk:"+u.Username).Count(&audits)
	if audits == 0 {
		t.Fatal("clone warning was not audited")
	}
	// A fresh counter still works.
	if code, _ := panelPasskeyLogin(t, admin.base, auth, []byte(u.ID), 6); code != 200 {
		t.Fatalf("login after clone attempts: %d", code)
	}
}

func TestUserPasskeySecondFactorAndChallengeBinding(t *testing.T) {
	s, admin, mc, u := userPasskeySetup(t)
	// The account has a password: begin refuses without it and with a
	// wrong one (the user-side equivalent of the admins' sudo gate).
	if code, out := mc.do("POST", "/auth/passkeys/user/register/begin", map[string]any{"name": "x"}); code != 400 {
		t.Fatalf("begin without second factor: %d %v", code, out)
	}
	if code, out := mc.do("POST", "/auth/passkeys/user/register/begin", map[string]any{"name": "x", "currentPassword": "wrong-password"}); code != 400 {
		t.Fatalf("begin with wrong password: %d %v", code, out)
	}
	code, out := mc.do("POST", "/auth/passkeys/user/register/begin", map[string]any{"name": "x", "currentPassword": "password-123"})
	if code != 200 {
		t.Fatalf("begin: %d %v", code, out)
	}
	challengeID, ch := out["challengeId"].(string), challengeOf(t, out)

	// Another member's session cannot finish this challenge.
	pwUser(t, s, "pkother", "pkother", "other@example.com", "password-456")
	code2, out2 := memberPasswordLogin(t, admin.base, "pkother", "password-456")
	if code2 != 200 {
		t.Fatalf("other member login: %d %v", code2, out2)
	}
	oc := &apiClient{t: t, base: admin.base, tok: out2["token"].(string)}
	auth := newSoftAuth(t)
	if code, out := oc.do("POST", "/auth/passkeys/user/register/finish", map[string]any{"challengeId": challengeID, "credential": auth.createPayload("https://panel.example.com", ch)}); code != 400 {
		t.Fatalf("cross-session finish: %d %v", code, out)
	}
	var n int64
	s.st.DB.Model(&store.UserPasskey{}).Count(&n)
	if n != 0 {
		t.Fatal("cross-session finish created a key")
	}

	// An expired challenge is refused (and was consumed by the try).
	code, out = mc.do("POST", "/auth/passkeys/user/register/begin", map[string]any{"name": "y", "currentPassword": "password-123"})
	if code != 200 {
		t.Fatalf("begin 2: %d %v", code, out)
	}
	id2, ch2 := out["challengeId"].(string), challengeOf(t, out)
	// UTC on purpose: the stored timestamps are UTC, and a local-time
	// value would compare wrong as a string in SQLite.
	s.st.DB.Model(&store.EphemeralKV{}).Where("name = ?", "userpasskey:register:"+id2).Update("expires_at", time.Now().UTC().Add(-time.Minute))
	if code, out := mc.do("POST", "/auth/passkeys/user/register/finish", map[string]any{"challengeId": id2, "credential": auth.createPayload("https://panel.example.com", ch2)}); code != 400 {
		t.Fatalf("expired challenge finish: %d %v", code, out)
	}

	// An epoch bump (any credential change) voids an in-flight
	// registration: the challenge dies with the session generation.
	code, out = mc.do("POST", "/auth/passkeys/user/register/begin", map[string]any{"name": "z", "currentPassword": "password-123"})
	if code != 200 {
		t.Fatalf("begin 3: %d %v", code, out)
	}
	id3, ch3 := out["challengeId"].(string), challengeOf(t, out)
	if err := s.st.DB.Transaction(func(tx *gorm.DB) error { return bumpUserEpochTx(tx, u.ID) }); err != nil {
		t.Fatal(err)
	}
	if code, _ := mc.do("POST", "/auth/passkeys/user/register/finish", map[string]any{"challengeId": id3, "credential": auth.createPayload("https://panel.example.com", ch3)}); code == 201 {
		t.Fatal("in-flight registration survived an epoch bump")
	}
	s.st.DB.Model(&store.UserPasskey{}).Where("user_id = ?", u.ID).Count(&n)
	if n != 0 {
		t.Fatal("voided registration created a key")
	}
}

func portalPasskeyRegister(t *testing.T, browser *portalClient, auth *softAuth, origin, name string) {
	t.Helper()
	code, body := browser.req("POST", portalPath+"/passkeys/register/begin", map[string]any{"name": name, "currentPassword": "password-123"}, true)
	if code != 200 {
		t.Fatalf("portal register begin: %d %s", code, body)
	}
	var begin struct {
		ChallengeID string         `json:"challengeId"`
		Options     map[string]any `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &begin); err != nil {
		t.Fatalf("portal register begin body: %v", err)
	}
	cred := auth.createPayload(origin, challengeOf(t, map[string]any{"options": begin.Options}))
	code, body = browser.req("POST", portalPath+"/passkeys/register/finish", map[string]any{"challengeId": begin.ChallengeID, "credential": cred}, true)
	if code != 201 {
		t.Fatalf("portal register finish: %d %s", code, body)
	}
}

func portalPasskeyLogin(t *testing.T, base string, auth *softAuth, origin string, handle []byte, counter uint32, subToken string) (int, *portalClient) {
	t.Helper()
	browser := newShopClient(t, base)
	code, body := browser.req("POST", portalPath+"/passkeys/login/begin", map[string]any{}, true)
	if code != 200 {
		t.Fatalf("portal login begin: %d %s", code, body)
	}
	var begin struct {
		ChallengeID string         `json:"challengeId"`
		Options     map[string]any `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &begin); err != nil {
		t.Fatalf("portal login begin body: %v", err)
	}
	cred := auth.assertionPayload(t, origin, challengeOf(t, map[string]any{"options": begin.Options}), handle, counter)
	code, _ = browser.req("POST", portalPath+"/passkeys/login/finish", map[string]any{"challengeId": begin.ChallengeID, "credential": cred}, true)
	if code == 200 {
		// The new browser now holds the session: the account page
		// must show the user, not the sign-in form.
		if _, page := browser.req("GET", portalPath, nil, false); !strings.Contains(page, subToken) {
			t.Fatal("portal passkey session does not show the account")
		}
	}
	return code, browser
}

func TestUserPasskeyPortalFaceAndRPIsolation(t *testing.T) {
	s, admin, _, _ := userPasskeySetup(t)
	s.st.SetSetting("portal_enabled", "true")
	u := pwUser(t, s, "pkportal", "pkportal", "", "password-123")
	// Align the fixture's subscription token with the assertion in
	// portalPasskeyLogin.
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("sub_token", "pkportal-sub-token")
	base := portalBaseOf(s, admin)

	// No portal_public_url: the face has no RP — begin refuses with
	// the configuration error and the login page hides the button.
	anon := newShopClient(t, base)
	if code, body := anon.req("POST", portalPath+"/passkeys/login/begin", map[string]any{}, true); code != 400 || !strings.Contains(body, "尚未配置") {
		t.Fatalf("portal begin without RP config: %d %s", code, body)
	}
	if _, page := anon.req("GET", portalPath, nil, false); strings.Contains(page, `id="pkgo"`) {
		t.Fatal("login page shows the passkey button without an RP")
	}

	s.st.SetSetting("portal_public_url", "https://portal.example.com")
	if _, page := anon.req("GET", portalPath, nil, false); !strings.Contains(page, `id="pkgo"`) {
		t.Fatal("login page hides the passkey button with an RP configured")
	}

	// Sign in with the password and register a key on the portal RP.
	browser := newShopClient(t, base)
	if code, body := browser.req("POST", portalPath+"/password/login", map[string]any{"username": "pkportal", "password": "password-123"}, true); code != 200 {
		t.Fatalf("portal password login: %d %s", code, body)
	}
	auth := newSoftAuth(t)
	portalPasskeyRegister(t, browser, auth, "https://portal.example.com", "门户密钥")
	var row store.UserPasskey
	if err := s.st.DB.First(&row, "user_id = ?", u.ID).Error; err != nil || row.RPID != "portal.example.com" {
		t.Fatalf("portal key row: %+v %v", row, err)
	}

	// The portal face accepts its own key…
	if code, _ := portalPasskeyLogin(t, base, auth, "https://portal.example.com", []byte(u.ID), 2, u.SubToken); code != 200 {
		t.Fatalf("portal passkey login: %d", code)
	}
	// …but the panel face cannot use it: the credential lookup is
	// scoped to the panel RP, where this key does not exist.
	if code, _ := panelPasskeyLogin(t, admin.base, auth, []byte(u.ID), 3); code != 401 {
		t.Fatalf("cross-RP login at the panel: %d", code)
	}

	// Same-domain deployment: pointing the portal RP at the panel's
	// domain makes panel-registered keys work on the portal face.
	s.st.SetSetting("portal_public_url", "https://panel.example.com")
	panelAuth := newSoftAuth(t)
	code, out := memberPasswordLogin(t, admin.base, "pkportal", "password-123")
	if code != 200 {
		t.Fatalf("member login for shared-RP user: %d %v", code, out)
	}
	mc := &apiClient{t: t, base: admin.base, tok: out["token"].(string)}
	registerPanelPasskey(t, mc, panelAuth, "共享密钥")
	if code, _ := portalPasskeyLogin(t, base, panelAuth, "https://panel.example.com", []byte(u.ID), 2, u.SubToken); code != 200 {
		t.Fatalf("shared-RP portal login with a panel key: %d", code)
	}
}

func TestUserPasskeyDeleteRedLineAndSessionKill(t *testing.T) {
	s, admin, mc, u := userPasskeySetup(t)
	s.st.SetSetting("portal_enabled", "true")
	s.st.SetSetting("portal_public_url", "https://panel.example.com")
	auth := newSoftAuth(t)
	keyID := registerPanelPasskey(t, mc, auth, "唯一密钥")

	// Sessions issued by the key, one per face.
	code, out := panelPasskeyLogin(t, admin.base, auth, []byte(u.ID), 2)
	if code != 200 {
		t.Fatalf("panel passkey login: %d %v", code, out)
	}
	keyClient := &apiClient{t: t, base: admin.base, tok: out["token"].(string)}
	code, keyBrowser := portalPasskeyLogin(t, portalBaseOf(s, admin), auth, "https://panel.example.com", []byte(u.ID), 3, u.SubToken)
	if code != 200 {
		t.Fatalf("portal passkey login: %d", code)
	}

	// While the password exists the key may go; strip the password
	// (directly — the self-service clear would itself be refused
	// only at one method) and the key becomes the last method.
	if code, out := mc.do("POST", "/auth/passkeys/user/delete", map[string]any{"id": keyID, "currentPassword": "wrong"}); code != 400 {
		t.Fatalf("delete with wrong second factor: %d %v", code, out)
	}
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Updates(map[string]any{"password_hash": "", "email": ""})
	if code, out := mc.do("POST", "/auth/passkeys/user/delete", map[string]any{"id": keyID}); code != 409 || out["code"] != "last_login_method" {
		t.Fatalf("delete last method: %d %v", code, out)
	}
	// Restore a second method; the delete now succeeds and every
	// session the key issued dies on both faces.
	s.st.DB.Model(&store.User{}).Where("id = ?", u.ID).Update("email", "pk@example.com")
	if code, out := mc.do("POST", "/auth/passkeys/user/delete", map[string]any{"id": keyID}); code != 200 {
		t.Fatalf("delete: %d %v", code, out)
	}
	if code, _ := keyClient.do("GET", "/auth/me", nil); code != 401 {
		t.Fatalf("panel session of a deleted key survived: %d", code)
	}
	if code, _ := keyBrowser.req("GET", portalPath+"/bindings", nil, false); code != 401 {
		t.Fatalf("portal session of a deleted key survived: %d", code)
	}
	var n int64
	s.st.DB.Model(&store.UserPasskey{}).Where("user_id = ?", u.ID).Count(&n)
	if n != 0 {
		t.Fatal("key row survived deletion")
	}
	// And the key itself no longer signs in anywhere.
	if code, _ := panelPasskeyLogin(t, admin.base, auth, []byte(u.ID), 4); code != 401 {
		t.Fatalf("deleted key login: %d", code)
	}
}

func TestUserPasskeyRename(t *testing.T) {
	_, _, mc, _ := userPasskeySetup(t)
	auth := newSoftAuth(t)
	keyID := registerPanelPasskey(t, mc, auth, "旧名字")
	if code, out := mc.do("POST", "/auth/passkeys/user/rename", map[string]any{"id": keyID, "name": "新名字"}); code != 200 {
		t.Fatalf("rename: %d %v", code, out)
	}
	if code, out := mc.do("GET", "/auth/bindings", nil); code != 200 {
		t.Fatalf("bindings: %d %v", code, out)
	} else if keys, _ := out["passkeys"].([]any); len(keys) != 1 || keys[0].(map[string]any)["name"] != "新名字" {
		t.Fatalf("bindings after rename: %v", out["passkeys"])
	}
	if code, _ := mc.do("POST", "/auth/passkeys/user/rename", map[string]any{"id": "missing", "name": "x"}); code != 400 {
		t.Fatalf("rename missing key: %d", code)
	}
}

func TestUserPasskeyAdmissionAndDisabled(t *testing.T) {
	s, admin, _ := memberSetup(t, false) // panel_member_login off
	s.cfg.PublicURL = "https://panel.example.com"
	anon := &apiClient{t: t, base: admin.base}
	if code, _ := anon.do("POST", "/auth/passkeys/user/login/begin", nil); code != 403 {
		t.Fatalf("panel passkey begin with admission off: %d", code)
	}

	s2, admin2, mc2, u2 := userPasskeySetup(t)
	auth := newSoftAuth(t)
	registerPanelPasskey(t, mc2, auth, "禁用测试")
	s2.st.DB.Model(&store.User{}).Where("id = ?", u2.ID).Update("status", "disabled")
	if code, _ := panelPasskeyLogin(t, admin2.base, auth, []byte(u2.ID), 2); code != 401 {
		t.Fatalf("disabled user passkey login: %d", code)
	}
}

func TestUserPasskeyAdminIsolation(t *testing.T) {
	s, admin, mc, u := userPasskeySetup(t)
	// A full admin ceremony with a real software authenticator: the
	// admin flow must work exactly as before, into its own table.
	adminAuth := newSoftAuth(t)
	if code, out := admin.do("POST", "/auth/sudo", map[string]any{"password": "old-password"}); code != 200 {
		t.Fatalf("sudo: %d %v", code, out)
	}
	code, out := admin.do("POST", "/auth/passkeys/register/begin", map[string]any{"name": "管理员密钥"})
	if code != 200 {
		t.Fatalf("admin register begin: %d %v", code, out)
	}
	cred := adminAuth.createPayload("https://panel.example.com", challengeOf(t, out))
	if code, out := admin.do("POST", "/auth/passkeys/register/finish", map[string]any{"challengeId": out["challengeId"], "credential": cred}); code != 201 {
		t.Fatalf("admin register finish: %d %v", code, out)
	}
	var adminKeys, userKeys int64
	s.st.DB.Model(&store.Passkey{}).Count(&adminKeys)
	s.st.DB.Model(&store.UserPasskey{}).Count(&userKeys)
	if adminKeys != 1 || userKeys != 0 {
		t.Fatalf("table counts after admin registration: admin=%d user=%d", adminKeys, userKeys)
	}
	// The admin's credential does not sign a user in…
	if code, _ := panelPasskeyLogin(t, admin.base, adminAuth, []byte("a1"), 2); code != 401 {
		t.Fatalf("admin credential at user login: %d", code)
	}
	// …and a user's credential does not sign an admin in.
	userAuth := newSoftAuth(t)
	registerPanelPasskey(t, mc, userAuth, "用户密钥")
	anon := &apiClient{t: t, base: admin.base}
	code, out = anon.do("POST", "/auth/passkeys/login/begin", nil)
	if code != 200 {
		t.Fatalf("admin login begin: %d %v", code, out)
	}
	cred = userAuth.assertionPayload(t, "https://panel.example.com", challengeOf(t, out), []byte(u.ID), 2)
	if code, out := anon.do("POST", "/auth/passkeys/login/finish", map[string]any{"challengeId": out["challengeId"], "credential": cred}); code != 401 {
		t.Fatalf("user credential at admin login: %d %v", code, out)
	}
	// The admin still signs in with their own key.
	code, out = anon.do("POST", "/auth/passkeys/login/begin", nil)
	if code != 200 {
		t.Fatalf("admin login begin 2: %d %v", code, out)
	}
	cred = adminAuth.assertionPayload(t, "https://panel.example.com", challengeOf(t, out), []byte("a1"), 2)
	if code, out := anon.do("POST", "/auth/passkeys/login/finish", map[string]any{"challengeId": out["challengeId"], "credential": cred}); code != 200 || out["token"] == "" {
		t.Fatalf("admin passkey login: %d %v", code, out)
	}
}

func TestUserPasskeyRPConfigAndInfo(t *testing.T) {
	s, admin, _ := memberSetup(t, true)
	anon := &apiClient{t: t, base: admin.base}
	// No PublicURL: no panel RP, so /info hides the member button.
	if code, out := anon.do("GET", "/info", nil); code != 200 || out["memberPasskeyLogin"] == true {
		t.Fatalf("info without RP: %d %v", code, out)
	}
	s.cfg.PublicURL = "https://panel.example.com"
	if code, out := anon.do("GET", "/info", nil); code != 200 || out["memberPasskeyLogin"] != true {
		t.Fatalf("info with RP: %d %v", code, out)
	}
	// Portal RP derivation follows the same rules as the panel's.
	for _, bad := range []string{"http://portal.example.com", "https://203.0.113.10", "https://user:pw@portal.example.com"} {
		s.st.SetSetting("portal_public_url", bad)
		if s.userPasskeyAvailable("portal") {
			t.Fatalf("unsafe portal RP accepted: %s", bad)
		}
	}
	s.st.SetSetting("portal_public_url", "https://portal.example.com")
	if !s.userPasskeyAvailable("portal") {
		t.Fatal("valid portal RP rejected")
	}
	s.st.SetSetting("portal_public_url", "")
	if s.userPasskeyAvailable("portal") {
		t.Fatal("empty portal RP accepted")
	}
}
