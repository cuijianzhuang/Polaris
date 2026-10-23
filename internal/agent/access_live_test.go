package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// This test creates an isolated dual-stack Docker network. It never changes
// the host's firewall; only the server container gets NET_ADMIN.
func TestLiveAccessFirewall(t *testing.T) {
	if os.Getenv("POLARIS_NFT_DOCKER_TEST") != "1" {
		t.Skip("POLARIS_NFT_DOCKER_TEST not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	docker := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "docker", args...)
		b, e := cmd.CombinedOutput()
		return strings.TrimSpace(string(b)), e
	}
	run := func(args ...string) string {
		t.Helper()
		out, e := docker(args...)
		if e != nil {
			t.Fatalf("docker %v: %v %s", args, e, out)
		}
		return out
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 16)
	network := "polaris-acl-" + suffix
	server := network + "-server"
	allowed := network + "-allowed"
	denied := network + "-denied"
	t.Cleanup(func() {
		for _, name := range []string{server, allowed, denied} {
			exec.Command("docker", "rm", "-f", name).Run()
		}
		exec.Command("docker", "network", "rm", network).Run()
	})
	// Configure a private IPv6 prefix inside the test namespaces. This also
	// works on Docker hosts without the legacy ip6tables filter module.
	run("network", "create", network)
	image := "python:3.12-alpine"
	run("run", "-d", "--name", server, "--network", network, "--cap-add", "NET_ADMIN", "--sysctl", "net.ipv6.conf.all.disable_ipv6=0", image, "sleep", "300")
	// Preserve the execution environment's trusted roots (including an HTTPS
	// proxy root when present) instead of disabling package TLS verification.
	if bundle, e := os.ReadFile("/etc/ssl/certs/ca-certificates.crt"); e == nil {
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", server, "sh", "-c", "cat > /etc/ssl/cert.pem")
		cmd.Stdin = bytes.NewReader(bundle)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("copy CA bundle: %v %s", e, out)
		}
	}
	run("exec", server, "apk", "add", "--no-cache", "nftables")
	run("exec", "-d", server, "python", "-m", "http.server", "18080", "-b", "::")
	for _, name := range []string{allowed, denied} {
		run("run", "-d", "--name", name, "--network", network, "--cap-add", "NET_ADMIN", "--sysctl", "net.ipv6.conf.all.disable_ipv6=0", image, "sleep", "300")
	}
	manualV6 := map[string]string{}
	for i, name := range []string{server, allowed, denied} {
		addr := "fd77:" + suffix[len(suffix)-4:] + "::" + strconv.Itoa(i+1)
		run("exec", name, "ip", "-6", "addr", "add", addr+"/64", "dev", "eth0")
		manualV6[name] = addr
	}
	addresses := func(name string) (string, string) {
		raw := run("inspect", "-f", "{{json .NetworkSettings.Networks}}", name)
		var nets map[string]struct{ IPAddress, GlobalIPv6Address string }
		if e := json.Unmarshal([]byte(raw), &nets); e != nil {
			t.Fatal(e)
		}
		v := nets[network]
		if v.IPAddress == "" {
			t.Fatal("dual-stack addresses missing", raw)
		}
		return v.IPAddress, manualV6[name]
	}
	s4, s6 := addresses(server)
	a4, a6 := addresses(allowed)
	httpGet := func(client, host string, port int) bool {
		_, e := docker("exec", client, "python", "-c", `import urllib.request,sys; urllib.request.build_opener(urllib.request.ProxyHandler({})).open('http://'+('['+sys.argv[1]+']' if ':' in sys.argv[1] else sys.argv[1])+':'+sys.argv[2]+'/',timeout=2).read()`, host, strconv.Itoa(port))
		return e == nil
	}
	ready := false
	for end := time.Now().Add(50 * time.Second); time.Now().Before(end); {
		if httpGet(denied, s4, 18080) {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		t.Fatal("server did not start", run("logs", server))
	}
	run("exec", "-d", server, "python", "-m", "http.server", "18081", "-b", "::")
	run("exec", "-d", server, "python", "-c", `import socket;s=socket.socket(socket.AF_INET6,socket.SOCK_DGRAM);s.setsockopt(socket.IPPROTO_IPV6,socket.IPV6_V6ONLY,0);s.bind(('::',18082))
while True:
 b,a=s.recvfrom(1024);s.sendto(b,a)`)
	udpEcho := func(client, host string) bool {
		_, e := docker("exec", client, "python", "-c", `import socket,sys;s=socket.socket(socket.AF_INET6 if ':' in sys.argv[1] else socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(2);s.sendto(b'echo',(sys.argv[1],18082));assert s.recvfrom(1024)[0]==b'echo'`, host)
		return e == nil
	}
	f := newFirewall(t.TempDir())
	f.look = func(name string) bool { return name == "nft" }
	f.run = func(c context.Context, name string, args ...string) (string, error) {
		args = append([]string(nil), args...)
		var input []byte
		for i := range args {
			if args[i] == "--file" && i+1 < len(args) {
				var e error
				input, e = os.ReadFile(args[i+1])
				if e != nil {
					return "", e
				}
				args[i+1] = "-"
			}
		}
		cmd := exec.CommandContext(c, "docker", append([]string{"exec", "-i", server, name}, args...)...)
		cmd.Stdin = bytes.NewReader(input)
		b, e := cmd.CombinedOutput()
		return string(b), e
	}
	run("exec", server, "nft", "add", "table", "inet", "polaris_unrelated")
	sources := []string{netip.PrefixFrom(netip.MustParseAddr(a4), 32).String(), netip.PrefixFrom(netip.MustParseAddr(a6), 128).String()}
	rules := []model.AccessRule{{Protocol: "tcp", Port: 18080, Sources: sources}, {Protocol: "udp", Port: 18082, Sources: sources}}
	if e := f.syncAccess(ctx, rules); e != nil {
		t.Fatal(e)
	}
	for _, host := range []string{s4, s6} {
		if !httpGet(allowed, host, 18080) || httpGet(denied, host, 18080) {
			t.Fatalf("TCP source guard failed for %s", host)
		}
		if !udpEcho(allowed, host) || udpEcho(denied, host) {
			t.Fatalf("UDP source guard failed for %s", host)
		}
		if !httpGet(denied, host, 18081) {
			t.Fatalf("unrelated port blocked for %s", host)
		}
	}
	for i := range rules {
		rules[i].Sources = nil
	}
	if e := f.syncAccess(ctx, rules); e != nil {
		t.Fatal(e)
	}
	if httpGet(allowed, s4, 18080) || httpGet(allowed, s6, 18080) {
		t.Fatal("empty source set permitted external clients")
	}
	if !httpGet(server, "127.0.0.1", 18080) {
		t.Fatal("loopback was blocked")
	}
	if e := f.syncAccess(ctx, nil); e != nil {
		t.Fatal(e)
	}
	if !httpGet(denied, s4, 18080) || !httpGet(denied, s6, 18080) {
		t.Fatal("explicit cleanup did not restore access")
	}
	run("exec", server, "nft", "list", "table", "inet", "polaris_unrelated")
	t.Log(fmt.Sprintf("verified TCP/UDP source restrictions over IPv4 %s and IPv6 %s", s4, s6))
}
