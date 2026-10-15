package panel_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cuijianzhuang/polaris/internal/agent"
	"github.com/cuijianzhuang/polaris/internal/panel"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// syncBuf is a goroutine-safe log sink for child processes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) tail(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := strings.Split(strings.TrimRight(s.b.String(), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// startProc starts a client core in the background and kills it on cleanup.
func startProc(t *testing.T, logs *syncBuf, bin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return cmd
}

// curlVia fetches target through a local socks5 proxy, retrying while the
// client core starts. It returns the body of the last attempt and curl's stderr.
func curlVia(proxyPort int, target string, attempts int) (string, string) {
	var body, errOut string
	for i := 0; i < attempts; i++ {
		var so, se bytes.Buffer
		// --noproxy "" plus a clean env: the sandbox's NO_PROXY lists
		// 127.0.0.1, which would otherwise make curl bypass the socks proxy.
		cmd := exec.Command("curl", "-sS", "-m", "5", "--noproxy", "", "--socks5-hostname", fmt.Sprintf("127.0.0.1:%d", proxyPort), target)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
		cmd.Stdout, cmd.Stderr = &so, &se
		cmd.Run()
		body, errOut = so.String(), se.String()
		if body != "" {
			return body, errOut
		}
		time.Sleep(700 * time.Millisecond)
	}
	return body, errOut
}

