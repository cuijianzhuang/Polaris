package panel

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cuijianzhuang/polaris/internal/backupfmt"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/secret"
	"github.com/cuijianzhuang/polaris/internal/store"
)

const (
	rotationOldKey   = "old-rotation-key-0123456789"
	rotationNewKey   = "new-rotation-key-0123456789"
	rotationThirdKey = "third-rotation-key-0123456789"
)

func newRotationServer(t *testing.T, dir, key, prevKey string) *Server {
	t.Helper()
	cfg := &Config{DataDir: dir, SecretKey: key, SecretKeyPrevious: prevKey, AdminPath: "/"}
	cfg.DB.DSN = filepath.Join(dir, "p.db")
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func closeServerDB(t *testing.T, s *Server) {
	t.Helper()
	if db, err := s.st.DB.DB(); err == nil {
		db.Close()
	}
}

// rotationToken signs a claims set with key, stamping a kid when asked
// (a pre-rotation token has none).
func rotationToken(t *testing.T, key string, withKid bool) string {
	t.Helper()
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "a1",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Username: "root",
		Version:  1,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	if withKid {
		tok.Header["kid"] = secret.KidFor(key)
	}
	signed, err := tok.SignedString([]byte("jwt:" + key))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestJWTVerifiesBothKeysAndRejectsUnknownKid(t *testing.T) {
	oldS := newRotationServer(t, t.TempDir(), rotationOldKey, "")
	defer closeServerDB(t, oldS)
	newS := newRotationServer(t, t.TempDir(), rotationNewKey, rotationOldKey)
	defer closeServerDB(t, newS)

	oldKidTok := rotationToken(t, rotationOldKey, true)
	legacyTok := rotationToken(t, rotationOldKey, false)
	newTok := rotationToken(t, rotationNewKey, true)
	thirdTok := rotationToken(t, rotationThirdKey, true)

	// The rotated server verifies tokens from both keys, with and
	// without a kid header.
	for name, tok := range map[string]string{"old-kid": oldKidTok, "legacy": legacyTok, "new": newTok} {
		c, err := newS.parseToken(tok)
		if err != nil || c.Username != "root" {
			t.Fatalf("rotated server rejected %s token: %v", name, err)
		}
		if got := tokenKid(tok); name == "new" && got != secret.KidFor(rotationNewKey) {
			t.Fatalf("new token kid %q, want %q", got, secret.KidFor(rotationNewKey))
		}
	}
	// A token whose kid names a key outside the configured set is rejected.
	if _, err := newS.parseToken(thirdTok); err == nil {
		t.Fatal("token with an unknown kid accepted")
	}
	// The old server does not know the new key at all.
	if _, err := oldS.parseToken(newTok); err == nil {
		t.Fatal("old server accepted a token signed with the new key")
	}
	// A token with a kid that lies about its key is rejected, not retried
	// against the other key.
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "a1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Username: "root",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tok.Header["kid"] = secret.KidFor(rotationNewKey)
	signed, err := tok.SignedString([]byte("jwt:" + rotationOldKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newS.parseToken(signed); err == nil {
		t.Fatal("token with a lying kid accepted")
	}
	// signToken stamps the current kid.
	if signed, err := newS.signToken(jwt.NewWithClaims(jwt.SigningMethodHS256, c)); err != nil {
		t.Fatal(err)
	} else if tokenKid(signed) != secret.KidFor(rotationNewKey) {
		t.Fatalf("signToken kid %q, want %q", tokenKid(signed), secret.KidFor(rotationNewKey))
	}
}

func TestSecretKeyRotationEndToEnd(t *testing.T) {
	dir := t.TempDir()
	oldS := newRotationServer(t, dir, rotationOldKey, "")
	// Seed sealed rows across the migrated tables. Most are v2 with the
	// old kid (what the panel writes now); one is a genuine v1 row from
	// before rotation support, sealed with the old single Box.
	oldS.st.SetSetting("tg_token", oldS.st.Box.Seal("123:bot-token"))
	oldBox, _ := secret.New(rotationOldKey)
	oldS.st.SetSetting("legacy_token", oldBox.Seal("legacy-value"))
	oldS.st.SetSetting("plain_note", "hello") // plaintext: must stay plaintext
	if err := oldS.st.DB.Create(&store.Certificate{ID: "cert1", Domain: "example.com", Provider: "manual", Cert: "CERT", Key: oldS.st.Box.Seal("cert-private-key")}).Error; err != nil {
		t.Fatal(err)
	}
	if err := oldS.st.DB.Create(&store.Admin{ID: "a1", Username: "root", PasswordHash: "x", TOTPSecret: oldS.st.Box.Seal("totp-secret")}).Error; err != nil {
		t.Fatal(err)
	}
	if err := oldS.st.DB.Create(&store.SubSource{ID: "ss1", Name: "s", Kind: "url", URL: oldS.st.Box.Seal("https://provider.example/x")}).Error; err != nil {
		t.Fatal(err)
	}
	if err := oldS.st.DB.Create(&store.CDNProfile{ID: "cdn1", Name: "c", Provider: "cloudflare", APIToken: oldS.st.Box.Seal("cf-token")}).Error; err != nil {
		t.Fatal(err)
	}
	if err := oldS.st.DB.Create(&store.Webhook{ID: "wh1", Name: "w", URL: "https://example.com/hook", Secret: oldS.st.Box.Seal("hook-secret")}).Error; err != nil {
		t.Fatal(err)
	}
	if err := oldS.st.DB.Create(&store.Inbound{ID: "in1", NodeID: "n1", Core: "xray", Tag: "t1", Listen: "0.0.0.0", Port: 443,
		Security: store.NewJSON(model.SecuritySpec{Type: "reality", RealityPrivateKey: oldS.st.Box.Seal("reality-key")})}).Error; err != nil {
		t.Fatal(err)
	}
	if err := oldS.st.DB.Create(&store.EphemeralKV{Name: "code:1", Value: oldS.st.Box.Seal(`{"code":"123456"}`), ExpiresAt: time.Now().Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	caKeyBefore, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	// A fresh panel already writes v2 (with the old kid); v1 only exists
	// in databases created before rotation support.
	if !strings.HasPrefix(string(caKeyBefore), "enc:v2:"+secret.KidFor(rotationOldKey)+":") {
		t.Fatalf("CA key file is not v2 with the old kid before rotation: %q", string(caKeyBefore)[:20])
	}
	oldKidTok := rotationToken(t, rotationOldKey, true)
	legacyTok := rotationToken(t, rotationOldKey, false)
	closeServerDB(t, oldS)

	// Rotate: new key + previous, same database.
	newS := newRotationServer(t, dir, rotationNewKey, rotationOldKey)
	defer closeServerDB(t, newS)
	newKid := secret.KidFor(rotationNewKey)

	// Old sessions still verify before the migration runs.
	for name, tok := range map[string]string{"old-kid": oldKidTok, "legacy": legacyTok} {
		if c, err := newS.parseToken(tok); err != nil || c.Username != "root" {
			t.Fatalf("pre-migration %s token rejected: %v", name, err)
		}
	}
	// The CA key file still carries the old kid until the migration runs.
	if b, _ := os.ReadFile(filepath.Join(dir, "ca.key")); !strings.HasPrefix(string(b), "enc:v2:"+secret.KidFor(rotationOldKey)+":") {
		t.Fatal("CA key file changed before the migration ran")
	}

	res := newS.runSecretMigration()
	if !res.Done || res.Failed != 0 {
		t.Fatalf("migration: done=%v failed=%d rows=%v", res.Done, res.Failed, res.FailedRows)
	}
	if res.Updated < 10 {
		t.Fatalf("migration updated %d rows, want at least 10", res.Updated)
	}
	if !res.CAChanged {
		t.Fatal("migration did not re-encrypt the CA key file")
	}
	// A second run is a no-op (idempotent, resumable progress).
	res2 := newS.runSecretMigration()
	if !res2.Done || res2.Updated != 0 || res2.Failed != 0 {
		t.Fatalf("second migration run: %+v", res2)
	}

	// Every seeded row is now v2 with the new kid and still opens.
	newPrefix := "enc:v2:" + newKid + ":"
	check := func(name, sealed, want string) {
		t.Helper()
		if !strings.HasPrefix(sealed, newPrefix) {
			t.Fatalf("%s not re-encrypted: %q", name, sealed)
		}
		if got := newS.st.Box.MustOpen(sealed); got != want {
			t.Fatalf("%s opens to %q, want %q", name, got, want)
		}
	}
	check("tg_token", newS.st.GetSetting("tg_token", ""), "123:bot-token")
	check("legacy_token", newS.st.GetSetting("legacy_token", ""), "legacy-value")
	if got := newS.st.GetSetting("plain_note", ""); got != "hello" {
		t.Fatalf("plaintext setting changed: %q", got)
	}
	var cert store.Certificate
	newS.st.DB.First(&cert, "id = ?", "cert1")
	check("certificate.key", cert.Key, "cert-private-key")
	var admin store.Admin
	newS.st.DB.First(&admin, "id = ?", "a1")
	check("admin.totp_secret", admin.TOTPSecret, "totp-secret")
	var src store.SubSource
	newS.st.DB.First(&src, "id = ?", "ss1")
	check("sub_source.url", src.URL, "https://provider.example/x")
	var cdn store.CDNProfile
	newS.st.DB.First(&cdn, "id = ?", "cdn1")
	check("cdn_profile.api_token", cdn.APIToken, "cf-token")
	var wh store.Webhook
	newS.st.DB.First(&wh, "id = ?", "wh1")
	check("webhook.secret", wh.Secret, "hook-secret")
	var in store.Inbound
	newS.st.DB.First(&in, "id = ?", "in1")
	check("inbound.reality_key", in.Security.V.RealityPrivateKey, "reality-key")
	var eph store.EphemeralKV
	newS.st.DB.First(&eph, "name = ?", "code:1")
	check("ephemeral value", eph.Value, `{"code":"123456"}`)
	var caRow store.ClusterSecret
	newS.st.DB.First(&caRow, "name = ?", "ca.key")
	check("cluster_secrets ca.key", caRow.Value, string(newS.ca.KeyPEM))
	caKeyAfter, _ := os.ReadFile(filepath.Join(dir, "ca.key"))
	check("ca.key file", strings.TrimSpace(string(caKeyAfter)), string(newS.ca.KeyPEM))
	if _, err := os.Stat(filepath.Join(dir, "ca.key.prev")); !os.IsNotExist(err) {
		t.Fatal("old CA key copy left behind after a finished migration")
	}
	if got := newS.st.GetSetting(secretKeyVersionSetting, ""); got != newKid {
		t.Fatalf("secret_key_version %q, want %q", got, newKid)
	}

	// Old sessions still verify after the migration; new sessions use
	// the new kid.
	for name, tok := range map[string]string{"old-kid": oldKidTok, "legacy": legacyTok} {
		if _, err := newS.parseToken(tok); err != nil {
			t.Fatalf("post-migration %s token rejected: %v", name, err)
		}
	}
	newTok := rotationToken(t, rotationNewKey, true)
	if _, err := newS.parseToken(newTok); err != nil {
		t.Fatalf("new token rejected: %v", err)
	}
	closeServerDB(t, newS)

	// Restart without the previous key: everything still works, old
	// sessions are now rejected.
	newOnly := newRotationServer(t, dir, rotationNewKey, "")
	defer closeServerDB(t, newOnly)
	if _, err := newOnly.parseToken(newTok); err != nil {
		t.Fatalf("new token rejected after restart without previous: %v", err)
	}
	for name, tok := range map[string]string{"old-kid": oldKidTok, "legacy": legacyTok} {
		if _, err := newOnly.parseToken(tok); err == nil {
			t.Fatalf("%s token still valid without the previous key", name)
		}
	}
	if got := newOnly.st.Box.MustOpen(newOnly.st.GetSetting("tg_token", "")); got != "123:bot-token" {
		t.Fatalf("sealed row unreadable without previous: %q", got)
	}
}

func TestSecretKeyMigrationRefreshesSettingsCache(t *testing.T) {
	dir := t.TempDir()
	oldS := newRotationServer(t, dir, rotationOldKey, "")
	oldS.st.SetSetting("tg_token", oldS.st.Box.Seal("123:bot-token"))
	closeServerDB(t, oldS)

	newS := newRotationServer(t, dir, rotationNewKey, rotationOldKey)
	defer closeServerDB(t, newS)
	// Populate the cache with the old sealed value, as a live panel would.
	if got := newS.st.GetSetting("tg_token", ""); !strings.HasPrefix(got, "enc:v2:"+secret.KidFor(rotationOldKey)+":") {
		t.Fatalf("seeded value not cached as old kid: %q", got)
	}
	res := newS.runSecretMigration()
	if !res.Done || res.Failed != 0 {
		t.Fatalf("migration: %+v", res)
	}
	if got := newS.st.GetSetting("tg_token", ""); !strings.HasPrefix(got, "enc:v2:"+secret.KidFor(rotationNewKey)+":") {
		t.Fatalf("settings cache still serves the old ciphertext after migration: %q", got)
	}
}

func TestSecretKeyRotationSkipsUnopenableRows(t *testing.T) {
	dir := t.TempDir()
	oldS := newRotationServer(t, dir, rotationOldKey, "")
	oldS.st.SetSetting("good_token", oldS.st.Box.Seal("good"))
	third, _ := secret.New(rotationThirdKey)
	oldS.st.SetSetting("broken_token", third.Seal("unopenable"))
	brokenBefore := oldS.st.GetSetting("broken_token", "")
	closeServerDB(t, oldS)

	newS := newRotationServer(t, dir, rotationNewKey, rotationOldKey)
	defer closeServerDB(t, newS)
	res := newS.runSecretMigration()
	if res.Done || res.Failed != 1 {
		t.Fatalf("migration with an unopenable row: done=%v failed=%d rows=%v", res.Done, res.Failed, res.FailedRows)
	}
	if len(res.FailedRows) != 1 || res.FailedRows[0] != "settings:broken_token" {
		t.Fatalf("failed rows: %v", res.FailedRows)
	}
	// The good row migrated; the bad row is untouched, never overwritten.
	if got := newS.st.Box.MustOpen(newS.st.GetSetting("good_token", "")); got != "good" {
		t.Fatalf("good row: %q", got)
	}
	if got := newS.st.GetSetting("broken_token", ""); got != brokenBefore {
		t.Fatalf("unopenable row changed: %q -> %q", brokenBefore, got)
	}
	// A retry still reports the failure and still leaves the row alone.
	res2 := newS.runSecretMigration()
	if res2.Done || res2.Failed != 1 {
		t.Fatalf("retry: done=%v failed=%d", res2.Done, res2.Failed)
	}
	if got := newS.st.GetSetting("broken_token", ""); got != brokenBefore {
		t.Fatalf("unopenable row changed on retry: %q", got)
	}
}

func TestBackupRestoreWithRotatedKey(t *testing.T) {
	dir1 := t.TempDir()
	s1 := newRotationServer(t, dir1, rotationOldKey, "")
	s1.st.DB.Create(&store.Admin{ID: "a1", Username: "root", PasswordHash: "x"})
	s1.st.SetSetting("tg_token", s1.st.Box.Seal("123:bot-token"))
	backupPath := filepath.Join(t.TempDir(), "old.tar.gz")
	if err := s1.writeBackup(backupPath, ""); err != nil {
		t.Fatal(err)
	}
	// The new backup records the key version it was written with.
	f, _ := os.Open(backupPath)
	arc, err := backupfmt.ReadArchive(f, "", s1.st.Box)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if arc.Manifest.KeyVersion != secret.KidFor(rotationOldKey) {
		t.Fatalf("backup key version %q, want %q", arc.Manifest.KeyVersion, secret.KidFor(rotationOldKey))
	}
	closeServerDB(t, s1)

	// A panel with only the new key cannot restore the old backup.
	dir2 := t.TempDir()
	s2 := newRotationServer(t, dir2, rotationNewKey, "")
	f, _ = os.Open(backupPath)
	err = s2.stageRestore(f, "", "")
	f.Close()
	if err == nil {
		t.Fatal("old backup staged without the previous key")
	}
	// Supplying the old key explicitly restores it, re-sealed with the
	// new key (the staged archive no longer needs the old key).
	f, _ = os.Open(backupPath)
	if err := s2.stageRestore(f, "", rotationOldKey); err != nil {
		t.Fatal(err)
	}
	f.Close()
	closeServerDB(t, s2)

	s3 := newRotationServer(t, dir2, rotationNewKey, "")
	defer closeServerDB(t, s3)
	var admin store.Admin
	if err := s3.st.DB.First(&admin, "username = ?", "root").Error; err != nil {
		t.Fatal("restored admin missing:", err)
	}
	sealed := s3.st.GetSetting("tg_token", "")
	if !strings.HasPrefix(sealed, "enc:v2:"+secret.KidFor(rotationNewKey)+":") {
		t.Fatalf("restored secret not re-sealed with the new key: %q", sealed)
	}
	if got := s3.st.Box.MustOpen(sealed); got != "123:bot-token" {
		t.Fatalf("restored secret: %q", got)
	}
}

func TestSecretKeyVersionRefusesUnknownKid(t *testing.T) {
	dir := t.TempDir()
	oldS := newRotationServer(t, dir, rotationOldKey, "")
	closeServerDB(t, oldS)
	// A start with only a different key must refuse: the database names
	// a key version this panel does not know.
	cfg := &Config{DataDir: dir, SecretKey: rotationNewKey, AdminPath: "/"}
	cfg.DB.DSN = filepath.Join(dir, "p.db")
	if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil); err == nil || !strings.Contains(err.Error(), "secret_key_version") {
		t.Fatalf("start with an unknown key version succeeded: %v", err)
	}
	// With the old key supplied as previous, the same start works.
	if s := newRotationServer(t, dir, rotationNewKey, rotationOldKey); s != nil {
		closeServerDB(t, s)
	}
}

func TestLoadConfigSecretKeyPrevious(t *testing.T) {
	t.Setenv("POLARIS_SECRET_KEY", rotationNewKey)
	t.Setenv("POLARIS_SECRET_KEY_PREVIOUS", rotationOldKey)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecretKeyPrevious != rotationOldKey {
		t.Fatalf("previous key not mapped from env: %q", cfg.SecretKeyPrevious)
	}
	t.Setenv("POLARIS_SECRET_KEY_PREVIOUS", rotationNewKey)
	if _, err := LoadConfig(""); err == nil || !strings.Contains(err.Error(), "secret_key_previous") {
		t.Fatalf("previous == current accepted: %v", err)
	}
	t.Setenv("POLARIS_SECRET_KEY_PREVIOUS", "short")
	if _, err := LoadConfig(""); err == nil || !strings.Contains(err.Error(), "secret_key_previous") {
		t.Fatalf("short previous key accepted: %v", err)
	}
}
