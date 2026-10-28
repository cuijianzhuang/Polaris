package panel

import (
	"bytes"
	"encoding/json"
	"github.com/cuijianzhuang/polaris/internal/backupfmt"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func passkeyFixture(t *testing.T) (*Server, *apiClient) {
	t.Helper()
	s, _, c := newAdminClient(t)
	s.cfg.PublicURL = "https://panel.example.com"
	return s, c
}
func TestPasskeyRPConfigurationAndSingleUse(t *testing.T) {
	s, c := passkeyFixture(t)
	for _, v := range []string{"", "http://public.example.com", "https://u:p@panel.example.com", "https://panel.example.com/?token=x", "https://203.0.113.10", "http://127.0.0.1"} {
		s.cfg.PublicURL = v
		if s.passkeysAvailable() {
			t.Fatal("unsafe RP config", v)
		}
	}
	s.cfg.PublicURL = "https://panel.example.com/secret"
	w, origin, e := s.webAuthn()
	if e != nil || origin != "https://panel.example.com" || w.Config.RPID != "panel.example.com" {
		t.Fatal(origin, e)
	}
	code, out := c.do("POST", "/auth/passkeys/register/begin", map[string]any{"name": "Phone"})
	if code != 200 {
		t.Fatal(code, out)
	}
	id := out["challengeId"].(string)
	opts := out["options"].(map[string]any)["publicKey"].(map[string]any)
	selection := opts["authenticatorSelection"].(map[string]any)
	if selection["userVerification"] != "required" || selection["residentKey"] != "required" {
		t.Fatal("weak ceremony", selection)
	}
	body := map[string]any{"challengeId": id, "credential": map[string]any{}}
	if code, out = c.do("POST", "/auth/passkeys/register/finish", body); code != 400 {
		t.Fatal("invalid credential accepted", code, out)
	}
	if code, out = c.do("POST", "/auth/passkeys/register/finish", body); code != 400 || !strings.Contains(out["error"].(string), "已使用") {
		t.Fatal("challenge not consumed on failure", code, out)
	}
	code, out = c.do("POST", "/auth/passkeys/register/begin", map[string]any{"name": "Expired"})
	if code != 200 {
		t.Fatal(code, out)
	}
	id = out["challengeId"].(string)
	s.st.DB.Model(&store.EphemeralKV{}).Where("name = ?", "passkey:register:"+id).Update("expires_at", time.Now().Add(-time.Minute))
	if code, out = c.do("POST", "/auth/passkeys/register/finish", map[string]any{"challengeId": id, "credential": map[string]any{}}); code != 400 {
		t.Fatal("expired challenge accepted", code, out)
	}
	// Purpose separation prevents submitting a registration challenge as login.
	code, out = c.do("POST", "/auth/passkeys/register/begin", map[string]any{"name": "Purpose"})
	id = out["challengeId"].(string)
	if code, out = c.do("POST", "/auth/passkeys/login/finish", map[string]any{"challengeId": id, "credential": map[string]any{}}); code != 400 {
		t.Fatal("cross-purpose challenge", code, out)
	}
}
func TestPasskeyRegistrationRequiresRecentSessionAndBindsSession(t *testing.T) {
	_, c := passkeyFixture(t)
	code, out := c.do("POST", "/auth/passkeys/register/begin", map[string]any{"name": "Phone"})
	if code != 200 {
		t.Fatal(code, out)
	}
	id := out["challengeId"].(string)
	other := &apiClient{t: t, base: c.base}
	code, out = other.do("POST", "/auth/login", map[string]any{"username": "root", "password": "old-password"})
	if code != 200 {
		t.Fatal(code, out)
	}
	other.tok = out["token"].(string)
	if code, out = other.do("POST", "/auth/passkeys/register/begin", map[string]any{"name": "No sudo"}); code != 403 || out["sudoRequired"] != true {
		t.Fatal("registration bypassed sudo", code, out)
	}
	other.do("POST", "/auth/sudo", map[string]any{"password": "old-password"})
	if code, out = other.do("POST", "/auth/passkeys/register/finish", map[string]any{"challengeId": id, "credential": map[string]any{}}); code != 400 || !strings.Contains(out["error"].(string), "注册会话") {
		t.Fatal("registration moved between sessions", code, out)
	}
}
func TestPasskeyRevokeKillsOnlyKeySessions(t *testing.T) {
	s, c := passkeyFixture(t)
	key := store.Passkey{ID: "key", AdminID: "a1", Name: "phone", RPID: "panel.example.com", CredentialHash: "lookup", Data: s.st.Box.Seal("private-metadata")}
	if e := s.st.DB.Create(&key).Error; e != nil {
		t.Fatal(e)
	}
	other := &apiClient{t: t, base: c.base}
	code, out := other.do("POST", "/auth/login", map[string]any{"username": "root", "password": "old-password"})
	if code != 200 {
		t.Fatal(code, out)
	}
	other.tok = out["token"].(string)
	cl, _ := s.parseToken(other.tok)
	s.st.DB.Model(&store.AdminSession{}).Where("id = ?", cl.SID).Updates(map[string]any{"via": "passkey", "passkey_id": key.ID})
	if code, out = other.do("GET", "/auth/me", nil); code != 200 {
		t.Fatal(code, out)
	}
	code, out = c.do("GET", "/auth/passkeys", nil)
	raw, _ := json.Marshal(out)
	if code != 200 || strings.Contains(string(raw), "private-metadata") || strings.Contains(string(raw), "enc:v1:") || strings.Contains(string(raw), "lookup") {
		t.Fatal("key metadata leaked", code, string(raw))
	}
	if code, out = c.do("DELETE", "/auth/passkeys/key", nil); code != 204 {
		t.Fatal(code, out)
	}
	if code, out = other.do("GET", "/auth/me", nil); code != 401 {
		t.Fatal("revoked key session survived", code, out)
	}
	if code, out = c.do("GET", "/auth/me", nil); code != 200 {
		t.Fatal("unrelated session revoked", code, out)
	}
}
func TestAccountRecoveryResetSingleUseAndSessionRevocation(t *testing.T) {
	s, c := passkeyFixture(t)
	code, out := c.do("POST", "/auth/account-recovery-codes", nil)
	if code != 200 {
		t.Fatal(code, out)
	}
	rawCode := out["recoveryCodes"].([]any)[0].(string)
	if len(rawCode) < 32 {
		t.Fatal("weak recovery code")
	}
	s.st.DB.Create(&store.Passkey{ID: "lost-key", AdminID: "a1", Name: "Lost", CredentialHash: "lost", Data: s.st.Box.Seal("key")})
	s.st.DB.Model(&store.Admin{}).Where("id = ?", "a1").Updates(map[string]any{"totp_enabled": true, "totp_secret": s.st.Box.Seal("lost-totp")})
	var rows []store.AccountRecoveryCode
	s.st.DB.Find(&rows)
	if len(rows) != 10 {
		t.Fatal("missing codes")
	}
	for _, r := range rows {
		if r.Hash == rawCode {
			t.Fatal("plaintext recovery code")
		}
	}
	anon := &apiClient{t: t, base: c.base}
	req := map[string]any{"username": "root", "code": rawCode, "password": "recovered-password"}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); status, _ := anon.do("POST", "/auth/account-recovery", req); codes <- status }()
	}
	wg.Wait()
	close(codes)
	success := 0
	for st := range codes {
		if st == 200 {
			success++
		} else if st != 401 {
			t.Fatal("unexpected recovery status", st)
		}
	}
	if success != 1 {
		t.Fatal("recovery code not atomic", success)
	}
	if code, out = c.do("GET", "/auth/me", nil); code != 401 {
		t.Fatal("old session survived", code, out)
	}
	var a store.Admin
	s.st.DB.First(&a, "id = ?", "a1")
	if a.TOTPEnabled || a.TOTPSecret != "" || !passwordMatches(a.PasswordHash, "recovered-password") {
		t.Fatal("recovery incomplete")
	}
	var n int64
	s.st.DB.Model(&store.Passkey{}).Count(&n)
	if n != 0 {
		t.Fatal("keys survived recovery")
	}
	s.st.DB.Model(&store.AccountRecoveryCode{}).Count(&n)
	if n != 0 {
		t.Fatal("old recovery codes survived")
	}
	if code, out = anon.do("POST", "/auth/account-recovery", req); code != 401 {
		t.Fatal("recovery replay accepted", code, out)
	}
	if code, out = anon.do("POST", "/auth/login", map[string]any{"username": "root", "password": "recovered-password"}); code != 200 {
		t.Fatal("password fallback unavailable", code, out)
	}
}

