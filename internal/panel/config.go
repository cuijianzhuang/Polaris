package panel

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Config is the panel configuration, loaded from an optional YAML file and
// POLARIS_* environment variables (POLARIS_DB_DSN -> db.dsn).
type Config struct {
	// Listen is the admin/API/subscription HTTP address (put Caddy/Nginx in front for HTTPS).
	Listen string `koanf:"listen"`
	// AgentListen is the mTLS gRPC address agents dial.
	AgentListen string `koanf:"agent_listen"`
	// AgentHosts are the DNS names / IPs agents use to reach AgentListen (server cert SANs).
	AgentHosts []string `koanf:"agent_hosts"`
	// PublicURL is the externally visible base URL, used in subscription links.
	PublicURL string `koanf:"public_url"`
	// AdminPath hides the admin UI and API under a secret prefix, e.g. "/x7k2".
	AdminPath string `koanf:"admin_path"`
	// DataDir holds the SQLite database, the CA and downloadable agent binaries.
	DataDir string `koanf:"data_dir"`
	DB      struct {
		DSN   string `koanf:"dsn"`
		Debug bool   `koanf:"debug"`
	} `koanf:"db"`
	// RedisURL (optional) shares online IPs, rate limits and alert
	// de-duplication between panel instances. Traffic billing never uses it.
	RedisURL string `koanf:"redis_url"`
	// SecretKey encrypts sensitive columns and signs JWTs. Required.
	SecretKey string `koanf:"secret_key"`
	// SecretKeyPrevious is the key being rotated away from, supplied only
	// during a rotation (POLARIS_SECRET_KEY_PREVIOUS). It is never written
	// to the database or to backups; remove it from the environment once
	// the rotation completes. See docs/design/secret-key-rotation.md.
	SecretKeyPrevious string `koanf:"secret_key_previous"`
	// InitialAdmin is created on first start if no admin exists.
	InitialAdmin struct {
		Username string `koanf:"username"`
		Password string `koanf:"password"`
	} `koanf:"initial_admin"`
	// SubRateLimit is the per-IP subscription request rate (requests/minute).
	SubRateLimit int `koanf:"sub_rate_limit"`
	// Timezone is the IANA name used for day boundaries (traffic daily
	// buckets and report presets), e.g. "Asia/Shanghai". Empty: server local.
	Timezone string `koanf:"timezone"`
	// Jobs holds the intervals of the high-frequency scheduled jobs.
	// Defaults are the historical hard-coded intervals; set them in
	// config.yaml (jobs: {settle_interval: 30s}) or via POLARIS_JOBS_* env
	// vars (POLARIS_JOBS_SETTLE_INTERVAL=30s) to trade freshness for load
	// on small panels or busy ones. Only the jobs listed here are
	// configurable; the rest keep their fixed schedules in startJobs.
	// Raising settle/offline/latency intervals delays quota enforcement,
	// node-offline detection (offline must stay under the 90s heartbeat
	// cutoff) and probe freshness; lowering them increases DB write load.
	Jobs struct {
		Settle          time.Duration `koanf:"settle_interval"`
		PaymentRecovery time.Duration `koanf:"payment_recovery_interval"`
		LatencyProbes   time.Duration `koanf:"latency_probes_interval"`
		Offline         time.Duration `koanf:"offline_interval"`
		RefundRecovery  time.Duration `koanf:"refund_recovery_interval"`
		Rollouts        time.Duration `koanf:"rollouts_interval"`
		UserReminders   time.Duration `koanf:"user_reminders_interval"`
		ForwardFailover time.Duration `koanf:"forward_failover_interval"`
		UpdateResult    time.Duration `koanf:"update_result_interval"`
		Webhooks        time.Duration `koanf:"webhooks_interval"`
		PluginCache     time.Duration `koanf:"plugin_cache_interval"`
		USDT            time.Duration `koanf:"usdt_interval"`
		RouteTraces     time.Duration `koanf:"route_traces_interval"`
		Health          time.Duration `koanf:"health_interval"`
		InboundProbe    time.Duration `koanf:"inbound_probe_interval"`
	} `koanf:"jobs"`

	loc *time.Location // parsed Timezone; see Location()
	// TrustedProxies lists the reverse proxies (IPs or CIDRs) whose
	// X-Forwarded-For is trusted. Empty: any loopback or private peer.
	TrustedProxies []string `koanf:"trusted_proxies"`
	// Decoy is served for unknown paths: "404" (default) or a URL to reverse-proxy.
	Decoy string `koanf:"decoy"`
}

