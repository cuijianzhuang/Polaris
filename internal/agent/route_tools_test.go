package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestRouteToolReadinessAndInstallGuards(t *testing.T) {
	for _, tc := range []struct {
		name, tool, manager, output, issue string
		root, linux                        bool
		runErr                             error
		state                              string
		can                                bool
	}{
		{name: "ready", output: " 1 127.0.0.1 0.1 ms", tool: "/usr/bin/traceroute", manager: "apt-get", root: true, linux: true, state: "ready"},
		{name: "missing", manager: "apt-get", root: true, linux: true, state: "missing", issue: "tool_missing", can: true},
		{name: "nonroot", manager: "apk", linux: true, state: "missing", issue: "root_required"},
		{name: "no manager", root: true, linux: true, state: "missing", issue: "package_manager_unavailable"},
		{name: "not linux", tool: "traceroute", manager: "apt-get", root: true, state: "unsupported", issue: "linux_required"},
		{name: "permission", tool: "traceroute", manager: "apt-get", root: true, linux: true, state: "unusable", issue: "permission_denied", runErr: os.ErrPermission},
		{name: "busybox arguments", tool: "traceroute", manager: "apk", root: true, linux: true, state: "unusable", issue: "incompatible_tool", runErr: errors.New("exit status 1"), output: "traceroute: invalid option -- '4'", can: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newRouteToolManager()
			m.linux = tc.linux
			m.root = func() bool { return tc.root }
			m.look = func(name string) (string, error) {
				if name == "traceroute" && tc.tool != "" {
					return tc.tool, nil
				}
				if name == tc.manager {
					return "/usr/bin/" + name, nil
				}
				return "", exec.ErrNotFound
			}
			var installs atomic.Int32
			m.run = func(ctx context.Context, name string, args ...string) (string, error) {
				if name == tc.tool {
					return tc.output, tc.runErr
				}
				installs.Add(1)
				return "", errors.New("guard failed")
			}
			s := m.status(t.Context())
			if s.State != tc.state || s.Issue != tc.issue || s.CanInstall != tc.can {
				t.Fatalf("%+v", s)
			}
			if !tc.can {
				s = m.install(t.Context())
				if s.InstallState == "installing" {
					t.Fatal(s)
				}
			}
			if installs.Load() != 0 {
				t.Fatal("readiness check or denied install ran a package manager")
			}
		})
	}
}

