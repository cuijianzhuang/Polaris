package coretest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Ports are released as soon as they are handed out; the kernel may offer the
// same one again, FreePort must not.
func TestFreePortDistinct(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 300; i++ {
		p := FreePort(t)
		if seen[p] {
			t.Fatalf("port %d handed out twice", p)
		}
		seen[p] = true
	}
}

// Also across processes, which is how Go runs the test packages.
func TestFreePortDistinctAcrossProcesses(t *testing.T) {
	const procs, each = 4, 60
	var mu sync.Mutex
	seen := map[int]int{}
	var wg sync.WaitGroup
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperPorts$", "-test.v")
			cmd.Env = append(os.Environ(), "POLARIS_PORT_HELPER=1")
			out, err := cmd.Output()
			if err != nil {
				t.Error(err)
				return
			}
			for _, l := range strings.Split(string(out), "\n") {
				if p, ok := strings.CutPrefix(l, "PORT "); ok {
					n, _ := strconv.Atoi(strings.TrimSpace(p))
					mu.Lock()
					seen[n]++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != procs*each {
		t.Fatalf("expected %d distinct ports, got %d (some handed to two processes)", procs*each, len(seen))
	}
}

// TestHelperPorts is the child of the test above: it prints the ports it got.
func TestHelperPorts(t *testing.T) {
	if os.Getenv("POLARIS_PORT_HELPER") != "1" {
		t.Skip("helper for TestFreePortDistinctAcrossProcesses")
	}
	for i := 0; i < 60; i++ {
		os.Stdout.WriteString("PORT " + strconv.Itoa(FreePort(t)) + "\n")
	}
}

func TestFreePortAvoidsEphemeralRange(t *testing.T) {
	raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Skip("Linux ephemeral range unavailable")
	}
	var low, high int
	if _, err := fmt.Sscanf(string(raw), "%d %d", &low, &high); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		p := FreePort(t)
		if p >= low && p <= high {
			t.Fatalf("listener %d overlaps ephemeral range %d-%d", p, low, high)
		}
		l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
	}
}

func TestFreePortRangeReservesDerivedListeners(t *testing.T) {
	const span = 128
	base := FreePortRange(t, span)
	for p := base; p < base+span; p++ {
		if Claim(p) {
			t.Fatalf("derived port %d was not reserved", p)
		}
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
		l, err := net.Listen("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		u, err := net.ListenPacket("udp4", addr)
		l.Close()
		if err != nil {
			t.Fatal(err)
		}
		u.Close()
	}
}
