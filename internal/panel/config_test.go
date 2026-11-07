package panel

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigTimezone(t *testing.T) {
	// Default: server local time.
	c := &Config{DataDir: t.TempDir(), SecretKey: "0123456789abcdef"}
	if got := c.Location(); got != time.Local {
		t.Fatalf("default location = %v, want %v", got, time.Local)
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("secret_key: 0123456789abcdef\ntimezone: Asia/Shanghai\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Location().String() != "Asia/Shanghai" {
		t.Fatalf("location = %v, want Asia/Shanghai", got.Location())
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("secret_key: 0123456789abcdef\ntimezone: Not/AZone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(bad); err == nil {
		t.Fatal("invalid timezone accepted")
	}
}
