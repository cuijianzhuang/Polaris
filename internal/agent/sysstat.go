package agent

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
)

// sysSampler computes CPU and NIC rates from successive /proc samples.
type sysSampler struct {
	mu       sync.Mutex
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
	rx, tx := readNet()
	now := time.Now()
	if !s.at.IsZero() && rx >= s.rx && tx >= s.tx {
		sec := now.Sub(s.at).Seconds()
		if sec > 0 {
			out.RxBps = uint64(float64(rx-s.rx) * 8 / sec)
			out.TxBps = uint64(float64(tx-s.tx) * 8 / sec)
		}
	}
	s.rx, s.tx, s.at = rx, tx, now
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			out.UptimeSeconds = uint64(v)
		}
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

func readNet() (rx, tx uint64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") {
			continue
		}
		fields := strings.Fields(line[i+1:])
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fields[0], 10, 64)
		t, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return
}
