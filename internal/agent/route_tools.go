package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Agent-local state, not panel-local: concurrent requests and panel replicas
// cannot start multiple package managers on one node. No installer is invoked
// by status checks, probes, or schedules.
var routeTools = newRouteToolManager()

type routeToolManager struct {
	mu     sync.Mutex
	look   func(string) (string, error)
	run    runner
	root   func() bool
	linux  bool
	job    string
	output string
	issue  string
}

func newRouteToolManager() *routeToolManager {
	return &routeToolManager{look: routeToolLookPath, run: runRouteToolCommand, root: func() bool { return os.Geteuid() == 0 }, linux: runtime.GOOS == "linux"}
}

// systemd/container PATH may omit sbin even when the package is installed.
// Only fixed system directories are considered; no working-directory fallback.
func routeToolLookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err == nil {
		return path, nil
	}
	for _, dir := range []string{"/usr/bin", "/usr/sbin", "/bin", "/sbin"} {
		if path, e := exec.LookPath(dir + "/" + name); e == nil {
			return path, nil
		}
	}
	return "", err
}

func runRouteToolCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive")
	cmd.WaitDelay = time.Second
	cancelRouteToolProcess(cmd)
	var out routeToolOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// Keep the end of package-manager logs, where the actual failure is usually
// printed. Bound memory even if an external command produces unlimited output.
type routeToolOutput struct{ value []byte }

func (b *routeToolOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= 16*1024 {
		b.value = append(b.value[:0], p[len(p)-16*1024:]...)
	} else {
		drop := len(b.value) + len(p) - 16*1024
		if drop > 0 {
			copy(b.value, b.value[drop:])
			b.value = b.value[:len(b.value)-drop]
		}
		b.value = append(b.value, p...)
	}
	return n, nil
}
func (b *routeToolOutput) String() string { return string(b.value) }

func (m *routeToolManager) packageManager() (string, string, [][]string) {
	for _, name := range []string{"apt-get", "dnf", "yum", "apk"} {
		if path, err := m.look(name); err == nil {
			switch name {
			case "apt-get":
				return path, "apt-get update && apt-get install -y traceroute", [][]string{{"update", "-o", "Acquire::Retries=0", "-o", "Acquire::http::Timeout=20", "-o", "Acquire::https::Timeout=20"}, {"install", "-y", "--no-install-recommends", "traceroute"}}
			case "dnf", "yum":
				return path, name + " install -y traceroute", [][]string{{"install", "-y", "traceroute"}}
			case "apk":
				return path, "apk add --no-cache traceroute", [][]string{{"add", "--no-cache", "traceroute"}}
			}
		}
	}
	return "", "", nil
}

func (m *routeToolManager) statusLocked(ctx context.Context) model.RouteTraceToolStatus {
	s := model.RouteTraceToolStatus{State: "unsupported", CheckedAt: time.Now().UTC(), InstallState: m.job, InstallOutput: m.output, InstallIssue: m.issue}
	if !m.linux {
		s.Issue = "linux_required"
		return s
	}
	if m.job == "installing" {
		s.State = "checking"
		return s
	}
	manager, manual, _ := m.packageManager()
	s.Manager, s.ManualCommand = manager, manual
	tool, err := m.look("traceroute")
	if err != nil {
		s.State, s.Issue = "missing", "tool_missing"
		if errors.Is(err, os.ErrPermission) {
			s.State, s.Issue = "unusable", "permission_denied"
		}
	} else {
		s.Path = tool
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		// Fixed loopback self-test (IPv6 fallback on IPv6-only hosts) detects executable/argument/permission errors
		// without using a public target or claiming IPv6/Internet reachability.
		out, runErr := m.run(checkCtx, tool, "-4", "-n", "-q", "1", "-w", "1", "-m", "1", "127.0.0.1")
		firstIssue := routeTraceIssue(out, runErr, checkCtx.Err() != nil, false, nil)
		if firstIssue == "address_family_unavailable" || firstIssue == "network_unreachable" {
			out, runErr = m.run(checkCtx, tool, "-6", "-n", "-q", "1", "-w", "1", "-m", "1", "::1")
		}
		hops, _ := parseRouteTrace(out, "127.0.0.1")
		if runErr == nil && checkCtx.Err() == nil && len(hops) > 0 {
			s.State = "ready"
		} else {
			s.State = "unusable"
			s.CheckOutput = out[:min(len(out), 4096)]
			s.Issue = routeTraceIssue(out, runErr, checkCtx.Err() != nil, false, nil)
			if runErr == nil && checkCtx.Err() == nil {
				s.Issue = "incompatible_tool"
			}
		}
		cancel()
	}
	s.CanInstall = (s.State == "missing" || s.Issue == "incompatible_tool") && manager != "" && m.root()
	if s.State == "missing" && manager == "" {
		s.Issue = "package_manager_unavailable"
	}
	if s.State == "missing" && manager != "" && !m.root() {
		s.Issue = "root_required"
	}
	return s
}

func (m *routeToolManager) status(ctx context.Context) model.RouteTraceToolStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked(ctx)
}

// Installation runs under the Agent stream lifetime with a hard limit. The
// command immediately returns a snapshot; HTTP timeouts never trigger a second
// installer. The UI polls status and can recover it after closing/reopening.
func (m *routeToolManager) install(ctx context.Context) model.RouteTraceToolStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.statusLocked(ctx)
	if !s.CanInstall {
		return s
	}
	manager, _, steps := m.packageManager()
	m.job, m.output, m.issue = "installing", "", ""
	go func() {
		installCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		var output routeToolOutput
		var err error
		for _, args := range steps {
			var out string
			out, err = m.run(installCtx, manager, args...)
			_, _ = output.Write([]byte(out))
			if err != nil || installCtx.Err() != nil {
				break
			}
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.output = output.String()
		m.job = "failed"
		switch {
		case installCtx.Err() != nil:
			m.issue = "install_interrupted"
		case err != nil:
			m.issue = "install_failed"
		default:
			// An exit code of zero is insufficient: verify the installed executable.
			m.job = "" // permit the self-test rather than returning installing
			checked := m.statusLocked(installCtx)
			if checked.State == "ready" {
				m.job = "succeeded"
				m.issue = ""
			} else {
				m.job = "failed"
				m.issue = "install_verification_failed"
			}
		}
	}()
	s.State, s.InstallState, s.CanInstall = "checking", "installing", false
	s.InstallIssue, s.InstallOutput = "", ""
	return s
}

func routeToolCommand(ctx context.Context, op string) (string, error) {
	var status model.RouteTraceToolStatus
	switch op {
	case "status":
		status = routeTools.status(ctx)
	case "install":
		status = routeTools.install(ctx)
	default:
		return "", errors.New("unsupported route tool operation")
	}
	raw, err := json.Marshal(status)
	return string(raw), err
}