func waitListen(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// TestSubscriptionMatrix runs a panel plus a real agent with xray and
// sing-box, creates one inbound per protocol/transport/security combination
// and connects through every node of every subscription format with real
// client cores (sing-box and mihomo).
func TestSubscriptionMatrix(t *testing.T) {
	xrayBin, sbBin, mihomoBin := os.Getenv("POLARIS_XRAY_BIN"), os.Getenv("POLARIS_SINGBOX_BIN"), os.Getenv("POLARIS_MIHOMO_BIN")
	if xrayBin == "" || sbBin == "" || mihomoBin == "" {
		t.Skip("POLARIS_XRAY_BIN, POLARIS_SINGBOX_BIN and POLARIS_MIHOMO_BIN must be set")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not found")
	}
	dir := t.TempDir()
	httpPort, agentPort := freePort(t), freePort(t)
	cfg := &panel.Config{
		Listen: fmt.Sprintf("127.0.0.1:%d", httpPort), AgentListen: fmt.Sprintf("127.0.0.1:%d", agentPort),
		AgentHosts: []string{"127.0.0.1"}, PublicURL: fmt.Sprintf("http://127.0.0.1:%d", httpPort),
		AdminPath: "/secret", DataDir: filepath.Join(dir, "panel"), SecretKey: "test-secret", SubRateLimit: 600,
	}
	cfg.DB.DSN = filepath.Join(dir, "panel", "p.db")
	cfg.InitialAdmin.Username, cfg.InitialAdmin.Password = "admin", "admin-password"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("POLARIS_E2E_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	srv, err := panel.New(cfg, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)

	a := &api{t: t, base: fmt.Sprintf("http://127.0.0.1:%d/secret/api/v1", httpPort)}
	waitFor(t, "panel up", 5*time.Second, func() bool {
		r, err := http.Get(a.base + "/info")
		if err == nil {
			r.Body.Close()
		}
		return err == nil
	})
	var login struct{ Token string }
	if a.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "admin-password"}, &login) != 200 {
		t.Fatal("login failed")
	}
	a.token = login.Token

	const xrayVer, sbVer = "26.3.27", "1.13.5"
	var created struct {
		Node  struct{ ID string }
		Token string
	}
	a.do("POST", "/nodes", map[string]any{"name": "lab", "address": "127.0.0.1", "region": "Tokyo", "flag": "jp",
		"cores": []map[string]string{{"type": "xray", "version": xrayVer}, {"type": "singbox", "version": sbVer}}}, &created)
	if created.Token == "" {
		t.Fatal("node not created")
	}
	agentDir := filepath.Join(dir, "agent")
	xDir := filepath.Join(agentDir, "cores", "xray", xrayVer)
	sDir := filepath.Join(agentDir, "cores", "singbox", sbVer)
	os.MkdirAll(xDir, 0o755)
	os.MkdirAll(sDir, 0o755)
	copyFile(t, xrayBin, filepath.Join(xDir, "xray"))
	for _, f := range []string{"geoip.dat", "geosite.dat"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(xrayBin), f)); err == nil {
			copyFile(t, filepath.Join(filepath.Dir(xrayBin), f), filepath.Join(xDir, f))
		}
	}
	copyFile(t, sbBin, filepath.Join(sDir, "sing-box"))

	var settings map[string]any
	a.do("GET", "/settings", nil, &settings)
	panelURL := fmt.Sprintf("https://127.0.0.1:%d", agentPort)
	if _, err := agent.Enroll(ctx, panelURL, created.Token, settings["ca_sha256"].(string), agentDir, "test"); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Config{PanelURL: panelURL, DataDir: agentDir, AuthListen: fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		Heartbeat: time.Second, Traffic: time.Second, APIPortBase: freePort(t), DisableIPDetect: true}, log)
	if err != nil {
		t.Fatal(err)
	}
	actx, acancel := context.WithCancel(ctx)
	agentDone := make(chan struct{})
	go func() { ag.Run(actx); close(agentDone) }()
	defer func() { acancel(); <-agentDone }()

	nodeState := func() (status string, desired, applied float64, raw map[string]any) {
		var n map[string]any
		a.do("GET", "/nodes/"+created.Node.ID, nil, &n)
		st, _ := n["status"].(string)
		d, _ := n["desiredRev"].(float64)
		ap, _ := n["appliedRev"].(float64)
		return st, d, ap, n
	}
	waitFor(t, "node online", 15*time.Second, func() bool { s, _, _, _ := nodeState(); return s == "online" })

	// REALITY camouflage target: local TLS server with HTTP/2 and a large cert.
	sans := []string{"example.com", "www.example.com"}
	for i := 0; i < 200; i++ {
		sans = append(sans, fmt.Sprintf("host%03d.example.com", i))
	}
	certPEM, keyPEM, err := pki.SelfSigned(sans, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	kp, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	realityTarget := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("camouflage"))
	}))
	realityTarget.EnableHTTP2 = true
	realityTarget.TLS = &tls.Config{Certificates: []tls.Certificate{kp}}
	realityTarget.StartTLS()
	defer realityTarget.Close()
	realityDest := realityTarget.Listener.Addr().String()

	// Destination behind the proxies.
	const okBody = "polaris-matrix-ok-7f3a"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(okBody)) }))
	defer target.Close()

	// Negative control: curl must really go through the socks port.
	if body, _ := curlVia(freePort(t), target.URL, 1); body != "" {
		t.Fatalf("curl reached the target without a proxy (body %q): connectivity checks would be meaningless", body)
	}

	var group struct{ ID string }
	a.do("POST", "/groups", map[string]any{"name": "matrix"}, &group)

	reality := map[string]any{"type": "reality", "realityDest": realityDest, "realityServerNames": []string{"example.com"}}
	tlsSec := map[string]any{"type": "tls"}
	type ib struct {
		tag, core string
		udp       bool
		proto     map[string]any
		trans     map[string]any
		sec       map[string]any
	}
	cases := []ib{
		{tag: "x-vless-vision-reality", core: "xray", proto: map[string]any{"type": "vless", "flow": "xtls-rprx-vision"},
			trans: map[string]any{"type": "tcp"}, sec: reality},
		{tag: "x-vless-ws-tls", core: "xray", proto: map[string]any{"type": "vless"},
			trans: map[string]any{"type": "ws", "path": "/vlws"}, sec: tlsSec},
		{tag: "x-vmess-ws", core: "xray", proto: map[string]any{"type": "vmess"},
			trans: map[string]any{"type": "ws", "path": "/vmws"}, sec: map[string]any{"type": "none"}},
		{tag: "x-trojan-tcp-tls", core: "xray", proto: map[string]any{"type": "trojan"},
			trans: map[string]any{"type": "tcp"}, sec: tlsSec},
		{tag: "x-trojan-grpc-tls", core: "xray", proto: map[string]any{"type": "trojan"},
			trans: map[string]any{"type": "grpc", "serviceName": "trgrpc"}, sec: tlsSec},
		{tag: "x-ss2022", core: "xray", proto: map[string]any{"type": "shadowsocks", "method": "2022-blake3-aes-128-gcm"},
			trans: map[string]any{"type": "tcp"}, sec: map[string]any{"type": "none"}},
		{tag: "x-vless-xhttp-tls", core: "xray", proto: map[string]any{"type": "vless"},
			trans: map[string]any{"type": "xhttp", "path": "/xh"}, sec: tlsSec},
		{tag: "x-vmess-tcp-http", core: "xray", proto: map[string]any{"type": "vmess"},
			trans: map[string]any{"type": "tcp", "headerType": "http"}, sec: map[string]any{"type": "none"}},
		{tag: "s-hy2", core: "singbox", udp: true, proto: map[string]any{"type": "hysteria2"}, sec: tlsSec},
		{tag: "s-hy2-obfs", core: "singbox", udp: true, proto: map[string]any{"type": "hysteria2", "obfs": "salamander", "obfsPassword": "obfs-secret-123"}, sec: tlsSec},
		{tag: "s-tuic", core: "singbox", udp: true, proto: map[string]any{"type": "tuic", "congestionControl": "bbr"}, sec: tlsSec},
		{tag: "s-anytls", core: "singbox", proto: map[string]any{"type": "anytls"}, sec: tlsSec},
		{tag: "s-vless-reality", core: "singbox", proto: map[string]any{"type": "vless", "flow": "xtls-rprx-vision"},
			trans: map[string]any{"type": "tcp"}, sec: reality},
	}
	ports := map[string]int{}
	type inboundRef struct{ tag, id, core string }
	inboundByPort := map[int]inboundRef{}
	// serverSide describes the inbound behind a client port: tag plus the
	// native server config Polaris rendered for it.
	serverSide := func(port any) string {
		var p int
		switch v := port.(type) {
		case float64:
			p = int(v)
		case int:
			p = v
		}
		ref, ok := inboundByPort[p]
		if !ok {
			return fmt.Sprintf("(no inbound on port %v)", port)
		}
		var prev struct{ Config string }
		a.do("GET", "/inbounds/"+ref.id+"/preview", nil, &prev)
		conf := prev.Config
		var full struct {
			Inbounds []map[string]any `json:"inbounds"`
		}
		if json.Unmarshal([]byte(prev.Config), &full) == nil {
			for _, in := range full.Inbounds {
				if in["tag"] == ref.tag {
					b, _ := json.MarshalIndent(in, "", "  ")
					conf = string(b)
				}
			}
		}
		var cores []map[string]any
		a.do("GET", "/nodes/"+created.Node.ID+"/cores", nil, &cores)
		logs := ""
		for _, c := range cores {
			if c["type"] == ref.core {
				var res struct{ Logs string }
				a.do("GET", fmt.Sprintf("/cores/%v/logs?lines=20", c["id"]), nil, &res)
				logs = res.Logs
			}
		}
		hint := ""
		if strings.Contains(logs, "accepted tcp:"+target.Listener.Addr().String()+" ["+ref.tag+" ") {
			hint = "NOTE: the server authenticated the client and dispatched to the target, so the protocol handshake works; " +
				"the reply never arrived (xray >= 26.10 freedom blackholes private/loopback targets by default).\n"
		}
		return fmt.Sprintf("%sserver inbound %s (%s):\n%s\nserver core log tail:\n%s", hint, ref.tag, ref.core, conf, logs)
	}
	var tcpPorts []int
	for _, c := range cases {
		port := freePort(t)
		body := map[string]any{"nodeId": created.Node.ID, "core": c.core, "tag": c.tag, "port": port, "listen": "127.0.0.1",
			"protocol": c.proto, "security": c.sec, "groupIds": []string{group.ID}}
		if c.trans != nil {
			body["transport"] = c.trans
		}
		var out struct {
			Core    string
			Inbound struct{ ID string }
		}
		var raw json.RawMessage
		code := a.do("POST", "/inbounds", body, &raw)
		json.Unmarshal(raw, &out)
		if code != 201 {
			t.Errorf("create inbound %s: HTTP %d %s", c.tag, code, raw)
			continue
		}
		if out.Core != c.core {
			t.Errorf("inbound %s: assigned core %q, want %q", c.tag, out.Core, c.core)
		}
		ports[c.tag] = port
		inboundByPort[port] = inboundRef{c.tag, out.Inbound.ID, c.core}
		if !c.udp {
			tcpPorts = append(tcpPorts, port)
		}
	}
	var user map[string]any
	a.do("POST", "/users", map[string]any{"username": "matrix", "groupIds": []string{group.ID}}, &user)

	// Wait for the node to apply everything.
	deadline := time.Now().Add(40 * time.Second)
	for {
		st, d, ap, raw := nodeState()
		if d > 0 && d == ap {
			break
		}
		if time.Now().After(deadline) {
			var revs []map[string]any
			a.do("GET", "/nodes/"+created.Node.ID+"/revisions", nil, &revs)
			rb, _ := json.MarshalIndent(revs, "", " ")
			nb, _ := json.Marshal(raw)
			t.Fatalf("node never applied revision (status %s desired %v applied %v)\nnode: %s\nrevisions: %s", st, d, ap, nb, rb)
		}
		time.Sleep(300 * time.Millisecond)
	}
	for _, p := range tcpPorts {
		if !waitListen(p, 10*time.Second) {
			t.Errorf("port %d is not listening after apply", p)
		}
	}
	time.Sleep(time.Second) // QUIC listeners have no TCP probe

	subURL := fmt.Sprintf("http://127.0.0.1:%d/sub/%s", httpPort, user["subToken"])
	work := t.TempDir()

	// ---- links ----
	linksB64, _ := get(t, subURL+"?format=links", "v2rayN/7.0")
	linksRaw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(linksB64))
	if err != nil {
		t.Errorf("links subscription is not base64: %v", err)
		linksRaw = []byte(linksB64)
	}
	t.Logf("links:\n%s", linksRaw)
	// ---- clash ----
	clashYAML, _ := get(t, subURL+"?format=clash", "clash-verge/1.0")
	clashFile := filepath.Join(work, "clash.yaml")
	os.WriteFile(clashFile, []byte(clashYAML), 0o644)
	// ---- sing-box ----
	sbJSON, _ := get(t, subURL+"?format=singbox", "SFA/1.11")
	sbFile := filepath.Join(work, "singbox.json")
	os.WriteFile(sbFile, []byte(sbJSON), 0o644)

	// Whole-config validation, offline.
	t.Run("validate/clash", func(t *testing.T) {
		d := t.TempDir()
		cmd := exec.Command(mihomoBin, "-t", "-d", d, "-f", clashFile)
		out, err := runWithTimeout(cmd, 60*time.Second)
		if err != nil || !strings.Contains(out, "test is successful") {
			t.Errorf("mihomo -t on the clash subscription failed: %v\n%s", err, lastLines(out, 30))
		}
	})
	t.Run("validate/singbox", func(t *testing.T) {
		cmd := exec.Command(sbBin, "check", "-c", sbFile)
		out, err := runWithTimeout(cmd, 60*time.Second)
		if err != nil {
			t.Errorf("sing-box check on the sing-box subscription failed: %v\n%s", err, lastLines(out, 30))
		}
	})

	// sing-box client run with exactly one outbound.
	runSingbox := func(t *testing.T, name string, ob map[string]any) {
		port := freePort(t)
		conf := map[string]any{
			"log":       map[string]any{"level": "debug"},
			"inbounds":  []any{map[string]any{"type": "mixed", "tag": "in", "listen": "127.0.0.1", "listen_port": port}},
			"outbounds": []any{ob},
		}
		b, _ := json.MarshalIndent(conf, "", "  ")
		f := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(f, b, 0o644)
		obj, _ := json.MarshalIndent(ob, "", "  ")
		if out, err := runWithTimeout(exec.Command(sbBin, "check", "-c", f), 30*time.Second); err != nil {
			t.Errorf("sing-box rejects outbound %s: %v\n%s\noutbound: %s", name, err, lastLines(out, 30), obj)
			return
		}
		logs := &syncBuf{}
		startProc(t, logs, sbBin, "run", "-c", f)
		if !waitListen(port, 10*time.Second) {
			t.Errorf("sing-box client did not start for %s\n%s", name, logs.tail(30))
			return
		}
		body, cerr := curlVia(port, target.URL, 3)
		if body != okBody {
			t.Errorf("no connectivity via %s: body=%q curl=%q\noutbound: %s\nclient log:\n%s\n%s", name, body, strings.TrimSpace(cerr), obj, logs.tail(30), serverSide(ob["server_port"]))
		}
	}

	// From the sing-box subscription.
	var sbConf struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(sbJSON), &sbConf); err != nil {
		t.Errorf("sing-box subscription is not JSON: %v", err)
	}
	sbCount := 0
	for _, ob := range sbConf.Outbounds {
		switch ob["type"] {
		case "selector", "urltest", "direct", "block", "dns":
			continue
		}
		sbCount++
		ob := ob
		name, _ := ob["tag"].(string)
		t.Run("singbox/"+name, func(t *testing.T) { runSingbox(t, name, ob) })
	}

	// From the links, converted by Polaris' own parser.
	linkCount := 0
	for _, line := range strings.Split(strings.TrimSpace(string(linksRaw)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		linkCount++
		line := line
		n, perr := sub.ParseLink(line)
		name := n.Name
		if perr != nil {
			name = fmt.Sprintf("unparsable-%d", linkCount)
		}
		t.Run("links/"+name, func(t *testing.T) {
			if perr != nil {
				t.Errorf("sub.ParseLink(%q): %v", line, perr)
				return
			}
			ob := sub.SingboxOutbound(n)
			if ob == nil && n.Transport.Type == "xhttp" {
				t.Skip("sing-box has no XHTTP transport")
			}
			if ob == nil && n.Transport.HeaderType == "http" {
				t.Skip("sing-box has no TCP HTTP header camouflage")
			}
			if ob == nil {
				t.Errorf("sub.SingboxOutbound returned nil for %q", line)
				return
			}
			runSingbox(t, name+" (link: "+line+")", ob)
		})
	}

	// From the clash subscription with mihomo.
	var clashConf struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal([]byte(clashYAML), &clashConf); err != nil {
		t.Errorf("clash subscription is not YAML: %v", err)
	}
	for _, p := range clashConf.Proxies {
		p := p
		name := fmt.Sprint(p["name"])
		t.Run("clash/"+name, func(t *testing.T) {
			port := freePort(t)
			conf := map[string]any{
				"mixed-port": port, "allow-lan": false, "mode": "rule", "log-level": "debug", "ipv6": false,
				"geodata-mode": false, "geo-auto-update": false,
				"proxies": []any{p}, "rules": []string{"MATCH," + name},
			}
			b, _ := yaml.Marshal(conf)
			d := t.TempDir()
			f := filepath.Join(d, "config.yaml")
			os.WriteFile(f, b, 0o644)
			pb, _ := yaml.Marshal(p)
			if out, err := runWithTimeout(exec.Command(mihomoBin, "-t", "-d", d, "-f", f), 30*time.Second); err != nil || !strings.Contains(out, "test is successful") {
				t.Errorf("mihomo rejects proxy %s: %v\n%s\nproxy:\n%s", name, err, lastLines(out, 30), pb)
				return
			}
			if p["network"] == "xhttp" {
				// mihomo releases without XHTTP accept the proxy and then
				// fail every connection; only newer clients can use it.
				t.Skip("this mihomo build may lack the XHTTP transport")
			}
			logs := &syncBuf{}
			startProc(t, logs, mihomoBin, "-d", d, "-f", f)
			if !waitListen(port, 15*time.Second) {
				t.Errorf("mihomo did not start for %s\n%s", name, logs.tail(30))
				return
			}
			body, cerr := curlVia(port, target.URL, 3)
			if body != okBody {
				t.Errorf("no connectivity via clash/%s: body=%q curl=%q\nproxy:\n%s\nmihomo log:\n%s\n%s", name, body, strings.TrimSpace(cerr), pb, logs.tail(30), serverSide(p["port"]))
			}
		})
	}

	want := len(ports)
	// sing-box has neither XHTTP nor the TCP HTTP header, so those two
	// nodes are expected to be skipped there.
	if sbCount != want-2 || linkCount != want || len(clashConf.Proxies) != want {
		t.Errorf("node counts: inbounds=%d singbox=%d links=%d clash=%d", want, sbCount, linkCount, len(clashConf.Proxies))
	}
	if t.Failed() {
		// Non-inbound parts of each core's server config (outbounds, routing).
		dumped := map[string]bool{}
		for _, ref := range inboundByPort {
			if dumped[ref.core] {
				continue
			}
			dumped[ref.core] = true
			var prev struct{ Config string }
			a.do("GET", "/inbounds/"+ref.id+"/preview", nil, &prev)
			var full map[string]any
			if json.Unmarshal([]byte(prev.Config), &full) == nil {
				delete(full, "inbounds")
				b, _ := json.MarshalIndent(full, "", "  ")
				t.Logf("%s server config without inbounds:\n%s", ref.core, b)
			}
		}
		var revs []map[string]any
		a.do("GET", "/nodes/"+created.Node.ID+"/revisions", nil, &revs)
		if len(revs) > 0 {
			rb, _ := json.Marshal(revs[0])
			t.Logf("latest revision: %s", rb)
		}
	}
}

func runWithTimeout(cmd *exec.Cmd, d time.Duration) (string, error) {
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(d):
		cmd.Process.Kill()
		<-done
		return buf.String(), fmt.Errorf("timed out after %s", d)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
