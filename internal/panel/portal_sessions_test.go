package panel

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/backupfmt"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
)

func portalCookieClient(t *testing.T, base, token string) *portalClient {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(base + "/u")
	jar.SetCookies(u, []*http.Cookie{{Name: portalCookie, Value: token, Path: "/u"}})
	return &portalClient{t: t, base: base, hc: &http.Client{Jar: jar}}
}

func TestPortalSessionsRevocationAndIsolation(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	alice := store.User{ID: "session-alice", Username: "alice", Email: "alice@example.com", SubToken: "alice-token", UUID: "alice-uuid", Password: "alice-password", Status: "active"}
	bob := store.User{ID: "session-bob", Username: "bob", Email: "bob@example.com", SubToken: "bob-token", Status: "active"}
	for _, u := range []*store.User{&alice, &bob} {
		if err := s.st.DB.Create(u).Error; err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.issuePortalMailToken(&alice)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.issuePortalMailToken(&alice)
	foreign, _ := s.issuePortalMailToken(&bob)
	first := portalCookieClient(t, srv.URL, a)
	second := portalCookieClient(t, srv.URL, b)
	other := portalCookieClient(t, srv.URL, foreign)
	current, _ := s.parseToken(a)
	if _, e := readPortalSession(s.st.DB, current); e != nil {
		t.Fatalf("issued session invalid: %+v: %v", current, e)
	}
	next, _ := s.parseToken(b)
	notOurs, _ := s.parseToken(foreign)
	code, body := first.req("GET", "/u/sessions", nil, false)
	var result struct {
		Items []portalSessionView `json:"items"`
	}
	json.Unmarshal([]byte(body), &result)
	if code != 200 || len(result.Items) != 2 {
		t.Fatal(code, body)
	}
	seenCurrent := false
	for _, row := range result.Items {
		if row.ID == notOurs.SID {
			t.Fatal("another user's session was disclosed")
		}
		seenCurrent = seenCurrent || row.Current && row.ID == current.SID
	}
	if !seenCurrent {
		t.Fatal("missing current device")
	}
	if code, _ := first.req("POST", "/u/sessions/revoke", map[string]any{"id": next.SID}, false); code != 403 {
		t.Fatal("missing CSRF check", code)
	}
	if out := first.jsonW("POST", "/u/sessions/revoke", map[string]any{"id": notOurs.SID}); out["revoked"] != float64(0) {
		t.Fatal("foreign session changed", out)
	}
	if code, _ := other.req("GET", "/u/sessions", nil, false); code != 200 {
		t.Fatal("another user was logged out")
	}
	if out := first.jsonW("POST", "/u/sessions/revoke-others", nil); out["revoked"] != float64(1) {
		t.Fatal(out)
	}
	if code, _ := second.req("GET", "/u/sessions", nil, false); code != 401 {
		t.Fatal("revoked cookie can be replayed", code)
	}
	if code, _ := first.req("GET", "/u/sessions", nil, false); code != 200 {
		t.Fatal("current device was revoked")
	}
	var saved store.User
	s.st.DB.First(&saved, "id = ?", alice.ID)
	if saved.SubToken != alice.SubToken || saved.UUID != alice.UUID || saved.Password != alice.Password {
		t.Fatal("session revocation rotated proxy credentials")
	}
	cookieURL, _ := url.Parse(srv.URL + "/u")
	var renewed string
	for _, ck := range first.hc.Jar.Cookies(cookieURL) {
		if ck.Name == portalCookie {
			renewed = ck.Value
		}
	}
	if renewed == "" || renewed == a {
		t.Fatal("generation was not renewed")
	}
	first.jsonW("POST", "/u/logout", nil)
	replay := portalCookieClient(t, srv.URL, renewed)
	if code, _ := replay.req("GET", "/u/sessions", nil, false); code != 401 {
		t.Fatal("logout did not revoke the copied cookie", code)
	}
	if code, _ := other.req("GET", "/u/sessions", nil, false); code != 200 {
		t.Fatal("logout affected another user")
	}
}

func TestPortalSessionFailureAndLegacyCookies(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	user := store.User{ID: "failure-user", Email: "failure@example.com", Username: "failure", SubToken: "unchanged-token", Status: "active"}
	s.st.DB.Create(&user)
	token, err := s.issuePortalMailToken(&user)
	if err != nil {
		t.Fatal(err)
	}
	client := portalCookieClient(t, srv.URL, token)
	name := "fail_portal_session_update"
	s.st.DB.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "portal_sessions" {
			tx.AddError(errors.New("simulated session storage failure"))
		}
	})
	code, _ := client.req("POST", "/u/logout", nil, true)
	s.st.DB.Callback().Update().Remove(name)
	if code != 500 {
		t.Fatal("logout hid revocation failure", code)
	}
	if code, _ := client.req("GET", "/u/sessions", nil, false); code != 200 {
		t.Fatal("failed logout cleared the browser session")
	}
	cl, _ := s.parseToken(token)
	originalSID := cl.SID
	cl.SID = ""
	legacy, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, cl).SignedString(s.jwtKey)
	if code, _ := portalCookieClient(t, srv.URL, legacy).req("GET", "/u/sessions", nil, false); code != 401 {
		t.Fatal("legacy stateless cookie bypassed revocation", code)
	}
	// Authentication state is runtime-only and is not revived by an archive.
	files, _, err := backupfmt.ExportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["portal_sessions"]; ok {
		t.Fatal("portal sessions were exported")
	}
	s.st.DB.Model(&store.PortalSession{}).Where("id = ?", originalSID).Update("expires_at", time.Now().UTC().Add(-time.Hour))
	if code, _ := client.req("GET", "/u/sessions", nil, false); code != 401 {
		t.Fatal("expired session record remained valid")
	}
	s.prunePortalSessions()
	var remaining int64
	s.st.DB.Model(&store.PortalSession{}).Where("id = ?", originalSID).Count(&remaining)
	if remaining != 0 {
		t.Fatal("expired session was not pruned")
	}
}

