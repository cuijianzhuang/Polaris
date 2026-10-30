package panel

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestCDNProbePairing(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	for _, id := range []string{"cf", "other"} {
		s.st.DB.Create(&store.CDNProfile{ID: id, Name: id, Provider: "cloudflare", OriginDomain: id + ".example.com",
			Ports: store.NewJSON([]int{443}), PreferredIPs: store.NewJSON([]string{})})
	}
	s.st.SetSetting("github_mirror", "https://mirror.example/")
	pair := func(profile, source string) (int, map[string]any) {
		return admin.do("POST", "/cdn-profiles/"+profile+"/probes", map[string]any{"source": source})
	}
	code, out := pair("cf", "上海电信")
	if code != 201 || !strings.HasPrefix(out["token"].(string), "plt_") || out["mirror"] != "https://mirror.example" || out["source"] != "上海电信" {
		t.Fatalf("pair: %d %v", code, out)
	}
	instance := out["instance"]
	if out["scheme"] != "https" || out["port"] != float64(443) || instance == "" {
		t.Fatal("missing installation metadata", out)
	}
	tok := out["token"].(string)
	if code, _ := pair("cf", "bad/name"); code != 400 {
		t.Fatalf("bad source accepted: %d", code)
	}
	probe := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: tok}
	report := func(c *apiClient, profile, source string) int {
		code, _ := c.do("POST", "/cdn-profiles/"+profile+"/preferred-ips", map[string]any{"source": source,
			"ips": []map[string]any{{"ip": "104.16.1.1", "latencyMs": 100, "loss": 0}}})
		return code
	}
	if c := report(probe, "cf", "上海电信"); c != 200 {
		t.Fatalf("paired report: %d", c)
	}
	// The token is bound to its profile and its name.
	if c := report(probe, "cf", "北京联通"); c != 403 {
		t.Fatalf("report under another name: %d", c)
	}
	if c := report(probe, "other", "上海电信"); c != 403 {
		t.Fatalf("report to another profile: %d", c)
	}
	// An ordinary cdn:ips:write token still reports anywhere.
	_, plain, _ := s.issueAPIToken("脚本", "", []string{ScopeCDNIPs}, 0, "root")
	if c := report(&apiClient{t: t, base: probe.base, tok: plain}, "other", "任意"); c != 200 {
		t.Fatalf("unbound token: %d", c)
	}

	// The list shows the paired point next to its reports.
	req, _ := http.NewRequest("GET", admin.base+"/cdn-profiles", nil)
	req.Header.Set("Authorization", "Bearer "+admin.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	var cf map[string]any
	for _, p := range list {
		if p["id"] == "cf" {
			cf = p
		}
	}
	probes := cf["probes"].([]any)
	if len(probes) != 1 || probes[0].(map[string]any)["source"] != "上海电信" || probes[0].(map[string]any)["lastUsedAt"] == nil {
		t.Fatalf("probes %v", probes)
	}

	// Pairing the same name again replaces the token.
	_, out = pair("cf", "上海电信")
	if out["instance"] != instance {
		t.Fatal("re-pair changed instance", out)
	}
	if c := report(probe, "cf", "上海电信"); c != 401 {
		t.Fatalf("old token still works after re-pairing: %d", c)
	}
	probe.tok = out["token"].(string)
	if c := report(probe, "cf", "上海电信"); c != 200 {
		t.Fatalf("new token: %d", c)
	}
	var n int64
	s.st.DB.Model(&store.APIToken{}).Where("cdn_profile_id = ?", "cf").Count(&n)
	if n != 1 {
		t.Fatalf("%d tokens for the point", n)
	}

	// Removing the point revokes its token.
	if code, _ := admin.do("DELETE", "/cdn-profiles/cf/ip-reports/"+url.PathEscape("上海电信"), nil); code != 204 {
		t.Fatalf("remove: %d", code)
	}
	if c := report(probe, "cf", "上海电信"); c != 401 {
		t.Fatalf("token survives removing the point: %d", c)
	}
	// Deleting a profile revokes the tokens of its points.
	pair("other", "广州移动")
	if code, _ := admin.do("DELETE", "/cdn-profiles/other", nil); code != 204 {
		t.Fatalf("delete profile: %d", code)
	}
	s.st.DB.Model(&store.APIToken{}).Where("cdn_profile_id = ?", "other").Count(&n)
	if n != 0 {
		t.Fatalf("%d tokens left after deleting the profile", n)
	}
}

func TestCDNHTTPProbeInstances(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	p := store.CDNProfile{ID: "http-pair", Name: "http", Provider: "cloudflare", Scheme: "http", Ports: store.NewJSON([]int{80, 8080})}
	if err := s.st.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	pair := func(name string, port int) (int, map[string]any) {
		return admin.do("POST", "/cdn-profiles/"+p.ID+"/probes", map[string]any{"source": name, "port": port})
	}
	code, a := pair("http-80", 80)
	if code != 201 || a["scheme"] != "http" || a["port"] != float64(80) || a["instance"] == "" {
		t.Fatal(code, a)
	}
	code, b := pair("http-8080", 8080)
	if code != 201 || a["instance"] == b["instance"] {
		t.Fatal("independent ports reused instance", code, b)
	}
	if code, _ = pair("bad-port", 443); code != 400 {
		t.Fatal("HTTPS port accepted for HTTP", code)
	}
	for _, p := range []map[string]any{a, b} {
		client := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: p["token"].(string)}
		code, out := client.do("POST", "/cdn-profiles/http-pair/preferred-ips", map[string]any{"source": p["source"], "scheme": "http", "port": p["port"], "ips": []map[string]any{{"ip": "104.16.1.1"}}})
		if code != 200 {
			t.Fatal(code, out)
		}
		code, _ = client.do("POST", "/cdn-profiles/http-pair/preferred-ips", map[string]any{"source": p["source"], "scheme": "http", "port": 8880, "ips": []map[string]any{{"ip": "104.16.1.1"}}})
		if code != 403 {
			t.Fatal("port binding bypassed", code)
		}
	}
}
