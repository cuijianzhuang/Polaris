package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestDNSProviderEditPreservesIdentityAndSecret(t *testing.T) {
	s, _, c := newAdminClient(t)
	code, created := c.do("POST", "/dns-providers", map[string]any{"name": "DNS", "kind": "cloudflare", "token": "private-token"})
	if code != 200 || created["id"] == "" || created["hasToken"] != true || created["token"] != nil {
		t.Fatalf("create: %d %v", code, created)
	}
	id := created["id"].(string)
	var before store.DNSProvider
	s.st.DB.First(&before, "id = ?", id)
	for _, name := range []string{"DNS edited", "DNS edited again"} {
		code, out := c.do("PUT", "/dns-providers/"+id, map[string]any{"name": name, "kind": "cloudflare", "token": ""})
		if code != 200 || out["id"] != id || out["hasToken"] != true || out["token"] != nil {
			t.Fatalf("edit: %d %v", code, out)
		}
		var got store.DNSProvider
		s.st.DB.First(&got, "id = ?", id)
		if got.Name != name || got.Token != before.Token || !got.CreatedAt.Equal(before.CreatedAt) {
			t.Fatal("editing changed the provider identity, creation time or stored secret")
		}
	}
	if code, out := c.do("PUT", "/dns-providers/missing", map[string]any{"name": "missing", "kind": "cloudflare"}); code != 404 {
		t.Fatalf("missing: %d %v", code, out)
	}
	var count int64
	s.st.DB.Model(&store.DNSProvider{}).Count(&count)
	if count != 1 {
		t.Fatalf("edit inserted duplicate providers: %d", count)
	}
}
