package panel

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

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
	// RedisURL enables Redis as the traffic counter buffer (optional).
	RedisURL string `koanf:"redis_url"`
	// SecretKey encrypts sensitive columns and signs JWTs. Required.
	SecretKey string `koanf:"secret_key"`
	// InitialAdmin is created on first start if no admin exists.
	InitialAdmin struct {
		Username string `koanf:"username"`
		Password string `koanf:"password"`
	} `koanf:"initial_admin"`
	// SubRateLimit is the per-IP subscription request rate (requests/minute).
	SubRateLimit int `koanf:"sub_rate_limit"`
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
		for _, prefix := range []string{"db_", "initial_admin_"} {
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
	if len(c.SecretKey) < 16 || strings.Contains(c.SecretKey, "change-me") {
		return nil, fmt.Errorf("secret_key must be a random string of at least 16 characters, not the example value (generate one with: openssl rand -hex 32)")
	}
	return c, nil
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
