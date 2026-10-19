package panel

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{DataDir: dir, SecretKey: "x", AdminPath: "/"}
	cfg.DB.DSN = filepath.Join(dir, "p.db")
	if os.Getenv("POLARIS_TEST_ALL_POSTGRES") != "" {
		// Run the whole suite on PostgreSQL (POLARIS_TEST_POSTGRES_DSN).
		cfg.DB.DSN = pgTestSchema(t, os.Getenv("POLARIS_TEST_POSTGRES_DSN"))
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if db, err := s.st.DB.DB(); err == nil {
			db.Close()
		}
	})
	return s
}
