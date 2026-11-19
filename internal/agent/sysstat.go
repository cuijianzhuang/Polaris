package agent

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
)

// sysSampler computes CPU and NIC rates from successive /proc samples.
type sysSampler struct {
	mu       sync.Mutex
	netEpoch string
	cpuIdle  uint64
	cpuTotal uint64
	rx, tx   uint64
	at       time.Time
}

func (s *sysSampler) Sample() *polarisv1.SystemStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &polarisv1.SystemStats{}
	idle, total := readCPU()
	if s.cpuTotal > 0 && total > s.cpuTotal {
		dt := float64(total - s.cpuTotal)
		out.CpuPercent = round1(100 * (dt - float64(idle-s.cpuIdle)) / dt)
	}
	s.cpuIdle, s.cpuTotal = idle, total
	mem := readMeminfo()
	out.MemPercent = round1(mem.percent())
	netSample, netErr := readNet()
	rx, tx := netSample.rx, netSample.tx
	now := time.Now()
	if netErr == nil && !s.at.IsZero() && netSample.epoch == s.netEpoch && rx >= s.rx && tx >= s.tx {
		sec := now.Sub(s.at).Seconds()
		if sec > 0 {
			out.RxBps = uint64(float64(rx-s.rx) * 8 / sec)
			out.TxBps = uint64(float64(tx-s.tx) * 8 / sec)
		}
	}
	if netErr == nil {
		s.rx, s.tx, s.at, s.netEpoch = rx, tx, now, netSample.epoch
		out.RxBytes, out.TxBytes = rx, tx
		out.NetEpoch = netSample.epoch
		out.NetBootId = netSample.boot
		out.NetCountersValid = true
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			out.UptimeSeconds = uint64(v)
			if netErr == nil && v > 0 {
				out.NetSampleUptimeMillis = uint64(v * 1000)
			}
		}
	}
	if out.NetSampleUptimeMillis == 0 {
		out.NetCountersValid = false
		out.NetEpoch = ""
		out.NetBootId = ""
		out.RxBytes = 0
		out.TxBytes = 0
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		out.Load1, out.Load5, out.Load15 = parseLoadavg(string(b))
	}
	out.MemTotalBytes = mem.totalKB * 1024
	if mem.swapTotalKB > 0 {
		out.SwapPercent = round1(mem.swapPercent())
		out.SwapTotalBytes = mem.swapTotalKB * 1024
	}
	out.Processes = readProcessCount()
	out.TcpConnections = readConnCount("/proc/net/tcp", "/proc/net/tcp6")
	out.UdpConnections = readConnCount("/proc/net/udp", "/proc/net/udp6")
	out.DiskPercent, out.DiskTotalBytes = readDisk("/")
	out.CpuCores = uint32(runtime.NumCPU())
	out.OsName = osName()
	return out
}

// memInfo is the /proc/meminfo figures the sampler reports, in KiB.
type memInfo struct {
	totalKB     uint64
	availKB     uint64
	swapTotalKB uint64
	swapFreeKB  uint64
}

func (m memInfo) percent() float64 {
	if m.totalKB == 0 || m.availKB >= m.totalKB {
		return 0
	}
	return 100 * float64(m.totalKB-m.availKB) / float64(m.totalKB)
}

// swapPercent is 0 when the host has no swap.
func (m memInfo) swapPercent() float64 {
	if m.swapTotalKB == 0 || m.swapFreeKB >= m.swapTotalKB {
		return 0
	}
	return 100 * float64(m.swapTotalKB-m.swapFreeKB) / float64(m.swapTotalKB)
}

func parseMeminfo(r io.Reader) memInfo {
	var m memInfo
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			m.totalKB = v
		case "MemAvailable:":
			m.availKB = v
		case "SwapTotal:":
			m.swapTotalKB = v
		case "SwapFree:":
			m.swapFreeKB = v
		}
	}
	return m
}

// readMeminfo reads /proc/meminfo once for memory and swap alike; a failed
// read leaves every figure zero.
func readMeminfo() memInfo {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return memInfo{}
	}
	defer f.Close()
	return parseMeminfo(f)
}

