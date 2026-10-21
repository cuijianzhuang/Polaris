package panel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/store"
	"gorm.io/gorm"
)

func TestNodeNetHealthFreshness(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	never := nodeNetHealthOf(&store.Node{}, now)
	if never.State != "never" || never.Mode != "unknown" || never.LastSampleAt != nil || never.SampleAgeSeconds != nil {
		t.Fatal("missing sample looked like healthy zero", never)
	}
	for _, tc := range []struct {
		age   time.Duration
		state string
	}{
		{5 * time.Second, "healthy"}, {119 * time.Second, "healthy"}, {120 * time.Second, "stale"}, {time.Hour, "stale"}, {-time.Minute, "healthy"},
	} {
		at := now.Add(-tc.age)
		h := nodeNetHealthOf(&store.Node{NetObservedAt: &at, NetEpoch: "nic", NetBootID: "boot"}, now)
		if h.State != tc.state || h.Mode != "boot" || h.SampleAgeSeconds == nil || *h.SampleAgeSeconds < 0 || h.StaleAfterSeconds != 120 {
			t.Fatal("wrong sample freshness", tc, h)
		}
	}
	for _, tc := range []struct{ epoch, boot, mode string }{
		{"", "", "legacy"}, {"nic", "", "epoch"}, {"nic", "boot", "boot"},
	} {
		if h := nodeNetHealthOf(&store.Node{NetObservedAt: &now, NetEpoch: tc.epoch, NetBootID: tc.boot}, now); h.Mode != tc.mode {
			t.Fatal("wrong source identification mode", h)
		}
	}
}

func TestNodeNetHealthAcceptedSamplesOnly(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "n", Name: "n"})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	load := func() store.Node {
		t.Helper()
		var n store.Node
		if err := s.st.DB.First(&n, "id = ?", "n").Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	beat := func(p *polarisv1.SystemStats, at time.Time) {
		t.Helper()
		if err := s.accountNodeNet("n", p, at); err != nil {
			t.Fatal(err)
		}
	}
	// An observed zero is valid and establishes a healthy baseline.
	beat(bootSample("boot-1", "nic-a", 100, 0, 0), now)
	n := load()
	if h := nodeNetHealthOf(&n, now); h.State != "healthy" || h.BaselineReason != "initial" || h.BaselineAt == nil || !h.BaselineAt.Equal(now) {
		t.Fatal("zero sample not observed", h)
	}
	// Rejected, unavailable, duplicate and out-of-order samples do not refresh it.
	beat(nil, now.Add(10*time.Minute))
	beat(&polarisv1.SystemStats{}, now.Add(10*time.Minute))
	beat(bootSample("boot-1", "nic-a", 100, 0, 0), now.Add(10*time.Minute))
	beat(bootSample("boot-1", "nic-b", 50, 1000, 2000), now.Add(10*time.Minute))
	if err := s.accountNodeNet("n", &polarisv1.SystemStats{NetCountersValid: true}, now.Add(10*time.Minute)); err == nil {
		t.Fatal("invalid sample accepted")
	}
	n = load()
	if !n.NetObservedAt.Equal(now) || n.NetLastGapAt != nil || nodeNetHealthOf(&n, now.Add(10*time.Minute)).State != "stale" {
		t.Fatal("rejected sample refreshed health", n)
	}
	// A long interval is retained after the next ordinary heartbeat.
	resumed := now.Add(180 * time.Second)
	beat(bootSample("boot-1", "nic-a", 200, 10, 20), resumed)
	beat(bootSample("boot-1", "nic-a", 300, 15, 30), resumed.Add(5*time.Second))
	n = load()
	if n.NetLastGapAt == nil || !n.NetLastGapAt.Equal(resumed) || n.NetLastGapSeconds != 180 || !n.NetBaselineAt.Equal(now) || n.NetRx != 15 || n.NetTx != 30 {
		t.Fatal("long gap changed billing or disappeared", n)
	}
	// Each rebase records only the evidence that was actually observed.
	for _, step := range []struct {
		sample *polarisv1.SystemStats
		reason string
	}{
		{bootSample("boot-1", "nic-b", 400, 1000, 2000), "interfaces_changed"},
		{bootSample("boot-1", "nic-b", 500, 1, 2), "counters_decreased"},
		{bootSample("boot-2", "nic-c", 10, 100, 200), "boot_changed"},
	} {
		resumed = resumed.Add(10 * time.Second)
		beat(step.sample, resumed)
		n = load()
		if n.NetBaselineReason != step.reason || !n.NetBaselineAt.Equal(resumed) {
			t.Fatal("wrong rebase reason", n, step.reason)
		}
	}
	// A retired boot cannot rewrite the current boot's diagnostic metadata.
	beat(bootSample("boot-1", "nic-a", 1000, 2000, 4000), resumed.Add(time.Hour))
	n = load()
	if n.NetBaselineReason != "boot_changed" || !n.NetObservedAt.Equal(resumed) {
		t.Fatal("retired boot rewrote health", n)
	}
}

