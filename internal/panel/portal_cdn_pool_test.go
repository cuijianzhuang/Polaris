package panel

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestPersonalCDNProbeIsolationAndLifecycle(t *testing.T) {
	s, base, admin := cdnRegionFixture(t)
	alice := regionPortalClient(t, s, base, "ra")
	bob := regionPortalClient(t, s, base, "rb")
	save := map[string]any{"profileId": "regional", "addresses": []string{"keep.example.com", "104.20.9.9"}, "disabled": []string{"104.20.9.9"}}
	if code, out := alice.req("POST", "/u/cdn-preferences", save, true); code != 200 {
		t.Fatal(code, out)
	}
	pair := func(client *portalClient) *apiClient {
		t.Helper()
		code, body := client.req("POST", "/u/cdn-pool", map[string]any{"profileId": "regional", "action": "pair"}, true)
		if code != 200 {
			t.Fatal(code, body)
		}
		var result struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"--profile 'regional'", "--source personal", "-interval 1h"} {
			if !strings.Contains(result.Command, want) {
				t.Fatal("invalid install command", want)
			}
		}
		secret := strings.Split(strings.Split(result.Command, "--token '")[1], "'")[0]
		return &apiClient{t: t, base: admin.base, tok: secret}
	}
	probe := pair(alice)
	bobProbe := pair(bob)
	payload := map[string]any{"source": "personal", "port": 443, "scheme": "https", "ips": []map[string]any{{"ip": "104.20.9.9", "loss": 0.0}, {"ip": "104.20.8.8", "loss": 0.0}, {"ip": "104.20.7.7", "loss": 1.0}}}
	if code, out := probe.do("POST", "/cdn-profiles/regional/preferred-ips", payload); code != 200 {
		t.Fatal(code, out)
	}
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"keep.example.com", "104.20.8.8"}) {
		t.Fatal("manual or disabled state overwritten", got)
	}
	if got := regionServers(t, s, "rb", "ct", "CN-44"); !slices.Equal(got, []string{"104.16.2.1"}) {
		t.Fatal("personal report leaked", got)
	}
	var public store.CDNProfile
	s.st.DB.First(&public, "id = ?", "regional")
	if !slices.Equal(public.PreferredIPs.V, []string{"104.16.1.1", "104.16.2.1", "104.16.3.1"}) {
		t.Fatal("changed public pool")
	}
	for _, path := range []string{"/users", "/tokens/self", "/cdn-profiles/foreign/ip-candidates"} {
		if code, _ := probe.do("GET", path, nil); code != 403 {
			t.Fatal("token escaped scope", path, code)
		}
	}
	if code, _ := probe.do("POST", "/cdn-profiles/foreign/preferred-ips", payload); code != 403 {
		t.Fatal("foreign profile admitted", code)
	}
	for _, change := range []map[string]any{{"source": "spoof"}, {"port": 2053}, {"scheme": "http"}} {
		bad := map[string]any{}
		for k, v := range payload {
			bad[k] = v
		}
		for k, v := range change {
			bad[k] = v
		}
		if code, _ := probe.do("POST", "/cdn-profiles/regional/preferred-ips", bad); code < 400 {
			t.Fatal("invalid target admitted", change)
		}
	}
	replacement := pair(alice)
	if code, _ := probe.do("POST", "/cdn-profiles/regional/preferred-ips", payload); code != 401 {
		t.Fatal("old token survived reset", code)
	}
	if code, _ := bobProbe.do("POST", "/cdn-profiles/regional/preferred-ips", payload); code != 200 {
		t.Fatal("alice reset revoked bob", code)
	}
	if code, _ := alice.req("POST", "/u/cdn-pool", map[string]any{"profileId": "regional", "action": "revoke"}, true); code != 200 {
		t.Fatal(code)
	}
	if code, _ := replacement.do("POST", "/cdn-profiles/regional/preferred-ips", payload); code != 401 {
		t.Fatal("revoked token survived", code)
	}
	var count int64
	s.st.DB.Model(&store.CDNIPReport{}).Where("source = ?", "personal").Count(&count)
	if count != 0 {
		t.Fatal("personal report entered shared table")
	}
	// A grant removed after pairing immediately stops updates.
	s.st.DB.Delete(&store.UserGroup{}, "user_id = ?", "rb")
	if code, _ := bobProbe.do("POST", "/cdn-profiles/regional/preferred-ips", payload); code != 403 {
		t.Fatal("revoked grant still accepted", code)
	}
}

