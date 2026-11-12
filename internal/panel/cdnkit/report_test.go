package cdnkit

import (
	"slices"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func ipResult(ip string, loss, speed, latency float64) store.CDNIPResult {
	return store.CDNIPResult{IP: ip, Loss: loss, Speed: speed, LatencyMs: latency}
}

func TestCheckReport(t *testing.T) {
	out, err := CheckReport("上海电信", []store.CDNIPResult{
		ipResult("104.16.0.2", 0.1, 10, 50),
		ipResult("104.16.0.1", 0.0, 5, 100),
		ipResult("104.16.0.3", 0.0, 20, 30),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Lowest loss first; among zero loss, fastest download first.
	if len(out) != 3 || out[0].IP != "104.16.0.3" || out[1].IP != "104.16.0.1" || out[2].IP != "104.16.0.2" {
		t.Fatalf("ranking: %+v", out)
	}
	// Duplicates keep the best result.
	out, err = CheckReport("dup", []store.CDNIPResult{
		ipResult("104.16.0.1", 0.5, 1, 500),
		ipResult("104.16.0.1", 0.0, 10, 50),
	})
	if err != nil || len(out) != 1 || out[0].Loss != 0.0 {
		t.Fatalf("dedup: %+v %v", out, err)
	}
	// Colo is normalised to upper case.
	out, err = CheckReport("colo", []store.CDNIPResult{{IP: "104.16.0.1", Colo: "hkg"}})
	if err != nil || out[0].Colo != "HKG" {
		t.Fatalf("colo: %+v %v", out, err)
	}
	for _, tc := range []struct {
		name string
		src  string
		ips  []store.CDNIPResult
	}{
		{"bad source", "", []store.CDNIPResult{ipResult("104.16.0.1", 0, 0, 0)}},
		{"no ips", "x", nil},
		{"private ip", "x", []store.CDNIPResult{ipResult("10.0.0.1", 0, 0, 0)}},
		{"not an ip", "x", []store.CDNIPResult{{IP: "example.com"}}},
		{"bad loss", "x", []store.CDNIPResult{ipResult("104.16.0.1", 2, 0, 0)}},
		{"bad latency", "x", []store.CDNIPResult{ipResult("104.16.0.1", 0, 0, -1)}},
		{"bad colo", "x", []store.CDNIPResult{{IP: "104.16.0.1", Colo: "toolongcode"}}},
	} {
		if _, err := CheckReport(tc.src, tc.ips); err == nil {
			t.Errorf("%s accepted", tc.name)
		}
	}
	if _, err := CheckReport("x", make([]store.CDNIPResult, ReportMaxIPs+1)); err == nil {
		t.Fatal("over-limit report accepted")
	}
	if !ValidSourceName("上海电信") || ValidSourceName("") || ValidSourceName(" bad") {
		t.Fatal("ValidSourceName")
	}
}

func TestCompareIPResults(t *testing.T) {
	a := ipResult("1.1.1.1", 0.1, 10, 50)
	b := ipResult("1.1.1.2", 0.0, 1, 500)
	if CompareIPResults(a, b) <= 0 || CompareIPResults(b, a) >= 0 || CompareIPResults(a, a) != 0 {
		t.Fatal("loss dominates")
	}
}

func report(source string, ips ...string) store.CDNIPReport {
	var rs []store.CDNIPResult
	for _, ip := range ips {
		rs = append(rs, store.CDNIPResult{IP: ip})
	}
	return store.CDNIPReport{Source: source, IPs: store.NewJSON(rs)}
}

func TestMergeReports(t *testing.T) {
	got := MergeReports([]store.CDNIPReport{
		report("b-point", "10.0.0.1", "10.0.0.2"),
		report("a-point", "10.0.0.2", "10.0.0.3"),
	})
	// Sources in name order, round-robin, duplicates once.
	want := []string{"10.0.0.2", "10.0.0.1", "10.0.0.3"}
	if !slices.Equal(got, want) {
		t.Fatalf("merge: %v want %v", got, want)
	}
	p := &store.CDNProfile{PinnedHosts: store.NewJSON([]string{"pinned.example.com", "10.0.0.1"})}
	got = MergedReportIPs(p, []store.CDNIPReport{report("a", "10.0.0.1")})
	if !slices.Equal(got, []string{"10.0.0.1", "pinned.example.com"}) {
		t.Fatalf("merged with pins: %v", got)
	}
}

func TestUsableIPsAt(t *testing.T) {
	p := &store.CDNProfile{
		BlockedHosts: store.NewJSON([]string{"blocked.example.com"}),
		NodeCheck:    true,
		PortChecks:   store.NewJSON(map[string]store.CDNPortCheck{"443": {BadIPs: []string{"10.0.0.2"}}}),
		BadIPs:       store.NewJSON([]string{"10.0.0.3"}),
	}
	list := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "blocked.example.com", "10.0.0.4"}
	// A per-port check exists for 443, so it wins over the profile-level
	// bad list; another port has neither.
	if got := UsableIPsAt(p, list, 443); !slices.Equal(got, []string{"10.0.0.1", "10.0.0.3", "10.0.0.4"}) {
		t.Fatalf("port 443: %v", got)
	}
	if got := UsableIPsAt(p, list, 2053); !slices.Equal(got, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}) {
		t.Fatalf("port 2053: %v", got)
	}
	// Without a per-port check, the default port falls back to the
	// profile-level bad list.
	p.PortChecks = store.NewJSON(map[string]store.CDNPortCheck{})
	if got := UsableIPsAt(p, list, 443); !slices.Equal(got, []string{"10.0.0.1", "10.0.0.2", "10.0.0.4"}) {
		t.Fatalf("default port fallback: %v", got)
	}
	p.NodeCheck = false
	if got := UsableIPsAt(p, list, 443); !slices.Equal(got, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}) {
		t.Fatalf("node check off: %v", got)
	}
	if got := UsableIPs(p); got != nil {
		t.Fatalf("UsableIPs on empty profile: %v", got)
	}
}

