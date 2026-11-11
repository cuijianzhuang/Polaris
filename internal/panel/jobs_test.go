package panel

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJobCronSpec(t *testing.T) {
	cases := []struct {
		d      time.Duration
		second int
		want   string
	}{
		{10 * time.Second, 0, "*/10 * * * * *"},
		{15 * time.Second, 0, "*/15 * * * * *"},
		{30 * time.Second, 0, "*/30 * * * * *"},
		{time.Minute, 45, "45 * * * * *"},
		{time.Minute, 0, "0 * * * * *"},
		{2 * time.Minute, 30, "30 */2 * * * *"},
		{15 * time.Minute, 30, "30 */15 * * * *"},
	}
	for _, tc := range cases {
		if got := jobCronSpec(tc.d, tc.second); got != tc.want {
			t.Errorf("jobCronSpec(%v, %d) = %q, want %q", tc.d, tc.second, got, tc.want)
		}
	}
}

func TestConfigJobsDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("secret_key: 0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Duration{
		"settle": 10 * time.Second, "payment-recovery": 10 * time.Second, "latency-probes": 10 * time.Second,
		"offline": 15 * time.Second, "refund-recovery": 15 * time.Second, "rollouts": 15 * time.Second,
		"user-reminders": 15 * time.Second, "forward-failover": 20 * time.Second,
		"update-result": 20 * time.Second, "webhooks": 30 * time.Second, "plugin-cache": 30 * time.Second,
		"usdt": 30 * time.Second, "route-traces": 30 * time.Second, "health": time.Minute, "inbound-probe": 2 * time.Minute,
	}
	got := map[string]time.Duration{
		"settle": c.Jobs.Settle, "payment-recovery": c.Jobs.PaymentRecovery, "latency-probes": c.Jobs.LatencyProbes,
		"offline": c.Jobs.Offline, "refund-recovery": c.Jobs.RefundRecovery, "rollouts": c.Jobs.Rollouts,
		"user-reminders": c.Jobs.UserReminders, "forward-failover": c.Jobs.ForwardFailover,
		"update-result": c.Jobs.UpdateResult, "webhooks": c.Jobs.Webhooks, "plugin-cache": c.Jobs.PluginCache,
		"usdt": c.Jobs.USDT, "route-traces": c.Jobs.RouteTraces, "health": c.Jobs.Health, "inbound-probe": c.Jobs.InboundProbe,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("jobs.%s = %v, want %v", name, got[name], w)
		}
	}
}

func TestConfigJobsEnvAndInvalid(t *testing.T) {
	t.Setenv("POLARIS_SECRET_KEY", "0123456789abcdef")
	t.Setenv("POLARIS_JOBS_SETTLE_INTERVAL", "30s")
	t.Setenv("POLARIS_JOBS_HEALTH_INTERVAL", "5m")
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Jobs.Settle != 30*time.Second {
		t.Fatalf("settle = %v, want 30s", c.Jobs.Settle)
	}
	if c.Jobs.Health != 5*time.Minute {
		t.Fatalf("health = %v, want 5m", c.Jobs.Health)
	}
	if c.Jobs.Offline != 15*time.Second {
		t.Fatalf("offline default changed: %v", c.Jobs.Offline)
	}

	t.Setenv("POLARIS_JOBS_SETTLE_INTERVAL", "7s") // does not divide 60
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("7s settle interval accepted")
	}
	t.Setenv("POLARIS_JOBS_SETTLE_INTERVAL", "30s")
	t.Setenv("POLARIS_JOBS_OFFLINE_INTERVAL", "5m") // past the 90s heartbeat cutoff
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("5m offline interval accepted")
	}
	t.Setenv("POLARIS_JOBS_OFFLINE_INTERVAL", "15s")
	t.Setenv("POLARIS_JOBS_HEALTH_INTERVAL", "1s") // below the 5s minimum
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("1s health interval accepted")
	}
}
