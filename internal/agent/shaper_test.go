package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestShaper(t *testing.T) {
	var cmds []string
	sh := &shaper{state: filepath.Join(t.TempDir(), "shaper.json"), look: func(string) bool { return true },
		run: func(_ context.Context, name string, args ...string) (string, error) {
			line := name + " " + strings.Join(args, " ")
			cmds = append(cmds, line)
			if line == "ip route show default" {
				return "default via 10.0.0.1 dev eth0 proto dhcp metric 100\n", nil
			}
			return "", nil
		}}
	ctx := context.Background()

	// Never limited: the interface is left alone.
	if s, err := sh.Sync(ctx, nil); s != "" || err != nil || len(cmds) != 0 {
		t.Fatalf("no limits touched tc: %q %v %v", s, err, cmds)
	}

	limits := []model.SpeedLimit{{Tag: "a", Port: 443, Network: "tcp,udp", Mbps: 100}, {Tag: "b", Port: 8443, Network: "udp", Mbps: 5}}
	s, err := sh.Sync(ctx, limits)
	if err != nil || !strings.Contains(s, "eth0") || !strings.Contains(s, "443=100Mbps") {
		t.Fatalf("sync: %q %v", s, err)
	}
	all := strings.Join(cmds, "\n")
	for _, want := range []string{
		"tc qdisc add dev eth0 root handle 1: htb default 2",
		"tc class add dev eth0 parent 1: classid 1:2 htb rate 100gbit",
		"tc class add dev eth0 parent 1: classid 1:10 htb rate 100mbit ceil 100mbit",
		"tc filter add dev eth0 parent 1: protocol ip prio 1 u32 match ip protocol 6 0xff match ip sport 443 0xffff flowid 1:10",
		"tc filter add dev eth0 parent 1: protocol ipv6 prio 2 u32 match ip6 protocol 17 0xff match ip6 sport 443 0xffff flowid 1:10",
		"tc filter add dev eth0 parent ffff: protocol ip prio 1 u32 match ip protocol 6 0xff match ip dport 443 0xffff police rate 100mbit burst 1300kb drop flowid :1",
		"tc class add dev eth0 parent 1: classid 1:11 htb rate 5mbit ceil 5mbit",
		"police rate 5mbit burst 65kb drop",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "dport 8443 0xffff police") && strings.Contains(all, "protocol 6 0xff match ip dport 8443") {
		t.Fatal("udp-only port got a tcp filter")
	}

	// Unchanged: nothing runs.
	cmds = nil
	if s, _ := sh.Sync(ctx, limits); s != "" || len(cmds) != 0 {
		t.Fatalf("re-applied unchanged limits: %v", cmds)
	}
	// A new process (restart / reboot) applies again.
	sh.last = ""
	if s, _ := sh.Sync(ctx, limits); s == "" {
		t.Fatal("restart did not re-apply")
	}

	// Removed: Polaris' qdiscs are deleted, then the interface is left alone.
	cmds = nil
	if s, _ := sh.Sync(ctx, nil); !strings.Contains(s, "removed") {
		t.Fatalf("remove: %q", s)
	}
	if strings.Join(cmds, "\n") != "tc qdisc del dev eth0 root\ntc qdisc del dev eth0 ingress" {
		t.Fatalf("remove cmds: %v", cmds)
	}
	cmds = nil
	sh.last = ""
	if sh.Sync(ctx, nil); len(cmds) != 0 {
		t.Fatalf("touched tc after removal: %v", cmds)
	}
}
