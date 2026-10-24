package singbox

import (
	"context"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

// sing-box names no user in its APIs, but at log level info it prints, per
// connection, where it came from and (on a second line, same connection id)
// which user it belongs to:
//
//	[3404549370 0ms] inbound/mixed[in]: inbound connection from 1.2.3.4:5678
//	[3404549370 1ms] inbound/mixed[in]: [alice] inbound connection to example.com:443
//
// Pairing the two gives user and source IP. Those lines (and the matching
// outbound ones) are consumed here instead of filling the log buffer.
var (
	ansiRe       = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	fromRe       = regexp.MustCompile(`\[(\d+) [^\]]*\] inbound/[^:\s]+: inbound (?:packet )?connection from (\S+)\s*$`)
	userRe       = regexp.MustCompile(`\[(\d+) [^\]]*\] inbound/[^:\s]+: \[([^\]]+)\] inbound (?:packet )?connection (?:to|from) `)
	singleFromRe = regexp.MustCompile(`\[(\d+) [^\]]*\] inbound/snell\[([^\]]+)\]: inbound connection from (\S+)\s*$`)
	singleUserRe = regexp.MustCompile(`\[(\d+) [^\]]*\] inbound/snell\[([^\]]+)\]: inbound (?:connection to|packet connection from) `)
	outboundRe   = regexp.MustCompile(`\] outbound/[^:\s]+: outbound (?:packet )?connection to `)
)

// onlineWindow is how long an IP counts as online after its last new
// connection; it tracks who is actively using the node, not open sockets.
const onlineWindow = 60 * time.Second

// maxPending bounds connections waiting for their user line.
const maxPending = 20000

type onlineTracker struct {
	singleOwners map[string]string
	mu           sync.Mutex
	pending      map[string]string               // connection id -> source IP
	seen         map[string]map[string]time.Time // user -> ip -> last new connection
}

func newOnlineTracker() *onlineTracker {
	return &onlineTracker{pending: map[string]string{}, seen: map[string]map[string]time.Time{}}
}

// line handles one output line; it reports true when the line is one of the
// per-connection ones that are not worth keeping.
func (t *onlineTracker) line(raw string) bool {
	if !strings.Contains(raw, "connection") {
		return false
	}
	s := ansiRe.ReplaceAllString(raw, "")
	if m := singleUserRe.FindStringSubmatch(s); m != nil {
		t.mu.Lock()
		if owner := t.singleOwners[m[2]]; owner != "" {
			if ip, ok := t.pending[m[1]]; ok {
				delete(t.pending, m[1])
				if t.seen[owner] == nil {
					t.seen[owner] = map[string]time.Time{}
				}
				t.seen[owner][ip] = time.Now()
			}
		}
		t.mu.Unlock()
		return true
	}
	if m := singleFromRe.FindStringSubmatch(s); m != nil {
		t.mu.Lock()
		owner := t.singleOwners[m[2]]
		if owner != "" {
			host, _, err := net.SplitHostPort(m[3])
			if err != nil {
				host = m[3]
			}
			if len(t.pending) >= maxPending {
				t.pending = map[string]string{}
			}
			t.pending[m[1]] = host
		}
		t.mu.Unlock()
		if owner != "" {
			return true
		}
	}
	if m := fromRe.FindStringSubmatch(s); m != nil {
		host, _, err := net.SplitHostPort(m[2])
		if err != nil {
			host = m[2]
		}
		t.mu.Lock()
		if len(t.pending) >= maxPending {
			t.pending = map[string]string{} // lines lost or never paired: start over
		}
		t.pending[m[1]] = host
		t.mu.Unlock()
		return true
	}
	if m := userRe.FindStringSubmatch(s); m != nil {
		t.mu.Lock()
		if ip, ok := t.pending[m[1]]; ok {
			delete(t.pending, m[1])
			if t.seen[m[2]] == nil {
				t.seen[m[2]] = map[string]time.Time{}
			}
			t.seen[m[2]][ip] = time.Now()
		}
		t.mu.Unlock()
		return true
	}
	return outboundRe.MatchString(s)
}

func (t *onlineTracker) online(now time.Time) map[string][]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string][]string{}
	for u, ips := range t.seen {
		for ip, at := range ips {
			if now.Sub(at) >= onlineWindow {
				delete(ips, ip)
				continue
			}
			out[u] = append(out[u], ip)
		}
		if len(ips) == 0 {
			delete(t.seen, u)
		}
	}
	return out
}

func (a *Adapter) OnlineIPs(context.Context) (map[string][]string, error) {
	return a.online.online(time.Now()), nil
}
