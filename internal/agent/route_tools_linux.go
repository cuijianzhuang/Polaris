//go:build linux

package agent

import (
	"os/exec"
	"syscall"
)

// Cancel the package manager and its child tools together when the Agent loses
// its stream or the operation expires. Never remove package-manager lock files.
func cancelRouteToolProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
