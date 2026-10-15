package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Host firewall management: after a spec is applied, the ports its inbounds,
// forwards and fronts listen on are opened in the active host firewall (ufw,
// firewalld, or iptables when the INPUT chain drops by default, as on Oracle
// Cloud images), and ports Polaris opened earlier but no longer needs are
// closed. Rules that existed before are never recorded, so they are never
// removed. Cloud security groups are outside the host and not touched.

type fwPort struct {
	Proto string `json:"proto"` // tcp / udp
	Port  int    `json:"port"`
}

func (p fwPort) String() string { return fmt.Sprintf("%d/%s", p.Port, p.Proto) }

// runner executes a command; replaced in tests.
type runner func(ctx context.Context, name string, args ...string) (string, error)

func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

type firewall struct {
	run   runner
	state string // file recording the ports Polaris opened
	look  func(string) bool
}

func newFirewall(dataDir string) *firewall {
	return &firewall{run: execRunner, state: filepath.Join(dataDir, "agent", "firewall.json"),
		look: func(name string) bool { _, err := exec.LookPath(name); return err == nil }}
}

// wantedPorts lists the public ports a spec listens on.
func wantedPorts(spec *model.NodeSpec) []fwPort {
	seen := map[fwPort]bool{}
	var out []fwPort
	for _, p := range portsOf(spec) {
		if p.Host != "" && isLoopbackHost(p.Host) {
			continue // e.g. fallback children: reachable through their master only
		}
		fp := fwPort{Proto: p.Proto, Port: p.Port}
		if p.Port > 0 && (fp.Proto == "tcp" || fp.Proto == "udp") && !seen[fp] {
			seen[fp] = true
			out = append(out, fp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || strings.HasPrefix(h, "127.") || h == "::1"
}

// backend detects the active host firewall ("" = none, nothing to do).
func (f *firewall) backend(ctx context.Context) string {
	if f.look("ufw") {
		if out, err := f.run(ctx, "ufw", "status"); err == nil && strings.Contains(out, "Status: active") {
			return "ufw"
		}
	}
	if f.look("firewall-cmd") {
		if out, err := f.run(ctx, "firewall-cmd", "--state"); err == nil && strings.TrimSpace(out) == "running" {
			return "firewalld"
		}
	}
	if f.look("iptables") {
		if out, err := f.run(ctx, "iptables", "-S", "INPUT"); err == nil && iptablesRestrictive(out) {
			return "iptables"
		}
	}
	return ""
}

// iptablesRestrictive: the INPUT chain drops by default or ends in a
// catch-all REJECT / DROP (the Oracle Cloud Ubuntu layout).
func iptablesRestrictive(rules string) bool {
	for _, l := range strings.Split(rules, "\n") {
		l = strings.TrimSpace(l)
		if l == "-P INPUT DROP" {
			return true
		}
		if strings.HasPrefix(l, "-A INPUT") && (strings.HasSuffix(l, "-j REJECT --reject-with icmp-host-prohibited") ||
			l == "-A INPUT -j REJECT" || l == "-A INPUT -j DROP") {
			return true
		}
	}
	return false
}

func (f *firewall) load() []fwPort {
	var ps []fwPort
	if b, err := os.ReadFile(f.state); err == nil {
		_ = json.Unmarshal(b, &ps)
	}
	return ps
}

func (f *firewall) save(ps []fwPort) {
	b, _ := json.Marshal(ps)
	_ = os.MkdirAll(filepath.Dir(f.state), 0o700)
	_ = os.WriteFile(f.state, b, 0o600)
}

// Sync opens wanted ports and closes the ones Polaris opened before but no
// longer wants. With enabled=false it only closes what Polaris opened. It
// returns a human-readable summary of changes (empty when nothing changed).
func (f *firewall) Sync(ctx context.Context, spec *model.NodeSpec, enabled bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	owned := f.load()
	var want []fwPort
	if enabled && spec != nil {
		want = wantedPorts(spec)
	}
	be := f.backend(ctx)
	if be == "" {
		if len(owned) > 0 {
			f.save(nil) // firewall gone or disabled: forget our records
		}
		return "", nil
	}
	wantSet := map[fwPort]bool{}
	for _, p := range want {
		wantSet[p] = true
	}
	ownedSet := map[fwPort]bool{}
	for _, p := range owned {
		ownedSet[p] = true
	}
	var opened, closed, errs []string
	var keep []fwPort
	for _, p := range owned {
		if wantSet[p] {
			keep = append(keep, p)
			continue
		}
		if err := f.close(ctx, be, p); err != nil {
			errs = append(errs, fmt.Sprintf("关闭 %s：%v", p, err))
			keep = append(keep, p)
			continue
		}
		closed = append(closed, p.String())
	}
	for _, p := range want {
		exists, err := f.isOpen(ctx, be, p)
		if err != nil {
			errs = append(errs, fmt.Sprintf("检查 %s：%v", p, err))
			continue
		}
		if exists {
			// Ours and still there, or opened by someone else (then it is
			// never recorded, so never removed).
			continue
		}
		// Missing: new, or ours but lost (iptables rules do not survive a
		// reboot; the agent re-applies its spec at startup).
		if err := f.open(ctx, be, p); err != nil {
			errs = append(errs, fmt.Sprintf("放行 %s：%v", p, err))
			continue
		}
		if !ownedSet[p] {
			keep = append(keep, p)
		}
		opened = append(opened, p.String())
	}
	if be == "firewalld" && (len(opened) > 0 || len(closed) > 0) {
		_, _ = f.run(ctx, "firewall-cmd", "--reload")
	}
	f.save(keep)
	var parts []string
	if len(opened) > 0 {
		parts = append(parts, be+" 已放行 "+strings.Join(opened, " "))
	}
	if len(closed) > 0 {
		parts = append(parts, be+" 已关闭 "+strings.Join(closed, " "))
	}
	var err error
	if len(errs) > 0 {
		err = fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return strings.Join(parts, "；"), err
}

const fwComment = "polaris"

func (f *firewall) isOpen(ctx context.Context, be string, p fwPort) (bool, error) {
	switch be {
	case "ufw":
		out, err := f.run(ctx, "ufw", "status")
		if err != nil {
			return false, err
		}
		for _, l := range strings.Split(out, "\n") {
			fs := strings.Fields(l)
			if len(fs) >= 2 && (fs[0] == p.String() || fs[0] == fmt.Sprint(p.Port)) && strings.EqualFold(fs[1], "ALLOW") {
				return true, nil
			}
		}
		return false, nil
	case "firewalld":
		out, _ := f.run(ctx, "firewall-cmd", "--permanent", "--query-port="+p.String())
		return strings.TrimSpace(out) == "yes", nil
	default:
		// Any ACCEPT rule for the port counts, not just one identical to ours
		// (an admin's "-p tcp --dport 8443 -j ACCEPT" must not get a duplicate
		// that Polaris would later remove).
		out, err := f.run(ctx, "iptables", "-S", "INPUT")
		if err != nil {
			return false, err
		}
		return iptablesAllows(out, p), nil
	}
}

func iptablesAllows(rules string, p fwPort) bool {
	for _, l := range strings.Split(rules, "\n") {
		fs := strings.Fields(l)
		if len(fs) < 2 || fs[0] != "-A" || fs[1] != "INPUT" || !strings.HasSuffix(strings.TrimSpace(l), "-j ACCEPT") {
			continue
		}
		proto, port := "", ""
		for i := 0; i+1 < len(fs); i++ {
			switch fs[i] {
			case "-p":
				proto = fs[i+1]
			case "--dport":
				port = fs[i+1]
			}
		}
		if proto == p.Proto && port == fmt.Sprint(p.Port) {
			return true
		}
	}
	return false
}

func iptArgs(op string, p fwPort) []string {
	a := []string{op, "INPUT"}
	if op == "-I" {
		a = append(a, "1")
	}
	return append(a, "-p", p.Proto, "--dport", fmt.Sprint(p.Port), "-m", "comment", "--comment", fwComment, "-j", "ACCEPT")
}

func (f *firewall) open(ctx context.Context, be string, p fwPort) error {
	var out string
	var err error
	switch be {
	case "ufw":
		out, err = f.run(ctx, "ufw", "allow", p.String(), "comment", fwComment)
	case "firewalld":
		out, err = f.run(ctx, "firewall-cmd", "--permanent", "--add-port="+p.String())
	default:
		if out, err = f.run(ctx, "iptables", iptArgs("-I", p)...); err == nil && f.look("ip6tables") {
			_, _ = f.run(ctx, "ip6tables", iptArgs("-I", p)...)
		}
	}
	if err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(out))
	}
	return nil
}

func (f *firewall) close(ctx context.Context, be string, p fwPort) error {
	var out string
	var err error
	switch be {
	case "ufw":
		out, err = f.run(ctx, "ufw", "delete", "allow", p.String())
	case "firewalld":
		out, err = f.run(ctx, "firewall-cmd", "--permanent", "--remove-port="+p.String())
	default:
		if out, err = f.run(ctx, "iptables", iptArgs("-D", p)...); err == nil && f.look("ip6tables") {
			_, _ = f.run(ctx, "ip6tables", iptArgs("-D", p)...)
		}
	}
	if err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(out))
	}
	return nil
}
