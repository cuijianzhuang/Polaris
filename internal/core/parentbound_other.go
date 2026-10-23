//go:build !linux

package core

import (
	"fmt"
	"os/exec"
)

func ParentBoundSupported() bool { return false }

func startParentBound(*exec.Cmd) (func() error, error) {
	return nil, fmt.Errorf("parent-bound processes require Linux")
}
