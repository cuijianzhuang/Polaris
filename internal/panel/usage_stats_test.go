package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostUsagePing(t *testing.T) {
	var got struct {
		Method, Path, UA string
		Body             map[string]string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Method, got.Path, got.UA = r.Method, r.URL.Path, r.Header.Get("User-Agent")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := postUsagePing(ctx, srv.Client(), srv.URL, "inst_test00001", "v0.1.0", "community"); err != nil {
		t.Fatal(err)
	}
	if got.Method != "POST" || got.Path != "/v1/usage" {
		t.Fatalf("ping %s %s", got.Method, got.Path)
	}
	if got.Body["instance"] != "inst_test00001" || got.Body["version"] != "v0.1.0" || got.Body["edition"] != "community" {
		t.Fatalf("payload %+v", got.Body)
	}
	if !strings.HasPrefix(got.UA, "polaris-panel/") {
		t.Fatalf("user-agent %q", got.UA)
	}
	// A rejecting server is an error to the caller (reportUsageStats logs it).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	if err := postUsagePing(ctx, bad.Client(), bad.URL, "inst_test00001", "v0.1.0", "community"); err == nil {
		t.Fatal("want error on 500")
	}
}

func TestFetchUsageCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage.json" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"instances_30d":42,"instances_7d":30,"pro_30d":10,"community_30d":32}`))
	}))
	defer srv.Close()
	c, err := fetchUsageCounts(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if c != (usageCounts{Instances30d: 42, Instances7d: 30, Pro30d: 10, Community30d: 32}) {
		t.Fatalf("counts %+v", c)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{oops`))
	}))
	defer bad.Close()
	if _, err := fetchUsageCounts(context.Background(), bad.Client(), bad.URL); err == nil {
		t.Fatal("want error on malformed JSON")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := fetchUsageCounts(ctx, slow.Client(), slow.URL); err == nil {
		t.Fatal("want error on timeout")
	}
}

func TestReportUsageStats(t *testing.T) {
	s := newTestServer(t)
	var hits atomic.Int32
	var lastBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &lastBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("POLARIS_LICENSE_SERVER", srv.URL)
	s.reportUsageStats()
	if hits.Load() != 1 {
		t.Fatalf("hits %d, want 1", hits.Load())
	}
	// The reported edition follows the build tag: "pro" under -tags pro.
	wantEdition := "community"
	if proBuild {
		wantEdition = "pro"
	}
	if !strings.HasPrefix(lastBody["instance"], "inst_") || lastBody["version"] != Version || lastBody["edition"] != wantEdition {
		t.Fatalf("payload %+v", lastBody)
	}
	// Opted out: no further pings.
	if err := s.st.SetSetting("usage_stats", "false"); err != nil {
		t.Fatal(err)
	}
	s.reportUsageStats()
	if hits.Load() != 1 {
		t.Fatalf("hits %d after opt-out, want 1", hits.Load())
	}
	// No license server configured: no ping either.
	if err := s.st.SetSetting("usage_stats", "true"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POLARIS_LICENSE_SERVER", "")
	s.reportUsageStats()
	if hits.Load() != 1 {
		t.Fatalf("hits %d without a server, want 1", hits.Load())
	}
}

func TestGlobalUsageCountsCache(t *testing.T) {
	s := newTestServer(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"instances_30d":7,"instances_7d":5,"pro_30d":2,"community_30d":5}`))
	}))
	defer srv.Close()
	t.Setenv("POLARIS_LICENSE_SERVER", srv.URL)
	c, ok := s.globalUsageCounts()
	if !ok || c.Instances30d != 7 {
		t.Fatalf("counts %+v ok=%v", c, ok)
	}
	// Second call is served from the cache: no second request.
	if _, ok := s.globalUsageCounts(); !ok || hits.Load() != 1 {
		t.Fatalf("cache miss: hits %d", hits.Load())
	}
	// An unreachable server reports false and never breaks the caller.
	s2 := newTestServer(t)
	t.Setenv("POLARIS_LICENSE_SERVER", "http://127.0.0.1:1")
	if _, ok := s2.globalUsageCounts(); ok {
		t.Fatal("want false for an unreachable server")
	}
}

func TestUsageStatsSettingEditable(t *testing.T) {
	s, _, c := newAdminClient(t)
	code, _ := c.do("PUT", "/settings", map[string]string{"usage_stats": "false"})
	if code != 200 {
		t.Fatalf("PUT /settings %d", code)
	}
	if got := s.st.GetSetting("usage_stats", "true"); got != "false" {
		t.Fatalf("usage_stats %q, want false", got)
	}
	if s.usageStatsEnabled() {
		t.Fatal("usage stats still enabled after opting out")
	}
	// GET /settings returns the key, so the toggle reflects saved state.
	if code, out := c.do("GET", "/settings", nil); code != 200 || out["usage_stats"] != "false" {
		t.Fatalf("GET /settings %d: usage_stats=%v", code, out["usage_stats"])
	}
}
