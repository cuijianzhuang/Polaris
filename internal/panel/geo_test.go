package panel

import (
	"strings"
	"testing"
)

func TestGeoBrowse(t *testing.T) {
	_, _, admin := newAdminClient(t)
	st, out := admin.do("GET", "/routing/geo?type=geosite&q=www.netflix.com", nil)
	if st != 200 || len(out["items"].([]any)) < 1000 || !strings.Contains(toJSONString(out["lookup"]), `"netflix"`) {
		t.Fatalf("geosite: %d lookup=%v", st, out["lookup"])
	}
	st, out = admin.do("GET", "/routing/geo?type=geoip", nil)
	if st != 200 || !strings.Contains(toJSONString(out), `"name":"cn"`) || !strings.Contains(toJSONString(out), `"singBox":false`) {
		t.Fatalf("geoip: %d", st)
	}
	st, out = admin.do("GET", "/routing/geo/geosite/openai", nil)
	if st != 200 || out["total"].(float64) == 0 || !strings.Contains(toJSONString(out), "openai.com") {
		t.Fatalf("entries: %d %v", st, out["total"])
	}
	if st, _ := admin.do("GET", "/routing/geo/geosite/nope-nope", nil); st != 404 {
		t.Fatal("unknown category")
	}
}