func TestPasskeyChallengesDoNotSurviveBackupRestore(t *testing.T) {
	s, _ := passkeyFixture(t)
	id, e := s.newPasskeyChallenge(passkeyChallenge{Kind: "login", Origin: "https://panel.example.com"})
	if e != nil {
		t.Fatal(e)
	}
	var archive bytes.Buffer
	if e = writeArchive(&archive, s.st, s.cfg.DataDir, s.cfg.SecretKey, "archive-key"); e != nil {
		t.Fatal(e)
	}
	var consumed passkeyChallenge
	if !s.eph.take("passkey:login:"+id, &consumed) {
		t.Fatal("challenge not stored")
	}
	arc, e := backupfmt.ReadArchive(bytes.NewReader(archive.Bytes()), "archive-key", s.st.Box)
	if e != nil {
		t.Fatal(e)
	}
	if e = restoreArchive(s.st, s.cfg.DataDir, arc, s.st.Box); e != nil {
		t.Fatal(e)
	}
	if s.eph.take("passkey:login:"+id, &consumed) {
		t.Fatal("restore revived a spent challenge")
	}
}
func TestPasskeySecurityResponsesAreNeverIdempotencyCached(t *testing.T) {
	s, c := passkeyFixture(t)
	req, _ := http.NewRequest("POST", c.base+"/auth/account-recovery-codes", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.tok)
	req.Header.Set("Idempotency-Key", "must-not-cache-secrets")
	response, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
	var n int64
	s.st.DB.Model(&store.IdempotencyKey{}).Count(&n)
	if n != 0 {
		t.Fatal("recovery code entered plaintext response cache")
	}
}
func TestPasskeySecurityMutationRejectsStaleSession(t *testing.T) {
	s, c := passkeyFixture(t)
	cl, _ := s.parseToken(c.tok)
	// Check the security state again inside the transaction, after middleware
	// could already have authenticated an in-flight request.
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("adminID", "a1")
	ctx.Set("sid", cl.SID)
	ctx.Set("tokenVersion", cl.Version)
	s.st.DB.Model(&store.AdminSession{}).Where("id = ?", cl.SID).Update("revoked", true)
	e := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if e := reservePasskeys(tx); e != nil {
			return e
		}
		return s.lockPasskeySession(tx, ctx)
	})
	if e == nil {
		t.Fatal("stale session could mint security credentials")
	}
}

