package cdnkit

import (
	"net/url"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestNormalizeRegion(t *testing.T) {
	cases := map[string]string{
		"上海": "CN-31", "上海市": "CN-31", "Shanghai": "CN-31",
		"cn-44": "CN-44", "广东": "CN-44", "广东省": "CN-44",
		"内蒙古自治区": "CN-15", "宁夏回族自治区": "CN-64", "新疆维吾尔自治区": "CN-65",
		"广西壮族自治区": "CN-45", "香港特别行政区": "HK",
		"JP": "JP", "us": "US",
	}
	for in, want := range cases {
		if got, err := NormalizeRegion(in); err != nil || got != want {
			t.Errorf("NormalizeRegion(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, err := NormalizeRegion(""); err != nil || got != "" {
		t.Errorf("empty region: %q %v", got, err)
	}
	if _, err := NormalizeRegion("火星"); err == nil {
		t.Fatal("unknown region accepted")
	}
	if _, err := NormalizeRegion("ABC"); err == nil {
		t.Fatal("three-letter code accepted")
	}
}

func TestRegionNameAndSource(t *testing.T) {
	if RegionName("CN-31") != "上海" || RegionName("XX") != "XX" {
		t.Fatal("RegionName")
	}
	if RegionFromSource("上海电信") != "CN-31" || RegionFromSource("Beijing Unicom") != "CN-11" || RegionFromSource("香港") != "HK" || RegionFromSource("东京") != "" {
		t.Fatal("RegionFromSource")
	}
	r := store.CDNIPReport{Region: "广东", Source: "x"}
	if EffectiveRegion(r) != "CN-44" {
		t.Fatalf("effective region from field: %q", EffectiveRegion(r))
	}
	r = store.CDNIPReport{Source: "上海电信"}
	if EffectiveRegion(r) != "CN-31" {
		t.Fatalf("effective region from source: %q", EffectiveRegion(r))
	}
	if got, err := ReportRegion("浙江", "x"); err != nil || got != "CN-33" {
		t.Fatalf("ReportRegion explicit: %q %v", got, err)
	}
	if got, err := ReportRegion("", "广东移动"); err != nil || got != "CN-44" {
		t.Fatalf("ReportRegion inferred: %q %v", got, err)
	}
	if _, err := ReportRegion("火星", "x"); err == nil {
		t.Fatal("ReportRegion accepted bad region")
	}
}

func TestAudienceHelpers(t *testing.T) {
	if AudienceRegion(nil) != "" || AudienceRegion([]Audience{{Region: "CN-31"}}) != "CN-31" {
		t.Fatal("AudienceRegion")
	}
	u := WithAudience("https://sub.example.com/abc?x=1", "ct", "CN-31")
	q, _ := url.ParseQuery(mustParseURL(t, u).RawQuery)
	if q.Get("carrier") != "ct" || q.Get("region") != "CN-31" || q.Get("x") != "1" {
		t.Fatalf("WithAudience: %s", u)
	}
	u = WithAudience("https://sub.example.com/abc", "", "")
	q, _ = url.ParseQuery(mustParseURL(t, u).RawQuery)
	if q.Get("carrier") != "all" || q.Get("region") != "all" {
		t.Fatalf("WithAudience defaults: %s", u)
	}
	if got := WithAudience("::bad url", "ct", "CN-31"); got != "::bad url" {
		t.Fatalf("WithAudience bad url: %q", got)
	}
	// ExplicitSelectionURL keeps an explicit choice, drops an invalid one,
	// and leaves auto/all untouched.
	req := url.Values{"carrier": {"cu"}, "region": {"广东"}}
	got := ExplicitSelectionURL("https://sub.example.com/abc", req)
	q, _ = url.ParseQuery(mustParseURL(t, got).RawQuery)
	if q.Get("carrier") != "cu" || q.Get("region") != "CN-44" {
		t.Fatalf("ExplicitSelectionURL: %s", got)
	}
	req = url.Values{"carrier": {"xx"}, "region": {"火星"}}
	got = ExplicitSelectionURL("https://sub.example.com/abc?carrier=ct", req)
	q, _ = url.ParseQuery(mustParseURL(t, got).RawQuery)
	if q.Get("carrier") != "ct" || q.Get("region") != "" {
		t.Fatalf("ExplicitSelectionURL invalid kept: %s", got)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
