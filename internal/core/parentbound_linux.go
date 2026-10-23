//go:build linux

package core

import (
	"os/exec"
	"runtime"
	"syscall"
)

func ParentBoundSupported() bool { return true }

func startParentBound(cmd *exec.Cmd) (func() error, error) {
	started := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
		err := cmd.Start()
		started <- err
		if err == nil {
			done <- cmd.Wait()
		}
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return func() error { return <-done }, nil
}