func TestAdminPasskeyEmergencyRecovery(t *testing.T) {
	s, c := passkeyFixture(t)
	s.st.DB.Create(&store.Passkey{ID: "own-key", AdminID: "a1", CredentialHash: "own", Data: s.st.Box.Seal("data")})
	s.st.DB.Create(&store.Admin{ID: "other-admin", Username: "other"})
	s.st.DB.Create(&store.Passkey{ID: "other-key", AdminID: "other-admin", CredentialHash: "other", Data: s.st.Box.Seal("data")})
	if code, out := c.do("POST", "/auth/account-recovery-codes", nil); code != 200 {
		t.Fatal(code, out)
	}
	if _, e := AdminResetPasskeys(s.cfg, "root"); e != nil {
		t.Fatal(e)
	}
	if code, out := c.do("GET", "/auth/me", nil); code != 401 {
		t.Fatal("emergency reset left old session", code, out)
	}
	var n int64
	s.st.DB.Model(&store.Passkey{}).Where("admin_id = ?", "a1").Count(&n)
	if n != 0 {
		t.Fatal("old keys survived")
	}
	s.st.DB.Model(&store.AccountRecoveryCode{}).Where("admin_id = ?", "a1").Count(&n)
	if n != 0 {
		t.Fatal("old recovery codes survived")
	}
	s.st.DB.Model(&store.Passkey{}).Where("admin_id = ?", "other-admin").Count(&n)
	if n != 1 {
		t.Fatal("another account's key removed")
	}
}
