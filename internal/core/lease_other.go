//go:build !linux

package core

import (
	"fmt"
	"os/exec"
	"time"
)

func leasedCommand(string, []string, time.Time) (*exec.Cmd, error) {
	return nil, fmt.Errorf("independent core leases require Linux")
}
