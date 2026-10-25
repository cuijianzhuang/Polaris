//go:build !linux

package agent

import "os/exec"

func cancelRouteToolProcess(cmd *exec.Cmd) {}
