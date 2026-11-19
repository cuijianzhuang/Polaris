package agent

import (
	"fmt"
	"strings"
	"testing"
)

func nicLine(name string, rx, tx uint64) string {
	return fmt.Sprintf("%s: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0\n", name, rx, tx)
}
func TestNetworkCounterIdentity(t *testing.T) {
	id := func(name string) (string, error) { return name + "-index", nil }
	raw := nicLine("eth0", 100, 200) + nicLine("lo", 1<<30, 1<<30) + nicLine("veth-test", 1<<30, 1<<30) + nicLine("wg0", 1<<30, 1<<30) + nicLine("tun0", 1<<30, 1<<30)
	a, e := parseNetCounters(strings.NewReader(raw), "boot-a", id)
	if e != nil {
		t.Fatal(e)
	}
	if a.rx != 100 || a.tx != 200 {
		t.Fatal("virtual interface duplicated traffic", a)
	}
	b, e := parseNetCounters(strings.NewReader(nicLine("eth0", 150, 250)), "boot-a", id)
	if e != nil {
		t.Fatal(e)
	}
	if a.epoch != b.epoch {
		t.Fatal("counter growth changed source identity")
	}
	if a.boot == "" || a.boot != b.boot {
		t.Fatal("counter growth changed boot identity")
	}
	c, _ := parseNetCounters(strings.NewReader(nicLine("eth0", 1, 2)), "boot-b", id)
	if a.epoch == c.epoch {
		t.Fatal("reboot preserved source identity")
	}
	if a.boot == c.boot {
		t.Fatal("reboot preserved boot identity")
	}
	d, _ := parseNetCounters(strings.NewReader(nicLine("eth0", 1, 2)+nicLine("eth1", 1000, 2000)), "boot-a", id)
	if a.epoch == d.epoch {
		t.Fatal("new interface did not rebaseline")
	}
	if a.boot != d.boot {
		t.Fatal("interface change lost boot ordering")
	}
	reindex, _ := parseNetCounters(strings.NewReader(nicLine("eth0", 1, 2)), "boot-a", func(string) (string, error) { return "replacement-index", nil })
	if a.epoch == reindex.epoch {
		t.Fatal("replaced interface preserved source identity")
	}
	if a.boot != reindex.boot {
		t.Fatal("replaced interface changed boot identity")
	}
}
func TestParseLoadavg(t *testing.T) {
	l1, l5, l15 := parseLoadavg("0.52 0.58 0.59 2/1234 5678\n")
	if l1 != 0.52 || l5 != 0.58 || l15 != 0.59 {
		t.Fatal("loadavg parsed wrong", l1, l5, l15)
	}
	l1, l5, l15 = parseLoadavg("")
	if l1 != 0 || l5 != 0 || l15 != 0 {
		t.Fatal("empty loadavg invented loads", l1, l5, l15)
	}
	l1, l5, l15 = parseLoadavg("broken")
	if l1 != 0 || l5 != 0 || l15 != 0 {
		t.Fatal("broken loadavg invented loads", l1, l5, l15)
	}
}

func TestParseMeminfo(t *testing.T) {
	raw := "MemTotal:       16384000 kB\nMemFree:         1000000 kB\nMemAvailable:    8192000 kB\nSwapTotal:       2097152 kB\nSwapFree:        1048576 kB\n"
	m := parseMeminfo(strings.NewReader(raw))
	if m.totalKB != 16384000 || m.availKB != 8192000 || m.swapTotalKB != 2097152 || m.swapFreeKB != 1048576 {
		t.Fatal("meminfo parsed wrong", m)
	}
	if m.percent() != 50 {
		t.Fatal("mem percent wrong", m.percent())
	}
	if got := round1(m.swapPercent()); got != 50 {
		t.Fatal("swap percent wrong", got)
	}
	// No swap: percent and total stay zero so the panel can hide the row.
	m = parseMeminfo(strings.NewReader("MemTotal: 1024000 kB\nMemAvailable: 512000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"))
	if m.swapPercent() != 0 || m.swapTotalKB != 0 {
		t.Fatal("swapless host reported swap", m)
	}
	if got := parseMeminfo(strings.NewReader("")); got.percent() != 0 || got.swapPercent() != 0 {
		t.Fatal("empty meminfo invented usage", got)
	}
}

func TestCountProcNames(t *testing.T) {
	names := []string{"1", "42", "12345", "self", "net", "cpuinfo", "12a", ""}
	if n := countProcNames(names); n != 3 {
		t.Fatal("process count wrong", n)
	}
	if n := countProcNames(nil); n != 0 {
		t.Fatal("empty listing invented processes", n)
	}
}

func TestParseNetTable(t *testing.T) {
	header := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	row := "   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1 0000000000000000 100 0 0 10 0\n"
	if n := parseNetTable(strings.NewReader(header + row + row)); n != 2 {
		t.Fatal("connection count wrong", n)
	}
	if n := parseNetTable(strings.NewReader(header)); n != 0 {
		t.Fatal("header alone counted as a connection", n)
	}
	if n := parseNetTable(strings.NewReader("")); n != 0 {
		t.Fatal("empty table invented connections", n)
	}
}

func TestNetworkCounterRejectsPartialRead(t *testing.T) {
	id := func(string) (string, error) { return "1", nil }
	for _, raw := range []string{nicLine("eth0", 100, 200) + "eth1: 1 2\n", strings.Replace(nicLine("eth0", 100, 200), "100", "broken", 1), nicLine("lo", 100, 200)} {
		if _, e := parseNetCounters(strings.NewReader(raw), "boot", id); e == nil {
			t.Fatal("partial sample accepted", raw)
		}
	}
	if _, e := parseNetCounters(strings.NewReader(nicLine("eth0", 100, 200)), "boot", func(string) (string, error) { return "", fmt.Errorf("interface disappeared") }); e == nil {
		t.Fatal("missing interface identity accepted")
	}
}
