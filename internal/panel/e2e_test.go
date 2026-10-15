package panel_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/agent"
	"github.com/cuijianzhuang/polaris/internal/panel"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type api struct {
	t     *testing.T
	base  string
	token string
}

func (a *api) do(method, path string, body any, out any) int {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			a.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	if resp.StatusCode >= 400 {
		a.t.Logf("%s %s -> %d %s", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode
}

func waitFor(t *testing.T, what string, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestEndToEnd runs the panel, enrols a real agent and drives Xray through
// inbound creation, hot user updates and subscription rendering.
func TestEndToEnd(t *testing.T) {
	xrayBin := os.Getenv("POLARIS_XRAY_BIN")
	if xrayBin == "" {
		t.Skip("POLARIS_XRAY_BIN not set")
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
	// Unknown paths outside the admin path are a plain 404.
	if r, _ := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/info", httpPort)); r.StatusCode != 404 {
		t.Fatalf("expected decoy 404, got %d", r.StatusCode)
	}
	var login struct{ Token string }
	if a.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "admin-password"}, &login) != 200 {
		t.Fatal("login failed")
	}
	a.token = login.Token

	// Create node with Xray, enrol a real agent.
	var created struct {
		Node    struct{ ID string }
		Token   string
		Command string
	}
	a.do("POST", "/nodes", map[string]any{"name": "tokyo", "address": "127.0.0.1", "region": "Tokyo", "flag": "jp",
		"cores": []map[string]string{{"type": "xray", "version": "26.3.27"}}}, &created)
	if created.Token == "" || !strings.Contains(created.Command, "--ca-sha256") {
		t.Fatalf("bad install command: %+v", created)
	}
	agentDir := filepath.Join(dir, "agent")
	coreDir := filepath.Join(agentDir, "cores", "xray", "26.3.27")
	os.MkdirAll(coreDir, 0o755)
	copyFile(t, xrayBin, filepath.Join(coreDir, "xray"))
	for _, f := range []string{"geoip.dat", "geosite.dat"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(xrayBin), f)); err == nil {
			copyFile(t, filepath.Join(filepath.Dir(xrayBin), f), filepath.Join(coreDir, f))
		}
	}
	var settings map[string]any
	a.do("GET", "/settings", nil, &settings)
	panelURL := fmt.Sprintf("https://127.0.0.1:%d", agentPort)
	if _, err := agent.Enroll(ctx, panelURL, created.Token, "00", agentDir, "test"); err == nil {
		t.Fatal("enrolment with a wrong CA pin must fail")
	}
	if _, err := agent.Enroll(ctx, panelURL, created.Token, settings["ca_sha256"].(string), agentDir, "test"); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Config{PanelURL: panelURL, DataDir: agentDir, AuthListen: fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		Heartbeat: time.Second, Traffic: time.Second, APIPortBase: freePort(t)}, log)
	if err != nil {
		t.Fatal(err)
	}
	actx, acancel := context.WithCancel(ctx)
	agentDone := make(chan struct{})
	go func() { ag.Run(actx); close(agentDone) }()
	defer func() { acancel(); <-agentDone }()

	nodeState := func() (status string, desired, applied float64) {
		var n map[string]any
		a.do("GET", "/nodes/"+created.Node.ID, nil, &n)
		return n["status"].(string), n["desiredRev"].(float64), n["appliedRev"].(float64)
	}
	// waitApplied waits for a revision newer than `after` to be applied.
	waitApplied := func(what string, after float64) float64 {
		var rev float64
		waitFor(t, what, 15*time.Second, func() bool {
			_, d, ap := nodeState()
			rev = d
			return d > after && d == ap
		})
		return rev
	}
	waitFor(t, "node online", 10*time.Second, func() bool { s, _, _ := nodeState(); return s == "online" })

	// Group + inbound + user.
	var group struct{ ID string }
	a.do("POST", "/groups", map[string]any{"name": "default"}, &group)
	inPort := freePort(t)
	var inb struct {
		Inbound struct{ ID string }
		Core    string
	}
	code := a.do("POST", "/inbounds", map[string]any{"nodeId": created.Node.ID, "tag": "vless-reality", "port": inPort, "listen": "127.0.0.1",
		"protocol": map[string]any{"type": "vless", "flow": "xtls-rprx-vision"}, "transport": map[string]any{"type": "tcp"},
		"security": map[string]any{"type": "reality", "realityDest": "www.example.com:443"}, "groupIds": []string{group.ID}}, &inb)
	if code != 201 || inb.Core != "xray" {
		t.Fatalf("create inbound: %d %+v", code, inb)
	}
	// Port conflict is rejected.
	if a.do("POST", "/inbounds", map[string]any{"nodeId": created.Node.ID, "tag": "dup", "port": inPort,
		"protocol": map[string]any{"type": "trojan"}, "security": map[string]any{"type": "tls"}}, nil) != 400 {
		t.Fatal("port conflict not detected")
	}
	// TUIC needs sing-box, which is not installed.
	if a.do("POST", "/inbounds", map[string]any{"nodeId": created.Node.ID, "tag": "tuic", "port": freePort(t),
		"protocol": map[string]any{"type": "tuic"}}, nil) != 400 {
		t.Fatal("capability check missing")
	}
	waitApplied("inbound applied", 1)
	waitFor(t, "xray listening", 5*time.Second, func() bool {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", inPort))
		if err == nil {
			c.Close()
		}
		return err == nil
	})

	_, before, _ := nodeState()
	var user map[string]any
	a.do("POST", "/users", map[string]any{"username": "alice", "groupIds": []string{group.ID}, "trafficLimit": 10 << 30}, &user)
	waitApplied("user applied", before)
	var revs []map[string]any
	a.do("GET", "/nodes/"+created.Node.ID+"/revisions", nil, &revs)
	if len(revs) < 2 || revs[0]["status"] != "applied" || revs[0]["method"] != "hot" {
		t.Fatalf("adding a user must be a hot update: %+v", revs[0])
	}

	// Subscription in several formats.
	subURL := fmt.Sprintf("http://127.0.0.1:%d/sub/%s", httpPort, user["subToken"])
	body, hdr := get(t, subURL, "v2rayNG/1.9")
	raw, _ := base64.StdEncoding.DecodeString(body)
	if !strings.Contains(string(raw), "vless://"+user["uuid"].(string)) || !strings.Contains(string(raw), "security=reality") {
		t.Fatalf("links: %s", raw)
	}
	if !strings.Contains(hdr.Get("subscription-userinfo"), "total=10737418240") {
		t.Fatalf("userinfo header: %q", hdr.Get("subscription-userinfo"))
	}
	if body, _ := get(t, subURL, "clash-verge/1.0"); !strings.Contains(body, "reality-opts") {
		t.Fatalf("clash: %s", body)
	}
	if body, _ := get(t, subURL, "SFA/1.11"); !strings.Contains(body, `"public_key"`) {
		t.Fatalf("sing-box: %s", body)
	}
	if body, _ := get(t, subURL, "Mozilla/5.0"); !strings.Contains(body, "data:image/png;base64") {
		t.Fatal("browser page missing QR")
	}

	// Disabling the user is a users-only change: hot update via gRPC.
	_, before, _ = nodeState()
	a.do("PUT", "/users/"+user["id"].(string), map[string]any{"username": "alice", "status": "disabled", "groupIds": []string{group.ID}}, nil)
	waitApplied("disable applied", before)
	a.do("GET", "/nodes/"+created.Node.ID+"/revisions", nil, &revs)
	if revs[0]["method"] != "hot" {
		t.Fatalf("removing a user must be a hot update: %+v", revs[0])
	}

	// Native config preview and redacted spec.
	var prev struct{ Config string }
	a.do("GET", "/inbounds/"+inb.Inbound.ID+"/preview", nil, &prev)
	if !strings.Contains(prev.Config, `"realitySettings"`) {
		t.Fatalf("preview: %s", prev.Config)
	}
	var spec map[string]any
	a.do("GET", "/nodes/"+created.Node.ID+"/spec", nil, &spec)
	if b, _ := json.Marshal(spec); strings.Contains(string(b), "PRIVATE KEY") {
		t.Fatal("spec leaks secrets")
	}

	// Overview, audit, templates preview.
	var ov map[string]any
	a.do("GET", "/stats/overview", nil, &ov)
	if ov["nodes"].(map[string]any)["online"].(float64) != 1 {
		t.Fatalf("overview: %+v", ov["nodes"])
	}
	var audit struct{ Total int }
	a.do("GET", "/audit-logs", nil, &audit)
	if audit.Total < 5 {
		t.Fatalf("audit log too short: %d", audit.Total)
	}
	var tpls []map[string]any
	a.do("GET", "/templates", nil, &tpls)
	if len(tpls) != 4 {
		t.Fatalf("expected 4 builtin templates, got %d", len(tpls))
	}
	var plan struct {
		Summary map[string]float64
		Plans   []struct{ Items []map[string]any }
	}
	for _, tp := range tpls {
		if tp["name"] == "全协议" {
			a.do("POST", "/templates/"+tp["id"].(string)+"/preview", map[string]any{"nodeIds": []string{created.Node.ID}, "reassign": true, "autoPort": true}, &plan)
		}
	}
	// Xray-only node: REALITY (443 → fix if taken), XHTTP new, Hy2/TUIC/AnyTLS skipped.
	if plan.Summary["skipped"] != 3 || plan.Summary["create"] != 2 {
		t.Fatalf("template plan: %+v %+v", plan.Summary, plan.Plans)
	}

	// Rollback creates a new revision from an older applied one.
	var rb map[string]any
	if a.do("POST", "/nodes/"+created.Node.ID+"/rollback", map[string]any{}, &rb) != 200 {
		t.Fatalf("rollback: %+v", rb)
	}
	waitApplied("rollback applied", rb["rev"].(float64)-1)
}

func get(t *testing.T, u, ua string) (string, http.Header) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", u, resp.StatusCode, b)
	}
	return string(b), resp.Header
}

func copyFile(t *testing.T, src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}