// A request already authenticated on another browser must not renew itself
// after revocation, even when it resumes with cached claims.
func TestPortalSessionRenewalCannotResurrect(t *testing.T) {
	s, srv, _ := newAdminClient(t)
	s.st.SetSetting("portal_enabled", "true")
	u := store.User{ID: "renewal-user", Username: "renewal", Email: "renewal@example.com", SubToken: "old-link", Status: "active"}
	if e := s.st.DB.Create(&u).Error; e != nil {
		t.Fatal(e)
	}
	a, _ := s.issuePortalMailToken(&u)
	b, _ := s.issuePortalMailToken(&u)
	current, _ := s.parseToken(a)
	parent, _ := s.parseToken(b)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/u/reset", nil)
	ctx.Set("portalClaims", parent)
	first := portalCookieClient(t, srv.URL, a)
	first.jsonW("POST", "/u/sessions/revoke-others", nil)
	if _, e := s.issuePortalTokenVia(&u, true, ctx); !errors.Is(e, errPortalSessionEnded) {
		t.Fatalf("stale request renewed: %v", e)
	}
	cookieURL, _ := url.Parse(srv.URL + "/u")
	var fresh *claims
	for _, ck := range first.hc.Jar.Cookies(cookieURL) {
		if ck.Name == portalCookie {
			fresh, _ = s.parseToken(ck.Value)
		}
	}
	if fresh == nil || fresh.SID != current.SID || fresh.PortalEpoch <= parent.PortalEpoch {
		t.Fatal("current generation not advanced")
	}
	ctx.Set("portalClaims", fresh)
	u.SubToken = "new-link"
	renewed, e := s.issuePortalTokenVia(&u, true, ctx)
	if e != nil {
		t.Fatal(e)
	}
	claimsAfter, _ := s.parseToken(renewed)
	if claimsAfter.SID != fresh.SID {
		t.Fatal("renewal created a second device")
	}
	if s.portalSessionAlive(fresh) {
		t.Fatal("old fingerprint survived renewal")
	}
	if !s.portalSessionAlive(claimsAfter) {
		t.Fatal("renewed device invalid")
	}
	// Single-device revocation also blocks an in-flight credential renewal.
	ctx.Set("portalClaims", claimsAfter)
	if e := s.st.DB.Model(&store.PortalSession{}).Where("id = ?", claimsAfter.SID).Update("revoked", true).Error; e != nil {
		t.Fatal(e)
	}
	if _, e := s.issuePortalTokenVia(&u, true, ctx); !errors.Is(e, errPortalSessionEnded) {
		t.Fatalf("revoked device renewed: %v", e)
	}
}
