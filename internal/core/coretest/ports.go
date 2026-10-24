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
	low, high := 32768, 65535 // conservative fallback on non-Linux hosts
	if raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
		var l, h int
		if _, err := fmt.Sscanf(string(raw), "%d %d", &l, &h); err == nil && l > 0 && h >= l && h <= 65535 {
			low, high = l, h
		}
	}
	start := rand.IntN(65536 - 1024)
	for i := 0; i < 65536-1024; i++ {
		p := 1024 + (start+i)%(65536-1024)
		if p >= low && p <= high {
			continue
		}
		l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			continue
		}
		ok := Claim(p)
		l.Close()
		if ok {
			return p
		}
	}
	t.Fatal("no free non-ephemeral test port left")
	return 0
}
