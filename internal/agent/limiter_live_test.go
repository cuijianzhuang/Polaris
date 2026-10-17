package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/core/mihomo"
	"github.com/cuijianzhuang/polaris/internal/core/singbox"
	"github.com/cuijianzhuang/polaris/internal/core/xray"
	"github.com/cuijianzhuang/polaris/internal/model"
)

// TestLimiterLive* run a real core with one limited and one unlimited user:
// the limited user's transfers follow its rate (also after a rate change),
// UDP passes, the other user is untouched, per-user routing rules still
// apply after the detour, and (Xray) traffic is counted once.
func TestLimiterLiveXray(t *testing.T) {
	ver := os.Getenv("POLARIS_XRAY_VERSION")
	if ver == "" {
		ver = "26.3.27"
	}
	limiterLive(t, model.CoreXray, os.Getenv("POLARIS_XRAY_BIN"), ver, func(o core.Options) core.CoreAdapter { return xray.New(o) }, "xray.json",
		func(cfg []byte) []byte {
			var m map[string]any
			json.Unmarshal(cfg, &m)
			for _, o := range m["outbounds"].([]any) { // recent Xray refuses private targets by default
				if ob := o.(map[string]any); ob["protocol"] == "freedom" {
					ob["settings"] = map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.0/8"}}}}
				}
			}
			cfg, _ = json.Marshal(m)
			return cfg
		}, true)
}

func TestLimiterLiveSingbox(t *testing.T) {
	limiterLive(t, model.CoreSingbox, os.Getenv("POLARIS_SINGBOX_BIN"), "1.13.5", func(o core.Options) core.CoreAdapter { return singbox.New(o) }, "singbox.json", nil, false)
}

func TestLimiterLiveMihomo(t *testing.T) {
	limiterLive(t, model.CoreMihomo, os.Getenv("POLARIS_MIHOMO_BIN"), "1.19.15", func(o core.Options) core.CoreAdapter { return mihomo.New(o) }, "mihomo.yaml", nil, false)
}