func TestRouteToolInstallAsyncDeduplicatedAndVerified(t *testing.T) {
	for _, manager := range []string{"apt-get", "dnf", "yum", "apk"} {
		t.Run(manager, func(t *testing.T) {
			m := newRouteToolManager()
			m.linux = true
			m.root = func() bool { return true }
			var installed atomic.Bool
			m.look = func(n string) (string, error) {
				if n == "traceroute" && installed.Load() {
					return "trace-fixture", nil
				}
				if n == manager {
					return "manager-fixture", nil
				}
				return "", exec.ErrNotFound
			}
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var callsMu sync.Mutex
			var calls [][]string
			var once sync.Once
			m.run = func(ctx context.Context, name string, args ...string) (string, error) {
				if name == "trace-fixture" {
					return " 1 127.0.0.1 0.1 ms", nil
				}
				callsMu.Lock()
				calls = append(calls, append([]string{}, args...))
				callsMu.Unlock()
				once.Do(func() {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
					}
				})
				if args[0] != "update" {
					installed.Store(true)
				}
				return strings.Repeat("output", 5000), nil
			}
			before := time.Now()
			s := m.install(t.Context())
			if s.InstallState != "installing" || time.Since(before) > time.Second {
				t.Fatal(s)
			}
			<-started
			for range 8 {
				if x := m.install(t.Context()); x.InstallState != "installing" {
					t.Fatal(x)
				}
			}
			if s = m.status(t.Context()); s.State != "checking" || s.CanInstall {
				t.Fatal(s)
			}
			close(release)
			go func() {
				for {
					s := m.status(t.Context())
					if s.InstallState != "installing" {
						close(done)
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("install stuck")
			}
			s = m.status(t.Context())
			if s.State != "ready" || s.InstallState != "succeeded" || len(s.InstallOutput) > 16*1024 {
				t.Fatal(s)
			}
			callsMu.Lock()
			defer callsMu.Unlock()
			switch manager {
			case "apt-get":
				if len(calls) != 2 || calls[0][0] != "update" || !reflect.DeepEqual(calls[1], []string{"install", "-y", "--no-install-recommends", "traceroute"}) {
					t.Fatal(calls)
				}
			case "apk":
				if !reflect.DeepEqual(calls, [][]string{{"add", "--no-cache", "traceroute"}}) {
					t.Fatal(calls)
				}
			default:
				if !reflect.DeepEqual(calls, [][]string{{"install", "-y", "traceroute"}}) {
					t.Fatal(calls)
				}
			}
			// A retry after a successful response does not reinstall an existing tool.
			if x := m.install(t.Context()); x.InstallState != "succeeded" {
				t.Fatal(x)
			}
		})
	}
}

func TestRouteToolInstallFailureAndVerification(t *testing.T) {
	for _, mode := range []string{"repository", "cancel", "zero exit missing binary", "installed unusable"} {
		t.Run(mode, func(t *testing.T) {
			m := newRouteToolManager()
			m.linux = true
			m.root = func() bool { return true }
			var installed atomic.Bool
			m.look = func(n string) (string, error) {
				if n == "apk" {
					return "apk", nil
				}
				if n == "traceroute" && installed.Load() {
					return "traceroute", nil
				}
				return "", exec.ErrNotFound
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m.run = func(c context.Context, n string, args ...string) (string, error) {
				if n == "traceroute" {
					return "Operation not permitted", errors.New("exit 1")
				}
				switch mode {
				case "repository":
					return "repository unavailable", errors.New("exit 1")
				case "cancel":
					cancel()
					return "interrupted", c.Err()
				case "installed unusable":
					installed.Store(true)
				}
				return "install exited zero", nil
			}
			m.install(ctx)
			deadline := time.Now().Add(time.Second)
			var s model.RouteTraceToolStatus
			for {
				s = m.status(t.Context())
				if s.InstallState != "installing" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("stuck")
				}
				time.Sleep(time.Millisecond)
			}
			want := "install_verification_failed"
			if mode == "repository" {
				want = "install_failed"
			}
			if mode == "cancel" {
				want = "install_interrupted"
			}
			if s.InstallState != "failed" || s.InstallIssue != want || s.InstallOutput == "" {
				t.Fatal(s)
			}
		})
	}
}

func TestRouteToolNativeProcessAndBounds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process cancellation")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "tool")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n[ \"$LC_ALL\" = C ] || exit 3\n[ \"$DEBIAN_FRONTEND\" = noninteractive ] || exit 4\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := runRouteToolCommand(t.Context(), script, "one", "literal;$(exit 4)")
	if err != nil || out != "one\nliteral;$(exit 4)\n" {
		t.Fatal(out, err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\n/bin/sleep 30 &\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = runRouteToolCommand(ctx, script)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatal(err, time.Since(start))
	}
}

func TestRouteTraceFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		out, issue string
		err        error
	}{
		{"socket: Operation not permitted", "permission_denied", errors.New("exit 1")},
		{"Address family not supported by protocol", "address_family_unavailable", errors.New("exit 1")},
		{"Cannot assign requested address", "address_family_unavailable", errors.New("exit 1")},
		{"connect: Network is unreachable", "network_unreachable", errors.New("exit 1")},
		{"traceroute: unrecognized option --4", "incompatible_tool", errors.New("exit 1")},
		{" 1 1.1.1.1 1 ms !H", "destination_unreachable", nil},
		{" 1 *\n 2 *", "no_reply", nil},
		{" 1 10.0.0.1 1 ms", "incomplete_path", nil},
		{" 1 1.1.1.1 1 ms", "", nil},
	} {
		t.Run(tc.issue, func(t *testing.T) {
			result, err := collectRouteTrace(t.Context(), model.RouteTraceRequest{IPv4: "1.1.1.1"}, func(context.Context, string, string) (string, error) { return tc.out, tc.err })
			if err != nil || len(result) != 1 || result[0].Issue != tc.issue || result[0].Raw != tc.out {
				t.Fatal(result, err)
			}
		})
	}
}