func TestStaleAfterAndReportHelpers(t *testing.T) {
	if StaleAfter(&store.CDNProfile{}) != 24*time.Hour {
		t.Fatal("default stale window")
	}
	if StaleAfter(&store.CDNProfile{StaleHours: 48}) != 48*time.Hour {
		t.Fatal("configured stale window")
	}
	if got := ReportIPs([]store.CDNIPReport{report("a", "10.0.0.1", "10.0.0.2")}); !slices.Equal(got, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatalf("ReportIPs: %v", got)
	}
	if got := WithPinned([]string{"a"}, []string{"b", "a"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("WithPinned: %v", got)
	}
	if EffectiveReportPort(store.CDNIPReport{}) != 443 || EffectiveReportPort(store.CDNIPReport{Port: 2053}) != 2053 {
		t.Fatal("EffectiveReportPort")
	}
	if EffectiveReportScheme(store.CDNIPReport{}) != "https" || EffectiveReportScheme(store.CDNIPReport{Scheme: "http"}) != "http" {
		t.Fatal("EffectiveReportScheme")
	}
	p := &store.CDNProfile{BlockedHosts: store.NewJSON([]string{"10.0.0.9", "bad.example.com."})}
	if !AddressBlocked(p, "10.0.0.9") || !AddressBlocked(p, "BAD.example.com") || AddressBlocked(p, "10.0.0.1") {
		t.Fatal("AddressBlocked")
	}
}

func TestReportTarget(t *testing.T) {
	httpsP := &store.CDNProfile{Scheme: "https", Provider: "cloudflare"}
	if port, scheme, err := ReportTarget(0, "", httpsP); err != nil || port != 0 || scheme != "" {
		t.Fatalf("default bucket: %d %q %v", port, scheme, err)
	}
	if port, scheme, err := ReportTarget(2053, "", httpsP); err != nil || port != 2053 || scheme != "https" {
		t.Fatalf("explicit port: %d %q %v", port, scheme, err)
	}
	if _, _, err := ReportTarget(80, "", httpsP); err == nil {
		t.Fatal("HTTP port on HTTPS profile accepted")
	}
	if _, _, err := ReportTarget(2053, "http", httpsP); err == nil {
		t.Fatal("scheme mismatch accepted")
	}
	if _, _, err := ReportTarget(0, "https", httpsP); err == nil {
		t.Fatal("port 0 with explicit scheme accepted")
	}
	if _, _, err := ReportTarget(70000, "", httpsP); err == nil {
		t.Fatal("out of range port accepted")
	}
}