func limiterLive(t *testing.T, typ model.CoreType, bin, ver string, mk func(core.Options) core.CoreAdapter, cfgName string, patch func([]byte) []byte, countTraffic bool) {
	xbin := os.Getenv("POLARIS_XRAY_BIN") // the clients
	if bin == "" || xbin == "" {
		t.Skip("core binary or POLARIS_XRAY_BIN not set")
	}
	dir := t.TempDir()
	lim := newLimiter(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer lim.close()
	wire := lim.wire(fmt.Sprintf("127.0.0.1:%d", freePortT(t)), fmt.Sprintf("127.0.0.1:%d", freePortT(t)))
	if wire == nil {
		t.Fatal("no limiter")
	}
	api := fmt.Sprintf("127.0.0.1:%d", freePortT(t))
	a := mk(core.Options{Version: ver, BinPath: bin, WorkDir: dir, APIListen: api, Limit: wire,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	users := coretest.Users()
	port := freePortT(t)
	proto := model.ProtoVLESS
	if typ == model.CoreMihomo {
		proto = model.ProtoVMess // mihomo refuses VLESS without TLS
	}
	in := coretest.Inbound(typ, "in", port, model.ProtocolSpec{Type: proto}, model.TransportSpec{}, model.SecuritySpec{})
	in.Listen = "127.0.0.1"
	in.Users = users
	const kbps = 8000 // 1 MB/s
	spec := &model.NodeSpec{
		Inbounds:   []model.Inbound{in},
		UserLimits: []model.UserLimit{{User: "u1", UpKbps: kbps, DownKbps: kbps}},
		// A per-user rule must still match after the detour (as "u1#lim").
		Routes: []model.RouteRule{{Priority: 1, Outbound: "block", Match: model.RouteMatch{Users: []string{"u1"}, IPCIDR: []string{"127.0.0.2/32"}}}},
	}
	lim.setLimits(spec.UserLimits)
	cfg, err := a.Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if patch != nil {
		cfg = patch(cfg)
	}
	if err := a.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	core.AtomicWrite(filepath.Join(dir, cfgName), cfg, 0o600)
	if d := os.Getenv("LIMITER_DUMP"); d != "" {
		os.WriteFile(filepath.Join(d, cfgName), cfg, 0o600)
	}
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(ctx)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))
	bin = xbin

	payload := strings.Repeat("x", 3<<20)
	serve := func(addr string) string {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skip("cannot listen on", addr, err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				n, _ := io.Copy(io.Discard, r.Body)
				fmt.Fprint(w, n)
				return
			}
			io.WriteString(w, payload)
		})}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
		return "http://" + ln.Addr().String() + "/"
	}
	target := serve("127.0.0.1:0")
	blocked := serve("127.0.0.2:0")

	client := func(u model.User) int {
		socks := freePortT(t)
		c := fmt.Sprintf(`{"inbounds":[{"listen":"127.0.0.1","port":%d,"protocol":"socks","settings":{"udp":true}}],"outbounds":[{"protocol":%q,
			"settings":{"vnext":[{"address":"127.0.0.1","port":%d,"users":[{"id":%q,"encryption":"none","security":"auto"}]}]}}]}`, socks, string(proto), port, u.UUID)
		p := filepath.Join(dir, u.ID+".json")
		os.WriteFile(p, []byte(c), 0o600)
		cl := exec.Command(bin, "run", "-c", p)
		if err := cl.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cl.Process.Kill() })
		waitTCP(t, fmt.Sprintf("127.0.0.1:%d", socks))
		return socks
	}
	s1, s2 := client(users[0]), client(users[1])
	get := func(socks int, url string, upload int) (int, time.Duration, error) {
		args := []string{"-sS", "-m", "30", "--noproxy", "", "--socks5-hostname", fmt.Sprintf("127.0.0.1:%d", socks)}
		if upload > 0 {
			args = append(args, "--data-binary", "@-")
		}
		cmd := exec.Command("curl", append(args, url)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		if upload > 0 {
			cmd.Stdin = strings.NewReader(strings.Repeat("y", upload))
		}
		start := time.Now()
		out, err := cmd.Output()
		return len(out), time.Since(start), err
	}

	// Unlimited user: fast.
	n, d, err := get(s2, target, 0)
	if err != nil || n != len(payload) {
		t.Fatalf("u2 download: %d bytes, %v", n, err)
	}
	if d > 1500*time.Millisecond {
		t.Errorf("unlimited user took %v", d)
	}
	// Limited user: 3 MB at 1 MB/s ≈ 3 s (the burst shaves a little).
	lim.stats()
	n, d, err = get(s1, target, 0)
	if st := lim.stats(); len(st) != 1 || st[0].DownBps < 6_000_000 || st[0].DownBps > 10_000_000 {
		t.Errorf("reported speed during a 8 Mbps download: %+v", st)
	}
	if err != nil || n != len(payload) {
		t.Fatalf("u1 download: %d bytes, %v", n, err)
	}
	if d < 2400*time.Millisecond || d > 4500*time.Millisecond {
		t.Errorf("limited user at 1 MB/s took %v for 3 MB", d)
	}
	t.Logf("u2 %v, u1 %v", "fast", d)
	// Upload is limited too.
	_, d, err = get(s1, target, 2<<20)
	if err != nil {
		t.Fatalf("u1 upload: %v", err)
	}
	if d < 1500*time.Millisecond {
		t.Errorf("limited upload of 2 MB took only %v", d)
	}
	// The per-user block rule still applies to u1 only.
	if n, _, _ := get(s1, blocked, 0); n == len(payload) {
		t.Error("u1 reached the blocked address through the limiter")
	}
	if n, _, err := get(s2, blocked, 0); n != len(payload) {
		t.Errorf("u2 blocked: %d bytes, %v", n, err)
	}
	// UDP goes through the limiter too (SOCKS5 UDP ASSOCIATE on both legs).
	if got := udpEcho(t, s1, "limited"); got != "limited" {
		t.Errorf("UDP through the limiter: %q", got)
	}
	// Raising the rate applies at once.
	lim.setLimits([]model.UserLimit{{User: "u1", UpKbps: kbps * 6, DownKbps: kbps * 6}})
	n, d, _ = get(s1, target, 0)
	if n != len(payload) || d > 1500*time.Millisecond {
		t.Errorf("after raising the rate: %d bytes in %v", n, d)
	}

	if !countTraffic {
		return
	}
	// Traffic: u1's downloads counted once, the detour ignored.
	ds, err := a.CollectTraffic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var u1 model.TrafficDelta
	for _, x := range ds {
		if x.UserID == "u1" {
			u1 = x
		}
	}
	want := int64(2 * len(payload))
	if u1.Down < want || u1.Down > want+want/5 {
		t.Errorf("u1 down = %d, want about %d (once per download): %+v", u1.Down, want, ds)
	}
	var detour bool
	for _, x := range ds {
		detour = detour || model.IsLimiterCounter(x.UserID)
	}
	if !detour {
		t.Log("no detour counters reported (nothing to filter)")
	}
}

// udpEcho sends one datagram through a no-auth SOCKS5 proxy to a local UDP
// echo server and returns the answer.
func udpEcho(t *testing.T, socks int, msg string) string {
	t.Helper()
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			echo.WriteToUDP(buf[:n], from)
		}
	}()
	ctl, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", socks))
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	ctl.SetDeadline(time.Now().Add(5 * time.Second))
	ctl.Write([]byte{5, 1, 0})
	r := make([]byte, 2)
	io.ReadFull(ctl, r)
	ctl.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	h := make([]byte, 4)
	if _, err := io.ReadFull(ctl, h); err != nil || h[1] != 0 {
		t.Fatalf("associate: %v %v", h, err)
	}
	bnd, err := socksReadAddr(ctl, h[3])
	if err != nil {
		t.Fatal(err)
	}
	relay, err := socksAddrUDP(bnd, fmt.Sprintf("127.0.0.1:%d", socks))
	if err != nil {
		t.Fatal(err)
	}
	u, err := net.DialUDP("udp", nil, relay)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	ea := echo.LocalAddr().(*net.UDPAddr)
	pkt := append([]byte{0, 0, 0, 1}, ea.IP.To4()...)
	pkt = append(pkt, byte(ea.Port>>8), byte(ea.Port))
	pkt = append(pkt, msg...)
	buf := make([]byte, 2048)
	for i := 0; i < 5; i++ {
		u.Write(pkt)
		u.SetReadDeadline(time.Now().Add(time.Second))
		n, err := u.Read(buf)
		if err == nil && n > 10 {
			return string(buf[10:n])
		}
	}
	return ""
}

func waitTCP(t *testing.T, addr string) {
	t.Helper()
	for i := 0; ; i++ {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		if i == 60 {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
