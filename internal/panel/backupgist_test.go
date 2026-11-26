package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Gist target only activates with the switch on, a token saved and a
// backup passphrase set — unencrypted archives never leave the machine.
func TestPushTargetsGistGating(t *testing.T) {
	s, _, _ := newAdminClient(t)
	has := func() bool {
		for _, tg := range s.pushTargets() {
			if tg == "gist" {
				return true
			}
		}
		return false
	}
	if has() {
		t.Fatal("gist active with nothing configured")
	}
	s.st.SetSetting("backup_gist", "true")
	if has() {
		t.Fatal("gist active without token")
	}
	s.st.SetSetting("backup_gist_token", s.st.Box.Seal("ghp_x"))
	if has() {
		t.Fatal("gist active without backup passphrase")
	}
	if !s.gistConfigured() {
		t.Fatal("gistConfigured should be true once switch and token are set")
	}
	s.st.SetSetting("backup_passphrase", s.st.Box.Seal("pw"))
	if !has() {
		t.Fatal("gist inactive with switch, token and passphrase set")
	}
	s.st.SetSetting("backup_gist", "false")
	if has() {
		t.Fatal("gist active with switch off")
	}
}

// gistCheck validates the token against /user and the saved Gist against
// /gists/:id.
func TestGistCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"octo"}`))
		case "/gists/g1":
			_, _ = w.Write([]byte(`{"id":"g1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer srv.Close()
	old := gistAPIBase
	gistAPIBase = srv.URL
	defer func() { gistAPIBase = old }()

	ctx := context.Background()
	if err := gistCheck(ctx, "bad", ""); err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("bad token: %v", err)
	}
	if err := gistCheck(ctx, "good", ""); err != nil {
		t.Fatalf("good token: %v", err)
	}
	if err := gistCheck(ctx, "good", "g1"); err != nil {
		t.Fatalf("readable gist: %v", err)
	}
	if err := gistCheck(ctx, "good", "gone"); err == nil {
		t.Fatal("unreadable gist accepted")
	}
}
