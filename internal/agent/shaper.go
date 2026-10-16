package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Port speed limits with tc. The cores cannot cap bandwidth per inbound, so
// the agent shapes the node's default-route interface:
//   - egress (what users download): an HTB root whose default class is
//     unlimited; each limited port gets a class at its rate, selected by
//     source port;
//   - ingress (what users upload): a police filter per port, by destination
//     port, dropping what exceeds the rate.
// Polaris owns these qdiscs only while a limit exists: with no limits, and
// none applied before, the interface is never touched.

type shaper struct {
	run   runner
	look  func(string) bool
	state string // records the interface Polaris shaped
	last  string // signature of the limits in effect (this process)
}

type shaperState struct {
	Dev string `json:"dev"`
}

func newShaper(dataDir string) *shaper {
	return &shaper{run: execRunner, state: filepath.Join(dataDir, "agent", "shaper.json"),
		look: func(name string) bool { _, err := exec.LookPath(name); return err == nil }}
}

func (sh *shaper) loadState() shaperState {
	var st shaperState
	if b, err := os.ReadFile(sh.state); err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

func (sh *shaper) saveState(st shaperState) {
	if st.Dev == "" {
		os.Remove(sh.state)
		return
	}
	os.MkdirAll(filepath.Dir(sh.state), 0o755)
	b, _ := json.Marshal(st)
	os.WriteFile(sh.state, b, 0o644)
}

// defaultDev is the interface of the default route (IPv4, else IPv6).
func (sh *shaper) defaultDev(ctx context.Context) string {
	for _, args := range [][]string{{"route", "show", "default"}, {"-6", "route", "show", "default"}} {
		out, err := sh.run(ctx, "ip", args...)
		if err != nil {
			continue
		}
		f := strings.Fields(out)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "dev" {
				return f[i+1]
			}
		}
	}
	return ""
}

func (sh *shaper) clear(ctx context.Context, dev string) {
	sh.run(ctx, "tc", "qdisc", "del", "dev", dev, "root")
	sh.run(ctx, "tc", "qdisc", "del", "dev", dev, "ingress")
}

func normLimits(ls []model.SpeedLimit) []model.SpeedLimit {
	out := make([]model.SpeedLimit, 0, len(ls))
	for _, l := range ls {
		if l.Port > 0 && l.Port < 65536 && l.Mbps > 0 {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// Sync makes the tc setup match limits; it returns a summary of what changed.
func (sh *shaper) Sync(ctx context.Context, limits []model.SpeedLimit) (string, error) {
	limits = normLimits(limits)
	b, _ := json.Marshal(limits)
	sig := string(b)
	if sig == sh.last {
		return "", nil
	}
	prev := sh.loadState()
	if len(limits) == 0 {
		sh.last = sig
		if prev.Dev == "" {
			return "", nil
		}
		sh.clear(ctx, prev.Dev)
		sh.saveState(shaperState{})
		return "speed limits removed from " + prev.Dev, nil
	}
	if !sh.look("tc") {
		return "", errors.New("未找到 tc 命令（请安装 iproute2），端口限速未生效")
	}
	dev := sh.defaultDev(ctx)
	if dev == "" {
		return "", errors.New("找不到默认路由网卡，端口限速未生效")
	}
	if prev.Dev != "" && prev.Dev != dev {
		sh.clear(ctx, prev.Dev)
	}
	sh.clear(ctx, dev)
	sh.saveState(shaperState{Dev: dev}) // before adding: a partial setup is still ours to remove

	var errs []string
	tc := func(args ...string) {
		if out, err := sh.run(ctx, "tc", args...); err != nil {
			errs = append(errs, strings.TrimSpace("tc "+strings.Join(args, " ")+": "+out))
		}
	}
	tc("qdisc", "add", "dev", dev, "root", "handle", "1:", "htb", "default", "2")
	tc("class", "add", "dev", dev, "parent", "1:", "classid", "1:2", "htb", "rate", "100gbit")
	tc("qdisc", "add", "dev", dev, "handle", "ffff:", "ingress")
	var ports []string
	for i, l := range limits {
		class := fmt.Sprintf("1:%x", 0x10+i)
		rate := fmt.Sprintf("%dmbit", l.Mbps)
		burst := fmt.Sprintf("%dkb", max(l.Mbps*13, 64)) // ~100 ms at the rate
		port := fmt.Sprint(l.Port)
		tc("class", "add", "dev", dev, "parent", "1:", "classid", class, "htb", "rate", rate, "ceil", rate)
		sh.run(ctx, "tc", "qdisc", "add", "dev", dev, "parent", class, "fq_codel") // optional: fairness inside the cap
		for _, proto := range strings.Split(l.Network, ",") {
			num := map[string]string{"tcp": "6", "udp": "17"}[strings.TrimSpace(proto)]
			if num == "" {
				continue
			}
			for _, fam := range []struct{ proto, match, prio string }{{"ip", "ip", "1"}, {"ipv6", "ip6", "2"}} {
				tc("filter", "add", "dev", dev, "parent", "1:", "protocol", fam.proto, "prio", fam.prio, "u32",
					"match", fam.match, "protocol", num, "0xff", "match", fam.match, "sport", port, "0xffff", "flowid", class)
				tc("filter", "add", "dev", dev, "parent", "ffff:", "protocol", fam.proto, "prio", fam.prio, "u32",
					"match", fam.match, "protocol", num, "0xff", "match", fam.match, "dport", port, "0xffff",
					"police", "rate", rate, "burst", burst, "drop", "flowid", ":1")
			}
		}
		ports = append(ports, fmt.Sprintf("%d=%dMbps", l.Port, l.Mbps))
	}
	if len(errs) > 0 {
		sh.last = "" // try again on the next apply
		return "", fmt.Errorf("端口限速部分失败：%s", strings.Join(errs, "; "))
	}
	sh.last = sig
	return "speed limits on " + dev + ": " + strings.Join(ports, " "), nil
}
