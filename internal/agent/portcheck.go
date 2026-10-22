package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// Before touching any core, make sure the ports a new spec needs are free.
// Otherwise a port held by another program (nginx on 443, a leftover panel…)
// only shows up as a core startup failure buried in its log.

type portUse struct {
	Proto string // tcp / udp
	Host  string // listen address ("" = all)
	Port  int
	Owner string // inbound / forward / front tag, for the message
}

func (p portUse) key() string { return fmt.Sprintf("%s/%d", p.Proto, p.Port) }

// portsOf lists every socket a spec opens on this host.
func portsOf(spec *model.NodeSpec) []portUse {
	if spec == nil {
		return nil
	}
	var out []portUse
	host := func(l string) string {
		if l == "" || l == "::" || l == "0.0.0.0" {
			return ""
		}
		return l
	}
	for i := range spec.Inbounds {
		in := &spec.Inbounds[i]
		if !in.Enabled || in.Port == 0 {
			continue
		}
		if in.UsesTCP() {
			out = append(out, portUse{"tcp", host(in.Listen), in.Port, in.Tag})
		}
		if in.UsesUDP() {
			out = append(out, portUse{"udp", host(in.Listen), in.Port, in.Tag})
		}
	}
	for _, f := range spec.Forwards {
		for _, n := range strings.Split(f.ListenNetwork(), ",") {
			out = append(out, portUse{strings.TrimSpace(n), f.ListenIP, f.Listen, f.Tag})
		}
	}
	seen := map[int]bool{}
	for _, f := range spec.Fronts {
		if !seen[f.Port] {
			seen[f.Port] = true
			out = append(out, portUse{"tcp", "", f.Port, "CDN 回源 " + f.Domain})
		}
	}
	return out
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// portConflicts returns the ports next needs that are neither held by our
// own cores (the specs they run) nor free to bind.
func portConflicts(held []*model.NodeSpec, next *model.NodeSpec) []string {
	ours := map[string]bool{}
	for _, s := range held {
		for _, p := range portsOf(s) {
			ours[p.key()] = true
		}
	}
	var msgs []string
	checked := map[string]bool{}
	for _, p := range portsOf(next) {
		if ours[p.key()] || checked[p.key()] {
			continue
		}
		checked[p.key()] = true
		if !portBusy(p) {
			continue
		}
		owner := portOwner(p)
		if owner != "" {
			owner = "（" + owner + "）"
		}
		msgs = append(msgs, fmt.Sprintf("端口 %d/%s 已被其他程序占用%s，入站「%s」无法监听：请停止占用该端口的程序，或在面板中修改端口", p.Port, p.Proto, owner, p.Owner))
	}
	sort.Strings(msgs)
	return msgs
}

// portBusy reports whether binding fails because something already listens.
// Other errors (permissions, missing address) are left to the core to report.
func portBusy(p portUse) bool {
	addr := net.JoinHostPort(p.Host, fmt.Sprint(p.Port))
	var err error
	if p.Proto == "udp" {
		var c net.PacketConn
		if c, err = net.ListenPacket("udp", addr); err == nil {
			c.Close()
		}
	} else {
		var l net.Listener
		if l, err = net.Listen("tcp", addr); err == nil {
			l.Close()
		}
	}
	return err != nil && errors.Is(err, syscall.EADDRINUSE)
}

var ssUsers = regexp.MustCompile(`users:\(\("([^"]+)",pid=(\d+)`)

// portOwner names the process holding a port (best effort, needs ss).
func portOwner(p portUse) string {
	flag := "-Hltnp"
	if p.Proto == "udp" {
		flag = "-Hlunp"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ss", flag, fmt.Sprintf("sport = :%d", p.Port)).Output()
	if err != nil {
		return ""
	}
	if m := ssUsers.FindSubmatch(out); m != nil {
		return fmt.Sprintf("%s，pid %s", m[1], m[2])
	}
	return ""
}
