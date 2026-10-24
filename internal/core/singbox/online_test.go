package singbox

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
)

// Lines exactly as sing-box prints them (with its colour codes).
const (
	lineFrom = "\x1b[36mINFO\x1b[0m[0001] [\x1b[38;5;51m3404549370\x1b[0m 0ms] inbound/mixed[in]: inbound connection from 127.0.0.1:49332"
	lineUser = "\x1b[36mINFO\x1b[0m[0001] [\x1b[38;5;51m3404549370\x1b[0m 1ms] inbound/mixed[in]: [alice] inbound connection to 127.0.0.1:28402"
	lineOut  = "\x1b[36mINFO\x1b[0m[0001] [\x1b[38;5;51m3404549370\x1b[0m 2ms] outbound/direct[direct]: outbound connection to 127.0.0.1:28402"
)

func TestOnlineTracker(t *testing.T) {
	tr := newOnlineTracker()
	for _, l := range []string{lineFrom, lineUser, lineOut} {
		if !tr.line(l) {
			t.Fatalf("connection line should be consumed: %q", l)
		}
	}
	got := tr.online(time.Now())
	if ips := got["alice"]; len(ips) != 1 || ips[0] != "127.0.0.1" {
		t.Fatalf("online = %v", got)
	}
	if len(tr.pending) != 0 {
		t.Fatalf("paired connection should not stay pending: %v", tr.pending)
	}

	// Lines that are not about connections stay in the log buffer.
	for _, l := range []string{
		"\x1b[36mINFO\x1b[0m[0000] sing-box started (0.00s)",
		"\x1b[31mERROR\x1b[0m[0003] [123 4ms] inbound/mixed[in]: process connection from 1.2.3.4:5: authentication failed",
		"\x1b[33mWARN\x1b[0m[0002] network: updated default interface eth0, index 4",
	} {
		if tr.line(l) {
			t.Fatalf("should be kept: %q", l)
		}
	}

	// IPv6 sources, a user line with no earlier "from" line (agent started
	// mid-connection), and expiry.
	tr.line("INFO[0009] [77 0ms] inbound/vless[v]: inbound connection from [2001:db8::1]:4433")
	tr.line("INFO[0009] [77 1ms] inbound/vless[v]: [bob] inbound connection to a.example:443")
	tr.line("INFO[0009] [88 1ms] inbound/vless[v]: [carol] inbound connection to a.example:443")
	got = tr.online(time.Now())
	if ips := got["bob"]; len(ips) != 1 || ips[0] != "2001:db8::1" {
		t.Fatalf("bob = %v", got["bob"])
	}
	if _, ok := got["carol"]; ok {
		t.Fatal("a user line without a source must not create an entry")
	}
	if got := tr.online(time.Now().Add(onlineWindow + time.Second)); len(got) != 0 {
		t.Fatalf("should expire: %v", got)
	}
}

// TestLiveOnlineIPs runs a real sing-box: a user connects through a mixed
// inbound and shows up with the source IP.
func TestLiveOnlineIPs(t *testing.T) {
	bin := os.Getenv("POLARIS_SINGBOX_BIN")
	if bin == "" {
		t.Skip("POLARIS_SINGBOX_BIN not set")
	}
	dir := t.TempDir()
	port := freePort(t)
	cfg := fmt.Sprintf(`{"log":{"level":"info"},"inbounds":[{"type":"mixed","tag":"in","listen":"127.0.0.1","listen_port":%d,
		"users":[{"username":"alice","password":"pw1"}]}],"outbounds":[{"type":"direct","tag":"direct"}]}`, port)
	path := filepath.Join(dir, "c.json")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(core.Options{Version: "1.13.5", WorkDir: dir})
	p := core.NewProcess("sing-box", bin, dir, slog.Default(), "run", "-c", path, "-D", dir)
	p.Filter = a.online.line
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer p.Stop(t.Context())

	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer web.Close()
	proxy, _ := url.Parse(fmt.Sprintf("http://alice:pw1@127.0.0.1:%d", port))
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}, Timeout: 5 * time.Second}
	// The proxy may need a moment to listen.
	var err error
	for i := 0; i < 20; i++ {
		var resp *http.Response
		if resp, err = c.Get(web.URL); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the log lines reach the pipe
	got, _ := a.OnlineIPs(t.Context())
	if ips := got["alice"]; len(ips) != 1 || ips[0] != "127.0.0.1" {
		t.Fatalf("online = %v; log:\n%s", got, strings.Join(p.Logs(20), "\n"))
	}
	for _, l := range p.Logs(50) {
		if strings.Contains(l, "inbound connection") {
			t.Fatalf("connection lines should not be kept in the log buffer: %q", l)
		}
	}
}

func TestOnlineSnellUDPUser(t *testing.T) {
	tr := newOnlineTracker()
	tr.line("INFO [77 0ms] inbound/snell[snell]: inbound connection from 192.0.2.1:4567")
	tr.line("INFO [77 1ms] inbound/snell[snell]: [alice] inbound packet connection from 192.0.2.1:4567")
	got := tr.online(time.Now())
	if len(got["alice"]) != 1 || got["alice"][0] != "192.0.2.1" {
		t.Fatal("Snell UDP user/source identity lost", got)
	}
}