// LoadConfig reads the config file (if any) and environment.
func LoadConfig(path string) (*Config, error) {
	k := koanf.New(".")
	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("load %s: %w", path, err)
		}
	}
	if err := k.Load(env.Provider("POLARIS_", ".", func(s string) string {
		s = strings.ToLower(strings.TrimPrefix(s, "POLARIS_"))
		// POLARIS_DB_DSN -> db.dsn, POLARIS_INITIAL_ADMIN_PASSWORD -> initial_admin.password
		for _, prefix := range []string{"db_", "initial_admin_", "jobs_"} {
			if strings.HasPrefix(s, prefix) {
				return strings.TrimSuffix(prefix, "_") + "." + strings.TrimPrefix(s, prefix)
			}
		}
		return s
	}), nil); err != nil {
		return nil, err
	}
	c := &Config{Listen: ":8080", AgentListen: ":9443", DataDir: "./data", AdminPath: "/", SubRateLimit: 30}
	if err := k.Unmarshal("", c); err != nil {
		return nil, err
	}
	// Job intervals: keep the historical defaults unless configured.
	// Validation runs here (not at schedule time) so a bad value fails
	// startup with a clear error instead of silently never running.
	// Defaults live in applyJobDefaults, not only here: panel.New also
	// calls it, because a Config built in code (every test, any embedder)
	// never goes through LoadConfig, and a zero interval used to render
	// as the invalid cron spec "*/0 * * * * *", silently killing all
	// fifteen configurable jobs (traffic settlement included).
	c.applyJobDefaults()
	j := &c.Jobs
	for name, d := range map[string]time.Duration{
		"settle": j.Settle, "payment-recovery": j.PaymentRecovery, "latency-probes": j.LatencyProbes,
		"offline": j.Offline, "refund-recovery": j.RefundRecovery, "rollouts": j.Rollouts,
		"user-reminders": j.UserReminders, "forward-failover": j.ForwardFailover,
		"update-result": j.UpdateResult, "webhooks": j.Webhooks, "plugin-cache": j.PluginCache,
		"usdt": j.USDT, "route-traces": j.RouteTraces, "health": j.Health, "inbound-probe": j.InboundProbe,
	} {
		if err := validateJobInterval(name, d); err != nil {
			return nil, err
		}
	}
	if j.Offline > time.Minute {
		return nil, fmt.Errorf("jobs.offline_interval %v exceeds 1m: markOfflineNodes treats nodes as offline after 90s without a heartbeat", j.Offline)
	}
	if v := k.String("agent_hosts"); v != "" && len(c.AgentHosts) <= 1 {
		c.AgentHosts = splitList(v)
	}
	if v := k.String("trusted_proxies"); v != "" && len(c.TrustedProxies) <= 1 {
		c.TrustedProxies = splitList(v)
	}
	trusted := make([]*net.IPNet, 0, len(c.TrustedProxies))
	for _, p := range c.TrustedProxies {
		if !strings.Contains(p, "/") {
			if ip := net.ParseIP(p); ip != nil && ip.To4() != nil {
				p += "/32"
			} else {
				p += "/128"
			}
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %q: %w", p, err)
		}
		trusted = append(trusted, n)
	}
	trustedProxies = trusted
	c.AdminPath = "/" + strings.Trim(c.AdminPath, "/")
	if c.DB.DSN == "" {
		c.DB.DSN = filepath.Join(c.DataDir, "polaris.db")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.SecretKey == "" {
		return nil, fmt.Errorf("secret_key (POLARIS_SECRET_KEY) is required")
	}
	// The key encrypts stored secrets and signs sessions; reject guessable ones.
	if err := validateSecretKey(c.SecretKey); err != nil {
		return nil, fmt.Errorf("secret_key: %w", err)
	}
	if c.SecretKeyPrevious != "" {
		if err := validateSecretKey(c.SecretKeyPrevious); err != nil {
			return nil, fmt.Errorf("secret_key_previous: %w", err)
		}
		if c.SecretKeyPrevious == c.SecretKey {
			return nil, fmt.Errorf("secret_key_previous is the same as secret_key")
		}
	}
	if c.Timezone != "" {
		loc, err := time.LoadLocation(c.Timezone)
		if err != nil {
			return nil, fmt.Errorf("timezone %q: %w", c.Timezone, err)
		}
		c.loc = loc
	}
	return c, nil
}

