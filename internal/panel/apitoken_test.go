package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestAPITokenScopes: a token reaches only the routes its scopes allow,
// everything unlisted is refused, and expired or deleted tokens stop working.
func TestAPITokenScopes(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	code, out := admin.do("POST", "/tokens", map[string]any{"name": "shop", "scopes": []string{"users:read", "users:write"}})
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	secret := out["secret"].(string)
	id := out["token"].(map[string]any)["id"].(string)
	if len(secret) != 44 || secret[:4] != "plt_" {
		t.Fatalf("secret %q", secret)
	}
	var stored store.APIToken
	s.st.DB.First(&stored, "id = ?", id)
	if stored.Hash == secret || stored.Hash != hashToken(secret) {
		t.Fatal("token must be stored hashed")
	}

	tc := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: secret}
	for _, x := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/users", 200},
		{"GET", "/tokens/self", 200},
		{"GET", "/nodes", 403},    // scope not granted
		{"GET", "/settings", 403}, // scope not granted
		{"POST", "/tokens", 403},  // never open to tokens
		{"GET", "/audit-logs", 403},
	} {
		if code, out := tc.do(x.method, x.path, nil); code != x.want {
			t.Fatalf("%s %s = %d %v, want %d", x.method, x.path, code, out, x.want)
		}
	}
	if code, out := admin.do("POST", "/tokens", map[string]any{"name": "x", "scopes": []string{"root"}}); code != 400 {
		t.Fatalf("unknown scope %d %v", code, out)
	}

	// expired
	past := time.Now().Add(-time.Minute)
	s.st.DB.Model(&store.APIToken{}).Where("id = ?", id).Update("expires_at", past)
	if code, _ := tc.do("GET", "/users", nil); code != 401 {
		t.Fatalf("expired token %d", code)
	}
	s.st.DB.Model(&store.APIToken{}).Where("id = ?", id).Update("expires_at", nil)
	if code, _ := admin.do("DELETE", "/tokens/"+id, nil); code != 204 {
		t.Fatalf("delete %d", code)
	}
	if code, _ := tc.do("GET", "/users", nil); code != 401 {
		t.Fatalf("deleted token %d", code)
	}
}

// TestRenewAndAddTrafficIdempotent: renew and add-traffic add to the user,
// and a call repeated with the same Idempotency-Key is applied once.
func TestRenewAndAddTrafficIdempotent(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	exp := time.Now().AddDate(0, 0, 5)
	u := store.User{ID: "u1", Username: "alice", UUID: "11111111-1111-1111-1111-111111111111", SubToken: "tok-alice-1234567890",
		TrafficLimit: 10 << 30, ExpireAt: &exp, Status: "active"}
	s.st.DB.Create(&u)
	_, out := admin.do("POST", "/tokens", map[string]any{"name": "shop", "scopes": []string{"users:write"}})
	secret := out["secret"].(string)

	post := func(path string, body any, key string) (int, map[string]any, string) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+s.cfg.APIBase()+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+secret)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m, resp.Header.Get("Idempotent-Replayed")
	}
	for i := 0; i < 3; i++ { // one order, callback delivered three times
		code, m, replay := post("/users/u1/renew", map[string]any{"days": 30}, "order-1")
		if code != 200 || (i > 0) != (replay == "true") {
			t.Fatalf("renew #%d %d %v replay=%q", i, code, m, replay)
		}
	}
	var got store.User
	s.st.DB.First(&got, "id = ?", "u1")
	if d := got.ExpireAt.Sub(exp); d < 30*24*time.Hour-time.Minute || d > 30*24*time.Hour+time.Minute {
		t.Fatalf("expiry moved by %v, want 30 days once", d)
	}
	post("/users/u1/add-traffic", map[string]any{"gb": 5}, "order-2")
	post("/users/u1/add-traffic", map[string]any{"gb": 5}, "order-2")
	post("/users/u1/add-traffic", map[string]any{"gb": 5}, "") // no key: applied
	s.st.DB.First(&got, "id = ?", "u1")
	if got.TrafficLimit != 20<<30 {
		t.Fatalf("traffic limit %d GB, want 20", got.TrafficLimit>>30)
	}
	// an expired account renews from today
	old := time.Now().AddDate(0, 0, -10)
	s.st.DB.Model(&store.User{}).Where("id = ?", "u1").Updates(map[string]any{"expire_at": old, "status": "expired"})
	if code, m, _ := post("/users/u1/renew", map[string]any{"days": 7}, "order-3"); code != 200 || m["status"] != "active" {
		t.Fatalf("renew expired %d %v", code, m)
	}
	s.st.DB.First(&got, "id = ?", "u1")
	if d := time.Until(*got.ExpireAt); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("expired renew gave %v", d)
	}
}

// TestTokenRoutesExist: every route a scope opens is a real route.
func TestTokenRoutesExist(t *testing.T) {
	s := newTestServer(t)
	have := map[string]bool{}
	for _, r := range s.Router().(*gin.Engine).Routes() {
		have[r.Method+" "+strings.TrimPrefix(r.Path, s.cfg.APIBase())] = true
	}
	for k := range tokenRoutes {
		if !have[k] {
			t.Errorf("tokenRoutes has %q, which is not a route", k)
		}
	}
}
