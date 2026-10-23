//go:build linux

package core

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helper is the owner process, equivalent to the agent. Keep it alive
// after Start's goroutine returns to expose parent-thread lifetime mistakes.
func TestParentBoundOwnerHelper(t *testing.T) {
	path := os.Getenv("POLARIS_PARENT_BOUND_PID")
	if path == "" {
		return
	}
	bin := os.Getenv("POLARIS_PARENT_BOUND_BIN")
	args := []string{"30"}
	if cfg := os.Getenv("POLARIS_PARENT_BOUND_CONFIG"); cfg != "" {
		args = []string{"run", "-c", cfg}
	}
	p := NewProcess("owned", bin, filepath.Dir(path), slog.New(slog.NewTextHandler(io.Discard, nil)), args...)
	p.BindToParent = true
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(p.PID())), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
}

func TestParentBoundChildDiesWithOwner(t *testing.T) {
	for _, kind := range []string{"sleep", "xray"} {
		t.Run(kind, func(t *testing.T) {
			bin := "/bin/sleep"
			config := ""
			dir := t.TempDir()
			if kind == "xray" {
				bin = os.Getenv("POLARIS_XRAY_BIN")
				if bin == "" {
					t.Skip("POLARIS_XRAY_BIN not set")
				}
				config = filepath.Join(dir, "native.json")
				if err := os.WriteFile(config, []byte(`{"log":{"loglevel":"none"},"outbounds":[{"protocol":"freedom"}]}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			pidfile := filepath.Join(dir, "pid")
			owner := exec.Command(exe, "-test.run=^TestParentBoundOwnerHelper$")
			owner.Env = append(os.Environ(), "POLARIS_PARENT_BOUND_PID="+pidfile, "POLARIS_PARENT_BOUND_BIN="+bin, "POLARIS_PARENT_BOUND_CONFIG="+config)
			if err = owner.Start(); err != nil {
				t.Fatal(err)
			}
			defer owner.Process.Kill()
			var pid int
			for until := time.Now().Add(5 * time.Second); time.Now().Before(until); {
				b, _ := os.ReadFile(pidfile)
				pid, _ = strconv.Atoi(string(b))
				if pid > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if pid <= 0 {
				t.Fatal("owner did not publish child pid")
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
			// The starting thread must still exist even after many GCs.
			time.Sleep(100 * time.Millisecond)
			if err = syscall.Kill(pid, 0); err != nil {
				t.Fatal("child died while owner alive", err)
			}
			owner.Process.Kill()
			owner.Wait()
			for until := time.Now().Add(2 * time.Second); time.Now().Before(until); {
				b, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
				if e != nil || strings.Contains(string(b), ") Z ") {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("child survived SIGKILL of its owner")
		})
	}
}
