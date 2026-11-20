package panel

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
)

// Watchdog suppression for planned sync upgrades (design
// docs/design/ha-sync-upgrade.md §4.3). Before the primary restarts for
// its stage of a sync upgrade, the standby's panel drops this file in its
// own data directory; the cf-failover.sh watchdog reads it and skips its
// flip decision while it is valid. The panel only ever writes its own
// data directory — the same boundary as update-request — and the root
// watchdog only reads.

// failoverSuppressTTL is the hard cap on a suppression window, counted
// from the moment the file is written. It is deliberately not
// configurable and never extended: if the primary really died during
// its upgrade, the watchdog resumes its normal rules once this passes.
const failoverSuppressTTL = 30 * time.Minute

func (s *Server) failoverSuppressPath() string {
	return filepath.Join(s.cfg.DataDir, "failover-suppress")
}

// writeFailoverSuppress drops the suppression file for sync upgrade op
// opID. An existing, still-valid file for the same op keeps its original
// expiry: a retried delivery must not extend the window.
func (s *Server) writeFailoverSuppress(opID string) error {
	if raw, err := os.ReadFile(s.failoverSuppressPath()); err == nil {
		if until, op, ok := parseFailoverSuppress(string(raw)); ok && op == opID && time.Now().Before(until) {
			return nil
		}
	}
	body := fmt.Sprintf("until=%d\nop=%s\n", time.Now().Add(failoverSuppressTTL).Unix(), opID)
	return core.AtomicWriteDurable(s.failoverSuppressPath(), []byte(body), 0o644)
}

// clearFailoverSuppress removes the suppression file. A missing file is
// not an error: the hard expiry covers a file that is never removed.
func (s *Server) clearFailoverSuppress() error {
	if err := os.Remove(s.failoverSuppressPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// parseFailoverSuppress reads the two-line format the watchdog script
// consumes: until=<unix seconds>, op=<sync upgrade op id>.
func parseFailoverSuppress(body string) (until time.Time, op string, ok bool) {
	for _, line := range strings.Split(body, "\n") {
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(k) {
		case "until":
			sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, "", false
			}
			until = time.Unix(sec, 0)
		case "op":
			op = strings.TrimSpace(v)
		}
	}
	if until.IsZero() {
		return time.Time{}, "", false
	}
	return until, op, true
}
