package agent

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
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
	out.MemPercent = round1(readMem())
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
		f := strings.Fields(string(b))
		if len(f) > 0 {
			out.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	return out
}

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

func readMem() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	var total, avail float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total == 0 {
		return 0
	}
	return 100 * (total - avail) / total
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
