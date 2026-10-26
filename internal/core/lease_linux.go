//go:build linux

package core

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

const leaseMode = "--polaris-internal-core-lease-v1"

// The same executable acts as a tiny supervisor before main runs. It creates
// the core only after arming a local deadline. The Agent's scheduling is not
// involved in expiry, and the child shares the supervisor's process group.
func init() {
	if len(os.Args) < 2 || os.Args[1] != leaseMode {
		return
	}
	if len(os.Args) < 5 {
		os.Exit(125)
	}
	nanos, err := strconv.ParseInt(os.Args[2], 10, 64)
	deadline := time.Unix(0, nanos)
	if err != nil || !deadline.After(time.Now()) || time.Until(deadline) > 30*time.Second {
		os.Exit(125)
	}
	runtime.LockOSThread() // PDEATHSIG belongs to the thread creating the core.
	cmd := exec.Command(os.Args[3], os.Args[4:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	timer := time.NewTimer(time.Until(deadline))
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "leased core start failed")
		os.Exit(125)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-timer.C:
		case <-signals:
		case <-done:
			return
		}
		_ = cmd.Process.Kill()
	}()
	err = cmd.Wait()
	close(done)
	timer.Stop()
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func leasedCommand(bin string, args []string, deadline time.Time) (*exec.Cmd, error) {
	if !deadline.After(time.Now()) || time.Until(deadline) > 30*time.Second {
		return nil, fmt.Errorf("invalid core lease deadline")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	argv := append([]string{leaseMode, strconv.FormatInt(deadline.UnixNano(), 10), bin}, args...)
	return exec.Command(exe, argv...), nil
}
