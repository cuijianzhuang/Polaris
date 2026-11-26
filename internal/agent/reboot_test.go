package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
)

// rebootRecorder is a fake hostRebootRun: it records every invocation and
// signals each one on a channel, so tests observe the delayed execution
// without sleeping or rebooting anything.
type rebootRecorder struct {
	mu    sync.Mutex
	calls [][]string
	errs  []error // per-call results; calls beyond the list succeed
	done  chan []string
}

func (r *rebootRecorder) run(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	r.mu.Lock()
	r.calls = append(r.calls, call)
	var err error
	if len(r.calls) <= len(r.errs) {
		err = r.errs[len(r.calls)-1]
	}
	r.mu.Unlock()
	r.done <- call
	return "", err
}

func (r *rebootRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func swapReboot(t *testing.T, rec *rebootRecorder, delay time.Duration) {
	t.Helper()
	oldRun, oldDelay := hostRebootRun, hostRebootDelay
	hostRebootRun, hostRebootDelay = rec.run, delay
	t.Cleanup(func() { hostRebootRun, hostRebootDelay = oldRun, oldDelay })
}

func TestRebootHostAcksBeforeExecuting(t *testing.T) {
	rec := &rebootRecorder{done: make(chan []string, 4)}
	swapReboot(t, rec, 150*time.Millisecond)
	a := &Agent{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	start := time.Now()
	a.handleCommand(context.Background(), &polarisv1.Command{Id: "c1", Kind: "reboot_host"})
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("handleCommand blocked for %v; the ack must not wait for the reboot", took)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("reboot ran %d times before the delay elapsed", n)
	}
	select {
	case call := <-rec.done:
		if len(call) != 2 || call[0] != "systemctl" || call[1] != "reboot" {
			t.Fatalf("first reboot attempt = %v, want [systemctl reboot]", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled reboot never executed")
	}
	// Exactly one command once the first attempt succeeds.
	time.Sleep(50 * time.Millisecond)
	if n := rec.count(); n != 1 {
		t.Fatalf("reboot commands executed %d times, want exactly 1", n)
	}
}

func TestRunHostRebootFallbackChain(t *testing.T) {
	rec := &rebootRecorder{done: make(chan []string, 4), errs: []error{errors.New("no systemctl"), nil}}
	old := hostRebootRun
	hostRebootRun = rec.run
	defer func() { hostRebootRun = old }()

	runHostReboot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n := rec.count(); n != 2 {
		t.Fatalf("fallback chain ran %d commands, want 2 (systemctl fails, reboot succeeds)", n)
	}
	rec.mu.Lock()
	first, second := rec.calls[0], rec.calls[1]
	rec.mu.Unlock()
	if first[0] != "systemctl" || second[0] != "reboot" {
		t.Fatalf("fallback order = %v then %v, want systemctl then reboot", first, second)
	}
}
