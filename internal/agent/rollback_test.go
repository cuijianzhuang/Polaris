package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func copyFileT(t *testing.T, src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestCoreUpgradeRollback runs a real Xray, then switches to a "release"
// that passes config validation but dies on start. The agent must report the
// failure and be back on the old version, serving the old config.
func TestCoreUpgradeRollback(t *testing.T) {
	bin := os.Getenv("POLARIS_XRAY_BIN")
	if bin == "" {
		t.Skip("POLARIS_XRAY_BIN not set")
	}
	dir := t.TempDir()
	inst := &Installer{Dir: filepath.Join(dir, "cores")}
	good, bad := "26.3.27", "99.0.0"
	copyFileT(t, bin, inst.BinPath(model.CoreXray, good))
	for _, f := range []string{"geoip.dat", "geosite.dat"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(bin), f)); err == nil {
			copyFileT(t, filepath.Join(filepath.Dir(bin), f), filepath.Join(filepath.Dir(inst.BinPath(model.CoreXray, good)), f))
		}
	}
	broken := inst.BinPath(model.CoreXray, bad)
	os.MkdirAll(filepath.Dir(broken), 0o755)
	// Accepts "run -test" (validation) and "version", crashes when actually run.
	os.WriteFile(broken, []byte("#!/bin/sh\ncase \"$*\" in *-test*|*version*) exit 0;; esac\necho 'boom' >&2\nexit 1\n"), 0o755)

	a := &Agent{cfg: Config{DataDir: dir, APIPortBase: freePortT(t), AuthListen: "127.0.0.1:0"},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), installer: inst, cores: map[model.CoreType]*managed{}}
	in := coretest.Inbound(model.CoreXray, "vless", freePortT(t), model.ProtocolSpec{Type: model.ProtoVLESS}, model.TransportSpec{}, model.SecuritySpec{})
	in.Listen = "127.0.0.1"
	spec := func(rev int64, ver string) *model.NodeSpec {
		return &model.NodeSpec{NodeID: "n1", Rev: rev, Cores: []model.CoreSpec{{Type: model.CoreXray, Version: ver}}, Inbounds: []model.Inbound{in}}
	}
	ctx := context.Background()
	defer a.shutdown()
	if r := a.Apply(ctx, spec(1, good)); !r.ok {
		t.Fatalf("initial apply: %v", r.err)
	}
	r := a.Apply(ctx, spec(2, bad))
	if r.ok || !r.rolledBack || !strings.Contains(r.err.Error(), "已回退到 "+good) {
		t.Fatalf("upgrade to a broken release: ok=%v rolledBack=%v err=%v", r.ok, r.rolledBack, r.err)
	}
	m := a.cores[model.CoreXray]
	if m.spec.Version != good || !m.adapter.Health(ctx).Running {
		t.Fatalf("not back on %s and running: %s running=%v", good, m.spec.Version, m.adapter.Health(ctx).Running)
	}
	if target, _ := os.Readlink(filepath.Join(inst.Dir, "xray", "current")); target != good {
		t.Fatalf("current -> %q", target)
	}
	if a.current.Rev != 1 {
		t.Fatalf("applied spec should stay at rev 1, got %d", a.current.Rev)
	}
	// A later good spec on the old version still applies normally.
	if r := a.Apply(ctx, spec(3, good)); !r.ok {
		t.Fatalf("apply after rollback: %v", r.err)
	}
}
