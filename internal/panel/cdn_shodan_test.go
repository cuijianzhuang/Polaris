package panel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/realityscan"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// The Shodan source keeps third-party addresses (Cloudflare's own are
// dropped), hands them to speed-test points through ip-candidates only while
// on, and the profile list marks reported third-party addresses.
func TestCDNShodanCandidates(t *testing.T) {
	var queries []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("query"))
		fmt.Fprint(w, `{"total":3,"matches":[{"ip_str":"47.242.1.2","port":443},{"ip_str":"104.16.5.5","port":443},{"ip_str":"8.210.3.4","port":443},{"ip_str":"47.242.1.2","port":443}]}`)
	}))
	defer fake.Close()
	old := realityscan.ShodanBase
	realityscan.ShodanBase = fake.URL
	defer func() { realityscan.ShodanBase = old }()

	s, srv, admin := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.CDNProfile{ID: "cf", Name: "hk-cdn", Provider: "cloudflare", OriginDomain: "hk-cdn.example.com",
		Ports: store.NewJSON([]int{443}), PreferredIPs: store.NewJSON([]string{"104.16.1.1", "47.242.1.2", "visa.cn"})})
	_, tok, err := s.issueAPIToken("测速", "", []string{ScopeCDNIPs}, 0, "root")
	if err != nil {
		t.Fatal(err)
	}
	probe := &apiClient{t: t, base: srv.URL + s.cfg.APIBase(), tok: tok}
	cands := func() string {
		code, out := probe.do("GET", "/cdn-profiles/cf/ip-candidates", nil)
		if code != 200 {
			t.Fatalf("candidates %d %v", code, out)
		}
		return fmt.Sprint(out["ips"])
	}
	if got := cands(); got != "[]" {
		t.Fatalf("off: %s", got)
	}
	// Turning it on needs a Shodan key.
	if code, _ := admin.do("PUT", "/cdn-shodan", map[string]any{"enabled": true}); code != 400 {
		t.Fatalf("without a key: %d", code)
	}
	s.st.SetSetting("shodan_api_key", s.st.Box.Seal("k"))
	code, out := admin.do("PUT", "/cdn-shodan", map[string]any{"enabled": true, "pages": 1})
	if code != 200 || out["count"] != float64(2) || out["error"] != "" {
		t.Fatalf("on: %d %v", code, out)
	}
	if len(queries) != 1 || !strings.Contains(queries[0], `-org:"Cloudflare"`) {
		t.Fatalf("queries %v", queries)
	}
	if got := cands(); got != "[47.242.1.2 8.210.3.4]" {
		t.Fatalf("candidates = %s", got)
	}
	// The list marks the third-party address among the preferred ones.
	_, list := admin.doList("GET", "/cdn-profiles")
	if got := fmt.Sprint(list[0].(map[string]any)["thirdParty"]); got != "[47.242.1.2]" {
		t.Fatalf("thirdParty = %s", got)
	}
	// Off again: points stop getting candidates.
	admin.do("PUT", "/cdn-shodan", map[string]any{"enabled": false})
	if got := cands(); got != "[]" {
		t.Fatalf("off again: %s", got)
	}
}
