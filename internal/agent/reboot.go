package agent

import (
	"context"
	"log/slog"
	"time"
)

// hostRebootDelay is the pause between acking a reboot_host command and
// actually rebooting the host: the ack has to reach the panel first.
// A var so tests can shrink it instead of waiting out the real delay.
var hostRebootDelay = 3 * time.Second

// hostRebootRun executes one reboot command; a var so tests record the
// calls instead of rebooting the test machine.
var hostRebootRun = execRunner

// hostRebootCommands is the fallback chain, most graceful first: systemd
// hosts take the first entry, minimal images fall through to the plain
// reboot / shutdown binaries.
var hostRebootCommands = [][]string{
	{"systemctl", "reboot"},
	{"reboot"},
	{"shutdown", "-r", "now"},
}

// scheduleHostReboot arranges a host reboot from a separate goroutine so
// the command handler can answer at once. The ack therefore means only
// "scheduled", never "rebooted": the host goes down hostRebootDelay
// later, and the panel learns the reboot really happened from the node
// dropping offline and reconnecting.
func (a *Agent) scheduleHostReboot() {
	a.log.Info("host reboot scheduled", "delay", hostRebootDelay)
	go func() {
		time.Sleep(hostRebootDelay)
		runHostReboot(context.Background(), a.log)
	}()
}

// runHostReboot walks the fallback chain until one command succeeds.
// The agent runs as root under systemd, so no sudo is needed (or wanted).
func runHostReboot(ctx context.Context, log *slog.Logger) {
	for _, cmd := range hostRebootCommands {
		if _, err := hostRebootRun(ctx, cmd[0], cmd[1:]...); err == nil {
			return
		} else {
			log.Warn("host reboot command failed, trying next", "cmd", cmd[0], "err", err)
		}
	}
	log.Error("all host reboot commands failed; host not rebooted")
}
