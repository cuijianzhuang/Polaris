package singbox

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core"
)

// A signal can be accepted while a child is still running its old config.
// A reload must replace the child, and must surface a failed replacement.
func TestReloadAcknowledgesReplacementAndStartupFailure(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "core")
	script := "#!/bin/sh\nif [ -f fail ]; then exit 17; fi\ntrap 'touch hup; while :; do sleep 1; done' HUP\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := New(core.Options{WorkDir: dir})
	a.proc = core.NewProcess("fake-singbox", bin, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	t.Cleanup(func() { _ = a.Stop(ctx) })
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	first := a.proc.PID()
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if a.proc.PID() == first {
		t.Fatal("reload acknowledged a signal without replacing the old process")
	}
	if _, err := os.Stat(filepath.Join(dir, "hup")); !os.IsNotExist(err) {
		t.Fatal("asynchronous reload path used")
	}
	if err := os.WriteFile(filepath.Join(dir, "fail"), []byte("fail next startup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(ctx); err == nil {
		t.Fatal("replacement startup failure acknowledged as success")
	}
	if a.proc.Running() {
		t.Fatal("failed replacement left an old process serving stale users")
	}
	if err := os.Remove(filepath.Join(dir, "fail")); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(ctx); err != nil {
		t.Fatal("could not restore previous config", err)
	}
}
