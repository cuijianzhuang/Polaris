package geodata

import (
	"slices"
	"testing"
	"time"
)

func TestEmbeddedGeoSite(t *testing.T) {
	start := time.Now()
	cats, err := Sites()
	if err != nil {
		t.Fatal(err)
	}
	if len(cats) < 1000 {
		t.Fatalf("only %d categories", len(cats))
	}
	t.Logf("%d categories, loaded in %v", len(cats), time.Since(start))
	names := map[string]Category{}
	for _, c := range cats {
		names[c.Name] = c
	}
	for _, n := range []string{"netflix", "openai", "google", "cn", "category-ads-all", "apple"} {
		if names[n].Count == 0 {
			t.Errorf("category %s missing or empty", n)
		}
	}
	if !slices.Contains(names["google"].Attrs, "cn") {
		t.Errorf("google has no @cn attribute: %v", names["google"].Attrs)
	}

	all, _ := Entries("google")
	cn, err := Entries("Google@cn")
	if err != nil || len(cn) == 0 || len(cn) >= len(all) {
		t.Fatalf("attribute filter: %d of %d, %v", len(cn), len(all), err)
	}
	if _, err := Entries("no-such-category"); err == nil {
		t.Fatal("unknown category accepted")
	}

	hits, err := Lookup("www.netflix.com")
	if err != nil || !slices.Contains(hits, "netflix") {
		t.Fatalf("lookup netflix: %v %v", hits, err)
	}
	if hits, _ := Lookup("chat.openai.com."); !slices.Contains(hits, "openai") {
		t.Fatalf("lookup openai: %v", hits)
	}
}

func TestEmbeddedGeoIP(t *testing.T) {
	ips, err := IPs()
	if err != nil || len(ips) < 200 {
		t.Fatalf("%d geoip categories, %v", len(ips), err)
	}
	var cn, private bool
	for _, c := range ips {
		cn = cn || c.Name == "cn" && c.V4 > 1000
		private = private || c.Name == "private"
	}
	if !cn || !private {
		t.Fatal("cn / private missing")
	}
	if !IPSingBox("jp") || !IPSingBox("private") || IPSingBox("telegram") {
		t.Fatal("sing-box support guess wrong")
	}
}

func TestDomainMatch(t *testing.T) {
	cases := []struct {
		d    Domain
		host string
		want bool
	}{
		{Domain{TypeDomain, "example.com", nil}, "a.example.com", true},
		{Domain{TypeDomain, "example.com", nil}, "badexample.com", false},
		{Domain{TypeFull, "example.com", nil}, "a.example.com", false},
		{Domain{TypePlain, "tube", nil}, "youtube.com", true},
		{Domain{TypeRegex, `^ads\d+\.`, nil}, "ads12.example.com", true},
	}
	for _, c := range cases {
		if got := c.d.matches(c.host); got != c.want {
			t.Errorf("%+v %s = %v", c.d, c.host, got)
		}
	}
}
