package agent

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
)

// busyPorts lists the TCP ports in LISTEN state and the bound UDP ports on
// this host (any process), from /proc/net. The panel avoids them when it
// picks a random port for a new inbound.
func busyPorts() (tcp, udp []int) {
	read := func(files []string, listenOnly bool) []int {
		seen := map[int]bool{}
		for _, f := range files {
			fh, err := os.Open(f)
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(fh)
			sc.Scan() // header
			for sc.Scan() {
				fs := strings.Fields(sc.Text())
				if len(fs) < 4 {
					continue
				}
				if listenOnly && fs[3] != "0A" { // TCP_LISTEN
					continue
				}
				i := strings.LastIndexByte(fs[1], ':')
				if i < 0 {
					continue
				}
				if p, err := strconv.ParseInt(fs[1][i+1:], 16, 32); err == nil && p > 0 {
					seen[int(p)] = true
				}
			}
			fh.Close()
		}
		out := make([]int, 0, len(seen))
		for p := range seen {
			out = append(out, p)
		}
		sort.Ints(out)
		return out
	}
	return read([]string{"/proc/net/tcp", "/proc/net/tcp6"}, true), read([]string{"/proc/net/udp", "/proc/net/udp6"}, false)
}
