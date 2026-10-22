package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func fwSpec(ports ...int) *model.NodeSpec {
	s := &model.NodeSpec{AutoFirewall: true}
	for _, p := range ports {
		in := model.Inbound{Tag: "in", Port: p, Enabled: true, Protocol: model.ProtocolSpec{Type: model.ProtoVLESS}, Transport: model.TransportSpec{Type: model.TransTCP}}
		s.Inbounds = append(s.Inbounds, in)
	}
	return s
}

// fakeUFW keeps an "ufw status" table.
type fakeUFW struct {
	rules map[string]bool
	cmds  []string
}

func (u *fakeUFW) run(_ context.Context, name string, args ...string) (string, error) {
	u.cmds = append(u.cmds, name+" "+strings.Join(args, " "))
	switch {
	case name == "ufw" && args[0] == "status":
		out := "Status: active\n\nTo Action From\n"
		for r := range u.rules {
			out += r + " ALLOW Anywhere\n"
		}
		return out, nil
	case name == "ufw" && args[0] == "allow":
		u.rules[args[1]] = true
	case name == "ufw" && args[0] == "delete":
		delete(u.rules, args[2])
	}
	return "", nil
}

func TestFirewallUFW(t *testing.T) {
	u := &fakeUFW{rules: map[string]bool{"22/tcp": true, "8443/tcp": true}} // the admin opened 8443 themselves
	f := &firewall{run: u.run, state: t.TempDir() + "/fw.json", look: func(n string) bool { return n == "ufw" }}
	ctx := context.Background()

	spec := fwSpec(443, 8443)
	spec.Inbounds = append(spec.Inbounds, model.Inbound{Tag: "hy2", Port: 8888, Enabled: true, Protocol: model.ProtocolSpec{Type: model.ProtoHysteria2}})
	child := model.Inbound{Tag: "child", Listen: "127.0.0.1", Port: 2001, Enabled: true, Protocol: model.ProtocolSpec{Type: model.ProtoVLESS}}
	spec.Inbounds = append(spec.Inbounds, child)
	sum, err := f.Sync(ctx, spec, true)
	if err != nil {
		t.Fatal(err)
	}
	if !u.rules["443/tcp"] || !u.rules["8888/udp"] || u.rules["2001/tcp"] {
		t.Fatalf("rules after sync: %v (%s)", u.rules, sum)
	}
	// Removing the inbounds closes only what Polaris opened.
	if _, err := f.Sync(ctx, fwSpec(), true); err != nil {
		t.Fatal(err)
	}
	if u.rules["443/tcp"] || u.rules["8888/udp"] || !u.rules["8443/tcp"] || !u.rules["22/tcp"] {
		t.Fatalf("pre-existing rules must stay, ours must go: %v", u.rules)
	}
	// Turning management off removes our rules, never the admin's.
	f.Sync(ctx, fwSpec(443), true)
	f.Sync(ctx, fwSpec(443), false)
	if u.rules["443/tcp"] || !u.rules["8443/tcp"] {
		t.Fatalf("disable: %v", u.rules)
	}
}

func TestFirewallNoActiveFirewall(t *testing.T) {
	var cmds []string
	run := func(_ context.Context, name string, args ...string) (string, error) {
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		if name == "ufw" {
			return "Status: inactive\n", nil
		}
		if name == "iptables" {
			return "-P INPUT ACCEPT\n", nil
		}
		return "", nil
	}
	f := &firewall{run: run, state: t.TempDir() + "/fw.json", look: func(string) bool { return true }}
	if sum, err := f.Sync(context.Background(), fwSpec(443), true); err != nil || sum != "" {
		t.Fatalf("%q %v", sum, err)
	}
	for _, c := range cmds {
		if strings.Contains(c, "allow") || strings.Contains(c, "-I INPUT") || strings.Contains(c, "add-port") {
			t.Fatalf("changed a host without an active firewall: %v", cmds)
		}
	}
}

func TestIptablesRestrictive(t *testing.T) {
	oracle := "-P INPUT ACCEPT\n-A INPUT -m state --state RELATED,ESTABLISHED -j ACCEPT\n-A INPUT -p tcp -m state --state NEW -m tcp --dport 22 -j ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"
	if !iptablesRestrictive(oracle) || !iptablesRestrictive("-P INPUT DROP\n") || iptablesRestrictive("-P INPUT ACCEPT\n") {
		t.Fatal("iptablesRestrictive")
	}
}

// TestFirewallIptablesReal runs real iptables. It must run inside a
// throwaway network namespace:
//
//	POLARIS_FW_REAL=1 unshare -n go test -run TestFirewallIptablesReal ./internal/agent/
func TestFirewallIptablesReal(t *testing.T) {
	if os.Getenv("POLARIS_FW_REAL") != "1" {
		t.Skip("set POLARIS_FW_REAL=1 and run under unshare -n")
	}
	ipt := func(args ...string) string {
		out, err := exec.Command("iptables", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("iptables %v: %v %s", args, err, out)
		}
		return string(out)
	}
	// Oracle Cloud Ubuntu layout, plus an admin rule for 8443.
	ipt("-A", "INPUT", "-p", "tcp", "--dport", "22", "-j", "ACCEPT")
	ipt("-A", "INPUT", "-p", "tcp", "--dport", "8443", "-j", "ACCEPT")
	ipt("-A", "INPUT", "-j", "REJECT", "--reject-with", "icmp-host-prohibited")
	f := newFirewall(t.TempDir())
	f.look = func(n string) bool { return n == "iptables" } // no ufw / firewalld; skip ip6tables
	ctx := context.Background()
	sum, err := f.Sync(ctx, fwSpec(443, 8443), true)
	if err != nil || !strings.Contains(sum, "iptables 已放行 443/tcp") || strings.Contains(sum, "8443") {
		t.Fatalf("sync: %q %v", sum, err)
	}
	rules := ipt("-S", "INPUT")
	lines := strings.Split(strings.TrimSpace(rules), "\n")
	if !strings.Contains(lines[1], "--dport 443") || !strings.Contains(lines[1], "polaris") {
		t.Fatalf("443 should be inserted before the REJECT rule:\n%s", rules)
	}
	// Rules lost (reboot): the next sync puts them back.
	ipt("-D", "INPUT", "-p", "tcp", "--dport", "443", "-m", "comment", "--comment", "polaris", "-j", "ACCEPT")
	if sum, _ := f.Sync(ctx, fwSpec(443, 8443), true); !strings.Contains(sum, "443/tcp") {
		t.Fatalf("lost rule not restored: %q", sum)
	}
	// Inbound removed: our rule goes, the admin's 8443 stays.
	f.Sync(ctx, fwSpec(), true)
	rules = ipt("-S", "INPUT")
	if strings.Contains(rules, "--dport 443 ") || !strings.Contains(rules, "--dport 8443") {
		t.Fatalf("after removal:\n%s", rules)
	}
}

func TestManagedRelayLoopbackFirewall(t *testing.T) {
	spec := &model.NodeSpec{Forwards: []model.ForwardRule{
		{Tag: "local-bridge", Engine: model.EngineGost, ListenIP: "127.0.0.1", Listen: 30001, Network: "tcp,udp"},
		{Tag: "public-receiver", Engine: model.EngineGost, TunnelRole: "server", TunnelType: "tcp", Listen: 30002, Network: "tcp,udp"},
	}}
	ports := wantedPorts(spec)
	if len(ports) != 1 || ports[0].Port != 30002 || ports[0].Proto != "tcp" {
		t.Fatalf("internal bridge must not open public firewall ports: %+v", ports)
	}
}
