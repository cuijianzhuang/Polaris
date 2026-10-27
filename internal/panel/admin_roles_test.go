package panel

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func staffLogin(t *testing.T, owner *apiClient, name, role string, ids []string) (*apiClient, string) {
	t.Helper()
	code, out := owner.do("POST", "/admins", map[string]any{"username": name, "password": "synthetic-staff-password", "role": role, "nodeIds": ids})
	if code != 201 {
		t.Fatalf("create staff %d %v", code, out)
	}
	c := &apiClient{t: t, base: owner.base}
	code, login := c.do("POST", "/auth/login", map[string]string{"username": name, "password": "synthetic-staff-password"})
	if code != 200 {
		t.Fatal(code, login)
	}
	c.tok = login["token"].(string)
	return c, out["id"].(string)
}

func staffRaw(t *testing.T, c *apiClient, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.tok)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, string(b)
}

func TestAdminRolesRejectPrivilegeEscalationAndProjectData(t *testing.T) {
	s, _, owner := newAdminClient(t)
	for _, id := range []string{"assigned", "other"} {
		if err := s.st.DB.Create(&store.Node{ID: id, Name: id, Address: "secret-address", LastError: "secret-config-error"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.st.DB.Create(&store.User{ID: "customer", Username: "customer", UUID: "synthetic-uuid", Password: "synthetic-proxy-password", SubToken: "synthetic-sub-token", Email: "private@example.test", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.Order{ID: "order", PlanName: "plan", Amount: "10.00", Currency: "USD", WebKey: "private-web-key", Detail: "private-order-detail"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.NodeCost{NodeID: "assigned", Amount: 12, Currency: "USD", Provider: "private-provider", Note: "private-provider-note"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"operator", "viewer", "support", "finance"} {
		t.Run(role, func(t *testing.T) {
			ids := []string(nil)
			if role == "operator" || role == "viewer" {
				ids = []string{"assigned"}
			}
			c, _ := staffLogin(t, owner, "staff-"+role, role, ids)
			for _, x := range []struct{ method, path string }{
				{"GET", "/settings"}, {"GET", "/admins"}, {"POST", "/admins"}, {"POST", "/tokens"}, {"GET", "/users"}, {"GET", "/users/customer/links"}, {"GET", "/users/export"},
				{"GET", "/orders"}, {"GET", "/webhooks"}, {"GET", "/backup"}, {"GET", "/audit-logs"}, {"GET", "/openapi.json"}, {"POST", "/orders/order/refund"}, {"GET", "/nodes/assigned/render"},
			} {
				if code, out := c.do(x.method, x.path, nil); code != 403 {
					t.Fatalf("%s %s %s: %d %v", role, x.method, x.path, code, out)
				}
			}
			if code, _ := c.do("GET", "/auth/me", nil); code != 200 {
				t.Fatal("no own account access", code)
			}
			if code, body := staffRaw(t, c, "/auth/sessions"); code != 200 || strings.Contains(body, "root") {
				t.Fatal("staff can see owner sessions", code, body)
			}
			switch role {
			case "operator":
				if code, body := staffRaw(t, c, "/nodes"); code != 200 || strings.Contains(body, `"id":"other"`) {
					t.Fatal("wrong node scope", code, body)
				}
				if code, _ := c.do("GET", "/nodes/other", nil); code != 403 {
					t.Fatal("cross-node read", code)
				}
				if code, _ := c.do("POST", "/nodes/other/sync", nil); code != 403 {
					t.Fatal("cross-node write", code)
				}
				if code, _ := c.do("GET", "/cores/missing/logs", nil); code != 404 {
					t.Fatal("operator logs denied before scope resolution", code)
				}
			case "viewer":
				code, body := staffRaw(t, c, "/staff/nodes")
				if code != 200 || !strings.Contains(body, "assigned") || strings.Contains(body, "other") || strings.Contains(body, "secret-") {
					t.Fatal("unsafe viewer projection", code, body)
				}
				if code, _ := c.do("GET", "/ws", nil); code != 403 {
					t.Fatal("viewer websocket exposed full node data", code)
				}
				if code, _ := c.do("POST", "/nodes/assigned/sync", nil); code != 403 {
					t.Fatal("viewer write allowed", code)
				}
			case "support":
				code, body := staffRaw(t, c, "/staff/users?q=customer")
				if code != 200 || !strings.Contains(body, "customer") {
					t.Fatal(code, body)
				}
				for _, secret := range []string{"synthetic-uuid", "synthetic-proxy-password", "synthetic-sub-token", "private@example.test"} {
					if strings.Contains(body, secret) {
						t.Fatal("credential exposed", secret)
					}
				}
				if code, _ := c.do("GET", "/tickets", nil); code != 200 {
					t.Fatal("support tickets unavailable", code)
				}
				if code, _ := c.do("DELETE", "/tickets/missing", nil); code != 403 {
					t.Fatal("support deletion allowed", code)
				}
			case "finance":
				for _, path := range []string{"/staff/orders", "/staff/costs"} {
					code, body := staffRaw(t, c, path)
					if code != 200 || strings.Contains(body, "private-") {
						t.Fatal("unsafe finance projection", code, body)
					}
				}
			}
		})
	}
}

func TestAdminAccessChangesRevokeSessionsAndProtectLastOwner(t *testing.T) {
	s, _, owner := newAdminClient(t)
	if err := s.st.DB.Where("id <> ?", "a1").Delete(&store.Admin{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Model(&store.Admin{}).Where("id = ?", "a1").Update("role", nil).Error; err != nil {
		t.Fatal(err)
	}
	if code, _ := owner.do("PUT", "/admins/a1/access", map[string]any{"role": "support", "nodeIds": []string{}}); code != 400 {
		t.Fatal("removed last owner", code)
	}
	if code, _ := owner.do("POST", "/admins", map[string]any{"username": "invalid", "password": "synthetic-staff-password", "role": "viewer"}); code != 400 {
		t.Fatal("empty node scope accepted", code)
	}
	staff, id := staffLogin(t, owner, "changing-staff", "support", nil)
	if code, out := owner.do("PUT", "/admins/"+id+"/access", map[string]any{"role": "finance", "nodeIds": []string{}}); code != 200 {
		t.Fatal(code, out)
	}
	if code, _ := staff.do("GET", "/auth/me", nil); code != 401 {
		t.Fatal("old session survived role change", code)
	}
	if code, out := staff.do("POST", "/auth/login", map[string]string{"username": "changing-staff", "password": "synthetic-staff-password"}); code != 200 {
		t.Fatal(code, out)
	} else {
		staff.tok = out["token"].(string)
	}
	if code, out := owner.do("POST", "/admins/"+id+"/password", map[string]string{"password": "replacement-staff-password"}); code != 200 {
		t.Fatal(code, out)
	}
	if code, _ := staff.do("GET", "/auth/me", nil); code != 401 {
		t.Fatal("old session survived password reset", code)
	}
	if code, _ := owner.do("DELETE", "/admins/"+id, nil); code != 204 {
		t.Fatal(code)
	}
	var n int64
	s.st.DB.Model(&store.AdminSession{}).Where("admin_id = ?", id).Count(&n)
	if n != 0 {
		t.Fatal("deleted account sessions retained")
	}
}

func TestAdminConcurrentOwnerRemovalLeavesOwner(t *testing.T) {
	s, _, owner := newAdminClient(t)
	if err := s.st.DB.Where("id <> ?", "a1").Delete(&store.Admin{}).Error; err != nil {
		t.Fatal(err)
	}
	second, id := staffLogin(t, owner, "second-owner", "owner", nil)
	if code, _ := second.do("POST", "/auth/sudo", map[string]string{"password": "synthetic-staff-password"}); code != 200 {
		t.Fatal(code)
	}
	// Downgrade requests may both pass HTTP authorization before either writes.
	// The transaction must recheck the actor and the last-owner invariant.
	var wg sync.WaitGroup
	for _, target := range []struct {
		c  *apiClient
		id string
	}{{owner, id}, {second, "a1"}} {
		wg.Add(1)
		go func(c *apiClient, id string) {
			defer wg.Done()
			c.do("PUT", "/admins/"+id+"/access", map[string]any{"role": "support", "nodeIds": []string{}})
		}(target.c, target.id)
	}
	wg.Wait()
	var n int64
	if err := s.st.DB.Model(&store.Admin{}).Where("role IN ?", []string{"", "owner"}).Count(&n).Error; err != nil || n < 1 {
		t.Fatal("concurrent mutations removed all owners", n, err)
	}
}

func TestStaffCapabilityChecksDoNotUseJWTClaimsAlone(t *testing.T) {
	s, _, owner := newAdminClient(t)
	c, id := staffLogin(t, owner, "unknown-role", "support", nil)
	// Keep the unknown role within Admin.Role's varchar(16) limit so PostgreSQL
	// reaches the authorization check instead of rejecting the fixture write.
	if err := s.st.DB.Model(&store.Admin{}).Where("id = ?", id).Update("role", "future-role").Error; err != nil {
		t.Fatal(err)
	}
	if code, _ := c.do("GET", "/staff/users", nil); code != 403 {
		t.Fatal("unknown role granted access", code)
	}
	// Existing accounts without role metadata remain owners after migration.
	if code, out := owner.do("GET", "/auth/me", nil); code != 200 || out["role"] != "owner" {
		t.Fatal(code, out)
	}
	b, _ := json.Marshal(store.Admin{Role: "support", PasswordHash: "must-not-serialize", TOTPSecret: "must-not-serialize", CreatedAt: time.Now()})
	if strings.Contains(string(b), "must-not-serialize") {
		t.Fatal("admin listing leaks secrets")
	}
}

func TestAdminScopeChangeRevokesOpenWebSocketBeforeSending(t *testing.T) {
	s, _, owner := newAdminClient(t)
	for _, id := range []string{"before", "after"} {
		if err := s.st.DB.Create(&store.Node{ID: id, Name: id}).Error; err != nil {
			t.Fatal(err)
		}
	}
	c, id := staffLogin(t, owner, "socket-operator", "operator", []string{"before"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(c.base, "http://", "ws://", 1)+"/ws?token="+c.tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, first, err := conn.Read(ctx)
	if err != nil || strings.Contains(string(first), `"id":"after"`) {
		t.Fatal("scope leaked on connect", err, string(first))
	}
	if code, out := owner.do("PUT", "/admins/"+id+"/access", map[string]any{"role": "operator", "nodeIds": []string{"after"}}); code != 200 {
		t.Fatal(code, out)
	}
	s.rt.mu.Lock()
	for _, client := range s.rt.clients {
		client.ch <- []byte(`{"type":"nodes","data":[{"id":"before"}]}`)
	}
	s.rt.mu.Unlock()
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatal("revoked socket accepted a new snapshot", err)
	}
}

func TestAdminRevocationRemainsAvailableDuringLicenseOutage(t *testing.T) {
	s, _, owner := newAdminClient(t)
	_, id := staffLogin(t, owner, "revocable-staff", "support", nil)
	policyLicense(t, s, false)
	expirePolicyGrace(t, s)
	if !s.license().ManagementReadOnly {
		t.Fatal("expected license management pause")
	}
	if code, out := owner.do("DELETE", "/admins/"+id, nil); code != 204 {
		t.Fatal("security revocation blocked", code, out)
	}
	if code, _ := owner.do("POST", "/users", map[string]string{"username": "blocked-business-write"}); code != 402 {
		t.Fatal("business management pause bypassed", code)
	}
}