// parseLoadavg reads the 1/5/15-minute loads from /proc/loadavg content;
// missing or unparsable fields stay zero.
func parseLoadavg(s string) (load1, load5, load15 float64) {
	f := strings.Fields(s)
	if len(f) > 0 {
		load1, _ = strconv.ParseFloat(f[0], 64)
	}
	if len(f) > 1 {
		load5, _ = strconv.ParseFloat(f[1], 64)
	}
	if len(f) > 2 {
		load15, _ = strconv.ParseFloat(f[2], 64)
	}
	return
}

// countProcNames counts the all-digit names in a /proc listing: one per
// process (threads live under <pid>/task, not at the top level).
func countProcNames(names []string) uint32 {
	var n uint32
	for _, name := range names {
		if name == "" {
			continue
		}
		digits := true
		for _, c := range name {
			if c < '0' || c > '9' {
				digits = false
				break
			}
		}
		if digits {
			n++
		}
	}
	return n
}

func readProcessCount() uint32 {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return countProcNames(names)
}

// parseNetTable counts the entries in a /proc/net/{tcp,udp} table: every
// line but the header is one socket.
func parseNetTable(r io.Reader) uint32 {
	var lines uint32
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			lines++
		}
	}
	if lines == 0 {
		return 0
	}
	return lines - 1
}

// readConnCount sums parseNetTable over the given tables; a missing table
// (e.g. no IPv6) contributes zero.
func readConnCount(paths ...string) uint32 {
	var n uint32
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		n += parseNetTable(f)
		f.Close()
	}
	return n
}

var osNameOnce = sync.OnceValue(func() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if len(v) > 64 {
				v = v[:64]
			}
			return v
		}
	}
	return runtime.GOOS
})

func osName() string { return osNameOnce() }

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

func readCPU() (idle, total uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		fields := strings.Fields(sc.Text())
		for i, v := range fields[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			total += n
			if i == 3 || i == 4 { // idle + iowait
				idle += n
			}
		}
	}
	return
}

type netCounters struct {
	rx, tx uint64
	epoch  string
	boot   string
}

func skipNetInterface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, prefix := range []string{"docker", "veth", "br-", "virbr", "tun", "tap", "wg", "tailscale", "cni", "flannel"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// A failed/partial read is unavailable, not a new (smaller) cumulative value.
func parseNetCounters(r io.Reader, boot string, identity func(string) (string, error)) (netCounters, error) {
	var out netCounters
	var ids []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, raw, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if skipNetInterface(name) {
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) < 16 {
			return out, fmt.Errorf("incomplete interface counters")
		}
		rx, e := strconv.ParseUint(fields[0], 10, 64)
		if e != nil {
			return out, e
		}
		tx, e := strconv.ParseUint(fields[8], 10, 64)
		if e != nil {
			return out, e
		}
		id, e := identity(name)
		if e != nil {
			return out, e
		}
		if ^uint64(0)-out.rx < rx || ^uint64(0)-out.tx < tx {
			return out, fmt.Errorf("network counters overflow")
		}
		out.rx += rx
		out.tx += tx
		ids = append(ids, name+":"+id)
	}
	if e := sc.Err(); e != nil {
		return out, e
	}
	if strings.TrimSpace(boot) == "" || len(ids) == 0 {
		return out, fmt.Errorf("network counter identity unavailable")
	}
	sort.Strings(ids)
	bootHash := sha256.Sum256([]byte(strings.TrimSpace(boot)))
	out.boot = hex.EncodeToString(bootHash[:])
	h := sha256.Sum256([]byte(strings.TrimSpace(boot) + "\n" + strings.Join(ids, "\n")))
	out.epoch = hex.EncodeToString(h[:])
	return out, nil
}
func readNet() (netCounters, error) {
	f, e := os.Open("/proc/net/dev")
	if e != nil {
		return netCounters{}, e
	}
	defer f.Close()
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return netCounters{}, e
	}
	return parseNetCounters(f, string(boot), func(name string) (string, error) {
		b, e := os.ReadFile("/sys/class/net/" + name + "/ifindex")
		return strings.TrimSpace(string(b)), e
	})
}
