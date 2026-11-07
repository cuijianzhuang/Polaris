//go:build !linux

package agent

// busyPorts is Linux-only: it reads /proc/net to list bound TCP/UDP ports.
// On other platforms there is no /proc/net, so report no busy ports. The agent
// runs on Linux in production; this stub keeps `go test ./...` green on macOS
// and other hosts.
func busyPorts() (tcp, udp []int) { return nil, nil }
