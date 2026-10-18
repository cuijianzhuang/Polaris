package coretest

import (
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