func TestNodeNetHealthLegacyAndFormatChanges(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "n", Name: "n"})
	now := time.Now()
	for i, step := range []struct {
		sample       *polarisv1.SystemStats
		reason, mode string
	}{
		{&polarisv1.SystemStats{RxBytes: 100, TxBytes: 200}, "initial", "legacy"},
		{nicSample("nic-a", 100, 110, 220), "format_changed", "epoch"},
		{nicSample("nic-b", 200, 120, 240), "source_changed", "epoch"},
		{bootSample("boot", "nic-b", 300, 130, 260), "format_changed", "boot"},
	} {
		at := now.Add(time.Duration(i) * time.Second)
		if err := s.accountNodeNet("n", step.sample, at); err != nil {
			t.Fatal(err)
		}
		var n store.Node
		s.st.DB.First(&n, "id = ?", "n")
		h := nodeNetHealthOf(&n, at)
		if h.BaselineReason != step.reason || h.Mode != step.mode {
			t.Fatal("legacy source misclassified", h)
		}
	}
}

func TestNodeNetHealthRollsBackWithLedger(t *testing.T) {
	onEngines(t, func(t *testing.T, dsn string) {
		ss := clusterServers(t, dsn, 2)
		s := ss[0]
		s.st.DB.Create(&store.Node{ID: "n", Name: "n"})
		now := time.Now().Truncate(time.Microsecond)
		if err := s.accountNodeNet("n", bootSample("boot", "nic-a", 100, 10, 20), now); err != nil {
			t.Fatal(err)
		}
		s.st.DB.Callback().Update().Before("gorm:update").Register("test:health-fail", func(tx *gorm.DB) {
			if tx.Statement.Table == "nodes" {
				tx.AddError(errors.New("node health write failed"))
			}
		})
		later := now.Add(180 * time.Second)
		if err := s.accountNodeNet("n", bootSample("boot", "nic-b", 200, 100, 200), later); err == nil {
			t.Fatal("failed metadata write acknowledged")
		}
		s.st.DB.Callback().Update().Remove("test:health-fail")
		var n store.Node
		ss[1].st.DB.First(&n, "id = ?", "n")
		if !n.NetObservedAt.Equal(now) || !n.NetBaselineAt.Equal(now) || n.NetBaselineReason != "initial" || n.NetLastGapAt != nil {
			t.Fatal("failed transaction changed health", n)
		}
		if err := ss[1].accountNodeNet("n", bootSample("boot", "nic-b", 200, 100, 200), later); err != nil {
			t.Fatal(err)
		}
		s.st.DB.First(&n, "id = ?", "n")
		if !n.NetObservedAt.Equal(later) || !n.NetBaselineAt.Equal(later) || n.NetBaselineReason != "interfaces_changed" || n.NetLastGapAt == nil || n.NetLastGapSeconds != 180 {
			t.Fatal("retry did not persist health across instances", n)
		}
	})
}

func TestNodeNetHealthReadViewsDoNotExposeSources(t *testing.T) {
	s, _, c := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "n", Name: "n"})
	if err := s.accountNodeNet("n", bootSample("internal-boot-identity", "internal-epoch-identity", 100, 0, 0), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/nodes", "/nodes/n", "/costs"} {
		req, _ := http.NewRequest("GET", c.base+path, nil)
		req.Header.Set("Authorization", "Bearer "+c.tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s %d %s %v", path, resp.StatusCode, raw, err)
		}
		if strings.Contains(string(raw), "internal-boot-identity") || strings.Contains(string(raw), "internal-epoch-identity") {
			t.Fatal("raw source identity leaked", path, string(raw))
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		switch path {
		case "/nodes":
			row = out.([]any)[0].(map[string]any)
		case "/costs":
			row = out.(map[string]any)["items"].([]any)[0].(map[string]any)
		default:
			row = out.(map[string]any)
		}
		h := row["netHealth"].(map[string]any)
		if h["state"] != "healthy" || h["mode"] != "boot" || h["baselineReason"] != "initial" || h["sampleAgeSeconds"] == nil {
			t.Fatal("health missing from view", path, h)
		}
	}
}
