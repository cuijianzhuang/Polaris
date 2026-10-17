package panel

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{DataDir: dir, SecretKey: "x", AdminPath: "/"}
	cfg.DB.DSN = filepath.Join(dir, "p.db")
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
