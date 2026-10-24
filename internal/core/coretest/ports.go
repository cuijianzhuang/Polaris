package coretest

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A test asks for a port, releases it, and only later does the code under
// test bind it, so the kernel may offer the same port to the next caller. Go
// runs packages as separate processes in parallel, so the register that
// prevents this has to be shared between processes: a file in the temp
// directory, locked while it is read and rewritten.
const portTTL = 10 * time.Minute

var (
	portsMu   sync.Mutex
	handedOut = map[int]bool{}
)

// Claim records port p as in use by a test and reports whether it was free:
// not handed out by this or any other test process in the last ten minutes.
func Claim(p int) bool {
	portsMu.Lock()
	defer portsMu.Unlock()
	if handedOut[p] {
		return false
	}
	ok := claimShared(p)
	if ok {
		handedOut[p] = true
	}
	return ok
}

func claimShared(p int) bool {
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "polaris-test-ports"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return true // no shared register: the in-process one still applies
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX) != nil {
		return true
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	now := time.Now()
	var keep []string
	taken := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		port, ts, ok := strings.Cut(sc.Text(), " ")
		n, err := strconv.ParseInt(ts, 10, 64)
		if !ok || err != nil || now.Sub(time.Unix(0, n)) > portTTL {
			continue
		}
		if port == strconv.Itoa(p) {
			taken = true
		}
		keep = append(keep, sc.Text())
	}
	if !taken {
		keep = append(keep, fmt.Sprintf("%d %d", p, now.UnixNano()))
	}
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(strings.Join(keep, "\n")+"\n"), 0)
	}
	return !taken
}

// FreePort avoids the OS ephemeral range: closing a reservation socket must
// not allow an outgoing connection to steal a future test listener's port.
func FreePort(t testing.TB) int {
	t.Helper()
	return FreePortRange(t, 1)
}

// FreePortRange reserves a contiguous TCP/UDP range, including derived Agent APIs.
func FreePortRange(t testing.TB, span int) int {
	t.Helper()
	if span < 1 || span > 65535-1024 {
		t.Fatal("invalid port span")
	}
	low, high := 49152, 65535 // IANA dynamic range fallback on non-Linux hosts
	if raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
		var l, h int
		if _, err := fmt.Sscanf(string(raw), "%d %d", &l, &h); err == nil && l > 0 && h >= l && h <= 65535 {
			low, high = l, h
		}
	}
	// Keep isolated listeners out of the Agent range pool so many single-port
	// tests cannot fragment all contiguous ranges during repeated suite runs.
	first, limit := 32768, 65536
	if span > 1 && span <= 32 {
		first, limit = 16384, 32768
	}
	if span > 32 {
		first, limit = 1024, 16384
	}
	candidates := (limit - first) / span
	if candidates == 0 {
		t.Fatal("port span exceeds test pool")
	}
	start := rand.IntN(candidates)
	stride := 1
	if span == 1 {
		stride = 7919
	} // coprime to the single-port pool size
	for i := 0; i < candidates; i++ {
		p := first + ((start+i*stride)%candidates)*span
		if p+span-1 > 65535 || (p <= high && p+span-1 >= low) {
			continue
		}
		var sockets []interface{ Close() error }
		ok := true
		for q := p; q < p+span; q++ {
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(q))
			l, err := net.Listen("tcp4", addr)
			if err != nil {
				ok = false
				break
			}
			sockets = append(sockets, l)
			u, err := net.ListenPacket("udp4", addr)
			if err != nil {
				ok = false
				break
			}
			sockets = append(sockets, u)
		}
		if ok {
			for q := p; q < p+span; q++ {
				if !Claim(q) {
					ok = false
					break
				}
			}
		}
		for _, socket := range sockets {
			socket.Close()
		}
		if ok {
			return p
		}
	}
	t.Fatal("no free non-ephemeral test port left")
	return 0
}
