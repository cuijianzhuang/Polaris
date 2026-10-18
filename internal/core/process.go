package core

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Process supervises one core child process. Cores run out-of-process so a
// crash never takes the agent down, versions can be swapped independently and
// GPL-licensed cores stay licence-isolated from the agent.
type Process struct {
	Name string
	Bin  string
	Args []string
	Dir  string
	Env  []string
	Log  *slog.Logger
	// Filter sees every output line first; returning true consumes it, so
	// it is not kept in the log buffer (used for high-volume lines an adapter
	// only needs to count). Set before Start.
	Filter func(line string) bool

	mu        sync.Mutex
	cmd       *exec.Cmd
	done      chan struct{}
	startedAt time.Time
	lastErr   error
	logs      *Ring
}

// NewProcess constructs a supervisor.
func NewProcess(name, bin, dir string, log *slog.Logger, args ...string) *Process {
	return &Process{Name: name, Bin: bin, Args: args, Dir: dir, Log: log, logs: NewRing(500)}
}

// Start launches the process if it is not running.
func (p *Process) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.running() {
		p.mu.Unlock()
		return nil
	}
	if _, err := os.Stat(p.Bin); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("%s binary: %w", p.Name, err)
	}
	cmd := exec.Command(p.Bin, p.Args...)
	cmd.Dir = p.Dir
	if len(p.Env) > 0 {
		cmd.Env = append(os.Environ(), p.Env...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("start %s: %w", p.Name, err)
	}
	done := make(chan struct{})
	p.cmd = cmd
	p.done = done
	p.startedAt = time.Now()
	p.lastErr = nil
	p.mu.Unlock()

	go p.pump(pr)
	go func() {
		err := cmd.Wait()
		pw.Close()
		p.mu.Lock()
		p.lastErr = err
		p.mu.Unlock()
		close(done)
		p.Log.Info("core exited", "core", p.Name, "err", err)
	}()

	// Fail fast if the process dies during startup (bad config, port in use).
	select {
	case <-done:
		return fmt.Errorf("%s exited during startup: %v; last log: %s", p.Name, p.LastError(), p.logs.Tail(5))
	case <-time.After(800 * time.Millisecond):
	case <-ctx.Done():
	}
	return nil
}

func (p *Process) pump(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if p.Filter != nil && p.Filter(line) {
			continue
		}
		p.logs.Add(line)
	}
}

func (p *Process) running() bool {
	if p.cmd == nil || p.done == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Running reports whether the process is alive.
func (p *Process) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running()
}

// Stop terminates the process gracefully, killing it after a timeout.
func (p *Process) Stop(ctx context.Context) error {
	p.mu.Lock()
	if !p.running() {
		p.mu.Unlock()
		return nil
	}
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	return nil
}

// Restart stops then starts the process.
func (p *Process) Restart(ctx context.Context) error {
	if err := p.Stop(ctx); err != nil {
		return err
	}
	return p.Start(ctx)
}

// Signal sends a signal to a running process.
func (p *Process) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running() {
		return fmt.Errorf("%s is not running", p.Name)
	}
	return p.cmd.Process.Signal(sig)
}

// PID returns the process id or 0.
func (p *Process) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running() {
		return 0
	}
	return p.cmd.Process.Pid
}

// StartedAt returns when the process was started.
func (p *Process) StartedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startedAt
}

// LastError returns the last exit error.
func (p *Process) LastError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

// Logs returns the most recent log lines.
func (p *Process) Logs(n int) []string { return p.logs.Last(n) }

// RunCheck runs a one-shot command (e.g. `xray run -test`) and returns its output on failure.
func RunCheck(bin string, args ...string) error {
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("binary %s not found: %w", filepath.Base(bin), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, tail(string(out), 2000))
	}
	return nil
}

// WriteTemp writes cfg to a temp file in dir and returns its path.
func WriteTemp(dir, pattern string, cfg []byte) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(cfg); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// AtomicWrite replaces path with data via rename.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// Ring is a fixed-size line buffer.
type Ring struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

func NewRing(n int) *Ring { return &Ring{lines: make([]string, n)} }

func (r *Ring) Add(s string) {
	r.mu.Lock()
	r.lines[r.next] = s
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

func (r *Ring) Last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []string
	if r.full {
		all = append(all, r.lines[r.next:]...)
	}
	all = append(all, r.lines[:r.next]...)
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

func (r *Ring) Tail(n int) string {
	out := ""
	for _, l := range r.Last(n) {
		out += l + "\n"
	}
	return out
}
