package panel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/agent"
	"github.com/cuijianzhuang/polaris/internal/panel"
)

// TestTrafficAccounting pushes real bytes through a node and checks that
// they land on the user, the node and the dashboard, up and down.
func TestTrafficAccounting(t *testing.T) {
	// Parallel: both wait for the same once-a-minute settlement.
	t.Run("vless-tcp", func(t *testing.T) { t.Parallel(); testTraffic(t, false) })
	// Vision splices TLS payloads kernel-side; they must still be counted.
	t.Run("reality-vision", func(t *testing.T) { t.Parallel(); testTraffic(t, true) })
}

func testTraffic(t *testing.T, vision bool) {
	xrayBin := os.Getenv("POLARIS_XRAY_BIN")
	if xrayBin == "" {
		t.Skip("POLARIS_XRAY_BIN not set")
	}
	ver := os.Getenv("POLARIS_XRAY_VERSION") // the binary's version, for version-gated config
	if ver == "" {
		ver = "26.3.27"
	}
	dir := t.TempDir()
	panelPorts := distinctPorts(t, 2) // reserved together: two freePort calls can return the same port
	httpPort, agentPort := panelPorts[0], panelPorts[1]
	cfg := &panel.Config{
		Listen: fmt.Sprintf("127.0.0.1:%d", httpPort), AgentListen: fmt.Sprintf("127.0.0.1:%d", agentPort),
		AgentHosts: []string{"127.0.0.1"}, PublicURL: fmt.Sprintf("http://127.0.0.1:%d", httpPort),
		AdminPath: "/secret", DataDir: filepath.Join(dir, "panel"), SecretKey: "test-secret", SubRateLimit: 600,
	}
	cfg.DB.DSN = filepath.Join(dir, "panel", "p.db")
	cfg.InitialAdmin.Username, cfg.InitialAdmin.Password = "admin", "admin-password"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
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
	a.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "admin-password"}, &login)
	a.token = login.Token

	var created struct {
		Node  struct{ ID string }
		Token string
	}
	a.do("POST", "/nodes", map[string]any{"name": "n1", "address": "127.0.0.1",
		"cores": []map[string]string{{"type": "xray", "version": ver}}}, &created)
	agentDir := filepath.Join(dir, "agent")
	coreDir := filepath.Join(agentDir, "cores", "xray", ver)
	os.MkdirAll(coreDir, 0o755)
	copyFile(t, xrayBin, filepath.Join(coreDir, "xray"))
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

	applied := func() bool {
		var n map[string]any
		a.do("GET", "/nodes/"+created.Node.ID, nil, &n)
		return n["status"] == "online" && n["desiredRev"].(float64) > 0 && n["desiredRev"] == n["appliedRev"]
	}
	var group struct{ ID string }
	a.do("POST", "/groups", map[string]any{"name": "g"}, &group)
	inPort := freePort(t)
	// REALITY borrows the handshake of a local TLS 1.3 site.
	decoy := httptest.NewTLSServer(http.NotFoundHandler())
	defer decoy.Close()
	ib := map[string]any{"nodeId": created.Node.ID, "tag": "plain", "port": inPort, "listen": "127.0.0.1",
		"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "tcp"},
		"security": map[string]any{"type": "none"}, "groupIds": []string{group.ID}}
	if vision {
		ib["protocol"] = map[string]any{"type": "vless", "flow": "xtls-rprx-vision"}
		ib["security"] = map[string]any{"type": "reality", "realityDest": strings.TrimPrefix(decoy.URL, "https://"),
			"realityServerNames": []string{"example.com"}}
	}
	a.do("POST", "/inbounds", ib, nil)
	waitFor(t, "inbound applied", 20*time.Second, applied)
	var user map[string]any
	a.do("POST", "/users", map[string]any{"username": "bob", "groupIds": []string{group.ID}}, &user)
	time.Sleep(time.Second)
	waitFor(t, "user applied", 20*time.Second, applied)
	if !waitListen(inPort, 10*time.Second) {
		t.Fatal("inbound not listening")
	}

	// Payload server: 3 MiB down. Vision only splices TLS, so serve HTTPS then.
	payload := strings.Repeat("x", 3<<20)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, payload)
	})
	target := httptest.NewServer(h)
	if vision {
		target.Close()
		target = httptest.NewTLSServer(h)
	}
	defer target.Close()
	stream := map[string]any{"network": "tcp", "security": "none"}
	vuser := map[string]any{"id": user["uuid"], "encryption": "none"}
	if vision {
		var subs struct{ Links []struct{ URI string } }
		a.do("GET", "/users/"+user["id"].(string)+"/links", nil, &subs)
		link := ""
		for _, l := range subs.Links {
			if strings.HasPrefix(l.URI, "vless://") {
				link = l.URI
			}
		}
		q, _ := url.Parse(link)
		if q == nil {
			t.Fatalf("no vless link: %v", subs)
		}
		v := q.Query()
		stream = map[string]any{"network": "tcp", "security": "reality", "realitySettings": map[string]any{
			"serverName": v.Get("sni"), "publicKey": v.Get("pbk"), "shortId": v.Get("sid"), "fingerprint": "chrome"}}
		vuser["flow"] = "xtls-rprx-vision"
	}

	// Client: xray with a socks inbound and a vless outbound to the node.
	socks := freePort(t)
	client := map[string]any{
		"log":      map[string]any{"loglevel": "warning"},
		"inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": socks, "protocol": "socks", "settings": map[string]any{"udp": true}}},
		"outbounds": []any{map[string]any{"protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{
			"address": "127.0.0.1", "port": inPort, "users": []any{vuser}}}},
			"streamSettings": stream}},
	}
	cb, _ := json.Marshal(client)
	cpath := filepath.Join(dir, "client.json")
	os.WriteFile(cpath, cb, 0o600)
	logs := &syncBuf{}
	startProc(t, logs, xrayBin, "run", "-c", cpath)
	if !waitListen(socks, 10*time.Second) {
		t.Fatalf("client: %s", logs.tail(20))
	}
	var body, errOut string
	for i := 0; i < 10 && len(body) != len(payload); i++ {
		var so, se strings.Builder
		cmd := exec.Command("curl", "-sSk", "-m", "10", "--noproxy", "", "--socks5-hostname", fmt.Sprintf("127.0.0.1:%d", socks), "-d", "hello", target.URL)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
		cmd.Stdout, cmd.Stderr = &so, &se
		cmd.Run()
		body, errOut = so.String(), se.String()
		if len(body) != len(payload) {
			time.Sleep(700 * time.Millisecond)
		}
	}
	if len(body) != len(payload) {
		t.Fatalf("download through node failed (%d bytes): %s %s", len(body), errOut, logs.tail(20))
	}

	// Settlement runs every minute.
	var got map[string]any
	waitFor(t, "user traffic settled", 90*time.Second, func() bool {
		a.do("GET", "/users/"+user["id"].(string), nil, &got)
		return got["usedDown"].(float64) > 0
	})
	if got["usedDown"].(float64) < float64(len(payload)) || got["usedUp"].(float64) <= 0 {
		t.Fatalf("user traffic: up %v down %v, want down >= %d", got["usedUp"], got["usedDown"], len(payload))
	}
	var ov struct {
		Today struct{ Up, Down float64 }
	}
	a.do("GET", "/stats/overview", nil, &ov)
	if ov.Today.Down < float64(len(payload)) {
		t.Fatalf("dashboard today: %+v", ov.Today)
	}
	var tags []map[string]any
	a.do("GET", "/stats/tags?kind=inbound", nil, &tags)
	if len(tags) == 0 {
		t.Fatal("no inbound traffic recorded")
	}
	var nt map[string]struct{ TodayDown, MonthDown, RawDown float64 }
	a.do("GET", "/stats/nodes", nil, &nt)
	if n := nt[created.Node.ID]; n.TodayDown < float64(len(payload)) || n.MonthDown < n.TodayDown || n.RawDown < float64(len(payload)) {
		t.Fatalf("node traffic: %+v", nt)
	}
}