func TestPersonalCDNFilteringFallbackAndNodeSettings(t *testing.T) {
	s, base, _ := cdnRegionFixture(t)
	a := regionPortalClient(t, s, base, "ra")
	for _, csrf := range []bool{false, true} {
		code, _ := a.req("POST", "/u/cdn-pool", map[string]any{"profileId": "foreign", "action": "pair"}, csrf)
		if code != 403 {
			t.Fatal("CSRF/ownership bypass", code)
		}
	}
	if code, _ := a.req("POST", "/u/cdn-preferences", map[string]any{"profileId": "regional", "addresses": []string{"104.20.9.9"}}, true); code != 200 {
		t.Fatal(code)
	}
	pool := store.PersonalCDNPool{Automatic: map[string][]string{"443": {"104.20.8.8"}, "2053": {"104.20.7.7"}}, Disabled: []string{"104.20.8.8"}, Checks: map[string]store.PersonalCDNCheck{"104.20.9.9:443": {OK: false, At: time.Now()}}}
	s.st.DB.Model(&store.User{}).Where("id = ?", "ra").Update("cdn_pool", store.NewJSON(map[string]store.PersonalCDNPool{"regional": pool}))
	if got := regionServers(t, s, "ra", "ct", "CN-31"); !slices.Equal(got, []string{"104.16.1.1"}) {
		t.Fatal("personal failures did not fall back", got)
	}
	// Even if the public pool includes an excluded personal address, it stays out.
	s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", "regional").Updates(map[string]any{"preferred_ips": store.NewJSON([]string{"104.20.9.9", "104.20.8.8", "104.16.3.1"}), "carrier_split": false, "region_split": false})
	if got := regionServers(t, s, "ra", "", ""); !slices.Equal(got, []string{"104.16.3.1"}) {
		t.Fatal("public fallback revived excluded address", got)
	}
	// Retargeting the entry preserves all transport settings and only uses a
	// measurement of that port; the 443 connectivity failure does not cross ports.
	s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "re").Updates(map[string]any{"port": 2053, "host": "host.example.com", "sni": "sni.example.com"})
	var u store.User
	s.st.DB.First(&u, "id = ?", "ra")
	nodes, err := s.subNodesFor(&u, "", "")
	if err != nil || len(nodes) != 2 {
		t.Fatal(err, nodes)
	}
	for _, n := range nodes {
		if n.Port != 2053 || n.TLS.ServerName != "sni.example.com" || n.Transport.Host != "host.example.com" {
			t.Fatal("connection settings changed", n)
		}
	}
	// Failures remain closed when every node check failed.
	var p store.CDNProfile
	p.NodeCheck = true
	p.BadIPs = store.NewJSON([]string{"104.20.9.9"})
	if got := usableCDNIPsAt(&p, []string{"104.20.9.9"}, cdnDefaultPort(&p)); len(got) != 0 {
		t.Fatal("all-bad fallback revived address", got)
	}
}

func TestPersonalCDNCheckRejectsPrivateTargets(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "::1", "169.254.169.254", "10.0.0.1", "100.64.0.1", "localhost"} {
		r := checkPersonalCDN(context.Background(), address, 80, "http", "example.com")
		if r.OK || !strings.Contains(r.Message, "private") {
			t.Fatal("private target reached", address, r)
		}
	}
}