// validateSecretKey rejects guessable keys: the key encrypts stored
// secrets and signs sessions.
func validateSecretKey(v string) error {
	if len(v) < 16 || strings.Contains(v, "change-me") {
		return fmt.Errorf("must be a random string of at least 16 characters, not the example value (generate one with: openssl rand -hex 32)")
	}
	return nil
}

// applyJobDefaults fills any unset job interval with its historical
// hard-coded value. Called from LoadConfig and from panel.New, so a
// Config built in code gets the same schedules as one loaded from file.
func (c *Config) applyJobDefaults() {
	j := &c.Jobs
	setDefault := func(p *time.Duration, d time.Duration) {
		if *p == 0 {
			*p = d
		}
	}
	setDefault(&j.Settle, 10*time.Second)
	setDefault(&j.PaymentRecovery, 10*time.Second)
	setDefault(&j.LatencyProbes, 10*time.Second)
	setDefault(&j.Offline, 15*time.Second)
	setDefault(&j.RefundRecovery, 15*time.Second)
	setDefault(&j.Rollouts, 15*time.Second)
	setDefault(&j.UserReminders, 15*time.Second)
	setDefault(&j.ForwardFailover, 20*time.Second)
	setDefault(&j.UpdateResult, 20*time.Second)
	setDefault(&j.Webhooks, 30*time.Second)
	setDefault(&j.PluginCache, 30*time.Second)
	setDefault(&j.USDT, 30*time.Second)
	setDefault(&j.RouteTraces, 30*time.Second)
	setDefault(&j.Health, time.Minute)
	setDefault(&j.InboundProbe, 2*time.Minute)
}

// validateJobInterval rejects intervals the cron scheduler cannot
// express: below one minute the interval must divide 60 (the cron spec
// "*/N" fires on the minute boundary), at or above one minute it must be
// a whole number of minutes. Minimum 5s: shorter would busy-loop the DB.
func validateJobInterval(name string, d time.Duration) error {
	if d < 5*time.Second {
		return fmt.Errorf("jobs.%s interval %v is below the 5s minimum", name, d)
	}
	if d < time.Minute {
		if 60%int(d/time.Second) != 0 {
			return fmt.Errorf("jobs.%s interval %v does not divide 60s: use a divisor of 60 (5s, 10s, 15s, 20s, 30s)", name, d)
		}
		return nil
	}
	if d%time.Minute != 0 || d > time.Hour {
		return fmt.Errorf("jobs.%s interval %v must be a whole number of minutes up to 1h", name, d)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// APIBase is the prefix of the REST API.
func (c *Config) APIBase() string {
	return strings.TrimRight(c.AdminPath, "/") + "/api/v1"
}

// Location returns the timezone used for day boundaries, defaulting to the
// server's local time when Timezone is empty.
func (c *Config) Location() *time.Location {
	if c.loc != nil {
		return c.loc
	}
	return time.Local
}