func TestRouteTraceAllowsSequentialHopBudget(t *testing.T) {
	_, err := collectRouteTrace(t.Context(), model.RouteTraceRequest{IPv4: "1.1.1.1"}, func(ctx context.Context, family, target string) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 20*time.Second {
			t.Error("twenty sequential one-second hops would be cut short", deadline)
		}
		return " 1 1.1.1.1 1 ms", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if model.RouteTraceStaleAfter <= model.RouteTraceRequestTimeout || model.RouteTraceRequestTimeout <= model.RouteTraceProbeTimeout {
		t.Fatal("timeout ordering")
	}
}

// Optional native acceptance: a distribution binary unpacked without installing
// a system package. No public destination or external business traffic is used.
func TestLiveRouteTraceTool(t *testing.T) {
	binary := os.Getenv("POLARIS_TRACEROUTE_BIN")
	if binary == "" {
		t.Skip("set POLARIS_TRACEROUTE_BIN to a native Linux traceroute executable")
	}
	dir := t.TempDir()
	if err := os.Symlink(binary, filepath.Join(dir, "traceroute")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	m := newRouteToolManager()
	checked := m.status(t.Context())
	if checked.State != "ready" || checked.CanInstall {
		t.Fatalf("native self-test failed: %+v", checked)
	}
	raw, err := runRouteTrace(t.Context(), "4", "127.0.0.1")
	if err != nil {
		t.Fatal(raw, err)
	}
	_, state := parseRouteTrace(raw, "127.0.0.1")
	if state != "reached" {
		t.Fatal(state, raw)
	}
	t.Log("native Linux UDP traceroute reached loopback and passed readiness")
}

func TestRouteToolIPv6OnlyAndLogTail(t *testing.T) {
	m := newRouteToolManager()
	m.linux = true
	m.look = func(name string) (string, error) {
		if name == "traceroute" {
			return "traceroute", nil
		}
		return "", exec.ErrNotFound
	}
	m.run = func(ctx context.Context, name string, args ...string) (string, error) {
		if args[0] == "-4" {
			return "Address family not supported by protocol", errors.New("exit 1")
		}
		if args[len(args)-1] != "::1" {
			t.Error(args)
		}
		return " 1 ::1 0.1 ms", nil
	}
	if s := m.status(t.Context()); s.State != "ready" {
		t.Fatal(s)
	}
	var log routeToolOutput
	for range 100 {
		_, _ = log.Write([]byte(strings.Repeat("x", 4000)))
	}
	_, _ = log.Write([]byte("ACTUAL FAILURE AT THE END"))
	if len(log.String()) != 16*1024 || !strings.HasSuffix(log.String(), "ACTUAL FAILURE AT THE END") {
		t.Fatal("bounded log lost final failure")
	}
}

func TestRouteToolRestrictedPATHAndExecutePermission(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux system paths")
	}
	t.Setenv("PATH", t.TempDir())
	path, err := routeToolLookPath("sh")
	if err != nil || !filepath.IsAbs(path) {
		t.Fatal(path, err)
	}
	m := newRouteToolManager()
	m.linux = true
	m.root = func() bool { return true }
	m.look = func(name string) (string, error) {
		if name == "traceroute" {
			return "", os.ErrPermission
		}
		return "", exec.ErrNotFound
	}
	m.run = func(context.Context, string, ...string) (string, error) {
		t.Error("unexecutable tool was run")
		return "", nil
	}
	s := m.status(t.Context())
	if s.State != "unusable" || s.Issue != "permission_denied" || s.CanInstall {
		t.Fatal(s)
	}
}
