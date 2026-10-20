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
	c, _ := parseNetCounters(strings.NewReader(nicLine("eth0", 1, 2)), "boot-b", id)
	if a.epoch == c.epoch {
		t.Fatal("reboot preserved source identity")
	}
	d, _ := parseNetCounters(strings.NewReader(nicLine("eth0", 1, 2)+nicLine("eth1", 1000, 2000)), "boot-a", id)
	if a.epoch == d.epoch {
		t.Fatal("new interface did not rebaseline")
	}
	reindex, _ := parseNetCounters(strings.NewReader(nicLine("eth0", 1, 2)), "boot-a", func(string) (string, error) { return "replacement-index", nil })
	if a.epoch == reindex.epoch {
		t.Fatal("replaced interface preserved source identity")
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
