package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type commandReply struct {
	status int
	body   []byte
	replay bool
	err    error
}

func callUserCommand(base, token, path, body, key string) commandReply {
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		return commandReply{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return commandReply{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return commandReply{resp.StatusCode, b, resp.Header.Get("Idempotent-Replayed") == "true", err}
}

// Every case runs through two independent Server/Store instances, not a
// shared Go mutex. PostgreSQL uses a disposable schema when its test DSN is set.
func TestUserCommandTransactions(t *testing.T) {
	for _, engine := range []string{"sqlite", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			dsn := ""
			if engine == "postgres" {
				dsn = os.Getenv("POLARIS_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("POLARIS_TEST_POSTGRES_DSN not set")
				}
			}
			for _, scenario := range []string{"concurrent-replay", "concurrent-increments", "result-rollback", "stable-identities", "request-conflict", "renew-and-caps", "bounds", "legacy-cache", "expiry-and-revocation", "scope-reduction"} {
				t.Run(scenario, func(t *testing.T) { runUserCommandScenario(t, dsn, scenario) })
			}
		})
	}
}

func runUserCommandScenario(t *testing.T, dsn, scenario string) {
	cfg := &Config{DataDir: t.TempDir(), SecretKey: "test-command-secret", AdminPath: "/"}
	cfg.DB.DSN = cfg.DataDir + "/commands.db"
	if dsn != "" {
		// Never clear an arbitrary database supplied by the developer.
		// All tables for this case live in a newly-created random schema.
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal("open PostgreSQL test database:", err)
		}
		schema := "test_commands_" + store.NewID()
		if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Exec("DROP SCHEMA " + schema + " CASCADE"); sqlDB, _ := db.DB(); sqlDB.Close() })
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		cfg.DB.DSN = parsed.String()
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	first, err := New(cfg, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(cfg, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Server{first, second} {
		db, _ := s.st.DB.DB()
		t.Cleanup(func() { db.Close() })
	}
	a := httptest.NewServer(first.Router())
	t.Cleanup(a.Close)
	b := httptest.NewServer(second.Router())
	t.Cleanup(b.Close)
	bases := []string{a.URL + cfg.APIBase(), b.URL + cfg.APIBase()}
	_, token, err := first.issueAPIToken("shop", "", []string{ScopeUsersWrite}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().AddDate(0, 0, 5)
	u := store.User{ID: "u1", Username: "customer", UUID: "11111111-1111-1111-1111-111111111111", Password: "proxy-secret", SubToken: "subscription-secret", TrafficLimit: 10 << 30, ExpireAt: &exp, Status: "active"}
	if err := first.st.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	post := func(body, key string) commandReply {
		return callUserCommand(bases[0], token, "/users/u1/add-traffic", body, key)
	}
	want := func(r commandReply, status int) {
		t.Helper()
		if r.err != nil || r.status != status {
			t.Fatalf("status=%d, err=%v, body=%s; want %d", r.status, r.err, r.body, status)
		}
	}
	quota := func() int64 {
		t.Helper()
		var got store.User
		if err := first.st.DB.First(&got, "id = ?", "u1").Error; err != nil {
			t.Fatal(err)
		}
		return got.TrafficLimit
	}
	switch scenario {
	case "concurrent-replay", "concurrent-increments":
		const n = 12
		start := make(chan struct{})
		results := make(chan commandReply, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				key := "one-order"
				if scenario == "concurrent-increments" {
					key = fmt.Sprintf("order-%d", i)
				}
				results <- callUserCommand(bases[i%2], token, "/users/u1/add-traffic", `{"gb":1}`, key)
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)
		replays := 0
		for r := range results {
			want(r, 200)
			if r.replay {
				replays++
			}
		}
		expected := int64(11 << 30)
		if scenario == "concurrent-increments" {
			expected = int64((10 + n) << 30)
		} else if replays != n-1 {
			t.Fatalf("replays=%d, want %d", replays, n-1)
		}
		if got := quota(); got != expected {
			t.Fatalf("quota=%d, want %d", got, expected)
		}
	case "result-rollback":
		// Inject failure after the user mutation and audit have been written,
		// exactly where the old response-only implementation could double-charge.
		name := "test:fail_command_result"
		if err := first.st.DB.Callback().Update().Before("gorm:update").Register(name, func(db *gorm.DB) {
			if db.Statement.Schema != nil && db.Statement.Schema.Table == "idempotency_keys" {
				db.AddError(errors.New("injected result storage failure"))
			}
		}); err != nil {
			t.Fatal(err)
		}
		want(post(`{"gb":1}`, "rollback-order"), 500)
		first.st.DB.Callback().Update().Remove(name)
		if quota() != 10<<30 {
			t.Fatal("failed result committed a user mutation")
		}
		var rows int64
		first.st.DB.Model(&store.IdempotencyKey{}).Count(&rows)
		if rows != 0 {
			t.Fatal("failed transaction left a reservation")
		}
		first.st.DB.Model(&store.AuditLog{}).Where("action = ?", "user.add_traffic").Count(&rows)
		if rows != 0 {
			t.Fatal("failed transaction left a business audit")
		}
		want(post(`{"gb":1}`, "rollback-order"), 200)
		want(callUserCommand(bases[1], token, "/users/u1/add-traffic", `{"gb":1}`, "rollback-order"), 200)
		if quota() != 11<<30 {
			t.Fatal("retried order applied more than once")
		}
	case "stable-identities":
		want(post(`{"gb":1}`, "same-key"), 200)
		_, other, err := first.issueAPIToken("shop", "", []string{ScopeUsersWrite}, 0, "test")
		if err != nil {
			t.Fatal(err)
		}
		want(callUserCommand(bases[1], other, "/users/u1/add-traffic", `{"gb":1}`, "same-key"), 200)
		if quota() != 12<<30 {
			t.Fatal("same-name tokens shared a response")
		}
		first.st.DB.Model(&store.APIToken{}).Where("hash = ?", hashToken(token)).Update("name", "renamed")
		r := post(`{"gb":1}`, "same-key")
		want(r, 200)
		if !r.replay || quota() != 12<<30 {
			t.Fatal("token rename lost its replay identity")
		}
	case "request-conflict":
		original := post(`{"gb":1}`, "body-order")
		want(original, 200)
		r := post(" {\n\"gb\": 1 } ", "body-order")
		want(r, 200)
		if !r.replay || !bytes.Equal(original.body, r.body) {
			t.Fatal("equivalent JSON did not replay the exact committed response")
		}
		want(post(`{"gb":2}`, "body-order"), 409)
		r = callUserCommand(bases[1], token, "/users/u1/add-traffic?mode=changed", `{"gb":1}`, "body-order")
		want(r, 409)
		if quota() != 11<<30 {
			t.Fatal("conflicting request changed quota")
		}
	case "renew-and-caps":
		first.st.DB.Model(&u).Updates(map[string]any{"expire_at": nil, "expire_after_days": 10, "used_up": 100, "used_down": 200, "status": "disabled"})
		first.st.DB.Create(&store.UserNodeQuota{UserID: u.ID, NodeID: "n1", Used: 500, Extra: 900, ExtraUsed: 400})
		path := "/users/u1/renew"
		body := `{"days":30,"resetTraffic":true}`
		want(callUserCommand(bases[0], token, path, body, "renew-order"), 200)
		want(callUserCommand(bases[1], token, path, body, "renew-order"), 200)
		var got store.User
		first.st.DB.First(&got, "id = ?", u.ID)
		var q store.UserNodeQuota
		first.st.DB.First(&q, "user_id = ?", u.ID)
		if got.ExpireAfterDays != 40 || got.Used() != 0 || got.Status != "disabled" || q.Used != 0 || q.ExtraUsed != 400 {
			t.Fatalf("invalid first-use renewal/cap reset: days=%d used=%d status=%s cap=%d pack=%d", got.ExpireAfterDays, got.Used(), got.Status, q.Used, q.ExtraUsed)
		}
	case "bounds":
		first.st.DB.Model(&u).Update("traffic_limit", int64(math.MaxInt64-1))
		want(post(`{"gb":1}`, "overflow"), 400)
		if quota() != math.MaxInt64-1 {
			t.Fatal("overflow changed quota")
		}
		want(post(`{"gb":0.0000000001}`, "tiny"), 400)
		first.st.DB.Model(&u).Updates(map[string]any{"traffic_limit": 0, "expire_at": nil})
		want(post(`{"gb":1}`, "unlimited"), 200)
		if quota() != 0 {
			t.Fatal("unlimited account became limited")
		}
		first.st.DB.Model(&u).Update("is_service", true)
		want(post(`{"gb":1}`, "service-account"), 404)
	case "legacy-cache":
		key := "token:shop POST " + cfg.APIBase() + "/users/u1/add-traffic old-order"
		first.st.DB.Create(&store.IdempotencyKey{Key: key, Status: 200, Body: []byte(`{"id":"other-caller"}`), CreatedAt: time.Now()})
		want(post(`{"gb":1}`, "old-order"), 409)
		if quota() != 10<<30 {
			t.Fatal("unsafe legacy record caused a duplicate mutation")
		}
	case "scope-reduction":
		original := post(`{"gb":1}`, "restricted-order")
		want(original, 200)
		if !bytes.Contains(original.body, []byte("subscription-secret")) {
			t.Fatal("broad response fixture lacks subscription credential")
		}
		first.st.DB.Model(&store.APIToken{}).Where("hash = ?", hashToken(token)).Update("scopes", store.NewJSON([]string{ScopeTrafficAdd}))
		replay := post(`{"gb":1}`, "restricted-order")
		want(replay, 200)
		if !replay.replay || quota() != 11<<30 {
			t.Fatal("scope reduction re-applied the order")
		}
		var view map[string]any
		if err := json.Unmarshal(replay.body, &view); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"password", "uuid", "subToken"} {
			if _, ok := view[field]; ok {
				t.Fatalf("replay after scope reduction exposed %s", field)
			}
		}
	case "expiry-and-revocation":
		want(post(`{"gb":1}`, "expiring-order"), 200)
		first.st.DB.Model(&store.IdempotencyKey{}).Where("request_hash <> ?", "").Update("created_at", time.Now().Add(-25*time.Hour))
		r := post(`{"gb":1}`, "expiring-order")
		want(r, 200)
		if r.replay || quota() != 12<<30 {
			t.Fatal("expired reservation was not replaced")
		}
		first.st.DB.Where("hash = ?", hashToken(token)).Delete(&store.APIToken{})
		want(post(`{"gb":1}`, "expiring-order"), 401)
	}
}

func TestNarrowTokenScopesAndOpenAPI(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	exp := time.Now().AddDate(0, 0, 2)
	s.st.DB.Create(&store.User{ID: "narrow", Username: "narrow", UUID: "11111111-1111-1111-1111-111111111111", Password: "proxy-secret", SubToken: "subscription-secret", TrafficLimit: 10 << 30, ExpireAt: &exp, Status: "active"})
	for _, sc := range []string{ScopeUsersRenew, ScopeTrafficAdd, ScopeSubsRead} {
		t.Run(sc, func(t *testing.T) {
			_, token, err := s.issueAPIToken(sc, "", []string{sc}, 0, "test")
			if err != nil {
				t.Fatal(err)
			}
			client := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: token}
			code, doc := client.do("GET", "/openapi.json", nil)
			if code != 200 {
				t.Fatalf("OpenAPI status=%d", code)
			}
			paths := doc["paths"].(map[string]any)
			expected := "/users/{id}/renew"
			body := `{"days":1}`
			path := "/users/narrow/renew"
			if sc == ScopeTrafficAdd {
				expected = "/users/{id}/add-traffic"
				path = "/users/narrow/add-traffic"
				body = `{"gb":1}`
			}
			if sc == ScopeSubsRead {
				expected = "/users/{id}/links"
			}
			if _, ok := paths[expected]; !ok {
				t.Fatalf("OpenAPI omitted allowed operation %s", expected)
			}
			for _, forbidden := range []string{"/settings", "/users", "/auth/password", "/tokens", "/plugins"} {
				if _, ok := paths[forbidden]; ok {
					t.Fatalf("OpenAPI leaked %s", forbidden)
				}
			}
			if code, _ := client.do("GET", "/users/narrow", nil); code != 403 {
				t.Fatalf("narrow token read full user: %d", code)
			}
			if code, _ := client.do("GET", "/settings", nil); code != 403 {
				t.Fatalf("narrow token read settings: %d", code)
			}
			if sc != ScopeSubsRead {
				r := callUserCommand(client.base, token, path, body, "narrow-order")
				if r.err != nil || r.status != 200 {
					t.Fatalf("command status=%d err=%v", r.status, r.err)
				}
				var result map[string]any
				if err := json.Unmarshal(r.body, &result); err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{"uuid", "password", "subToken", "ssSeed"} {
					if _, ok := result[secret]; ok {
						t.Fatalf("narrow command exposed %s", secret)
					}
				}
				r = callUserCommand(client.base, token, "/users/narrow/renew", `{"days":1}`, "other-order")
				if sc == ScopeTrafficAdd && r.status != 403 {
					t.Fatal("traffic-only token renewed user")
				}
			} else if code, _ := client.do("GET", "/users/narrow/sub-check", nil); code != 200 {
				t.Fatalf("subscription diagnostic status=%d", code)
			}
		})
	}
	code, doc := admin.do("GET", "/openapi.json", nil)
	if code != 200 {
		t.Fatal(code)
	}
	if _, ok := doc["paths"].(map[string]any)["/auth/password"]; !ok {
		t.Fatal("administrator OpenAPI was filtered")
	}
	// Broad legacy scopes still cover the new split endpoints.
	for _, scope := range []string{ScopeUsersRead, ScopeUsersWrite} {
		token := store.APIToken{Scopes: store.NewJSON([]string{scope})}
		route := "GET /users/:id/links"
		if scope == ScopeUsersWrite {
			route = "POST /users/:id/renew"
		}
		if !tokenAllows(&token, route) {
			t.Fatalf("legacy scope %s lost compatibility", scope)
		}
	}
}
