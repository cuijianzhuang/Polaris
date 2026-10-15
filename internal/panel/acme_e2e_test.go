package panel_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/agent"
	"github.com/cuijianzhuang/polaris/internal/panel"
)

// TestACMEHTTP01 issues a real certificate from Pebble (Let's Encrypt's test
// CA) over HTTP-01: the panel asks the enrolled agent to answer the
// challenge, and Pebble connects to it to validate. The identifier is an IP
// address, which also exercises the short-lived profile.
func TestACMEHTTP01(t *testing.T) {
	pdir := os.Getenv("POLARIS_PEBBLE_DIR") // pebble binary, config.json, cert.pem, key.pem, pebble.minica.pem
	if pdir == "" {
		t.Skip("POLARIS_PEBBLE_DIR not set")
	}
	dir := t.TempDir()
	// All ports are reserved together: freePort releases its port at once,
	// so separate calls can hand out the same number twice (the HTTP-01
	// port then collides with a listener of this process).
	ports := distinctPorts(t, 7)
	acmePort, challengePort, mgmtPort, httpPort, agentPort, authPort, apiBase := ports[0], ports[1], ports[2], ports[3], ports[4], ports[5], ports[6]
	var pcfg map[string]map[string]any
	raw, _ := os.ReadFile(filepath.Join(pdir, "config.json"))
	if err := json.Unmarshal(raw, &pcfg); err != nil {
		t.Fatal(err)
	}
	p := pcfg["pebble"]
	p["listenAddress"] = fmt.Sprintf("127.0.0.1:%d", acmePort)
	p["managementListenAddress"] = fmt.Sprintf("127.0.0.1:%d", mgmtPort)
	p["certificate"], p["privateKey"] = filepath.Join(pdir, "cert.pem"), filepath.Join(pdir, "key.pem")
	p["httpPort"] = challengePort
	b, _ := json.Marshal(pcfg)
	cfgFile := filepath.Join(dir, "pebble.json")
	os.WriteFile(cfgFile, b, 0o600)
	peb := exec.Command(filepath.Join(pdir, "pebble"), "-config", cfgFile)
	peb.Env = append(os.Environ(), "PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0")
	if testing.Verbose() {
		peb.Stdout, peb.Stderr = os.Stderr, os.Stderr
	}
	if err := peb.Start(); err != nil {
		t.Fatal(err)
	}
	defer peb.Process.Kill()
	t.Setenv("LEGO_CA_CERTIFICATES", filepath.Join(pdir, "pebble.minica.pem"))

	cfg := &panel.Config{
		Listen: fmt.Sprintf("127.0.0.1:%d", httpPort), AgentListen: fmt.Sprintf("127.0.0.1:%d", agentPort),
		AgentHosts: []string{"127.0.0.1"}, PublicURL: fmt.Sprintf("http://127.0.0.1:%d", httpPort),
		AdminPath: "/secret", DataDir: filepath.Join(dir, "panel"), SecretKey: "test-secret", SubRateLimit: 600,
	}
	cfg.DB.DSN = filepath.Join(dir, "panel", "p.db")
	cfg.InitialAdmin.Username, cfg.InitialAdmin.Password = "admin", "admin-password"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
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
	a.do("POST", "/nodes", map[string]any{"name": "n1", "address": "127.0.0.1"}, &created)
	var settings map[string]any
	a.do("GET", "/settings", nil, &settings)
	agentDir := filepath.Join(dir, "agent")
	panelURL := fmt.Sprintf("https://127.0.0.1:%d", agentPort)
	if _, err := agent.Enroll(ctx, panelURL, created.Token, settings["ca_sha256"].(string), agentDir, "test"); err != nil {
		t.Fatal(err)
	}
	ag, err := agent.New(agent.Config{PanelURL: panelURL, DataDir: agentDir, AuthListen: fmt.Sprintf("127.0.0.1:%d", authPort),
		Heartbeat: time.Second, APIPortBase: apiBase, DisableIPDetect: true}, log)
	if err != nil {
		t.Fatal(err)
	}
	actx, acancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { ag.Run(actx); close(done) }()
	defer func() { acancel(); <-done }()
	waitFor(t, "agent online", 15*time.Second, func() bool {
		var n map[string]any
		a.do("GET", "/nodes/"+created.Node.ID, nil, &n)
		return n["status"] == "online"
	})

	if code := a.do("PUT", "/settings", map[string]string{"acme_email": "ops@example.com",
		"acme_directory": fmt.Sprintf("https://127.0.0.1:%d/dir", acmePort), "acme_http_port": fmt.Sprint(challengePort)}, nil); code != 200 && code != 204 {
		t.Fatalf("settings: %d", code)
	}
	// No node given: the panel finds it from the node's address.
	var cert struct {
		ID, Provider, Challenge, NodeID, Cert string
	}
	if code := a.do("POST", "/certificates", map[string]string{"domain": "127.0.0.1", "provider": "acme"}, &cert); code != 201 {
		t.Fatalf("issue: %d", code)
	}
	if cert.Provider != "acme" || cert.Challenge != "http" || cert.NodeID != created.Node.ID {
		t.Fatalf("certificate: %+v", cert)
	}
	blk, _ := pem.Decode([]byte(cert.Cert))
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "127.0.0.1" {
		t.Fatalf("certificate is not for 127.0.0.1: %v %v", leaf.IPAddresses, leaf.DNSNames)
	}
	if life := leaf.NotAfter.Sub(leaf.NotBefore); life > 7*24*time.Hour {
		t.Fatalf("IP certificate should use the short-lived profile, valid for %s", life)
	}
	// Renewal goes through the same challenge and node.
	if code := a.do("POST", "/certificates/"+cert.ID+"/renew", nil, nil); code >= 300 {
		t.Fatalf("renew: %d", code)
	}
}

// distinctPorts reserves n free ports at once, away from each other's
// neighbourhood (the agent uses apiBase+0..+20 for core APIs).
func distinctPorts(t *testing.T, n int) []int {
	var ls []net.Listener
	defer func() {
		for _, l := range ls {
			l.Close()
		}
	}()
	var out []int
	for len(out) < n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		p := l.Addr().(*net.TCPAddr).Port
		near := false
		for _, q := range out {
			if p > q-32 && p < q+32 {
				near = true
			}
		}
		if !near {
			out = append(out, p)
		}
	}
	return out
}
